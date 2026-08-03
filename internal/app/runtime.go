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
	"github.com/akz142857/Heimdall/internal/budget"
	"github.com/akz142857/Heimdall/internal/buildinfo"
	"github.com/akz142857/Heimdall/internal/config"
	"github.com/akz142857/Heimdall/internal/domain"
	gatewaycore "github.com/akz142857/Heimdall/internal/gateway"
	"github.com/akz142857/Heimdall/internal/gatewayapi"
	"github.com/akz142857/Heimdall/internal/id"
	"github.com/akz142857/Heimdall/internal/ledger"
	"github.com/akz142857/Heimdall/internal/metricsauth"
	"github.com/akz142857/Heimdall/internal/provider"
	"github.com/akz142857/Heimdall/internal/redaction"
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
	tokenGuard          *tokenguard.Manager
	redactor            *redaction.Engine
	alerts              *alert.Dispatcher
	audit               *audit.Log
	auditBatchMu        sync.Mutex
	auditBatchPending   []adminAuditRequest
	auditBatchRunning   bool
	adminTopologyMu     sync.Mutex
	providerModelsMu    sync.Mutex
	providerModels      map[string]providerModelCatalogCache
	adminProjectMu      sync.Mutex
	adminAlertMu        sync.Mutex
	adminSettingsMu     sync.Mutex
	adminIdentityMu     sync.Mutex
	metricsTokenHash    [32]byte
	metricsAuthorizer   *metricsauth.Authorizer
	metricsScrapes      chan struct{}
	metricsAuthFailed   atomic.Uint64
	metricsBusy         atomic.Uint64
	metricsRenderErrs   atomic.Uint64
	startedAt           time.Time
	kmsRecoveryLastUsed time.Time
	adminSessions       *adminauth.Manager
	adminLoginMu        sync.Mutex
	adminLogin          map[string]adminLoginWindow
	adminSetupRateMu    sync.Mutex
	adminSetupRate      map[string]adminLoginWindow
	setupMu             sync.Mutex
	setupToken          string
	setupTokenNeeded    bool
	backgroundCtx       context.Context
	backgroundCancel    context.CancelFunc
	backgroundWait      sync.WaitGroup
	usage               *usage.Aggregate
	usageCollector      *usage.Collector
	usageExporter       *usage.Exporter
	usageLocation       *time.Location
	closeOnce           sync.Once
	closeErr            error
	draining            atomic.Bool
	runtimeSettings     atomic.Pointer[domain.RuntimeSettings]
	uiSettings          atomic.Pointer[domain.InstanceUISettings]
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
	var metricsAuthorizer *metricsauth.Authorizer
	if cfg.Metrics.Enabled && cfg.Metrics.RequireAuth {
		if cfg.Metrics.CredentialFile != "" {
			metricsAuthorizer, err = metricsauth.NewAuthorizer(cfg.Metrics.CredentialFile)
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
	accountingStatus := ledger.NewStatus()
	ledgerOptions := ledger.Options{
		QueueCapacity: cfg.Usage.WALQueueCapacity,
		MaxBatch:      cfg.Usage.WALMaxBatch,
		FlushInterval: cfg.Usage.WALFlushInterval.Value(),
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
	if _, err := ledgerLog.Replay(ledger.Watermark{}, ledgerState.Apply); err != nil {
		ledgerLog.Close()
		metadata.Close()
		secretVault.Close()
		return fail(fmt.Errorf("replay ledger: %w", err))
	}
	usageAggregate, usageWatermark := restoreUsageAggregate(metadata, logger)
	if _, err := ledgerLog.Replay(usageWatermark, usageAggregate.Apply); err != nil {
		ledgerLog.Close()
		metadata.Close()
		secretVault.Close()
		return fail(fmt.Errorf("replay usage aggregate: %w", err))
	}
	usageExporter, err := usage.NewExporter(cfg.UsagePath())
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
	location, err := time.LoadLocation(cfg.Usage.Timezone)
	if err != nil {
		ledgerLog.Close()
		metadata.Close()
		secretVault.Close()
		return fail(fmt.Errorf("load usage timezone: %w", err))
	}
	accounting, err := budget.New(ledgerLog, ledgerState, location)
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
			MaxAttempts:                cfg.Gateway.MaxTotalAttempts,
			MaxAttemptsPerTarget:       cfg.Retry.MaxAttemptsPerTarget,
			RetryBaseDelay:             cfg.Retry.BaseDelay.Value(),
			RetryMaxDelay:              cfg.Retry.MaxDelay.Value(),
			RetryJitter:                cfg.Retry.Jitter,
			CircuitFailureThreshold:    cfg.CircuitBreaker.ConsecutiveFailures,
			CircuitOpenDuration:        cfg.CircuitBreaker.OpenDuration.Value(),
			CircuitHalfOpenMaxRequests: cfg.CircuitBreaker.HalfOpenMaxRequests,
			TokenGuard:                 tokenGuard,
			Redactor:                   redactor,
			Resources:                  metadata,
			ResourceObjectDir:          filepath.Join(cfg.Storage.DataDir, "provider-objects"),
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
	gatewayHandler, err := gatewayapi.NewWithOptions(gatewayService, gatewayapi.Options{
		MaxRequestBytes:   cfg.Server.MaxRequestBytes,
		RouteTimeout:      cfg.Gateway.RouteTotalTimeout.Value(),
		StreamTimeout:     cfg.Gateway.StreamMaxDuration.Value(),
		WriteTimeout:      cfg.Gateway.DownstreamWriteTimeout.Value(),
		TrustProxyHeaders: cfg.Security.TrustProxyHeaders,
		TrustedProxyCIDRs: parsePrefixes(cfg.Security.TrustedProxyCIDRs),
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
	kmsRecoveryLastUsed, err := lastKMSRecoveryUse(auditLog)
	if err != nil {
		auditLog.Close()
		alertDispatcher.Close()
		ledgerLog.Close()
		metadata.Close()
		providerRegistry.Close()
		secretVault.Close()
		return fail(fmt.Errorf("inspect Recovery Slot audit history: %w", err))
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
		tokenGuard:          tokenGuard,
		redactor:            redactor,
		alerts:              alertDispatcher,
		audit:               auditLog,
		metricsTokenHash:    metricsTokenHash,
		metricsAuthorizer:   metricsAuthorizer,
		metricsScrapes:      make(chan struct{}, cfg.Metrics.MaxConcurrentScrapes),
		startedAt:           time.Now(),
		kmsRecoveryLastUsed: kmsRecoveryLastUsed,
		adminSessions:       adminSessions,
		adminLogin:          make(map[string]adminLoginWindow),
		adminSetupRate:      make(map[string]adminLoginWindow),
		setupToken:          setupToken,
		setupTokenNeeded:    setupRequiresToken(cfg),
		usage:               usageAggregate,
		usageCollector:      usageCollector,
		usageExporter:       usageExporter,
		usageLocation:       location,
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
	runtime.backgroundWait.Add(5)
	go func() {
		defer runtime.backgroundWait.Done()
		runtime.usageCollector.Run(backgroundContext)
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

func restoreUsageAggregate(store *boltstore.Store, logger *slog.Logger) (*usage.Aggregate, ledger.Watermark) {
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

func parsePrefixes(raw []string) []netip.Prefix {
	result := make([]netip.Prefix, 0, len(raw))
	for _, value := range raw {
		if prefix, err := netip.ParsePrefix(value); err == nil {
			result = append(result, prefix)
		}
	}
	return result
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

// RunWithReady binds every configured listener before calling ready. This
// keeps startup guidance and service-manager readiness signals from claiming
// success when one of the ports cannot actually be opened.
func (r *Runtime) RunWithReady(ctx context.Context, ready func() error) error {
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

func (r *Runtime) gatewayRouter() http.Handler {
	router := chi.NewRouter()
	router.Use(r.recoverPanics)
	router.Get("/health/live", r.live)
	router.Get("/health/ready", r.ready)
	router.Post("/v1/chat/completions", r.gateway.ChatCompletions)
	router.Post("/v1/responses", r.gateway.Responses)
	router.Post("/v1/messages", r.gateway.Messages)
	router.Post("/v1/embeddings", r.gateway.Embeddings)
	router.Post("/v1/moderations", r.gateway.Moderations)
	router.Post("/v1/images/generations", r.gateway.Images)
	router.Post("/v1/audio/speech", r.gateway.Speech)
	router.Post("/v1/audio/transcriptions", r.gateway.Transcriptions)
	router.Post("/v1/rerank", r.gateway.Rerank)
	router.Post("/v1/async/invocations", r.gateway.StartAsyncInvoke)
	router.Get("/v1/async/invocations/{asyncID}", r.gateway.GetAsyncInvoke)
	router.Post("/v1/async/invocations/{asyncID}/cancel", r.gateway.CancelAsyncInvoke)
	router.Post("/v1/files", r.gateway.CreateFile)
	router.Get("/v1/files/{fileID}", r.gateway.GetFile)
	router.Get("/v1/files/{fileID}/content", r.gateway.DownloadFile)
	router.Delete("/v1/files/{fileID}", r.gateway.DeleteFile)
	router.Post("/v1/batches", r.gateway.CreateBatch)
	router.Get("/v1/batches/{batchID}", r.gateway.GetBatch)
	router.Post("/v1/batches/{batchID}/cancel", r.gateway.CancelBatch)
	router.Get("/", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, map[string]any{
			"name":    "heimdall",
			"version": buildinfo.Current(),
		})
	})
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
	router.With(r.requireAdminSetupMutation).Post("/admin/api/v1/session/logout", r.logoutAdmin)
	router.With(r.requireAdminMutation).Post("/admin/api/v1/session/password", r.changeAdminPassword)
	router.With(r.requireAdminBase).Get("/admin/api/v1/security/mfa", r.getAdminMFA)
	router.With(r.requireAdminSetupMutation).Post("/admin/api/v1/security/mfa/authenticators", r.createAdminMFAAuthenticator)
	router.With(r.requireAdminSetupMutation).Post("/admin/api/v1/security/mfa/authenticators/{id}/confirm", r.confirmAdminMFAAuthenticator)
	router.With(r.requireAdminSetupMutation).Delete("/admin/api/v1/security/mfa/authenticators/{id}/pending", r.cancelPendingAdminMFAAuthenticator)
	router.With(r.requireAdminMutation).Patch("/admin/api/v1/security/mfa/authenticators/{id}", r.renameAdminMFAAuthenticator)
	router.With(r.requireAdminMutation).Delete("/admin/api/v1/security/mfa/authenticators/{id}", r.deleteAdminMFAAuthenticator)
	router.With(r.requireAdminMutation).Post("/admin/api/v1/security/mfa/recovery-codes/regenerate", r.regenerateAdminMFARecoveryCodes)
	router.With(r.requireAdminMutation).Delete("/admin/api/v1/security/mfa", r.disableAdminMFA)
	router.With(r.requireAdmin).Get("/admin/api/v1/dashboard", r.adminDashboard)
	router.With(r.requireAdmin).Get("/admin/api/v1/master-key/custody", r.adminMasterKeyCustody)
	router.With(r.requireAdmin).Get("/admin/api/v1/usage", r.adminUsage)
	router.With(r.requireAdmin).Get("/admin/api/v1/usage/requests/{requestID}", r.adminUsageRequest)
	router.With(r.requireAdmin).Get("/admin/api/v1/system/status", r.adminSystemStatus)
	router.With(r.requireAdmin).Get("/admin/api/v1/settings", r.getAdminSettings)
	router.With(r.requireAdminMutation).Put("/admin/api/v1/settings", r.updateAdminSettings)
	router.With(r.requireAdmin).Get("/admin/api/v1/settings/ui", r.getAdminUISettings)
	router.With(r.requireAdminMutation).Put("/admin/api/v1/settings/ui", r.updateAdminUISettings)
	router.With(r.requireAdmin).Get("/admin/api/v1/preferences", r.getAdminPreferences)
	router.With(r.requireAdminMutation).Put("/admin/api/v1/preferences", r.updateAdminPreferences)
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

func (r *Runtime) ready(writer http.ResponseWriter, _ *http.Request) {
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
