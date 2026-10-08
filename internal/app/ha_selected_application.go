package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/akz142857/Halro/internal/adminauth"
	"github.com/akz142857/Halro/internal/auth"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/bearercred"
	"github.com/akz142857/Halro/internal/config"
	gatewaycore "github.com/akz142857/Halro/internal/gateway"
	"github.com/akz142857/Halro/internal/gatewayapi"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/modelcatalog"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/routegate"
	"github.com/akz142857/Halro/internal/sourcelimit"
	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/vault"
)

// A trace only selects this opener. Every permission is subsequently obtained
// from the real Member, Ordering, source carrier, Vault and selected roots.
func tryOpenHASelectedApplication(ctx context.Context, cfg config.Config, logger *slog.Logger, guard *lock.Lock, pressure *resourcePressureRuntime) (*Runtime, bool, error) {
	if cfg.Replication == nil {
		return nil, false, nil
	}
	info, err := os.Lstat(haMemoryStateBootstrapPath(cfg))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() || pressure == nil || pressure.logicalBudget() == nil {
		return nil, false, authenticatedstate.ErrSealIneligible
	}
	r := &Runtime{config: cfg, logger: logger, lock: guard, startedAt: time.Now(), now: time.Now, metricsScrapes: make(chan struct{}, max(1, cfg.Metrics.MaxConcurrentScrapes)), capabilityMetrics: newCapabilityMetrics(), adminLogin: adminRateState{windows: make(map[string]adminLoginWindow)}, adminSetupRate: adminRateState{windows: make(map[string]adminLoginWindow)}, adminStepUp: make(map[string]adminLoginWindow), adminElevation: adminElevationState{grants: make(map[[32]byte]adminElevationGrant)}, runtimeResourceOwnership: runtimeResourceOwnership{resourcePressure: pressure, haSelectedApplicationOwner: true}}
	if err := pressure.monitor.WithSampling(ctx, r.openHASelectedApplication); err != nil {
		closeErr := r.Close()
		return nil, true, &haSelectedApplicationFailure{Owner: r, Cause: errors.Join(err, closeErr)}
	}
	r.backgroundCtx, r.backgroundCancel = context.WithCancel(context.Background())
	// Business drain must not stop the actual sampler before terminal common
	// publication and native/control retirement. The foundation owns its join.
	monitorCtx, stopMonitor := context.WithCancel(context.Background())
	f := r.haSelectedState
	f.pressureCancel, f.pressureDone = stopMonitor, make(chan struct{})
	go func() { defer close(f.pressureDone); r.runResourcePressure(monitorCtx, pressure) }()
	if r.replication.role == replication.RolePrimary {
		r.startHASelectedPrimaryWorkers()
	}
	return r, true, nil
}

type haSelectedApplicationFailure struct {
	Owner *Runtime
	Cause error
}

func (e *haSelectedApplicationFailure) Error() string { return e.Cause.Error() }
func (e *haSelectedApplicationFailure) Unwrap() error { return e.Cause }

func (r *Runtime) openHASelectedApplication(ctx context.Context) (resultErr error) {
	cfg := r.config
	// A partial native/Ordering bank is not whole-vector completion authority.
	// Withdraw this opt-in until all source and derived targets are installed by
	// the actual role-specific preparation, before any Gateway can be exposed.
	if cfg.ResourceBudget.PhysicalTerminalReserve {
		return errors.New("HA physical terminal vector is not prepared")
	}
	var master []byte
	var unlock *haMemoryUnlockPayload
	var err error
	if cfg.Storage.MasterKey.Mode == config.MasterKeyModeKeySlots {
		master, unlock, err = unlockHAMemoryCandidate(ctx, cfg, r.resourcePressure.logicalBudget(), defaultKMSWrapperFactory)
	} else if cfg.Storage.MasterKey.Mode == config.MasterKeyModeFile {
		master, err = unlockMemberMasterKey(ctx, cfg)
	} else {
		return authenticatedstate.ErrSealIneligible
	}
	if err != nil {
		return err
	}
	defer clear(master)
	if unlock != nil {
		defer func() { resultErr = errors.Join(resultErr, unlock.Close()) }()
	}
	r.vault, err = vault.New(master)
	if err != nil {
		return err
	}
	f, err := openHASelectedRuntimeFoundation(ctx, cfg, master, r.lock, r.resourcePressure.logicalBudget(), r.logger)
	r.haSelectedState = f
	if err != nil {
		return fmt.Errorf("open actual HA selected foundation: %w", err)
	}
	primary := f.replication.role == replication.RolePrimary
	if err := f.prepareSelectedServicesWithLease(ctx, primary); err != nil {
		return fmt.Errorf("open actual HA selected business owners: %w", err)
	}
	if unlock != nil {
		if err := unlock.validateSelected(ctx, f); err != nil {
			return err
		}
	}
	if err := f.installHASelectedGovernanceService(ctx); err != nil {
		return err
	}
	c := f.reopened.Owners.Components
	r.store, r.ledger, r.status, r.audit, r.replication = f.metadataStore, c.NativeLedger, c.NativeLedger.Status(), c.Audit, f.replication
	r.accountingState.reader = c.Ledger
	r.accounting = f.business.accounting
	r.governance.log, r.governance.manager = c.NativeGovernance, f.business.governance
	r.usage, r.usageCollector = &runtimeUsage{disk: c.Usage}, f.business.collector
	r.instanceID = f.bootstrap.Evidence.InstanceID
	if err := verifyVaultKeyCheck(r.store, r.vault); err != nil {
		return err
	}
	if err := configurePricingResources(cfg, c.Budget, r.store); err != nil {
		return err
	}
	sessionKey, err := vault.DeriveAdminSessionKey(master)
	if err != nil {
		return err
	}
	defer clear(sessionKey)
	if primary {
		r.adminSessions, err = adminauth.NewManager(r.store, sessionKey, cfg.Admin.SessionTTL.Value(), cfg.Admin.IdleTimeout.Value())
	} else {
		sessions, e := adminauth.NewMemorySessionStore(r.store)
		if e != nil {
			return e
		}
		r.adminSessions, err = adminauth.NewManager(sessions, sessionKey, cfg.Admin.SessionTTL.Value(), cfg.Admin.IdleTimeout.Value())
	}
	if err != nil {
		return err
	}
	if cfg.Metrics.Enabled && cfg.Metrics.RequireAuth {
		if cfg.Metrics.CredentialFile != "" {
			r.metricsAuthorizer, err = bearercred.NewAuthorizer(cfg.Metrics.CredentialFile)
		} else {
			token, e := vault.DeriveMetricsBearerToken(master)
			err = e
			if e == nil {
				r.metricsTokenHash = sha256.Sum256(token)
				clear(token)
			}
		}
		if err != nil {
			return err
		}
	}
	if cfg.Audit.Anchor.Enabled && cfg.Audit.Anchor.Sink == config.AuditAnchorSinkDeadManPull {
		r.anchorAuthorizer, err = bearercred.NewAuthorizer(cfg.Audit.Anchor.CredentialFile)
		if err != nil {
			return err
		}
	}
	settings, err := r.store.RuntimeSettings()
	if err != nil {
		return fmt.Errorf("read actual HA Runtime settings: %w", err)
	}
	r.runtimeSettings.Store(&settings)
	ui, err := r.store.InstanceUISettings()
	if err != nil {
		return fmt.Errorf("read actual HA UI settings: %w", err)
	}
	r.uiSettings.Store(&ui)
	window, err := r.store.InstanceUsageSettings()
	if err != nil {
		return fmt.Errorf("read actual HA Usage settings: %w", err)
	}
	r.usageSettings.Store(&window)
	if primary {
		if err := r.resourcePressure.setSelectedCompletionProfile(ledger.HASelectedCompletionWorkspaceRequirements()); err != nil {
			return err
		}
		if err := r.openHASelectedGateway(ctx); err != nil {
			return err
		}
		if err := f.prepareHASelectedPrimaryStartup(ctx); err != nil {
			return err
		}
	}
	if cfg.ResourceBudget.PhysicalTerminalReserve {
		if err := f.prepareSelectedTransportWithPhysicalCredit(r.logger); err != nil {
			return err
		}
	} else if err := f.prepareSelectedTransport(r.logger); err != nil {
		return err
	}
	return nil
}

// Builds the same real provider/auth/admission/pricing/data plane used by the
// ordinary Runtime, with its actual selected HA accounting service. No provider
// request is issued by construction; producer workers wait for the real startup.
func (r *Runtime) openHASelectedGateway(ctx context.Context) error {
	cfg, metadata, v := r.config, r.store, r.vault
	r.auth = auth.NewSnapshot()
	if err := r.auth.Refresh(ctx, metadata); err != nil {
		return err
	}
	catalog, err := modelcatalog.NewManager(modelcatalog.ManagerOptions{Enabled: cfg.ModelCatalog.Enabled, PinnedRevision: cfg.ModelCatalog.PinnedRevision, RefreshInterval: cfg.ModelCatalog.RefreshInterval.Value(), DataDir: cfg.Storage.DataDir, TrustRoots: modelcatalog.ProductionTrustRoots(), ConnectTimeout: cfg.Gateway.AttemptConnectTimeout.Value(), ResponseTimeout: cfg.Gateway.AttemptResponseHeaderTimeout.Value(), MaxDownloadBytes: cfg.ModelCatalog.MaxDownloadBytes, MaxDecodedBytes: cfg.ModelCatalog.MaxDecodedBytes, MaxCompressionRatio: cfg.ModelCatalog.MaxCompressionRatio, MaxEntries: cfg.ModelCatalog.MaxEntries, Now: time.Now})
	if err != nil {
		return fmt.Errorf("HA signed catalog: %w", err)
	}
	current := modelcatalog.Builtin()
	unavailable := true
	if catalog != nil {
		current = catalog.Current()
		unavailable = catalog.Status().State != modelcatalog.CatalogStateCurrent
	}
	egress, err := newProviderEgressRegistry(ctx, metadata, v)
	if err != nil {
		return err
	}
	r.providerEgress = newProviderEgressManager(egress)
	registry, loaded, err := loadProviderRegistryWithCatalogAndEgress(ctx, cfg, metadata, v, current, unavailable, egress)
	if err != nil {
		return err
	}
	r.providers = registry
	logCapabilityWithholdings(r.logger, loaded)
	r.publishRouteWithholdings(loaded)
	r.tokenGuard, err = loadTokenGuard(ctx, metadata, r.logger)
	if err != nil {
		return err
	}
	r.redactor, err = loadRedaction(ctx, metadata)
	if err != nil {
		return err
	}
	r.alerts, err = loadAlertDispatcher(ctx, cfg, metadata, v)
	if err != nil {
		return err
	}
	r.failureCapture, err = openFailureCaptureContext(ctx, cfg, v)
	if err != nil {
		return err
	}
	r.routes = routegate.New(routegate.Config{AvailabilityThreshold: cfg.Routing.AvailabilityFailures, AvailabilityWindow: cfg.Routing.SuspendFor.Value(), MaxAvailabilityWindow: cfg.Routing.MaxSuspendFor.Value(), HalfOpenMaxRequests: cfg.Routing.ProbeRequests})
	r.routes.UsePersistence(metadata, func(err error) { r.logger.Error("persist HA route suspension", "error", err) })
	suspensions, err := metadata.ListRouteSuspensions(ctx)
	if err != nil {
		return err
	}
	r.routes.Restore(suspensions)
	registry.SetEligibility(r.routes)
	p := r.resourcePressure
	terminalAdmission := r.replication.reserveTerminalRequest
	if cfg.ResourceBudget.PhysicalTerminalReserve {
		terminalAdmission = r.haSelectedState.peerPhysicalTerminalAdmission
	}
	r.gatewayService, err = gatewaycore.NewServiceWithOptions(r.auth, registry, r.accounting, gatewaycore.ServiceOptions{MaxAttempts: cfg.Gateway.MaxTotalAttempts, TerminalAdmission: terminalAdmission, MaxAttemptsPerTarget: cfg.Retry.MaxAttemptsPerTarget, RetryBaseDelay: cfg.Retry.BaseDelay.Value(), RetryMaxDelay: cfg.Retry.MaxDelay.Value(), RetryJitter: cfg.Retry.Jitter, RouteGate: r.routes, TokenGuard: r.tokenGuard, Redactor: r.redactor, Resources: metadata, ResourceObjectDir: filepath.Join(cfg.Storage.DataDir, "provider-objects"), ResourceObjectSealer: v, ResourceObjectReplicator: r.replication.replicateProviderObject, DeferredExecutionTimeout: cfg.Gateway.RouteTotalTimeout.Value(), ResourceBudget: p.logicalBudget(), DeferredRAMBytes: p.deferredEnvelope(), CompletionRAMBytes: p.completionRAMEnvelope(), CompletionDiskBytes: p.completionDiskEnvelope(), CompletionWorkspace: p.completionProfile(), CaptureRAMBytes: p.captureEnvelope(), DeferredResponseWorkers: cfg.Gateway.DeferredResponseWorkers, Pricing: metadata, PricingClockRollbackTolerance: cfg.Gateway.PricingClockRollbackTolerance.Value(), PricingClockForwardTolerance: cfg.Gateway.PricingClockForwardTolerance.Value(), PricingUnknownPolicy: cfg.Gateway.PricingUnknownPolicy, FailureCapture: captureFor(cfg.Gateway.FailureCapture.Enabled, r.failureCapture), Logger: r.logger})
	if err != nil {
		return err
	}
	trusted, err := parsePrefixes(cfg.Security.TrustedProxyCIDRs)
	if err != nil {
		return err
	}
	r.sourceLimiter = sourcelimit.New(cfg.Gateway.SourceRateLimit.SourceRequestsPerMinute(), cfg.Gateway.SourceRateLimit.MaxTrackedSources)
	r.gateway, err = gatewayapi.NewWithOptions(r.gatewayService, gatewayapi.Options{MaxRequestBytes: cfg.Server.MaxRequestBytes, RouteTimeout: cfg.Gateway.RouteTotalTimeout.Value(), StreamTimeout: cfg.Gateway.StreamMaxDuration.Value(), WriteTimeout: cfg.Gateway.DownstreamWriteTimeout.Value(), TrustProxyHeaders: cfg.Security.TrustProxyHeaders, TrustedProxyCIDRs: trusted, SourceLimiter: r.sourceLimiter, AuthorizeKey: func(key string) (int64, error) {
		principal, err := r.auth.Authenticate(key, time.Now())
		if err != nil {
			return 0, err
		}
		return principal.Project.MaxRequestBytes, nil
	}})
	if err != nil {
		return err
	}
	r.capabilityResolution.catalog = catalog
	if catalog != nil {
		catalog.SetObserver(r.observeModelCatalogRefresh)
		catalog.SetActivationPreparer(r.prepareModelCatalogActivation)
		r.capabilityMetrics.setSignedCatalogState(catalog.Status())
	}
	r.capabilityDetections = capabilityDetectionRuntime{cancels: make(map[string]context.CancelFunc), global: make(chan struct{}, cfg.Admin.ModelCapabilityDetection.GlobalConcurrency), providers: make(map[string]chan struct{}), rate: adminRateState{windows: make(map[string]adminLoginWindow)}}
	return nil
}

func (r *Runtime) startHASelectedPrimaryWorkers() {
	r.alerts.SetObserver(r.auditAlertDelivery)
	workers := []func(context.Context){r.usageCollector.Run, r.gatewayService.RunDeferredResponses, r.runActiveDeploymentProbes, r.runCapabilityDetectionMaintenance, r.runProviderResourceMaintenance, func(ctx context.Context) { r.runActivationRecovery(ctx, activationRetryInterval) }, func(ctx context.Context) {
		forwardTokenGuardAlerts(ctx, r.tokenGuard.Events(), r.alerts, r.auditAlertSubmission)
	}, func(ctx context.Context) { r.alerts.Start(); <-ctx.Done() }, r.runAuditAnchorMaintenance}
	if catalog := r.capabilityResolution.catalog; catalog != nil {
		workers = append(workers, catalog.Run)
	}
	for _, work := range workers {
		r.backgroundWait.Add(1)
		go func(work func(context.Context)) {
			defer r.backgroundWait.Done()
			if r.awaitPrimaryStartup(r.backgroundCtx) {
				work(r.backgroundCtx)
			}
		}(work)
	}
}

// Used after Runtime has withdrawn and joined real HTTP/export/background work.
// Unknown retirement keeps the entire parent, vault and directory lock reachable.
func (r *Runtime) closeHASelectedApplication() error {
	r.clearSetupToken()
	r.stopBackgroundWork()
	var err error
	if r.gatewayService != nil {
		err = errors.Join(err, r.gatewayService.ShutdownFailureCapture(context.Background()))
	}
	err = errors.Join(err, r.failureCapture.Close())
	if r.providers != nil {
		r.providers.Close()
	}
	if r.providerEgress != nil {
		r.providerEgress.Close()
	}
	if r.adminSessions != nil {
		r.adminSessions.Close()
	}
	if err != nil {
		return err
	}
	if r.haSelectedState != nil {
		f := r.haSelectedState
		if r.config.ResourceBudget != nil && r.config.ResourceBudget.PhysicalTerminalReserve && r.store != nil && r.ledger != nil {
			finishCtx, cancelFinish := context.WithTimeout(context.Background(), r.config.Server.ShutdownTimeout.Value())
			defer cancelFinish()
			if err = f.finishHAPhysicalBusiness(finishCtx); err != nil {
				return fmt.Errorf("finish actual HA physical business: %w", err)
			}
			if err = f.closeHAPhysicalNative(finishCtx); err != nil {
				return fmt.Errorf("join actual HA physical native: %w", err)
			}
		}
		if err = r.haSelectedState.Close(); err != nil {
			return err
		}
	}
	if r.vault != nil {
		r.vault.Close()
	}
	return r.lock.Close()
}
