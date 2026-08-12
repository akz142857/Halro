package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"sync"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/modelcatalog"
	"github.com/akz142857/Halro/internal/provider"
	anthropicprovider "github.com/akz142857/Halro/internal/provider/anthropic"
	bedrockprovider "github.com/akz142857/Halro/internal/provider/bedrock"
	bedrockmantleprovider "github.com/akz142857/Halro/internal/provider/bedrockmantle"
	geminiprovider "github.com/akz142857/Halro/internal/provider/gemini"
	openaiprovider "github.com/akz142857/Halro/internal/provider/openai"
	"github.com/akz142857/Halro/internal/safetransport"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/vault"
)

// providerEndpointPolicy is the one place the provider-endpoint rules are
// derived from configuration.
//
// It exists because they used to be spelled out at each call site, and one of
// the two calls every site makes — ValidateURL and then AudienceWithPolicy —
// was given a policy while the other silently got the strictest one. The
// configured opt-in was therefore cancelled everywhere it was read. Deriving
// both from the same value makes that failure shape unavailable.
//
// AllowedHosts is left to the caller: it is per-provider, not global.
func providerEndpointPolicy(cfg config.Config) safetransport.Policy {
	return safetransport.Policy{
		RequireHTTPS: true,
		AllowPrivate: cfg.Security.AllowPrivateProviderEndpoints,
	}
}

func (r *Runtime) reloadProviderRegistry(ctx context.Context) error {
	finalize, err := r.prepareProviderRegistryActivation(ctx, r.effectiveModelCatalog(), r.modelCatalogUnavailable())
	if err != nil {
		return err
	}
	finalize(true)
	return nil
}

// activationTimeout bounds the work that carries a durable Admin mutation into
// the running snapshots. It is there to stop a wedged store read from holding an
// Admin request open indefinitely — not to let the client's patience decide
// whether a revocation takes effect.
const activationTimeout = 30 * time.Second

// activationContext returns the context that carries an already-committed
// mutation into the live snapshots.
//
// Deliberately not the Admin request's. Activation only begins once the write is
// durable, so a client that disconnects — or a proxy that gives up, or a request
// deadline that expires — used to abort the rebuild and leave the persisted
// state and the serving state disagreeing, with the data plane on the stale
// side: a revoked Gateway Key still authenticating, a deleted Route still
// routing, until some later mutation happened to succeed or the process
// restarted. Because the store refuses reads on a canceled context, cancelling
// at any point after the commit was enough; it was not a narrow race.
//
// Still bounded, and it dies with the runtime rather than outliving it.
func (r *Runtime) activationContext() (context.Context, context.CancelFunc) {
	parent := r.backgroundCtx
	if parent == nil {
		// Open assigns backgroundCtx late; a mutation cannot arrive before then,
		// but activation must not depend on that ordering to be uncancellable.
		parent = context.Background()
	}
	return context.WithTimeout(parent, activationTimeout)
}

// activateTopology rebuilds the routing registry after a durable Provider,
// Deployment, Route, Credential or price mutation. It takes no context on
// purpose: the only context that belongs here is one the Admin client cannot
// cancel, so the call sites are not offered the choice.
func (r *Runtime) activateTopology() error {
	ctx, cancel := r.activationContext()
	defer cancel()
	if err := r.reloadProviderRegistry(ctx); err != nil {
		r.logger.Error("routing registry activation failed after a durable mutation", "error", err)
		r.activation.markStale(activationDomainTopology, "routing registry: "+err.Error(), time.Now().UTC())
		return err
	}
	r.activation.markCurrent(activationDomainTopology)
	return nil
}

// activateTopologyAfterCommit carries an already-committed topology mutation
// into the live registry.
//
// The error is deliberately not returned to the Admin caller. Under the commit
// protocol the store commit is the commit point, so a failed activation means
// the mutation happened and is not yet in force — reporting it as a failed
// request would describe a change that did take effect as one that did not.
// The runtime is marked stale instead, which refuses data-plane traffic until
// the snapshots catch up, and the recovery loop retries.
func (r *Runtime) activateTopologyAfterCommit() {
	_ = r.activateTopology()
}

// prepareModelCatalogActivation serializes a signed-catalog commit with every
// Provider, Deployment, and Route mutation. The lock is acquired before the
// candidate registry reads the store and is held until the Manager commits or
// aborts it, so a stale candidate cannot resurrect topology an Admin mutation
// already removed.
func (r *Runtime) prepareModelCatalogActivation(candidate *modelcatalog.Catalog) (func(bool), error) {
	r.adminTopologyMu.Lock()
	finalize, err := r.prepareProviderRegistryActivation(r.backgroundCtx, candidate, false)
	if err != nil {
		r.adminTopologyMu.Unlock()
		return nil, err
	}
	var once sync.Once
	return func(activate bool) {
		once.Do(func() {
			defer r.adminTopologyMu.Unlock()
			finalize(activate)
		})
	}, nil
}

// reconcileProviderRegistryWithCatalogState applies a degraded/unavailable
// catalog transition under the same topology coordinator as successful signed
// activation. It is called after the Manager has moved its resolution-effective
// catalog to the bundled fallback.
func (r *Runtime) reconcileProviderRegistryWithCatalogState(ctx context.Context) error {
	r.adminTopologyMu.Lock()
	defer r.adminTopologyMu.Unlock()
	return r.reloadProviderRegistry(ctx)
}

func (r *Runtime) prepareProviderRegistryActivation(ctx context.Context, catalog *modelcatalog.Catalog, unavailable bool) (func(bool), error) {
	next, report, err := loadProviderRegistryWithCatalog(ctx, r.config, r.store, r.vault, catalog, unavailable)
	if err != nil {
		return nil, err
	}
	return func(activate bool) {
		if !activate {
			next.Close()
			return
		}
		logCapabilityWithholdings(r.logger, report)
		r.auditCapabilityWithholdings(ctx, report)
		retired := r.providers.Replace(next)
		if len(retired) == 0 {
			return
		}
		grace := max(
			r.config.Gateway.RouteTotalTimeout.Value(),
			r.config.Gateway.StreamMaxDuration.Value(),
		) + time.Second
		r.backgroundWait.Add(1)
		go func() {
			defer r.backgroundWait.Done()
			timer := time.NewTimer(grace)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-r.backgroundCtx.Done():
			}
			for _, adapter := range retired {
				adapter.Close()
			}
		}()
	}, nil
}

// capabilityWithholding records a route kept out of the routing candidates
// because its deployment's capability snapshot no longer describes something
// this build and the catalog still support.
//
// It is returned rather than logged in place so the caller decides what to do
// with it — start-up logs it, a hot reload logs and audits it — and so a test
// can assert the load succeeded *and* withheld the right route.
type capabilityWithholding struct {
	RouteID           string
	DeploymentID      string
	State             domain.CapabilityReviewState
	Reason            string
	NoLongerSupported []string
}

// Why a route could not become a Target. Distinct from a capability
// withholding: drift is an expected state an operator resolves by reviewing the
// deployment's claim, while these mean the records the route points at cannot
// produce a Target at all.
const (
	withheldDeploymentUnavailable = "deployment_unavailable"
	withheldBindingUnavailable    = "binding_unavailable"
	withheldProviderModelRejected = "provider_model_rejected"
	withheldPriceUnreadable       = "price_unreadable"
	withheldTargetRejected        = "target_rejected"
)

// Why a Provider, or one of its bindings, was left out of the load. Scoped wider
// than a route withholding: nothing on this provider or binding can serve.
//
// These are the failures an operator or an upgrade causes — a tightened endpoint
// policy, a profile this build narrowed, an adapter that will not construct.
// Integrity and vault-trust failures are not here: a credential that disagrees
// with its own audience, or will not decrypt under it, still refuses the whole
// load, because that is corrupt security state rather than a configuration the
// operator can be told about and left running.
const (
	excludedEndpointRejected           = "endpoint_rejected"
	excludedBindingProfileIncompatible = "binding_profile_incompatible"
	excludedAdapterUnavailable         = "adapter_unavailable"
	excludedCapabilityCeilingExceeded  = "capability_ceiling_exceeded"
)

// referenceWithholding records a route kept out of the routing candidates
// because what it references cannot produce a Target: the Deployment is gone or
// switched off, the Provider Binding produced no adapter, or the upstream model
// is not one its profile accepts.
//
// These used to fail the entire load. That made one dangling reference strictly
// worse than a whole provider being switched off: disabling a Provider or
// Deployment merely drops its routes, while a route left pointing at a disabled
// *Binding* refused to build any registry at all — so the process could not
// start, and no unrelated route could be edited. Withholding follows what drift
// already does, and for the same reason: one broken record must not take down
// every working one, and must never make the binary unable to open a data
// directory it has already written.
//
// The route is fail-closed either way. What changes is the blast radius.
type referenceWithholding struct {
	RouteID      string
	DeploymentID string
	ProviderID   string
	BindingID    string
	Reason       string
}

// providerExclusion records a Provider — or one binding of one — that could not
// be loaded. BindingID is empty when the problem is provider-wide.
//
// Like a reference withholding, these used to fail the entire load, with the
// same consequence: a state the operator could not edit their way out of and a
// binary that would not open a directory it wrote. The reachable one is
// endpoint_rejected, because an endpoint that was legal when the provider was
// created stops being legal the moment allow_private_provider_endpoints is
// tightened — and tightening a security setting must not be what prevents
// start-up. Excluding the provider keeps every other provider serving and keeps
// this one fail-closed.
type providerExclusion struct {
	ProviderID string
	BindingID  string
	// Reason is a class, never the underlying error: endpoint and credential
	// failures carry hostnames and key material, and this record is durable.
	Reason string
}

// loadReport carries what a registry load excluded and why. The kinds stay
// separate because they mean different things to an operator and are audited
// differently: drift is reviewed, a dangling reference is repaired, and an
// excluded provider is usually a credential or a policy the operator changed.
type loadReport struct {
	Drifted  []capabilityWithholding
	Dangling []referenceWithholding
	Excluded []providerExclusion
}

// logCapabilityWithholdings names the routes that are up but not routing. IDs
// only: a capability set is configuration, not a secret, but there is nothing
// here that needs more than the identifier an operator would look up.
func logCapabilityWithholdings(logger *slog.Logger, report loadReport) {
	if logger == nil {
		return
	}
	for _, item := range report.Drifted {
		logger.Warn("route withheld from routing candidates",
			"route", item.RouteID, "deployment", item.DeploymentID,
			"capability_review_state", string(item.State))
	}
	// Louder than drift: drift is a state the design expects a binary upgrade to
	// produce, while a dangling reference means the stored topology disagrees
	// with itself and an operator has to repair it.
	for _, item := range report.Dangling {
		logger.Error("route withheld: reference cannot produce a routing target",
			"route", item.RouteID, "deployment", item.DeploymentID,
			"provider", item.ProviderID, "binding", item.BindingID,
			"reason", item.Reason)
	}
	for _, item := range report.Excluded {
		logger.Error("provider excluded from routing candidates",
			"provider", item.ProviderID, "binding", item.BindingID,
			"reason", item.Reason)
	}
}

func loadProviderRegistry(
	ctx context.Context,
	cfg config.Config,
	store *boltstore.Store,
	secretVault *vault.Vault,
) (*provider.Registry, loadReport, error) {
	return loadProviderRegistryWithCatalog(ctx, cfg, store, secretVault, modelcatalog.Builtin(), false)
}

func loadProviderRegistryWithCatalog(
	ctx context.Context,
	cfg config.Config,
	store *boltstore.Store,
	secretVault *vault.Vault,
	catalog *modelcatalog.Catalog,
	catalogUnavailable bool,
) (*provider.Registry, loadReport, error) {
	var report loadReport
	instances, err := store.ListProviders(ctx)
	if err != nil {
		return nil, report, fmt.Errorf("list providers: %w", err)
	}
	routes, err := store.ListRoutes(ctx)
	if err != nil {
		return nil, report, fmt.Errorf("list routes: %w", err)
	}
	deployments, err := store.ListDeployments(ctx)
	if err != nil {
		return nil, report, fmt.Errorf("list deployments: %w", err)
	}
	deploymentByID := make(map[string]domain.Deployment, len(deployments))
	for _, deployment := range deployments {
		deploymentByID[deployment.ID] = deployment
	}
	instanceByID := make(map[string]domain.ProviderInstance, len(instances))
	for _, instance := range instances {
		instanceByID[instance.ID] = instance
	}
	registry := provider.NewRegistry()
	pricingSelectedAt := time.Now().UTC()
	adapters := make(map[string]provider.Adapter)
	providerBindingIDs := make(map[string][]string)
	providerLimits := make(map[string]int64)
	// Integrity and vault-trust failures still refuse the load, and the adapters
	// built so far are closed rather than leaked. This is the narrow answer to
	// "should any load failure stop the process": yes — the ones that say the
	// stored state has been tampered with or that the vault cannot be trusted.
	// Everything else excludes what it affects and keeps serving the rest.
	refuse := func(err error) (*provider.Registry, loadReport, error) {
		for _, adapter := range adapters {
			adapter.Close()
		}
		return nil, loadReport{}, err
	}
	excludeProvider := func(instance domain.ProviderInstance, reason string) {
		report.Excluded = append(report.Excluded, providerExclusion{ProviderID: instance.ID, Reason: reason})
	}
	excludeBinding := func(instance domain.ProviderInstance, bindingID, reason string) {
		report.Excluded = append(report.Excluded, providerExclusion{
			ProviderID: instance.ID, BindingID: bindingID, Reason: reason,
		})
	}
	for _, instance := range instances {
		if !instance.Enabled || instance.DeletedAt != nil {
			continue
		}
		credential, err := store.GetCredential(ctx, instance.CredentialID)
		if err != nil {
			return refuse(fmt.Errorf("provider %q credential: %w", instance.ID, err))
		}
		if credential.Type != instance.Type {
			return refuse(fmt.Errorf("provider %q credential type mismatch", instance.ID))
		}
		policy := providerEndpointPolicy(cfg)
		policy.AllowedHosts = instance.AllowedHosts
		endpoint, err := safetransport.ValidateURL(instance.BaseURL, policy)
		if err != nil {
			// The reachable one: an endpoint legal when the provider was created
			// can become illegal when the operator tightens
			// allow_private_provider_endpoints. Tightening a security setting must
			// not be what stops the process from starting.
			excludeProvider(instance, excludedEndpointRejected)
			continue
		}
		audience, err := safetransport.AudienceWithPolicy(instance.BaseURL, string(instance.Type), policy)
		if err != nil {
			excludeProvider(instance, excludedEndpointRejected)
			continue
		}
		if credential.Audience != audience {
			return refuse(fmt.Errorf("provider %q credential audience mismatch", instance.ID))
		}
		providerLimits[instance.ID] = instance.MaxConcurrency
		for _, binding := range instance.EffectiveProfileBindings() {
			if !binding.Enabled {
				continue
			}
			manifest, ok := provider.BuiltinProfile(binding.ProfileID)
			if !ok || manifest.ProviderType != instance.Type || manifest.AccessSurface != binding.AccessSurface || manifest.CredentialScheme != binding.CredentialScheme {
				excludeBinding(instance, binding.ID, excludedBindingProfileIncompatible)
				continue
			}
			// A stored binding that claims more than its profile allows is
			// withheld here rather than clamped. Clamping would keep serving a
			// record whose declared capabilities and actual behaviour disagree,
			// and the disagreement is exactly what capability evidence is
			// supposed to make impossible. Withholding is also why this is not
			// fatal: the write paths reject the state, so reaching it means a
			// record predating the check, and one such record must not stop the
			// process from loading every other provider.
			if domain.IsImmutableCapabilityProfile(binding.ProfileID) &&
				!domain.ProviderCapabilitiesSubset(binding.Capabilities, domain.DefaultProviderCapabilitiesForProfile(instance.Type, binding.ProfileID)) {
				excludeBinding(instance, binding.ID, excludedCapabilityCeilingExceeded)
				continue
			}
			if credential.AccessSurface != binding.AccessSurface || credential.Scheme != binding.CredentialScheme {
				return refuse(fmt.Errorf("provider %q binding %q credential profile mismatch", instance.ID, binding.ID))
			}
			plaintext, decryptErr := secretVault.DecryptCredential(credential.ID, string(credential.Type), credential.Audience, credential.Ciphertext)
			if decryptErr != nil {
				// Fatal on purpose. A credential that will not decrypt under its own
				// audience means either the master key or the record is wrong, and
				// neither is a state to keep serving other providers through.
				return refuse(fmt.Errorf("provider %q binding %q decrypt credential: %w", instance.ID, binding.ID, decryptErr))
			}
			adapter, adapterErr := newProviderBindingAdapter(cfg, instance, binding, endpoint, policy, plaintext)
			clear(plaintext)
			if adapterErr != nil {
				excludeBinding(instance, binding.ID, excludedAdapterUnavailable)
				continue
			}
			profiled, bridgeErr := provider.NewLegacyAdapterBridge(adapter, manifest, binding.CapabilityEvidence)
			if bridgeErr != nil {
				adapter.Close()
				excludeBinding(instance, binding.ID, excludedAdapterUnavailable)
				continue
			}
			adapters[binding.ID] = profiled
			providerBindingIDs[instance.ID] = append(providerBindingIDs[instance.ID], binding.ID)
			if err := registry.RegisterBindingAdapter(instance.ID, binding.ID, profiled); err != nil {
				profiled.Close()
				delete(adapters, binding.ID)
				excludeBinding(instance, binding.ID, excludedAdapterUnavailable)
				continue
			}
		}
	}
	for _, route := range routes {
		if !route.Enabled || route.DeletedAt != nil {
			continue
		}
		inputPrice, outputPrice, fixedPrice := int64(0), int64(0), int64(0)
		deploymentID := route.DeploymentID
		bindingID := ""
		deploymentLimit := int64(0)
		var capabilities provider.Capabilities
		var providerID, providerModel string
		{
			deployment, exists := deploymentByID[deploymentID]
			if !exists || !deployment.Enabled || deployment.DeletedAt != nil {
				// Withheld rather than fatal. The Admin API refuses to disable or
				// delete a deployment that an enabled route still names, so this
				// is a state normal operation cannot reach — but "cannot reach"
				// is not "impossible", and the cost of being wrong here was a
				// data directory the binary refuses to open.
				report.Dangling = append(report.Dangling, referenceWithholding{
					RouteID: route.ID, DeploymentID: deploymentID,
					ProviderID: deployment.ProviderID, BindingID: deployment.BindingID,
					Reason: withheldDeploymentUnavailable,
				})
				continue
			}
			// Drift is resolved here rather than on the request path, so a
			// profile this build narrowed is a state an operator can see instead
			// of production traffic failing one request at a time.
			//
			// The drifted deployment is withheld from the candidates; it does not
			// stop the load. Refusing to build the registry would turn one stale
			// snapshot into a process that cannot start, taking down every other
			// route with it — the opposite of what fail-closed is meant to buy.
			// reviewForDeployment rather than an inline lookup, so this path and
			// the Admin/doctor one read a missing provider the same way. They
			// used to disagree: this skipped the check and admitted, that
			// answered drifted.
			//
			// No reachable state distinguishes them today — ListProviders
			// returns tombstones too, so instanceByID holds every provider
			// record that exists, and a deployment can only miss one by
			// referencing an ID with no record at all, which the store refuses.
			// It is shared rather than duplicated because the alternative is two
			// spellings of one rule, which is what a later change gets wrong.
			review := reviewForDeploymentWithCatalogState(instanceByID, deployment, catalog, catalogUnavailable)
			if !capabilityReviewAdmitsTraffic(review.State) {
				report.Drifted = append(report.Drifted, capabilityWithholding{
					RouteID: route.ID, DeploymentID: deployment.ID, State: review.State,
					Reason: review.Reason, NoLongerSupported: slices.Clone(review.NoLongerSupported),
				})
				continue
			}
			providerID = deployment.ProviderID
			bindingID = deployment.BindingID
			if bindingID == "" {
				bindingID = matchingBindingID(instanceByID[providerID], deployment.ProfileID)
			}
			providerModel = deployment.ProviderModel
			price, priceErr := store.SelectDeploymentPriceVersion(ctx, deployment.ID, pricingSelectedAt)
			if priceErr != nil && !errors.Is(priceErr, domain.ErrPriceUnavailable) {
				// A price that cannot be read is this deployment's problem, not the
				// registry's. ErrPriceUnavailable is the ordinary "no current price"
				// case and is tolerated with a zero projection; anything else is a
				// store-level failure for this one record.
				report.Dangling = append(report.Dangling, referenceWithholding{
					RouteID: route.ID, DeploymentID: deployment.ID,
					ProviderID: deployment.ProviderID, BindingID: deployment.BindingID,
					Reason: withheldPriceUnreadable,
				})
				continue
			}
			if priceErr == nil {
				inputPrice = price.InputMicrosPerMillion
				outputPrice = price.OutputMicrosPerMillion
				fixedPrice = price.FixedRequestMicrosUSD
			} else {
				inputPrice, outputPrice, fixedPrice = 0, 0, 0
			}
			deploymentLimit = deployment.MaxConcurrency
			capabilities = deploymentCapabilities(deployment, adapters[bindingID])
		}
		adapter := adapters[bindingID]
		if adapter == nil {
			// This is the reachable one, and the reason this whole path stopped
			// being fatal. A Profile Binding can be switched off or dropped from
			// a Provider's binding list while a deployment and route still name
			// it; before, that made the registry unbuildable, so the mutation
			// persisted, activation failed, every later topology edit failed, and
			// the process could not restart.
			report.Dangling = append(report.Dangling, referenceWithholding{
				RouteID: route.ID, DeploymentID: deploymentID,
				ProviderID: providerID, BindingID: bindingID,
				Reason: withheldBindingUnavailable,
			})
			continue
		}
		if profiled, ok := adapter.(provider.ProfiledAdapter); ok {
			if err := bedrockprovider.ValidateProfileModel(profiled.Profile().ID, providerModel); err != nil {
				report.Dangling = append(report.Dangling, referenceWithholding{
					RouteID: route.ID, DeploymentID: deploymentID,
					ProviderID: providerID, BindingID: bindingID,
					Reason: withheldProviderModelRejected,
				})
				continue
			}
		}
		if err := registry.Register(provider.Target{
			ID:                     route.ID,
			DeploymentID:           deploymentID,
			ProviderID:             providerID,
			BindingID:              bindingID,
			PublicModel:            route.PublicModel,
			ProviderModel:          providerModel,
			AccessSurface:          deploymentByID[deploymentID].AccessSurface,
			ProfileID:              deploymentByID[deploymentID].ProfileID,
			Region:                 deploymentByID[deploymentID].Region,
			Adapter:                adapter,
			InputMicrosPerMillion:  inputPrice,
			OutputMicrosPerMillion: outputPrice,
			FixedRequestMicrosUSD:  fixedPrice,
			Priority:               route.Priority,
			Strategy:               route.Strategy,
			Capabilities:           capabilities,
			CapabilityEvidence:     deploymentByID[deploymentID].CapabilityEvidence.Clone(),
			MaxConcurrency:         providerLimits[providerID],
			DeploymentConcurrency:  deploymentLimit,
		}); err != nil {
			report.Dangling = append(report.Dangling, referenceWithholding{
				RouteID: route.ID, DeploymentID: deploymentID,
				ProviderID: providerID, BindingID: bindingID,
				Reason: withheldTargetRejected,
			})
			continue
		}
	}
	return registry, report, nil
}

func newProviderBindingAdapter(cfg config.Config, instance domain.ProviderInstance, binding domain.ProviderProfileBinding, endpoint *url.URL, policy safetransport.Policy, plaintext []byte) (provider.Adapter, error) {
	client, err := safetransport.NewClient(safetransport.Options{
		Policy: policy, ConnectTimeout: cfg.Gateway.AttemptConnectTimeout.Value(),
		ResponseHeaderTimeout: cfg.Gateway.AttemptResponseHeaderTimeout.Value(),
	})
	if err != nil {
		return nil, err
	}
	capabilities := providerCapabilities(binding.Capabilities)
	var adapter provider.Adapter
	var authorizer provider.Authorizer
	switch instance.Type {
	case domain.ProviderOpenAI, domain.ProviderOpenAICompatible, domain.ProviderDeepSeek:
		authorizer, err = provider.NewStaticHeaderAuthorizer(binding.CredentialScheme, "Authorization", "Bearer ", plaintext, "api-key")
		if err == nil {
			adapter, err = openaiprovider.NewWithOptions(openaiprovider.Options{Endpoint: endpoint, Authorizer: authorizer, Client: client, ProviderType: string(instance.Type), Capabilities: capabilities})
		}
	case domain.ProviderAnthropic:
		authorizer, err = provider.NewStaticHeaderAuthorizer(binding.CredentialScheme, "x-api-key", "", plaintext, "Authorization")
		if err == nil {
			adapter, err = anthropicprovider.New(anthropicprovider.Options{Endpoint: endpoint, Authorizer: authorizer, Client: client, Capabilities: capabilities})
		}
	case domain.ProviderAzureOpenAI:
		authorizer, err = provider.NewStaticHeaderAuthorizer(binding.CredentialScheme, "api-key", "", plaintext, "Authorization")
		if err == nil {
			adapter, err = openaiprovider.NewWithOptions(openaiprovider.Options{Endpoint: endpoint, Authorizer: authorizer, Client: client, ProviderType: string(instance.Type), APIVersion: instance.APIVersion, Azure: true, Capabilities: capabilities})
		}
	case domain.ProviderGemini:
		authorizer, err = provider.NewStaticHeaderAuthorizer(binding.CredentialScheme, "x-goog-api-key", "", plaintext, "Authorization")
		if err == nil {
			adapter, err = geminiprovider.New(geminiprovider.Options{Endpoint: endpoint, Authorizer: authorizer, Client: client})
		}
	case domain.ProviderBedrock:
		switch binding.ProfileID {
		case domain.ProfileBedrockConverseText, domain.ProfileBedrockInvokeTitanEmbedV2, domain.ProfileBedrockInvokeTitanImageV2, domain.ProfileBedrockAsyncNovaReel, domain.ProfileBedrockAgentRerankCohere35:
			authorizer, err = bedrockprovider.NewAuthorizer(endpoint, plaintext, nil)
			if err == nil {
				adapter, err = bedrockprovider.New(bedrockprovider.Options{Endpoint: endpoint, Authorizer: authorizer, Client: client, ProfileID: binding.ProfileID})
			}
		case domain.ProfileBedrockMantleOpenAIChat:
			err = bedrockmantleprovider.ValidateEndpoint(endpoint)
			if err == nil {
				authorizer, err = provider.NewStaticHeaderAuthorizer(binding.CredentialScheme, "Authorization", "Bearer ", plaintext, "api-key", "x-api-key")
			}
			if err == nil {
				adapter, err = openaiprovider.NewWithOptions(openaiprovider.Options{Endpoint: endpoint, Authorizer: authorizer, Client: client, ProviderType: string(domain.ProviderBedrock), CredentialScheme: binding.CredentialScheme, Capabilities: capabilities})
			}
		case domain.ProfileBedrockMantleOpenAIResponses:
			err = bedrockmantleprovider.ValidateEndpoint(endpoint)
			if err == nil {
				authorizer, err = provider.NewStaticHeaderAuthorizer(binding.CredentialScheme, "Authorization", "Bearer ", plaintext, "api-key", "x-api-key")
			}
			if err == nil {
				adapter, err = bedrockmantleprovider.NewResponses(bedrockmantleprovider.ResponsesOptions{Endpoint: endpoint, Authorizer: authorizer, Client: client, Capabilities: capabilities})
			}
		case domain.ProfileBedrockMantleAnthropicMessages:
			err = bedrockmantleprovider.ValidateEndpoint(endpoint)
			if err == nil {
				authorizer, err = provider.NewStaticHeaderAuthorizer(binding.CredentialScheme, "x-api-key", "", plaintext, "Authorization", "api-key")
			}
			if err == nil {
				adapter, err = anthropicprovider.New(anthropicprovider.Options{Endpoint: endpoint, Authorizer: authorizer, Client: client, Capabilities: capabilities, ProviderType: string(domain.ProviderBedrock), CredentialScheme: binding.CredentialScheme, MessagesPath: "anthropic/v1/messages"})
			}
		default:
			err = errors.New("Bedrock provider profile is not implemented")
		}
	default:
		err = errors.New("provider type is not implemented")
	}
	if err != nil {
		if authorizer != nil {
			authorizer.Close()
		}
		client.CloseIdleConnections()
		return nil, err
	}
	return adapter, nil
}

func matchingBindingID(instance domain.ProviderInstance, profileID domain.ProviderProfileID) string {
	matched := ""
	for _, binding := range instance.EffectiveProfileBindings() {
		if !binding.Enabled || binding.ProfileID != profileID {
			continue
		}
		if matched != "" {
			return ""
		}
		matched = binding.ID
	}
	return matched
}

func adapterForDeployment(registry *provider.Registry, instance domain.ProviderInstance, deployment domain.Deployment) (provider.Adapter, bool) {
	bindingID := deployment.BindingID
	if bindingID == "" {
		bindingID = matchingBindingID(instance, deployment.ProfileID)
	}
	if adapter, ok := registry.AdapterForBinding(instance.ID, bindingID); ok {
		return adapter, true
	}
	// Compatibility for extension registries that still expose exactly one
	// Provider-keyed adapter. AdapterForProvider fails closed when ambiguous.
	return registry.AdapterForProvider(instance.ID)
}

func providerCapabilities(capabilities domain.ProviderCapabilities) provider.Capabilities {
	return provider.Capabilities{
		Chat: capabilities.Chat, Streaming: capabilities.Streaming, Embeddings: capabilities.Embeddings,
		Moderations: capabilities.Moderations, Images: capabilities.Images, Transcriptions: capabilities.Transcriptions,
		Speech: capabilities.Speech, Files: capabilities.Files, Batches: capabilities.Batches,
		Rerank: capabilities.Rerank, AsyncGenerate: capabilities.AsyncGenerate, Tools: capabilities.Tools,
		Vision: capabilities.Vision, JSONMode: capabilities.JSONMode, DeveloperRole: capabilities.DeveloperRole,
		Reasoning: capabilities.Reasoning, StreamUsage: capabilities.StreamUsage,
		MaxContextTokens: capabilities.MaxContextTokens, MaxOutputTokens: capabilities.MaxOutputTokens,
	}
}

func deploymentCapabilities(deployment domain.Deployment, adapter provider.Adapter) provider.Capabilities {
	available := adapterCapabilitiesFor(adapter)
	declared := deployment.Capabilities
	if !declared.AnyOperation() {
		return available
	}
	return provider.Capabilities{
		Chat:        available.Chat && declared.Chat,
		Streaming:   available.Streaming && declared.Streaming,
		Embeddings:  available.Embeddings && declared.Embeddings,
		Moderations: available.Moderations && declared.Moderations, Images: available.Images && declared.Images, Transcriptions: available.Transcriptions && declared.Transcriptions, Speech: available.Speech && declared.Speech, Files: available.Files && declared.Files, Batches: available.Batches && declared.Batches, Rerank: available.Rerank && declared.Rerank, AsyncGenerate: available.AsyncGenerate && declared.AsyncGenerate,
		Tools:            available.Tools && declared.Tools,
		Vision:           available.Vision && declared.Vision,
		JSONMode:         available.JSONMode && declared.JSONMode,
		DeveloperRole:    available.DeveloperRole && declared.DeveloperRole,
		Reasoning:        available.Reasoning && declared.Reasoning,
		StreamUsage:      available.StreamUsage && declared.StreamUsage,
		MaxContextTokens: minimumCapabilityLimit(available.MaxContextTokens, declared.MaxContextTokens),
		MaxOutputTokens:  minimumCapabilityLimit(available.MaxOutputTokens, declared.MaxOutputTokens),
	}
}

func minimumCapabilityLimit(left, right int64) int64 {
	if left == 0 {
		return right
	}
	if right == 0 || left < right {
		return left
	}
	return right
}

func adapterCapabilitiesFor(adapter provider.Adapter) provider.Capabilities {
	if reporter, ok := adapter.(provider.CapabilityReporter); ok {
		return reporter.Capabilities()
	}
	// Custom adapters used by embedders and tests predate capability reporting.
	return provider.Capabilities{Chat: true, Streaming: true, Embeddings: true}
}
