package app

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/akz142857/Heimdall/internal/adminauth"
	"github.com/akz142857/Heimdall/internal/alert"
	"github.com/akz142857/Heimdall/internal/audit"
	"github.com/akz142857/Heimdall/internal/auth"
	"github.com/akz142857/Heimdall/internal/bearercred"
	"github.com/akz142857/Heimdall/internal/budget"
	"github.com/akz142857/Heimdall/internal/buildinfo"
	"github.com/akz142857/Heimdall/internal/config"
	"github.com/akz142857/Heimdall/internal/domain"
	gatewaycore "github.com/akz142857/Heimdall/internal/gateway"
	"github.com/akz142857/Heimdall/internal/gatewayapi"
	"github.com/akz142857/Heimdall/internal/id"
	"github.com/akz142857/Heimdall/internal/ledger"
	"github.com/akz142857/Heimdall/internal/provider"
	"github.com/akz142857/Heimdall/internal/redaction"
	"github.com/akz142857/Heimdall/internal/sourcelimit"
	boltstore "github.com/akz142857/Heimdall/internal/store/bolt"
	"github.com/akz142857/Heimdall/internal/store/lock"
	"github.com/akz142857/Heimdall/internal/tokenguard"
	"github.com/akz142857/Heimdall/internal/usage"
	"github.com/akz142857/Heimdall/internal/vault"
	"github.com/akz142857/Heimdall/internal/webui"
	"github.com/go-chi/chi/v5"
)

type Runtime struct {
	config              config.Config
	logger              *slog.Logger
	lock                *lock.Lock
	store               *boltstore.Store
	ledger              *ledger.Log
	state               *ledger.State
	status              *ledger.Status
	vault               *vault.Vault
	auth                *auth.Snapshot
	providers           *provider.Registry
	accounting          *budget.Manager
	gateway             *gatewayapi.Handler
	gatewayService      *gatewaycore.Service
	sourceLimiter       *sourcelimit.Limiter
	tokenGuard          *tokenguard.Manager
	redactor            *redaction.Engine
	alerts              *alert.Dispatcher
	audit               *audit.Log
	auditBatchMu        sync.Mutex
	auditBatchPending   []adminAuditRequest
	auditBatchRunning   bool
	gatewayHandlerOnce  sync.Once
	gatewayHandlerValue http.Handler
	adminTopologyMu     sync.Mutex
	providerModelsMu    sync.Mutex
	providerModels      map[string]providerModelCatalogCache
	adminProjectMu      sync.Mutex
	adminAlertMu        sync.Mutex
	adminSettingsMu     sync.Mutex
	adminIdentityMu     sync.Mutex
	metricsTokenHash    [32]byte
	metricsAuthorizer   *bearercred.Authorizer
	metricsScrapes      chan struct{}
	metricsAuthFailed   atomic.Uint64
	metricsBusy         atomic.Uint64
	metricsRenderErrs   atomic.Uint64
	startedAt           time.Time
	// now is the clock the admin rate limiter buckets requests into. Its window
	// is a wall-clock minute, so a test that sprays requests without pinning
	// this depends on whether the spray happens to straddle a minute boundary —
	// which is how the login-spray test failed roughly once in a hundred CI
	// runs while passing every time locally.
	now                 func() time.Time
	kmsRecoveryLastUsed time.Time
	adminSessions       *adminauth.Manager
	adminLoginMu        sync.Mutex
	adminLogin          adminRateState
	adminSetupRateMu    sync.Mutex
	adminSetupRate      adminRateState
	adminStepUpMu       sync.Mutex
	adminStepUp         map[string]adminLoginWindow
	setupMu             sync.Mutex
	setupToken          string
	setupTokenNeeded    bool
	backgroundCtx       context.Context
	backgroundCancel    context.CancelFunc
	backgroundWait      sync.WaitGroup
	usage               *usage.Aggregate
	usageCollector      *usage.Collector
	usageExporter       *usage.Exporter
	periods             *budget.PeriodResolver
	closeOnce           sync.Once
	closeErr            error
	draining            atomic.Bool
	runtimeSettings     atomic.Pointer[domain.RuntimeSettings]
	uiSettings          atomic.Pointer[domain.InstanceUISettings]
	instanceID          string
	anchorAuthorizer    *bearercred.Authorizer
	anchorAuthFailed    atomic.Uint64
	// Anchoring is fail-open on purpose: a witness that cannot be reached must
	// not stop the gateway. That makes it the one subsystem whose total
	// failure is indistinguishable from working, so these have to reach
	// /metrics or nobody finds out for months.
	anchorLastEmitUnix atomic.Int64
	anchorEmitFailures atomic.Uint64
}

func Open(ctx context.Context, cfg config.Config, logger *slog.Logger) (*Runtime, error) {
	dataLock, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Runtime, error) {
		dataLock.Close()
		return nil, err
	}
	kmsAudit := &kmsAuditRecorder{}
	masterKey, err := unlockMasterKey(withKMSAuditRecorder(ctx, kmsAudit), cfg)
	if err != nil {
		return fail(err)
	}
	defer clear(masterKey)
	adminSessionKey, err := vault.DeriveAdminSessionKey(masterKey)
	if err != nil {
		clear(masterKey)
		return fail(err)
	}
	defer clear(adminSessionKey)
	var metricsTokenHash [32]byte
	var metricsAuthorizer *bearercred.Authorizer
	if cfg.Metrics.Enabled && cfg.Metrics.RequireAuth {
		if cfg.Metrics.CredentialFile != "" {
			metricsAuthorizer, err = bearercred.NewAuthorizer(cfg.Metrics.CredentialFile)
			if err != nil {
				return fail(fmt.Errorf("load metrics credentials: %w", err))
			}
		} else {
			metricsToken, err := vault.DeriveMetricsBearerToken(masterKey)
			if err != nil {
				clear(masterKey)
				return fail(err)
			}
			metricsTokenHash = sha256.Sum256(metricsToken)
			clear(metricsToken)
		}
	}
	var anchorAuthorizer *bearercred.Authorizer
	if cfg.Audit.Anchor.Enabled && cfg.Audit.Anchor.Sink == config.AuditAnchorSinkDeadManPull {
		// Config validation already requires a credential file here — unlike
		// metrics, there is no zero-config derived-token fallback, since the
		// anchor endpoint is off by default and turning it on is already a
		// deliberate step.
		anchorAuthorizer, err = bearercred.NewAuthorizer(cfg.Audit.Anchor.CredentialFile)
		if err != nil {
			return fail(fmt.Errorf("load audit anchor credentials: %w", err))
		}
	}
	secretVault, err := vault.New(masterKey)
	if err != nil {
		return fail(err)
	}
	metadata, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		secretVault.Close()
		return fail(err)
	}
	adminCount, err := metadata.AdminUserCount(ctx)
	if err != nil {
		metadata.Close()
		secretVault.Close()
		return fail(fmt.Errorf("inspect admin setup state: %w", err))
	}
	setupToken := ""
	if adminCount == 0 {
		setupToken, err = id.New("setup")
		if err != nil {
			metadata.Close()
			secretVault.Close()
			return fail(err)
		}
	}
	adminSessions, err := adminauth.NewManager(
		metadata,
		adminSessionKey,
		cfg.Admin.SessionTTL.Value(),
		cfg.Admin.IdleTimeout.Value(),
	)
	if err != nil {
		metadata.Close()
		secretVault.Close()
		return fail(err)
	}
	cleanupAdminSessions := true
	defer func() {
		if cleanupAdminSessions {
			adminSessions.Close()
		}
	}()
	if err := verifyVaultKeyCheck(metadata, secretVault); err != nil {
		metadata.Close()
		secretVault.Close()
		return fail(err)
	}
	auditKey, err := loadAuditHMACKey(metadata, secretVault, masterKey)
	if err != nil {
		metadata.Close()
		secretVault.Close()
		return fail(err)
	}
	defer clear(auditKey)
	ledgerKey, err := loadLedgerHMACKey(metadata, secretVault, masterKey)
	if err != nil {
		metadata.Close()
		secretVault.Close()
		return fail(err)
	}
	defer clear(ledgerKey)
	// The gate only guarantees "at least" a reader version, not an exact one:
	// the accounting-timezone work (schema v16) shipped frame epoch 3 without
	// ever moving this gate off epoch 2 (docs/review/260805/progress.md
	// P2-16), which is exactly the bug a strict equality check produces. Migration 17
	// jumped straight to epoch 4 to retroactively cover that gap; requiring
	// only a floor here means the next epoch bump does not need to repeat it.
	compatibilityGate, err := metadata.LedgerCompatibilityGate()
	if err != nil || compatibilityGate.FeatureEpoch < 4 ||
		compatibilityGate.MinimumReaderVersion != fmt.Sprintf("v%d", compatibilityGate.FeatureEpoch) {
		metadata.Close()
		secretVault.Close()
		if err == nil {
			err = fmt.Errorf("unsupported Ledger compatibility gate: %#v", compatibilityGate)
		}
		return fail(err)
	}
	accountingStatus := ledger.NewStatus()
	ledgerOptions := ledger.Options{
		QueueCapacity: cfg.Usage.WALQueueCapacity,
		MaxBatch:      cfg.Usage.WALMaxBatch,
		FlushInterval: cfg.Usage.WALFlushInterval.Value(),
		ChainKey:      ledgerKey,
	}
	if cfg.Usage.Durability == "strict" {
		ledgerOptions.MaxBatch = 1
		ledgerOptions.FlushInterval = 0
	}
	ledgerLog, err := ledger.OpenWithOptions(cfg.LedgerPath(), accountingStatus, ledgerOptions)
	if err != nil {
		metadata.Close()
		secretVault.Close()
		return fail(err)
	}
	ledgerState := ledger.NewState()
	ledgerHead, err := ledgerLog.Replay(ledger.Watermark{}, ledgerState.Apply)
	if err != nil {
		ledgerLog.Close()
		metadata.Close()
		secretVault.Close()
		return fail(fmt.Errorf("replay ledger: %w", err))
	}
	if err := reconcileLedgerChainCheckpoint(metadata, ledgerLog); err != nil {
		ledgerLog.Close()
		metadata.Close()
		secretVault.Close()
		return fail(err)
	}
	usageAggregate, usageWatermark := restoreUsageAggregate(metadata, ledgerHead, logger)
	if _, err := ledgerLog.Replay(usageWatermark, usageAggregate.Apply); err != nil {
		ledgerLog.Close()
		metadata.Close()
		secretVault.Close()
		return fail(fmt.Errorf("replay usage aggregate: %w", err))
	}
	usageExporter, err := usage.NewExporterWithOptions(cfg.UsagePath(), usage.Options{Format: cfg.Usage.ExportFormat})
	if err != nil {
		ledgerLog.Close()
		metadata.Close()
		secretVault.Close()
		return fail(fmt.Errorf("create usage exporter: %w", err))
	}
	authSnapshot := auth.NewSnapshot()
	if err := authSnapshot.Refresh(ctx, metadata); err != nil {
		ledgerLog.Close()
		metadata.Close()
		secretVault.Close()
		return fail(fmt.Errorf("load auth snapshot: %w", err))
	}
	// config.yaml seeds the accounting timezone the first time and has no say
	// afterwards: the stored setting is versioned and audited, and a later edit
	// of the file must not move a boundary out from under a deliberate change.
	accountingSettings, err := metadata.SeedInstanceAccountingSettings(cfg.Usage.Timezone, time.Now())
	if err != nil {
		ledgerLog.Close()
		metadata.Close()
		secretVault.Close()
		return fail(fmt.Errorf("load accounting settings: %w", err))
	}
	periods, err := budget.NewPeriodResolver(accountingSettings)
	if err != nil {
		ledgerLog.Close()
		metadata.Close()
		secretVault.Close()
		return fail(fmt.Errorf("resolve accounting timezone: %w", err))
	}
	accounting, err := budget.New(ledgerLog, ledgerState, periods)
	if err != nil {
		ledgerLog.Close()
		metadata.Close()
		secretVault.Close()
		return fail(fmt.Errorf("create accounting manager: %w", err))
	}
	usageCollector, err := usage.NewCollector(usageAggregate, ledgerLog, cfg.Usage.AnalyticsQueueCapacity)
	if err != nil {
		ledgerLog.Close()
		metadata.Close()
		secretVault.Close()
		return fail(fmt.Errorf("create usage collector: %w", err))
	}
	accounting.AddObserver(usageCollector.Observe)
	if err := metadata.RecoverDeploymentPricePins(ctx, ledgerState); err != nil {
		ledgerLog.Close()
		metadata.Close()
		secretVault.Close()
		return fail(fmt.Errorf("recover deployment price pins: %w", err))
	}
	if err := accounting.RecoverPendingLeases(ctx); err != nil {
		ledgerLog.Close()
		metadata.Close()
		secretVault.Close()
		return fail(fmt.Errorf("recover pending accounting leases: %w", err))
	}
	providerRegistry, err := loadProviderRegistry(ctx, cfg, metadata, secretVault)
	if err != nil {
		ledgerLog.Close()
		metadata.Close()
		secretVault.Close()
		return fail(err)
	}
	tokenGuard, err := loadTokenGuard(ctx, metadata, logger)
	if err != nil {
		providerRegistry.Close()
		ledgerLog.Close()
		metadata.Close()
		secretVault.Close()
		return fail(err)
	}
	redactor, err := loadRedaction(ctx, metadata)
	if err != nil {
		providerRegistry.Close()
		ledgerLog.Close()
		metadata.Close()
		secretVault.Close()
		return fail(err)
	}
	alertDispatcher, err := loadAlertDispatcher(ctx, cfg, metadata, secretVault)
	if err != nil {
		providerRegistry.Close()
		ledgerLog.Close()
		metadata.Close()
		secretVault.Close()
		return fail(err)
	}
	gatewayService, err := gatewaycore.NewServiceWithOptions(
		authSnapshot,
		providerRegistry,
		accounting,
		gatewaycore.ServiceOptions{
			MaxAttempts:                   cfg.Gateway.MaxTotalAttempts,
			MaxAttemptsPerTarget:          cfg.Retry.MaxAttemptsPerTarget,
			RetryBaseDelay:                cfg.Retry.BaseDelay.Value(),
			RetryMaxDelay:                 cfg.Retry.MaxDelay.Value(),
			RetryJitter:                   cfg.Retry.Jitter,
			CircuitFailureThreshold:       cfg.CircuitBreaker.ConsecutiveFailures,
			CircuitOpenDuration:           cfg.CircuitBreaker.OpenDuration.Value(),
			CircuitHalfOpenMaxRequests:    cfg.CircuitBreaker.HalfOpenMaxRequests,
			TokenGuard:                    tokenGuard,
			Redactor:                      redactor,
			Resources:                     metadata,
			ResourceObjectDir:             filepath.Join(cfg.Storage.DataDir, "provider-objects"),
			Pricing:                       metadata,
			PricingClockRollbackTolerance: cfg.Gateway.PricingClockRollbackTolerance.Value(),
			PricingClockForwardTolerance:  cfg.Gateway.PricingClockForwardTolerance.Value(),
			PricingUnknownPolicy:          cfg.Gateway.PricingUnknownPolicy,
		},
	)
	if err != nil {
		alertDispatcher.Close()
		ledgerLog.Close()
		metadata.Close()
		providerRegistry.Close()
		secretVault.Close()
		return fail(fmt.Errorf("create gateway service: %w", err))
	}
	trustedProxies, err := parsePrefixes(cfg.Security.TrustedProxyCIDRs)
	if err != nil {
		alertDispatcher.Close()
		ledgerLog.Close()
		metadata.Close()
		providerRegistry.Close()
		secretVault.Close()
		return fail(err)
	}
	sourceLimiter := sourcelimit.New(
		cfg.Gateway.SourceRateLimit.SourceRequestsPerMinute(),
		cfg.Gateway.SourceRateLimit.MaxTrackedSources,
	)
	gatewayHandler, err := gatewayapi.NewWithOptions(gatewayService, gatewayapi.Options{
		MaxRequestBytes:   cfg.Server.MaxRequestBytes,
		RouteTimeout:      cfg.Gateway.RouteTotalTimeout.Value(),
		StreamTimeout:     cfg.Gateway.StreamMaxDuration.Value(),
		WriteTimeout:      cfg.Gateway.DownstreamWriteTimeout.Value(),
		TrustProxyHeaders: cfg.Security.TrustProxyHeaders,
		TrustedProxyCIDRs: trustedProxies,
		SourceLimiter:     sourceLimiter,
		// The same snapshot the service authenticates against, so the guard
		// cannot turn away a request the service would have accepted.
		AuthorizeKey: func(plaintextKey string) error {
			_, err := authSnapshot.Authenticate(plaintextKey, time.Now())
			return err
		},
	})
	if err != nil {
		alertDispatcher.Close()
		ledgerLog.Close()
		metadata.Close()
		providerRegistry.Close()
		secretVault.Close()
		return fail(fmt.Errorf("create gateway handler: %w", err))
	}
	auditLog, err := audit.Open(cfg.AuditPath(), auditKey)
	if err != nil {
		alertDispatcher.Close()
		ledgerLog.Close()
		metadata.Close()
		providerRegistry.Close()
		secretVault.Close()
		return fail(fmt.Errorf("open audit log: %w", err))
	}
	if err := reconcileAuditCheckpoint(metadata, auditLog.Summary()); err != nil {
		auditLog.Close()
		alertDispatcher.Close()
		ledgerLog.Close()
		metadata.Close()
		providerRegistry.Close()
		secretVault.Close()
		return fail(err)
	}
	if err := drainKeySlotAuditIntent(ctx, metadata, auditLog); err != nil {
		auditLog.Close()
		alertDispatcher.Close()
		ledgerLog.Close()
		metadata.Close()
		providerRegistry.Close()
		secretVault.Close()
		return fail(fmt.Errorf("recover pending Key Slot audit: %w", err))
	}
	if err := drainMasterKeyRotationAuditIntent(ctx, metadata, auditLog); err != nil {
		auditLog.Close()
		alertDispatcher.Close()
		ledgerLog.Close()
		metadata.Close()
		providerRegistry.Close()
		secretVault.Close()
		return fail(fmt.Errorf("recover pending Master Key rotation audit: %w", err))
	}
	if err := appendKMSProviderAudit(ctx, auditLog, metadata, kmsAudit); err != nil {
		auditLog.Close()
		alertDispatcher.Close()
		ledgerLog.Close()
		metadata.Close()
		providerRegistry.Close()
		secretVault.Close()
		return fail(fmt.Errorf("append KMS provider audit: %w", err))
	}
	if err := appendSystemAudit(auditLog, metadata, "system.startup"); err != nil {
		auditLog.Close()
		alertDispatcher.Close()
		ledgerLog.Close()
		metadata.Close()
		providerRegistry.Close()
		secretVault.Close()
		return fail(err)
	}
	kmsRecoveryLastUsed, err := lastKMSRecoveryUse(auditLog, cfg.Storage.MasterKey.RecoverySlot)
	if err != nil {
		auditLog.Close()
		alertDispatcher.Close()
		ledgerLog.Close()
		metadata.Close()
		providerRegistry.Close()
		secretVault.Close()
		return fail(fmt.Errorf("inspect Recovery Slot audit history: %w", err))
	}
	// A candidate is generated on every start; SeedInstanceID only accepts it
	// the first time and returns whatever is already stored on every call
	// after that. An anchor names the instance that emitted it (ADR 0015),
	// and generating that name here — rather than asking an operator to
	// coordinate one across a fleet in config — costs nothing on instances
	// that will never turn anchoring on.
	instanceIDCandidate, err := id.New("ins")
	if err != nil {
		auditLog.Close()
		alertDispatcher.Close()
		ledgerLog.Close()
		metadata.Close()
		providerRegistry.Close()
		secretVault.Close()
		return fail(err)
	}
	instanceID, err := metadata.SeedInstanceID(instanceIDCandidate)
	if err != nil {
		auditLog.Close()
		alertDispatcher.Close()
		ledgerLog.Close()
		metadata.Close()
		providerRegistry.Close()
		secretVault.Close()
		return fail(fmt.Errorf("seed instance ID: %w", err))
	}
	runtime := &Runtime{
		config:              cfg,
		logger:              logger,
		lock:                dataLock,
		store:               metadata,
		ledger:              ledgerLog,
		state:               ledgerState,
		status:              accountingStatus,
		vault:               secretVault,
		auth:                authSnapshot,
		providers:           providerRegistry,
		accounting:          accounting,
		gateway:             gatewayHandler,
		gatewayService:      gatewayService,
		sourceLimiter:       sourceLimiter,
		tokenGuard:          tokenGuard,
		redactor:            redactor,
		alerts:              alertDispatcher,
		audit:               auditLog,
		metricsTokenHash:    metricsTokenHash,
		metricsAuthorizer:   metricsAuthorizer,
		metricsScrapes:      make(chan struct{}, cfg.Metrics.MaxConcurrentScrapes),
		instanceID:          instanceID,
		anchorAuthorizer:    anchorAuthorizer,
		startedAt:           time.Now(),
		kmsRecoveryLastUsed: kmsRecoveryLastUsed,
		adminSessions:       adminSessions,
		adminLogin:          adminRateState{windows: make(map[string]adminLoginWindow)},
		adminSetupRate:      adminRateState{windows: make(map[string]adminLoginWindow)},
		adminStepUp:         make(map[string]adminLoginWindow),
		setupToken:          setupToken,
		setupTokenNeeded:    setupRequiresToken(cfg),
		usage:               usageAggregate,
		usageCollector:      usageCollector,
		usageExporter:       usageExporter,
		periods:             periods,
		now:                 time.Now,
	}
	if err := runtime.drainAdminMFAAuditIntents(ctx); err != nil {
		auditLog.Close()
		alertDispatcher.Close()
		ledgerLog.Close()
		metadata.Close()
		providerRegistry.Close()
		secretVault.Close()
		return fail(fmt.Errorf("recover pending Admin MFA audit: %w", err))
	}
	if err := runtime.drainPricingAuditIntents(ctx); err != nil {
		auditLog.Close()
		alertDispatcher.Close()
		ledgerLog.Close()
		metadata.Close()
		providerRegistry.Close()
		secretVault.Close()
		return fail(fmt.Errorf("recover pending pricing audit: %w", err))
	}
	settings, err := metadata.RuntimeSettings()
	if errors.Is(err, boltstore.ErrNotFound) {
		settings = domain.RuntimeSettings{
			HealthProbeIntervalSeconds: int64(cfg.Gateway.HealthProbeInterval.Value() / time.Second),
			UpdatedAt:                  time.Now().UTC(),
		}
		settings, err = metadata.PutRuntimeSettings(settings, 0)
		if err != nil {
			auditLog.Close()
			alertDispatcher.Close()
			ledgerLog.Close()
			metadata.Close()
			providerRegistry.Close()
			secretVault.Close()
			return fail(fmt.Errorf("initialize runtime settings: %w", err))
		}
	} else if err != nil {
		auditLog.Close()
		alertDispatcher.Close()
		ledgerLog.Close()
		metadata.Close()
		providerRegistry.Close()
		secretVault.Close()
		return fail(fmt.Errorf("load runtime settings: %w", err))
	}
	runtime.runtimeSettings.Store(&settings)
	uiSettings, err := metadata.InstanceUISettings()
	if errors.Is(err, boltstore.ErrNotFound) {
		uiSettings = domain.InstanceUISettings{
			DefaultLocale: domain.LocaleZhCN,
			UpdatedAt:     time.Now().UTC(),
		}
		uiSettings, err = metadata.PutInstanceUISettings(uiSettings, 0)
	}
	if err != nil {
		auditLog.Close()
		alertDispatcher.Close()
		ledgerLog.Close()
		metadata.Close()
		providerRegistry.Close()
		secretVault.Close()
		return fail(fmt.Errorf("initialize UI settings: %w", err))
	}
	runtime.uiSettings.Store(&uiSettings)
	cleanupAdminSessions = false
	backgroundContext, backgroundCancel := context.WithCancel(context.Background())
	runtime.backgroundCtx = backgroundContext
	runtime.backgroundCancel = backgroundCancel
	runtime.alerts.SetObserver(runtime.auditAlertDelivery)
	runtime.alerts.Start()
	runtime.backgroundWait.Add(6)
	go func() {
		defer runtime.backgroundWait.Done()
		runtime.usageCollector.Run(backgroundContext)
	}()
	go func() {
		defer runtime.backgroundWait.Done()
		runtime.runAuditAnchorMaintenance(backgroundContext)
	}()
	go func() {
		defer runtime.backgroundWait.Done()
		forwardTokenGuardAlerts(
			backgroundContext, runtime.tokenGuard.Events(), runtime.alerts,
			runtime.auditAlertSubmission,
		)
	}()
	go func() {
		defer runtime.backgroundWait.Done()
		runtime.runUsageMaintenance(backgroundContext)
	}()
	go func() {
		defer runtime.backgroundWait.Done()
		runtime.runActiveDeploymentProbes(backgroundContext)
	}()
	go func() {
		defer runtime.backgroundWait.Done()
		runtime.runProviderResourceMaintenance(backgroundContext)
	}()
	return runtime, nil
}

// head is the watermark the ledger actually replayed to. A checkpoint that
// claims to have consumed more of the WAL than the WAL contains describes a
// log that has since shrunk, and resuming from it would seek past the real
// data and land mid-frame on whatever is written next — a corruption reported
// minutes later, far from its cause. Rebuilding from scratch is always safe:
// the aggregate is a derivative, and the WAL is the authority.
func restoreUsageAggregate(store *boltstore.Store, head ledger.Watermark, logger *slog.Logger) (*usage.Aggregate, ledger.Watermark) {
	watermark, payload, err := store.UsageCheckpoint()
	if errors.Is(err, boltstore.ErrNotFound) {
		return usage.NewAggregate(), ledger.Watermark{}
	}
	if err != nil {
		logger.Warn("usage checkpoint ignored", "error", err)
		return usage.NewAggregate(), ledger.Watermark{}
	}
	aggregate, err := usage.RestoreCheckpoint(payload)
	if err != nil {
		logger.Warn("usage checkpoint ignored", "error", err)
		return usage.NewAggregate(), ledger.Watermark{}
	}
	if aggregate.Snapshot().Watermark != watermark {
		logger.Warn("usage checkpoint ignored", "error", "envelope watermark does not match payload")
		return usage.NewAggregate(), ledger.Watermark{}
	}
	if watermark.Sequence > head.Sequence || watermark.Offset > head.Offset {
		logger.Warn("usage checkpoint ignored", "error", "checkpoint is ahead of the ledger head")
		return usage.NewAggregate(), ledger.Watermark{}
	}
	return aggregate, watermark
}

func (r *Runtime) runUsageMaintenance(ctx context.Context) {
	checkpointTicker := time.NewTicker(r.config.Usage.CheckpointInterval.Value())
	parquetTicker := time.NewTicker(r.config.Usage.ParquetInterval.Value())
	defer checkpointTicker.Stop()
	defer parquetTicker.Stop()
	for {
		select {
		case <-checkpointTicker.C:
			r.saveUsageCheckpoint()
			r.saveTokenGuardCheckpoint()
			// Settles the stored record once a scheduled timezone change is
			// due. The resolver already reports the new zone from the effective
			// instant, so lateness here changes no boundary — only when the
			// audit event lands.
			if err := r.applyDueAccountingTimezoneChange(time.Now()); err != nil {
				r.logger.Warn("scheduled accounting timezone change not applied", "error", err)
			}
		case <-parquetTicker.C:
			r.exportUsageParquet()
		case <-ctx.Done():
			r.saveUsageCheckpoint()
			r.saveTokenGuardCheckpoint()
			r.exportUsageParquet()
			return
		}
	}
}

func (r *Runtime) saveTokenGuardCheckpoint() {
	payload, err := r.tokenGuard.MarshalCheckpoint()
	if err != nil {
		r.logger.Warn("Token Guard checkpoint encode failed", "error", err)
		return
	}
	if err := r.store.PutTokenGuardCheckpoint(payload); err != nil {
		r.logger.Warn("Token Guard checkpoint save failed", "error", err)
	}
}

func (r *Runtime) saveUsageCheckpoint() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := r.usageCollector.CatchUp(ctx); err != nil {
		r.logger.Warn("usage checkpoint catch-up failed", "error", err)
		return
	}
	watermark, payload, err := r.usage.MarshalCheckpoint()
	if err != nil {
		r.logger.Warn("usage checkpoint encode failed", "error", err)
		return
	}
	if watermark.Sequence == 0 {
		return
	}
	if err := r.store.PutUsageCheckpoint(watermark, payload); err != nil {
		r.logger.Warn("usage checkpoint save failed", "error", err)
	}
	r.advanceLedgerChainCheckpoint()
}

// advanceLedgerChainCheckpoint moves the trusted chain head forward while the
// process runs. Startup reconciliation alone left the checkpoint pinned to
// whatever the last restart saw, so an instance up for a month protected only
// the frames written before it started: everything since could be truncated
// back to that point and still reconcile cleanly on the next boot. Riding the
// usage checkpoint's ticker bounds that window to one interval and costs a
// small bbolt write on a path that already does a larger one.
func (r *Runtime) advanceLedgerChainCheckpoint() {
	sequence, offset, hash, ok := r.ledger.ChainHead()
	if !ok {
		return
	}
	checkpoint, err := r.store.LedgerChainCheckpoint()
	if err != nil {
		r.logger.Warn("ledger chain checkpoint load failed", "error", err)
		return
	}
	if checkpoint.Sequence >= sequence {
		return
	}
	if err := r.store.PutLedgerChainCheckpoint(boltstore.LedgerChainCheckpoint{
		Sequence: sequence, Offset: offset, Hash: hash,
	}); err != nil {
		r.logger.Warn("ledger chain checkpoint save failed", "error", err)
	}
}

func (r *Runtime) exportUsageParquet() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := r.usageCollector.CatchUp(ctx); err != nil {
		r.logger.Warn("usage parquet catch-up failed", "error", err)
		return
	}
	if _, err := r.usageExporter.Export(r.usage.Snapshot()); err != nil {
		r.logger.Warn("usage parquet export failed", "error", err)
	}
}

// parsePrefixes refuses the whole list rather than skipping what it cannot
// parse. Configuration validation rejects a malformed CIDR before this runs, but
// silently dropping one here would mean the set of addresses Heimdall trusts as
// proxies differs from the set the operator wrote down — and the difference
// decides whose X-Forwarded-For is believed.
func parsePrefixes(raw []string) ([]netip.Prefix, error) {
	result := make([]netip.Prefix, 0, len(raw))
	for _, value := range raw {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			return nil, fmt.Errorf("trusted proxy CIDR %q: %w", value, err)
		}
		result = append(result, prefix)
	}
	return result, nil
}

func verifyVaultKeyCheck(store *boltstore.Store, secretVault *vault.Vault) error {
	envelope, err := store.VaultKeyCheck()
	if err != nil {
		return fmt.Errorf("load vault key check: %w", err)
	}
	plaintext, err := secretVault.DecryptCredential(vaultKeyCheckID, vaultKeyCheckProvider, vaultKeyCheckAudience, envelope)
	if err != nil {
		return errors.New("master key does not authenticate the metadata store")
	}
	defer clear(plaintext)
	if string(plaintext) != vaultKeyCheckPlaintext {
		return errors.New("metadata vault key check is invalid")
	}
	return nil
}

func (r *Runtime) Run(ctx context.Context) error {
	return r.RunWithReady(ctx, nil)
}

// warnAboutReachableWorkbench says once, at startup, that the Admin listener is
// carrying data-plane traffic on an address other machines can reach. The
// workbench costs nothing on a loopback-only instance — that is the quickstart,
// and it is where the first-run guidance sends people — but on a listener bound
// to a routable address it means whatever network controls guard the Gateway
// listener (firewall, WAF, mTLS, IP allowlists) do not cover this path. It is
// still an authenticated administrator holding a valid Gateway Key on the other
// end; this is a lateral capability, not an open door, so it warns rather than
// refuses.
func (r *Runtime) warnAboutReachableWorkbench() {
	if !r.developerWorkbenchEnabled() {
		return
	}
	host, _, err := net.SplitHostPort(r.config.Server.AdminListen)
	if err != nil || config.ListenerHostIsLoopback(host) {
		return
	}
	r.logger.Warn(
		"developer workbench serves Gateway calls on a non-loopback Admin listener; "+
			"network controls applied only to the Gateway listener do not cover this path",
		"admin_listen", r.config.Server.AdminListen,
		"remedy", "set admin.developer_workbench to disabled",
	)
}

// RunWithReady binds every configured listener before calling ready. This
// keeps startup guidance and service-manager readiness signals from claiming
// success when one of the ports cannot actually be opened.
func (r *Runtime) RunWithReady(ctx context.Context, ready func() error) error {
	r.warnAboutReachableWorkbench()
	r.warnAboutMissingAnchorSink()
	var metricsTLSConfig *tls.Config
	if r.config.Metrics.Enabled && r.config.Metrics.TLS.Enabled {
		var err error
		metricsTLSConfig, err = r.metricsTLSConfig()
		if err != nil {
			return err
		}
	}
	servers := []*http.Server{
		r.server("gateway", r.config.Server.GatewayListen, r.gatewayRouter()),
		r.server("admin", r.config.Server.AdminListen, r.adminRouter()),
	}
	if r.config.Metrics.Enabled {
		servers = append(servers, r.server("metrics", r.config.Server.MetricsListen, r.metricsRouter()))
	}

	type boundServer struct {
		name     string
		server   *http.Server
		listener net.Listener
	}
	bound := make([]boundServer, 0, len(servers))
	for _, server := range servers {
		listener, err := net.Listen("tcp", server.Addr)
		if err != nil {
			for _, item := range bound {
				_ = item.listener.Close()
			}
			return fmt.Errorf("bind listener %s: %w", server.Addr, err)
		}
		name := "gateway"
		if server.Addr == r.config.Server.AdminListen {
			name = "admin"
		} else if server.Addr == r.config.Server.MetricsListen {
			name = "metrics"
		}
		bound = append(bound, boundServer{name: name, server: server, listener: listener})
	}
	if ready != nil {
		if err := ready(); err != nil {
			for _, item := range bound {
				_ = item.listener.Close()
			}
			return err
		}
	}

	errs := make(chan error, len(bound))
	for _, item := range bound {
		item := item
		go func() {
			r.logger.Info("listener started", "address", item.server.Addr)
			var err error
			if item.name == "metrics" && r.config.Metrics.TLS.Enabled {
				err = item.server.Serve(tls.NewListener(item.listener, metricsTLSConfig.Clone()))
			} else if r.config.TLS.Enabled {
				err = item.server.ServeTLS(item.listener, r.config.TLS.CertFile, r.config.TLS.KeyFile)
			} else {
				err = item.server.Serve(item.listener)
			}
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				errs <- err
			}
		}()
	}

	select {
	case <-ctx.Done():
		// Continue to graceful shutdown.
	case err := <-errs:
		r.draining.Store(true)
		runErr := fmt.Errorf("listener failed: %w", err)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var shutdownErrors []error
		for _, server := range servers {
			if shutdownErr := server.Shutdown(shutdownCtx); shutdownErr != nil {
				shutdownErrors = append(shutdownErrors, shutdownErr)
			}
		}
		return errors.Join(runErr, errors.Join(shutdownErrors...))
	}

	r.draining.Store(true)
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	var shutdownErrors []error
	for _, server := range servers {
		if err := server.Shutdown(shutdownCtx); err != nil {
			shutdownErrors = append(shutdownErrors, err)
		}
	}
	return errors.Join(shutdownErrors...)
}

func (r *Runtime) metricsTLSConfig() (*tls.Config, error) {
	certificate, err := tls.LoadX509KeyPair(r.config.Metrics.TLS.CertFile, r.config.Metrics.TLS.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load metrics TLS keypair: %w", err)
	}
	caPayload, err := os.ReadFile(r.config.Metrics.TLS.ClientCAFile)
	if err != nil {
		return nil, fmt.Errorf("read metrics client CA: %w", err)
	}
	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(caPayload) {
		return nil, errors.New("metrics client CA contains no certificates")
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{certificate},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientCAs,
		NextProtos:   []string{"h2", "http/1.1"},
	}, nil
}

func (r *Runtime) Close() error {
	r.closeOnce.Do(func() {
		r.draining.Store(true)
		r.backgroundCancel()
		r.backgroundWait.Wait()
		r.alerts.Close()
		// Frames written since the last tick would otherwise sit outside the
		// checkpoint until the next start, which is precisely the window a
		// shutdown-then-truncate would use.
		r.advanceLedgerChainCheckpoint()
		auditErr := appendSystemAudit(r.audit, r.store, "system.shutdown")
		r.closeErr = errors.Join(
			auditErr,
			func() error {
				r.providers.Close()
				return nil
			}(),
			r.ledger.Close(),
			r.audit.Close(),
			func() error {
				r.adminSessions.Close()
				return nil
			}(),
			r.store.Close(),
			func() error {
				r.vault.Close()
				return nil
			}(),
			r.lock.Close(),
		)
	})
	return r.closeErr
}

func appendSystemAudit(log *audit.Log, store *boltstore.Store, action string) error {
	eventID, err := id.New("aud")
	if err != nil {
		return err
	}
	_, err = log.Append(context.Background(), audit.Event{
		EventID: eventID, OccurredAt: time.Now().UTC(), ActorType: "system",
		Action: action, TargetType: "gateway", TargetID: "local", Outcome: "success",
	})
	if err != nil {
		return fmt.Errorf("append %s audit event: %w", action, err)
	}
	return checkpointAudit(store, log.Summary())
}

func checkpointAudit(store *boltstore.Store, summary audit.Summary) error {
	if err := store.PutAuditCheckpoint(boltstore.AuditCheckpoint{
		Records: summary.Records, Bytes: summary.Bytes, LastHash: summary.LastHash,
	}); err != nil {
		return fmt.Errorf("checkpoint audit log: %w", err)
	}
	return nil
}

func reconcileAuditCheckpoint(store *boltstore.Store, summary audit.Summary) error {
	checkpoint, err := store.AuditCheckpoint()
	if err != nil {
		return fmt.Errorf("load audit checkpoint: %w", err)
	}
	if checkpoint.Records > summary.Records ||
		(checkpoint.Records == summary.Records &&
			(checkpoint.Bytes != summary.Bytes || checkpoint.LastHash != summary.LastHash)) {
		return errors.New("audit log does not match its trusted checkpoint")
	}
	if checkpoint.Records < summary.Records {
		return checkpointAudit(store, summary)
	}
	return nil
}

// reconcileLedgerChainCheckpoint is the ledger's counterpart to
// reconcileAuditCheckpoint: a chain that has gone backwards, or that
// disagrees with a previously observed head at the same sequence, means the
// WAL was truncated or rewritten since the last trusted checkpoint — exactly
// the "log is shorter today" case per-frame MAC verification alone cannot
// catch (ADR 0016). A ledger that has never written an epoch-4 frame (a
// brand new instance, or one still entirely on legacy epochs) has nothing to
// reconcile yet; the checkpoint stays at its seeded zero value until the
// first epoch-4 append advances it.
//
// The checkpoint is loaded unconditionally, mirroring reconcileAuditCheckpoint.
// "No epoch-4 frame in the file" and "the file was deleted" are the same
// observation from ChainHead, and only the checkpoint tells them apart: a
// non-zero checkpoint against an empty chain is the most complete truncation
// there is, not a fresh install. Returning early on ok == false read that
// case as brand new and let a wiped WAL start clean.
func reconcileLedgerChainCheckpoint(store *boltstore.Store, ledgerLog *ledger.Log) error {
	sequence, offset, hash, ok := ledgerLog.ChainHead()
	checkpoint, err := store.LedgerChainCheckpoint()
	if err != nil {
		return fmt.Errorf("load ledger chain checkpoint: %w", err)
	}
	if !ok {
		if checkpoint.Sequence > 0 {
			return errors.New("ledger chain does not match its trusted checkpoint")
		}
		return nil
	}
	if checkpoint.Sequence > sequence ||
		(checkpoint.Sequence == sequence && (checkpoint.Offset != offset || checkpoint.Hash != hash)) {
		return errors.New("ledger chain does not match its trusted checkpoint")
	}
	if checkpoint.Sequence < sequence {
		if err := store.PutLedgerChainCheckpoint(boltstore.LedgerChainCheckpoint{
			Sequence: sequence, Offset: offset, Hash: hash,
		}); err != nil {
			return fmt.Errorf("checkpoint ledger chain: %w", err)
		}
	}
	return nil
}

func (r *Runtime) server(name, address string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              address,
		Handler:           handler,
		ReadHeaderTimeout: r.config.Server.ReadHeaderTimeout.Value(),
		ReadTimeout:       r.config.Server.ReadBodyTimeout.Value(),
		MaxHeaderBytes:    r.config.Server.MaxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(r.logger.Handler(), slog.LevelError),
	}
}

// gatewayHandler reuses one route tree. gatewayRouter builds a fresh one, which is fine
// at startup but wasteful on the per-request developer proxy path.
func (r *Runtime) gatewayHandler() http.Handler {
	r.gatewayHandlerOnce.Do(func() { r.gatewayHandlerValue = r.gatewayRouter() })
	return r.gatewayHandlerValue
}

func (r *Runtime) gatewayRouter() http.Handler {
	router := chi.NewRouter()
	router.Use(r.recoverPanics)
	router.Use(r.gateway.WithWriteDeadline)
	// Health stays outside the per-source limiter on purpose: an orchestrator
	// probes from one address on a fixed interval, so limiting by source would
	// eventually mark a healthy instance unready rather than shed any load.
	router.Get("/health/live", r.live)
	router.Get("/health/ready", r.ready)
	// A key that cannot authenticate is turned away before its body is read.
	// Without this the request is parsed first and only then authenticated, so
	// an anonymous caller sets the parsing cost and the per-project limiter
	// that would bound it does not yet apply.
	router.Group(func(guarded chi.Router) {
		guarded.Use(r.gateway.LimitOpenAI)
		guarded.Use(r.gateway.GuardOpenAI)
		guarded.Post("/v1/chat/completions", r.gateway.ChatCompletions)
		guarded.Post("/v1/responses", r.gateway.Responses)
		guarded.Post("/v1/embeddings", r.gateway.Embeddings)
		guarded.Post("/v1/moderations", r.gateway.Moderations)
		guarded.Post("/v1/images/generations", r.gateway.Images)
		guarded.Post("/v1/audio/speech", r.gateway.Speech)
		guarded.Post("/v1/audio/transcriptions", r.gateway.Transcriptions)
		guarded.Post("/v1/rerank", r.gateway.Rerank)
		guarded.Post("/v1/async/invocations", r.gateway.StartAsyncInvoke)
		guarded.Get("/v1/async/invocations/{asyncID}", r.gateway.GetAsyncInvoke)
		guarded.Post("/v1/async/invocations/{asyncID}/cancel", r.gateway.CancelAsyncInvoke)
		guarded.Post("/v1/files", r.gateway.CreateFile)
		guarded.Get("/v1/files/{fileID}", r.gateway.GetFile)
		guarded.Get("/v1/files/{fileID}/content", r.gateway.DownloadFile)
		guarded.Delete("/v1/files/{fileID}", r.gateway.DeleteFile)
		guarded.Post("/v1/batches", r.gateway.CreateBatch)
		guarded.Get("/v1/batches/{batchID}", r.gateway.GetBatch)
		guarded.Post("/v1/batches/{batchID}/cancel", r.gateway.CancelBatch)
	})
	router.Group(func(guarded chi.Router) {
		guarded.Use(r.gateway.LimitAnthropic)
		guarded.Use(r.gateway.GuardAnthropic)
		guarded.Post("/v1/messages", r.gateway.Messages)
	})
	router.Get("/", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, map[string]any{
			"name":    "heimdall",
			"version": buildinfo.Current(),
		})
	})
	router.NotFound(r.gateway.NotFound)
	router.MethodNotAllowed(r.gateway.MethodNotAllowed)
	return router
}

func (r *Runtime) adminRouter() http.Handler {
	router := chi.NewRouter()
	router.Use(r.recoverPanics)
	router.Use(adminSecurityHeaders)
	router.Get("/health/live", r.live)
	router.Get("/health/ready", r.ready)
	router.Get("/admin/api/v1/setup/status", r.getAdminSetupStatus)
	router.Get("/admin/api/v1/ui/bootstrap", r.getAdminUIBootstrap)
	router.Post("/admin/api/v1/setup/admin", r.setupAdmin)
	router.Post("/admin/api/v1/session/login", r.loginAdmin)
	router.Post("/admin/api/v1/session/mfa/totp", r.completeAdminMFATOTP)
	router.Post("/admin/api/v1/session/mfa/recovery-code", r.completeAdminMFARecovery)
	router.Delete("/admin/api/v1/session/mfa/challenge", r.cancelAdminMFAChallenge)
	router.With(r.requireAdminBase).Get("/admin/api/v1/session", r.getAdminSession)
	router.With(r.requireAdminSelfSetupMutation).Post("/admin/api/v1/session/logout", r.logoutAdmin)
	router.With(r.requireAdminSelfMutation).Post("/admin/api/v1/session/password", r.changeAdminPassword)
	router.With(r.requireAdminBase).Get("/admin/api/v1/security/mfa", r.getAdminMFA)
	router.With(r.requireAdminSelfSetupMutation).Post("/admin/api/v1/security/mfa/authenticators", r.createAdminMFAAuthenticator)
	router.With(r.requireAdminSelfSetupMutation).Post("/admin/api/v1/security/mfa/authenticators/{id}/confirm", r.confirmAdminMFAAuthenticator)
	router.With(r.requireAdminSelfSetupMutation).Delete("/admin/api/v1/security/mfa/authenticators/{id}/pending", r.cancelPendingAdminMFAAuthenticator)
	router.With(r.requireAdminSelfMutation).Patch("/admin/api/v1/security/mfa/authenticators/{id}", r.renameAdminMFAAuthenticator)
	router.With(r.requireAdminSelfMutation).Delete("/admin/api/v1/security/mfa/authenticators/{id}", r.deleteAdminMFAAuthenticator)
	router.With(r.requireAdminSelfMutation).Post("/admin/api/v1/security/mfa/recovery-codes/regenerate", r.regenerateAdminMFARecoveryCodes)
	router.With(r.requireAdminSelfMutation).Delete("/admin/api/v1/security/mfa", r.disableAdminMFA)
	router.With(r.requireAdmin).Get("/admin/api/v1/admin-users", r.listAdminUsers)
	router.With(r.requireAdminMutation).Post("/admin/api/v1/admin-users", r.createAdminUser)
	router.With(r.requireAdminMutation).Delete("/admin/api/v1/admin-users/{username}", r.deleteAdminUser)
	router.With(r.requireAdmin).Get("/admin/api/v1/dashboard", r.adminDashboard)
	router.With(r.requireAdmin).Get("/admin/api/v1/master-key/custody", r.adminMasterKeyCustody)
	router.With(r.requireAdmin).Get("/admin/api/v1/master-key/runbooks/lifecycle", r.adminMasterKeyLifecycleRunbook)
	router.With(r.requireAdmin).Get("/admin/api/v1/master-key/runbooks/recovery", r.adminMasterKeyRecoveryRunbook)
	router.With(r.requireAdmin).Get("/admin/api/v1/usage", r.adminUsage)
	router.With(r.requireAdmin).Get("/admin/api/v1/usage/requests/{requestID}", r.adminUsageRequest)
	router.With(r.requireAdmin).Get("/admin/api/v1/system/status", r.adminSystemStatus)
	router.With(r.requireAdmin).Get("/admin/api/v1/system/config", r.adminSystemConfig)
	router.With(r.requireAdmin).Get("/admin/api/v1/developer/config", r.getAdminDeveloperConfig)
	router.With(r.requireAdminMutation).Post("/admin/api/v1/developer/execute/{endpoint}", r.executeAdminDeveloperRequest)
	router.With(r.requireAdmin).Get("/admin/api/v1/settings", r.getAdminSettings)
	router.With(r.requireAdminMutation).Put("/admin/api/v1/settings", r.updateAdminSettings)
	router.With(r.requireAdmin).Get("/admin/api/v1/settings/ui", r.getAdminUISettings)
	router.With(r.requireAdminMutation).Put("/admin/api/v1/settings/ui", r.updateAdminUISettings)
	router.With(r.requireAdmin).Get("/admin/api/v1/settings/accounting", r.getAdminAccountingSettings)
	router.With(r.requireAdminMutation).Put("/admin/api/v1/settings/accounting", r.updateAdminAccountingSettings)
	router.With(r.requireAdminMutation).Delete("/admin/api/v1/settings/accounting/pending", r.cancelAdminAccountingTimezoneChange)
	router.With(r.requireAdmin).Get("/admin/api/v1/preferences", r.getAdminPreferences)
	router.With(r.requireAdminSelfMutation).Put("/admin/api/v1/preferences", r.updateAdminPreferences)
	router.With(r.requireAdmin).Get("/admin/api/v1/projects", r.listAdminProjects)
	router.With(r.requireAdminMutation).Post("/admin/api/v1/projects", r.createAdminProject)
	router.With(r.requireAdmin).Get("/admin/api/v1/projects/{id}", r.getAdminProject)
	router.With(r.requireAdminMutation).Put("/admin/api/v1/projects/{id}", r.updateAdminProject)
	router.With(r.requireAdminMutation).Delete("/admin/api/v1/projects/{id}", r.deleteAdminProject)
	router.With(r.requireAdminMutation).Post("/admin/api/v1/projects/{id}/unblock", r.unblockAdminProject)
	router.With(r.requireAdmin).Get("/admin/api/v1/projects/{id}/keys", r.listAdminProjectKeys)
	router.With(r.requireAdminMutation).Post("/admin/api/v1/projects/{id}/keys", r.createAdminProjectKey)
	router.With(r.requireAdmin).Get("/admin/api/v1/projects/{id}/keys/{keyID}", r.getAdminProjectKey)
	router.With(r.requireAdminMutation).Put("/admin/api/v1/projects/{id}/keys/{keyID}", r.updateAdminProjectKey)
	router.With(r.requireAdminMutation).Delete("/admin/api/v1/projects/{id}/keys/{keyID}", r.deleteAdminProjectKey)
	router.With(r.requireAdmin).Get("/admin/api/v1/credentials", r.listAdminCredentials)
	router.With(r.requireAdminMutation).Post("/admin/api/v1/credentials", r.createAdminCredential)
	router.With(r.requireAdmin).Get("/admin/api/v1/credentials/{id}", r.getAdminCredential)
	router.With(r.requireAdminMutation).Put("/admin/api/v1/credentials/{id}", r.updateAdminCredential)
	router.With(r.requireAdminMutation).Delete("/admin/api/v1/credentials/{id}", r.deleteAdminCredential)
	router.With(r.requireAdmin).Get("/admin/api/v1/providers", r.listAdminProviders)
	router.With(r.requireAdminMutation).Post("/admin/api/v1/providers", r.createAdminProvider)
	router.With(r.requireAdmin).Get("/admin/api/v1/providers/{id}", r.getAdminProvider)
	router.With(r.requireAdmin).Get("/admin/api/v1/providers/{id}/models", r.listAdminProviderModels)
	router.With(r.requireAdminMutation).Put("/admin/api/v1/providers/{id}", r.updateAdminProvider)
	router.With(r.requireAdminMutation).Delete("/admin/api/v1/providers/{id}", r.deleteAdminProvider)
	router.With(r.requireAdminMutation).Post("/admin/api/v1/providers/{id}/test", r.testAdminProvider)
	router.With(r.requireAdmin).Get("/admin/api/v1/deployments", r.listAdminDeployments)
	router.With(r.requireAdminMutation).Post("/admin/api/v1/deployments", r.createAdminDeployment)
	router.With(r.requireAdmin).Get("/admin/api/v1/deployments/{id}", r.getAdminDeployment)
	router.With(r.requireAdminMutation).Put("/admin/api/v1/deployments/{id}", r.updateAdminDeployment)
	router.With(r.requireAdminMutation).Delete("/admin/api/v1/deployments/{id}", r.deleteAdminDeployment)
	router.With(r.requireAdminMutation).Post("/admin/api/v1/deployments/{id}/test", r.testAdminDeployment)
	router.With(r.requireAdmin).Get("/admin/api/v1/deployments/{id}/prices", r.listAdminDeploymentPrices)
	router.With(r.requireAdminMutation).Post("/admin/api/v1/deployments/{id}/prices", r.createAdminDeploymentPrice)
	router.With(r.requireAdminMutation).Post("/admin/api/v1/deployments/{id}/prices/preview", r.previewAdminDeploymentPrice)
	router.With(r.requireAdminMutation).Post("/admin/api/v1/deployments/{id}/prices/restore-confirm", r.confirmRestoredDeploymentPricing)
	router.With(r.requireAdminMutation).Post("/admin/api/v1/deployments/{id}/prices/{priceID}/cancel", r.cancelAdminDeploymentPrice)
	router.With(r.requireAdmin).Get("/admin/api/v1/deployments/{id}/price-proposals", r.listAdminDeploymentPriceProposals)
	router.With(r.requireAdminMutation).Post("/admin/api/v1/deployments/{id}/price-proposals", r.createAdminDeploymentPriceProposal)
	router.With(r.requireAdminMutation).Post("/admin/api/v1/deployments/{id}/price-proposals/{proposalID}/adopt", r.adoptAdminDeploymentPriceProposal)
	router.With(r.requireAdminMutation).Post("/admin/api/v1/deployments/{id}/price-proposals/{proposalID}/reject", r.rejectAdminDeploymentPriceProposal)
	router.With(r.requireAdmin).Get("/admin/api/v1/routes", r.listAdminRoutes)
	router.With(r.requireAdminMutation).Post("/admin/api/v1/routes", r.createAdminRoute)
	router.With(r.requireAdmin).Get("/admin/api/v1/routes/{id}", r.getAdminRoute)
	router.With(r.requireAdminMutation).Put("/admin/api/v1/routes/{id}", r.updateAdminRoute)
	router.With(r.requireAdminMutation).Delete("/admin/api/v1/routes/{id}", r.deleteAdminRoute)
	router.With(r.requireAdminMutation).Post("/admin/api/v1/routes/{id}/test", r.testAdminRoute)
	router.With(r.requireAdmin).Get("/admin/api/v1/token-guard-policies", r.listAdminTokenGuardPolicies)
	router.With(r.requireAdminMutation).Post("/admin/api/v1/token-guard-policies", r.createAdminTokenGuardPolicy)
	router.With(r.requireAdmin).Get("/admin/api/v1/token-guard-policies/{id}", r.getAdminTokenGuardPolicy)
	router.With(r.requireAdminMutation).Put("/admin/api/v1/token-guard-policies/{id}", r.updateAdminTokenGuardPolicy)
	router.With(r.requireAdminMutation).Delete("/admin/api/v1/token-guard-policies/{id}", r.deleteAdminTokenGuardPolicy)
	router.With(r.requireAdminMutation).Post("/admin/api/v1/token-guard-policies/{id}/test", r.testAdminTokenGuardPolicy)
	router.With(r.requireAdmin).Get("/admin/api/v1/redaction-policies", r.listAdminRedactionPolicies)
	router.With(r.requireAdminMutation).Post("/admin/api/v1/redaction-policies", r.createAdminRedactionPolicy)
	router.With(r.requireAdmin).Get("/admin/api/v1/redaction-policies/{id}", r.getAdminRedactionPolicy)
	router.With(r.requireAdminMutation).Put("/admin/api/v1/redaction-policies/{id}", r.updateAdminRedactionPolicy)
	router.With(r.requireAdminMutation).Delete("/admin/api/v1/redaction-policies/{id}", r.deleteAdminRedactionPolicy)
	router.With(r.requireAdminMutation).Post("/admin/api/v1/redaction-policies/{id}/test", r.testAdminRedactionPolicy)
	router.With(r.requireAdmin).Get("/admin/api/v1/alerts", r.listAdminAlerts)
	router.With(r.requireAdminMutation).Post("/admin/api/v1/alerts", r.createAdminAlert)
	router.With(r.requireAdminMutation).Post("/admin/api/v1/alerts/test", r.testAdminAlertSelection)
	router.With(r.requireAdmin).Get("/admin/api/v1/alerts/{id}", r.getAdminAlert)
	router.With(r.requireAdminMutation).Put("/admin/api/v1/alerts/{id}", r.updateAdminAlert)
	router.With(r.requireAdminMutation).Delete("/admin/api/v1/alerts/{id}", r.deleteAdminAlert)
	router.With(r.requireAdminMutation).Post("/admin/api/v1/alerts/{id}/test", r.testAdminAlert)
	router.With(r.requireAdmin).Get("/admin/api/v1/audit", r.listAdminAudit)
	ui := webui.Handler()
	router.Handle("/admin", ui)
	router.Handle("/admin/*", ui)
	return router
}

func adminSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Cache-Control", "no-store")
		writer.Header().Set("Content-Security-Policy",
			"default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; "+
				"img-src 'self' data:; object-src 'none'; base-uri 'none'; "+
				"frame-ancestors 'none'; form-action 'self'")
		writer.Header().Set("Referrer-Policy", "no-referrer")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("Permissions-Policy",
			"camera=(), microphone=(), geolocation=(), payment=(), usb=()")
		next.ServeHTTP(writer, request)
	})
}

func (r *Runtime) metricsRouter() http.Handler {
	router := chi.NewRouter()
	router.Use(r.recoverPanics)
	router.Get("/health/live", r.live)
	if r.config.Audit.Anchor.Enabled && r.config.Audit.Anchor.Sink == config.AuditAnchorSinkDeadManPull {
		router.Get("/audit/anchors", r.adminAuditAnchors)
	}
	router.Get("/metrics", func(writer http.ResponseWriter, request *http.Request) {
		if r.config.Metrics.RequireAuth && !r.authorizeMetrics(request) {
			r.metricsAuthFailed.Add(1)
			writer.Header().Set("WWW-Authenticate", `Bearer realm="heimdall-metrics"`)
			writeJSON(writer, http.StatusUnauthorized, map[string]any{
				"error": "metrics authentication required",
			})
			return
		}
		select {
		case r.metricsScrapes <- struct{}{}:
			defer func() { <-r.metricsScrapes }()
		default:
			r.metricsBusy.Add(1)
			writeJSON(writer, http.StatusServiceUnavailable, map[string]any{
				"error": "metrics scrape concurrency limit reached",
			})
			return
		}
		controller := http.NewResponseController(writer)
		if err := controller.SetWriteDeadline(time.Now().Add(r.config.Metrics.WriteTimeout.Value())); err != nil {
			r.logger.Warn("metrics response write deadline unavailable", "error", err)
		}
		if err := r.writeMetrics(writer); err != nil {
			r.metricsRenderErrs.Add(1)
			r.logger.Warn("metrics response write failed", "error", err)
		}
	})
	return router
}

func (r *Runtime) live(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]string{"status": "live"})
}

func (r *Runtime) ready(writer http.ResponseWriter, request *http.Request) {
	if r.draining.Load() {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{
			"status": "draining",
		})
		return
	}
	if r.status.Load() != ledger.AccountingHealthy {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]any{
			"status":     "not_ready",
			"accounting": r.status.Load(),
		})
		return
	}
	if err := r.store.PricingReadiness(request.Context()); err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]any{
			"status": "not_ready", "pricing": "quarantined",
		})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]string{
		"status":     "ready",
		"accounting": "healthy",
	})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
