package app

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/akz142857/Halro/internal/adminauth"
	"github.com/akz142857/Halro/internal/alert"
	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/auth"
	"github.com/akz142857/Halro/internal/bearercred"
	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/buildinfo"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/failurecapture"
	gatewaycore "github.com/akz142857/Halro/internal/gateway"
	"github.com/akz142857/Halro/internal/gatewayapi"
	"github.com/akz142857/Halro/internal/id"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/modelcatalog"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/redaction"
	"github.com/akz142857/Halro/internal/sourcelimit"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/tokenguard"
	"github.com/akz142857/Halro/internal/usage"
	"github.com/akz142857/Halro/internal/vault"
	"github.com/akz142857/Halro/internal/webui"
	"github.com/go-chi/chi/v5"
)

type Runtime struct {
	config config.Config
	logger *slog.Logger
	lock   *lock.Lock
	store  *boltstore.Store
	ledger *ledger.Log
	state  *ledger.State
	status *ledger.Status
	vault  *vault.Vault
	// failureCapture holds the payloads of failed requests. Nil when the
	// operator has not switched it on, which is the default; every read path
	// treats nil as "the feature is off" rather than as an error.
	failureCapture      *failurecapture.Store
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
	// activation records whether the live snapshots still reflect the store, so
	// a durable mutation that failed to activate refuses traffic instead of
	// being served from a snapshot known to be behind it.
	activation activationTracker
	// routeWithheld is what the live registry refused. It answers a question
	// the store cannot: a route is Enabled there whether or not it reached
	// routing.
	routeWithheld        routeWithholdingState
	capabilityResolution capabilityResolutionRuntime
	capabilityDetections capabilityDetectionRuntime
	adminProjectMu       sync.Mutex
	adminAlertMu         sync.Mutex
	adminSettingsMu      sync.Mutex
	adminIdentityMu      sync.Mutex
	metricsTokenHash     [32]byte
	metricsAuthorizer    *bearercred.Authorizer
	metricsScrapes       chan struct{}
	capabilityMetrics    *capabilityMetrics
	metricsAuthFailed    atomic.Uint64
	metricsBusy          atomic.Uint64
	metricsRenderErrs    atomic.Uint64
	startedAt            time.Time
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
	// Kept in memory on purpose rather than on the session record: an elevation
	// that survived a restart would be one the operator never granted to the
	// process now holding it, and the durable session schema stays untouched.
	// Lock and map travel together in one field so this stays one entry in the
	// breadth this type is allowed, rather than two.
	adminElevation   adminElevationState
	setupMu          sync.Mutex
	setupToken       string
	setupTokenNeeded bool
	backgroundCtx    context.Context
	backgroundCancel context.CancelFunc
	backgroundWait   sync.WaitGroup
	usage            *usage.Aggregate
	usageCollector   *usage.Collector
	usageExporter    *usage.Exporter
	periods          *budget.PeriodResolver
	closeOnce        sync.Once
	closeErr         error
	draining         atomic.Bool
	runtimeSettings  atomic.Pointer[domain.RuntimeSettings]
	uiSettings       atomic.Pointer[domain.InstanceUISettings]
	instanceID       string
	anchorAuthorizer *bearercred.Authorizer
	anchorAuthFailed atomic.Uint64
	// Anchoring is fail-open on purpose: a witness that cannot be reached must
	// not stop the gateway. That makes it the one subsystem whose total
	// failure is indistinguishable from working, so these have to reach
	// /metrics or nobody finds out for months.
	anchorLastEmitUnix atomic.Int64
	anchorEmitFailures atomic.Uint64
	// reload owns everything SIGHUP may replace. It is one field rather than
	// five because the material, the sources it comes from, and the record of
	// what was applied are one subsystem, and Runtime is already wide.
	reload reloadRuntime
}

type capabilityResolutionRuntime struct {
	mu                sync.Mutex
	invocationTargets map[string]invocationTargetCatalogCache
	catalog           *modelcatalog.Manager
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
	usageAggregate, usageWatermark, err := restoreUsageAggregate(metadata, ledgerHead, logger)
	if err != nil {
		ledgerLog.Close()
		metadata.Close()
		secretVault.Close()
		return fail(err)
	}
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
	catalogManager, catalogErr := modelcatalog.NewManager(modelcatalog.ManagerOptions{
		Enabled: cfg.ModelCatalog.Enabled, PinnedRevision: cfg.ModelCatalog.PinnedRevision,
		RefreshInterval: cfg.ModelCatalog.RefreshInterval.Value(), DataDir: cfg.Storage.DataDir,
		TrustRoots:          modelcatalog.ProductionTrustRoots(),
		ConnectTimeout:      cfg.Gateway.AttemptConnectTimeout.Value(),
		ResponseTimeout:     cfg.Gateway.AttemptResponseHeaderTimeout.Value(),
		MaxDownloadBytes:    cfg.ModelCatalog.MaxDownloadBytes,
		MaxDecodedBytes:     cfg.ModelCatalog.MaxDecodedBytes,
		MaxCompressionRatio: cfg.ModelCatalog.MaxCompressionRatio,
		MaxEntries:          cfg.ModelCatalog.MaxEntries, Now: time.Now,
	})
	if catalogErr != nil {
		logger.Error("initialize signed model catalog", "error", catalogErr)
	}
	effectiveCatalog := modelcatalog.Builtin()
	if catalogManager != nil {
		effectiveCatalog = catalogManager.Current()
	}
	catalogUnavailable := catalogManager == nil || catalogManager.Status().State != modelcatalog.CatalogStateCurrent
	providerRegistry, loaded, err := loadProviderRegistryWithCatalog(ctx, cfg, metadata, secretVault, effectiveCatalog, catalogUnavailable)
	if err != nil {
		ledgerLog.Close()
		metadata.Close()
		secretVault.Close()
		return fail(err)
	}
	// Starting with a route withheld is a state an operator has to be told
	// about: the process is up and every other route works, so nothing else
	// would say that this one is gone.
	logCapabilityWithholdings(logger, loaded)
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
	// The failure capture store, when the operator has asked for one. It is
	// opened before the gateway because the gateway holds it: an install with
	// capture off gets a nil here and a gateway that never touches it.
	// Opened whenever the directory exists, not only when capture is enabled.
	// Switching the feature off is the security-conscious action, and it used
	// to stop the retention sweep — so an operator who turned it off after an
	// incident stopped *deleting* the prompts they had collected rather than
	// stopping their collection, and nothing would ever have removed them.
	// What `enabled` decides is whether the gateway writes; expiring what is
	// already on disk is not optional.
	captureStore, err := openFailureCapture(cfg, secretVault)
	if err != nil {
		alertDispatcher.Close()
		ledgerLog.Close()
		metadata.Close()
		providerRegistry.Close()
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
			FailureCapture:                captureFor(cfg.Gateway.FailureCapture.Enabled, captureStore),
			Logger:                        logger,
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
		AuthorizeKey: func(plaintextKey string) (int64, error) {
			principal, err := authSnapshot.Authenticate(plaintextKey, time.Now())
			if err != nil {
				return 0, err
			}
			return principal.Project.MaxRequestBytes, nil
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
		failureCapture:      captureStore,
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
		capabilityMetrics:   newCapabilityMetrics(),
		instanceID:          instanceID,
		anchorAuthorizer:    anchorAuthorizer,
		startedAt:           time.Now(),
		kmsRecoveryLastUsed: kmsRecoveryLastUsed,
		adminSessions:       adminSessions,
		adminLogin:          adminRateState{windows: make(map[string]adminLoginWindow)},
		adminSetupRate:      adminRateState{windows: make(map[string]adminLoginWindow)},
		adminStepUp:         make(map[string]adminLoginWindow),
		adminElevation:      adminElevationState{grants: make(map[[32]byte]adminElevationGrant)},
		setupToken:          setupToken,
		setupTokenNeeded:    setupRequiresToken(cfg),
		usage:               usageAggregate,
		usageCollector:      usageCollector,
		usageExporter:       usageExporter,
		periods:             periods,
		now:                 time.Now,
		capabilityDetections: capabilityDetectionRuntime{
			cancels:   make(map[string]context.CancelFunc),
			global:    make(chan struct{}, cfg.Admin.ModelCapabilityDetection.GlobalConcurrency),
			providers: make(map[string]chan struct{}),
			rate:      adminRateState{windows: make(map[string]adminLoginWindow)},
		},
	}
	// The start-up load never passes through the activation path, so without
	// this the console would show every withheld route as Enabled until the
	// first topology mutation happened to republish them.
	runtime.publishRouteWithholdings(loaded)
	if catalogManager != nil {
		catalogManager.SetObserver(runtime.observeModelCatalogRefresh)
		catalogManager.SetActivationPreparer(runtime.prepareModelCatalogActivation)
		runtime.capabilityResolution.catalog = catalogManager
		runtime.capabilityMetrics.setSignedCatalogState(catalogManager.Status())
		runtime.auditModelCatalogConfiguration()
	}
	detectionRecoveryAt := time.Now().UTC()
	recoveredDetections, err := metadata.InterruptModelCapabilityDetections(ctx, detectionRecoveryAt)
	if err != nil {
		auditLog.Close()
		alertDispatcher.Close()
		ledgerLog.Close()
		metadata.Close()
		providerRegistry.Close()
		secretVault.Close()
		return fail(fmt.Errorf("recover model capability detections: %w", err))
	}
	if recoveredDetections > 0 {
		items, listErr := metadata.ListModelCapabilityDetections(ctx)
		if listErr != nil {
			auditLog.Close()
			alertDispatcher.Close()
			ledgerLog.Close()
			metadata.Close()
			providerRegistry.Close()
			secretVault.Close()
			return fail(fmt.Errorf("list recovered model capability detections: %w", listErr))
		}
		for _, detection := range items {
			if detection.Status != domain.DetectionInterrupted || detection.CompletedAt == nil || !detection.CompletedAt.Equal(detectionRecoveryAt) {
				continue
			}
			_ = runtime.appendAdminAuditWithMetadata("system", "halro", "model_capability_detection.failed", "model_capability_detection", detection.ID, "success", "", detectionAuditMetadata(detection))
		}
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
	// An admin mutation is durable together with its audit record, so anything
	// still pending here is a crash between that commit and the append. It is
	// delivered before the instance serves, which is what makes "the mutation
	// happened but nothing recorded it" unreachable rather than merely rare.
	if err := runtime.drainAdminAuditIntents(ctx); err != nil {
		auditLog.Close()
		alertDispatcher.Close()
		ledgerLog.Close()
		metadata.Close()
		providerRegistry.Close()
		secretVault.Close()
		return fail(fmt.Errorf("recover pending admin audit: %w", err))
	}
	// Emitted here rather than at the load itself: the audit log only exists
	// once the runtime is assembled, and a deployment that came up withheld is
	// exactly the state §4.4 wants a durable record of.
	runtime.auditCapabilityWithholdings(ctx, loaded)
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
	runtime.backgroundWait.Add(9)
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
		runtime.runCapabilityDetectionMaintenance(backgroundContext)
	}()
	go func() {
		defer runtime.backgroundWait.Done()
		runtime.runProviderResourceMaintenance(backgroundContext)
	}()
	go func() {
		defer runtime.backgroundWait.Done()
		runtime.runActivationRecovery(backgroundContext, activationRetryInterval)
	}()
	// Read the manager here, on the goroutine that installed it, rather than
	// inside the worker. The field is written once during Open and never again
	// in production, but reading it from the worker leaves the only unguarded
	// access to Runtime state after Open returns — enough for the race detector
	// to fire against anything that reaches in afterwards.
	catalogWorker := runtime.capabilityResolution.catalog
	go func() {
		defer runtime.backgroundWait.Done()
		if catalogWorker != nil {
			catalogWorker.Run(backgroundContext)
		}
	}()
	return runtime, nil
}

// head is the watermark the ledger actually replayed to. A checkpoint that
// claims to have consumed more of the WAL than the WAL contains describes a
// log that has since shrunk, and resuming from it would seek past the real
// data and land mid-frame on whatever is written next — a corruption reported
// minutes later, far from its cause. Rebuilding from scratch is always safe:
// the aggregate is a derivative, and the WAL is the authority.
//
// The daily rollup is discarded with it, always. The two are written in one
// transaction and describe the same prefix of the WAL, so keeping one while
// the other rebuilds from zero would count every stored row a second time —
// silently, because a replay reports no error. That is also why a reset
// failure refuses the start instead of degrading: serving doubled totals is
// worse than not serving.
func restoreUsageAggregate(
	store *boltstore.Store,
	head ledger.Watermark,
	logger *slog.Logger,
) (*usage.Aggregate, ledger.Watermark, error) {
	aggregate, watermark, reason := loadUsageCheckpoint(store, head)
	if reason == "" {
		return aggregate, watermark, nil
	}
	logger.Warn("usage derivatives rebuilt from the ledger", "reason", reason)
	if err := store.ResetUsageDerivatives(); err != nil {
		return nil, ledger.Watermark{}, fmt.Errorf("reset usage derivatives: %w", err)
	}
	return usage.NewAggregate(), ledger.Watermark{}, nil
}

// loadUsageCheckpoint returns the restored aggregate, or the reason it cannot
// be trusted. Every rejection path names itself so the log says which one fired
// rather than only that something was ignored.
func loadUsageCheckpoint(store *boltstore.Store, head ledger.Watermark) (*usage.Aggregate, ledger.Watermark, string) {
	watermark, payload, err := store.UsageCheckpoint()
	if errors.Is(err, boltstore.ErrNotFound) {
		return nil, ledger.Watermark{}, "no usage checkpoint"
	}
	if err != nil {
		return nil, ledger.Watermark{}, fmt.Sprintf("usage checkpoint unreadable: %v", err)
	}
	aggregate, err := usage.RestoreCheckpoint(payload)
	if err != nil {
		return nil, ledger.Watermark{}, fmt.Sprintf("usage checkpoint rejected: %v", err)
	}
	if aggregate.Snapshot().Watermark != watermark {
		return nil, ledger.Watermark{}, "usage checkpoint envelope does not match its payload"
	}
	if watermark.Sequence > head.Sequence || watermark.Offset > head.Offset {
		return nil, ledger.Watermark{}, "usage checkpoint is ahead of the ledger head"
	}
	rollup, err := store.UsageRollupState()
	if errors.Is(err, boltstore.ErrNotFound) {
		return nil, ledger.Watermark{}, "usage rollup state is missing"
	}
	if err != nil {
		return nil, ledger.Watermark{}, fmt.Sprintf("usage rollup state unreadable: %v", err)
	}
	if rollup.Version != domain.RollupVersion {
		return nil, ledger.Watermark{}, fmt.Sprintf(
			"usage rollup version %d is not %d", rollup.Version, domain.RollupVersion)
	}
	// Equality, not ordering: a rollup that lags its checkpoint would have the
	// gap replayed into rows that already counted it.
	if rollup.Watermark != watermark {
		return nil, ledger.Watermark{}, "usage rollup and checkpoint describe different ledger positions"
	}
	return aggregate, watermark, ""
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
			// Retention is swept on the same tick rather than on a timer of its
			// own: this store's promise is that captured caller material is
			// gone after the window, and a promise kept by a goroutine nobody
			// notices stopping is not one.
			r.purgeFailureCaptures()
		case <-ctx.Done():
			r.saveUsageCheckpoint()
			r.saveTokenGuardCheckpoint()
			r.exportUsageParquet()
			// A process whose uptime never reaches one export interval — a
			// crash loop, a restart per config change, a long interval — would
			// otherwise never sweep at all.
			r.purgeFailureCaptures()
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
	snapshot, err := r.usage.TakeCheckpoint()
	if err != nil {
		r.logger.Warn("usage checkpoint encode failed", "error", err)
		return
	}
	// TakeCheckpoint drained the rollup increment, so every path from here that
	// does not persist it has to hand it back. Dropping it would leave those
	// events counted in no stored row and in no later increment.
	if snapshot.Watermark.Sequence == 0 {
		r.returnUsageCheckpoint(snapshot)
		return
	}
	if err := r.store.PutUsageCheckpoint(
		snapshot.Watermark, snapshot.Payload, domain.RollupVersion, snapshot.Rollup,
	); err != nil {
		r.logger.Warn("usage checkpoint save failed", "error", err)
		r.returnUsageCheckpoint(snapshot)
		return
	}
	r.advanceLedgerChainCheckpoint()
}

func (r *Runtime) returnUsageCheckpoint(snapshot usage.CheckpointSnapshot) {
	if err := r.usage.ReturnCheckpoint(snapshot); err != nil {
		r.logger.Error("usage rollup increment could not be restored", "error", err)
	}
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
// silently dropping one here would mean the set of addresses Halro trusts as
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
	// Certificates are loaded before any listener binds. A keypair that cannot
	// be read is a refusal to start, not a listener that comes up in the clear.
	if err := r.openTLSMaterial(); err != nil {
		return err
	}
	servers := []*http.Server{
		r.server("gateway", r.config.Server.GatewayListen, r.gatewayRouter()),
		r.server("admin", r.config.Server.AdminListen, r.adminRouter()),
	}
	if r.config.Metrics.Enabled {
		servers = append(servers, r.server("metrics", r.config.Server.MetricsListen, r.metricsRouter()))
	}
	shutdownServers := make([]shutdownHTTPServer, len(servers))
	for index, server := range servers {
		shutdownServers[index] = server
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
			// ServeTLS with empty file names uses the server's own TLSConfig,
			// which is where the reloadable certificate source lives. It also
			// keeps net/http's HTTP/2 setup, which a hand-rolled tls.NewListener
			// would have to reproduce.
			if item.server.TLSConfig != nil {
				err = item.server.ServeTLS(item.listener, "", "")
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
		shutdownCtx, cancel := context.WithTimeout(context.Background(), r.config.Server.ShutdownTimeout.Value())
		defer cancel()
		shutdownErrors := r.shutdownHTTPServers(shutdownCtx, shutdownServers)
		return errors.Join(runErr, errors.Join(shutdownErrors...))
	}

	r.draining.Store(true)
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.config.Server.ShutdownTimeout.Value())
	defer cancel()
	shutdownErrors := r.shutdownHTTPServers(shutdownCtx, shutdownServers)
	return errors.Join(shutdownErrors...)
}

type shutdownHTTPServer interface {
	Shutdown(context.Context) error
	Close() error
}

func (r *Runtime) shutdownHTTPServers(ctx context.Context, servers []shutdownHTTPServer) []error {
	return gracefullyShutdownHTTPServers(ctx, servers, r.activeProviderAttemptCount, r.recordShutdownTruncatedAttempts)
}

func gracefullyShutdownHTTPServers(
	ctx context.Context,
	servers []shutdownHTTPServer,
	activeProviderAttempts func() uint64,
	recordTruncatedAttempts func(uint64) error,
) []error {
	var shutdownErrors []error
	for _, server := range servers {
		if err := server.Shutdown(ctx); err != nil {
			shutdownErrors = append(shutdownErrors, err)
		}
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return shutdownErrors
	}

	// The grace budget is exhausted. Persist the count before forcing the
	// sockets closed so Prometheus can observe the terminal event after the
	// next start instead of losing an in-memory counter with this process.
	if truncated := activeProviderAttempts(); truncated > 0 {
		if err := recordTruncatedAttempts(truncated); err != nil {
			shutdownErrors = append(shutdownErrors, fmt.Errorf("record shutdown-truncated Provider attempts: %w", err))
		}
	}
	for _, server := range servers {
		if err := server.Close(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			shutdownErrors = append(shutdownErrors, err)
		}
	}
	return shutdownErrors
}

func (r *Runtime) activeProviderAttemptCount() uint64 {
	var total uint64
	for _, active := range r.gatewayService.ActiveProviderRequests() {
		if active > 0 {
			total += uint64(active)
		}
	}
	return total
}

func (r *Runtime) recordShutdownTruncatedAttempts(delta uint64) error {
	total, err := r.store.AddShutdownTruncatedAttempts(delta)
	if err != nil {
		return err
	}
	r.logger.Warn("graceful shutdown budget expired with active Provider attempts", "truncated_attempts", delta, "total", total)
	return nil
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
		TLSConfig:         r.listenerTLSConfig(name),
	}
}

// listenerTLSConfig returns nil for a listener that serves plaintext, which is
// what RunWithReady reads to decide between ServeTLS and Serve.
func (r *Runtime) listenerTLSConfig(name string) *tls.Config {
	// Dedicated Metrics material wins where it is configured: it is mutually
	// authenticated against a client CA the other listeners know nothing about.
	if name == "metrics" && r.reload.metricsTLS != nil {
		return r.reload.metricsTLS.serverConfig()
	}
	// Otherwise the Metrics listener falls back to the server certificate, the
	// same as Gateway and Admin. Without this, enabling tls without also
	// enabling metrics.tls would quietly move /metrics back to plaintext — and
	// `halro stats` would still be addressing it as https.
	if r.reload.serving == nil {
		return nil
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: r.reload.serving.getCertificate}
}

// openTLSMaterial loads every configured keypair once, before the first bind.
func (r *Runtime) openTLSMaterial() error {
	if r.config.TLS.Enabled && r.reload.serving == nil {
		holder, err := newCertificateHolder(r.config.TLS.Certificates, r.logger)
		if err != nil {
			return err
		}
		r.reload.serving = holder
	}
	if r.config.Metrics.Enabled && r.config.Metrics.TLS.Enabled && r.reload.metricsTLS == nil {
		holder, err := newMetricsTLSHolder(r.config.Metrics.TLS, r.logger)
		if err != nil {
			return err
		}
		r.reload.metricsTLS = holder
	}
	return nil
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
		guarded.Use(r.refuseWhileSnapshotsStale(staleErrorOpenAI))
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
		guarded.Use(r.refuseWhileSnapshotsStale(staleErrorAnthropic))
		guarded.Use(r.gateway.LimitAnthropic)
		guarded.Use(r.gateway.GuardAnthropic)
		guarded.Post("/v1/messages", r.gateway.Messages)
		guarded.Post("/v1/messages/count_tokens", r.gateway.CountTokens)
	})
	router.Get("/", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, map[string]any{
			"name":    "halro",
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
	router.With(r.requireAdmin).Get("/admin/api/v1/onboarding/readiness", r.getAdminOnboardingReadiness)
	router.With(r.requireAdmin).Get("/admin/api/v1/master-key/custody", r.adminMasterKeyCustody)
	router.With(r.requireAdmin).Get("/admin/api/v1/master-key/runbooks/lifecycle", r.adminMasterKeyLifecycleRunbook)
	router.With(r.requireAdmin).Get("/admin/api/v1/master-key/runbooks/recovery", r.adminMasterKeyRecoveryRunbook)
	router.With(r.requireAdmin).Get("/admin/api/v1/runbooks/gateway-key-compromise", r.adminGatewayKeyCompromiseRunbook)
	router.With(r.requireAdmin).Get("/admin/api/v1/runbooks/configuration-stale", r.adminConfigurationStaleRunbook)
	router.With(r.requireAdmin).Get("/admin/api/v1/runbooks/file-master-key-rotation", r.adminFileMasterKeyRotationRunbook)
	router.With(r.requireAdmin).Get("/admin/api/v1/usage", r.adminUsage)
	router.With(r.requireAdmin).Get("/admin/api/v1/usage/summary", r.adminUsageSummary)
	router.With(r.requireAdmin).Get("/admin/api/v1/usage/failures", r.adminUsageFailures)
	router.With(r.requireAdmin).Get("/admin/api/v1/usage/failures/{requestID}/payload", r.adminUsageFailurePayload)
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
	// Compile-time metadata about what this build can serve. Same bar as the
	// other Admin reads — a read_only role may fetch it, since it is what any
	// connection form needs before it can offer anything.
	router.With(r.requireAdmin).Get("/admin/api/v1/provider-profiles", r.getAdminProviderProfiles)
	router.With(r.requireAdmin).Get("/admin/api/v1/model-catalog", r.getAdminModelCatalog)
	router.With(r.requireAdminMutation).Post("/admin/api/v1/model-catalog/refresh", r.refreshAdminModelCatalog)
	router.With(r.requireAdminMutation).Post("/admin/api/v1/providers", r.createAdminProvider)
	router.With(r.requireAdmin).Get("/admin/api/v1/providers/{id}", r.getAdminProvider)
	router.With(r.requireAdmin).Get("/admin/api/v1/providers/{id}/invocation-targets", r.listAdminInvocationTargets)
	// Both of these reach a Provider with the operator's credential, so they are
	// mutations of the outside world and carry a CSRF token. See
	// refreshAdminInvocationTargets for why an Origin check on a GET is not an
	// option here.
	router.With(r.requireAdminMutation).Post("/admin/api/v1/providers/{id}/invocation-targets", r.refreshAdminInvocationTargets)
	router.With(r.requireAdminMutation).Post("/admin/api/v1/providers/{id}/invocation-targets/*", r.resolveAdminInvocationTarget)
	router.With(r.requireAdminMutation).Put("/admin/api/v1/providers/{id}", r.updateAdminProvider)
	router.With(r.requireAdminMutation).Delete("/admin/api/v1/providers/{id}", r.deleteAdminProvider)
	router.With(r.requireAdminMutation).Post("/admin/api/v1/providers/{id}/test", r.testAdminProvider)
	router.With(r.requireAdminMutation).Post("/admin/api/v1/providers/{id}/model-capability-detections", r.createAdminModelCapabilityDetection)
	router.With(r.requireAdmin).Get("/admin/api/v1/model-capability-detections/{id}", r.getAdminModelCapabilityDetection)
	router.With(r.requireAdminMutation).Delete("/admin/api/v1/model-capability-detections/{id}", r.cancelAdminModelCapabilityDetection)
	router.With(r.requireAdmin).Get("/admin/api/v1/deployments", r.listAdminDeployments)
	router.With(r.requireAdminMutation).Post("/admin/api/v1/deployments", r.createAdminDeployment)
	router.With(r.requireAdmin).Get("/admin/api/v1/deployments/{id}", r.getAdminDeployment)
	router.With(r.requireAdminMutation).Put("/admin/api/v1/deployments/{id}", r.updateAdminDeployment)
	router.With(r.requireAdminMutation).Delete("/admin/api/v1/deployments/{id}", r.deleteAdminDeployment)
	router.With(r.requireAdminMutation).Post("/admin/api/v1/deployments/{id}/test", r.testAdminDeployment)
	router.With(r.requireAdminMutation).Post("/admin/api/v1/deployments/{id}/capabilities/preflight", r.preflightAdminDeploymentCapabilities)
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
			writer.Header().Set("WWW-Authenticate", `Bearer realm="halro-metrics"`)
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
		if err := r.writeMetrics(request.Context(), writer); err != nil {
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
	// A durable configuration change that has not reached the snapshots is not
	// a ready instance: the data plane is refusing requests, so an orchestrator
	// that keeps sending them is the only one who does not know.
	if activation := r.activation.status(); activation.Stale {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]any{
			"status": "not_ready", "activation": activation,
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
