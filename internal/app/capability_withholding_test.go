package app

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/modelcatalog"
	"github.com/akz142857/Halro/internal/provider"
)

func bootstrapForCapabilityTest(t *testing.T) (*Runtime, BootstrapResult) {
	t.Helper()
	runtime, bootstrap, _ := openBootstrappedRuntime(t)
	return runtime, bootstrap
}

func openBootstrappedRuntime(t *testing.T) (*Runtime, BootstrapResult, func() *Runtime) {
	return openBootstrappedRuntimeForModel(t, "gpt-test")
}

func openBootstrappedRuntimeForModel(t *testing.T, providerModel string) (*Runtime, BootstrapResult, func() *Runtime) {
	t.Helper()
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapAdmin(context.Background(), cfg, "admin", []byte("correct horse battery staple")); err != nil {
		t.Fatal(err)
	}
	bootstrap, err := Bootstrap(context.Background(), cfg, BootstrapOptions{
		ProviderName: "OpenAI", ProviderType: domain.ProviderOpenAI,
		ProviderBaseURL: "https://api.openai.com", ProviderModel: providerModel, PublicModel: "chat",
		ProjectName: "Capabilities", BillingMode: domain.BillingModeFree,
	}, []byte("provider-secret"))
	if err != nil {
		t.Fatal(err)
	}
	open := func() *Runtime {
		runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatalf("open runtime: %v", err)
		}
		return runtime
	}
	runtime := open()
	t.Cleanup(func() { runtime.Close() })
	return runtime, bootstrap, open
}

type catalogRoundTripFunc func(*http.Request) (*http.Response, error)

func (f catalogRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func signedCatalogPayload(t *testing.T, now time.Time, entries ...modelcatalog.SnapshotEntry) ([]byte, modelcatalog.TrustRoot) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	root := modelcatalog.TrustRoot{KeyID: "test-root", PublicKey: publicKey, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(4 * time.Hour)}
	snapshot, canonical, err := modelcatalog.PrepareSnapshot(modelcatalog.Snapshot{
		SchemaVersion: modelcatalog.MinReadableSchema, Sequence: 1,
		GeneratedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour),
		CapabilityDictionaryVersion: modelcatalog.CapabilityDictionaryVersion, Entries: entries,
	})
	if err != nil {
		t.Fatal(err)
	}
	signature := base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, canonical))
	envelope, err := modelcatalog.AssembleSignedSnapshot(canonical, root.KeyID, signature, []modelcatalog.TrustRoot{root})
	if err != nil {
		t.Fatal(err)
	}
	if envelope.Payload.CatalogRevision != snapshot.CatalogRevision {
		t.Fatal("assembled catalog revision changed")
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return payload, root
}

func waitForCapabilityCandidates(t *testing.T, runtime *Runtime, publicModel string, want int) []provider.Target {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		targets := runtime.providers.ResolveCandidatesForEvidence(publicModel, provider.OperationChat, domain.EvidenceDeclared)
		if len(targets) == want {
			return targets
		}
		if time.Now().After(deadline) {
			t.Fatalf("candidates for %q did not reach %d: %+v", publicModel, want, targets)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// driftDeployment makes one deployment's snapshot claim a capability the
// running profile does not have. That is the shape of a binary upgrade
// narrowing a profile: nothing about the deployment record changes, but what
// supports it does — and only this deployment is affected.
func driftDeployment(t *testing.T, runtime *Runtime, deploymentID string) {
	t.Helper()
	deployment, err := runtime.store.GetDeployment(context.Background(), deploymentID)
	if err != nil {
		t.Fatal(err)
	}
	deployment.ModelCapabilitySnapshot.Capabilities.Rerank = true
	if _, err := runtime.store.PutDeployment(context.Background(), deployment, deployment.Revision, nil); err != nil {
		t.Fatal(err)
	}
	instance, err := runtime.store.GetProvider(context.Background(), deployment.ProviderID)
	if err != nil {
		t.Fatal(err)
	}
	if deploymentBinding(instance, deployment).Capabilities.Rerank {
		t.Fatal("the profile already allows rerank, so this does not model a narrowing")
	}
}

// addSecondRoute gives the topology a route that nothing drifts, so a test can
// tell "the drifted one was withheld" apart from "the load gave up".
func addSecondRoute(t *testing.T, runtime *Runtime, bootstrap BootstrapResult, publicModel string) string {
	t.Helper()
	source, err := runtime.store.GetDeployment(context.Background(), bootstrap.DeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	second := source
	second.ID = "dpl-second"
	second.Name = "Second"
	second.Revision = 0
	if _, err := runtime.store.PutDeployment(context.Background(), second, 0, nil); err != nil {
		t.Fatal(err)
	}
	route := domain.Route{
		ID: "rte-second", PublicModel: publicModel, DeploymentID: second.ID,
		Enabled: true, CreatedAt: source.CreatedAt, UpdatedAt: source.UpdatedAt,
	}
	if _, err := runtime.store.PutRoute(context.Background(), route, 0, nil); err != nil {
		t.Fatal(err)
	}
	return second.ID
}

// §4.4: "核对失败本身不得阻止进程启动，但受影响 Deployment 一律 fail-closed."
//
// Refusing to build the registry would turn one stale snapshot into a process
// that cannot start, taking every unrelated route down with it. The drifted
// deployment is withheld instead.
func TestDriftWithholdsTheRouteAndStillLoadsTheRegistry(t *testing.T) {
	runtime, bootstrap := bootstrapForCapabilityTest(t)
	driftDeployment(t, runtime, bootstrap.DeploymentID)

	registry, report, err := loadProviderRegistry(context.Background(), runtime.config, runtime.store, runtime.vault)
	if err != nil {
		t.Fatalf("a drifted deployment stopped the registry from loading: %v", err)
	}
	defer registry.Close()

	if len(report.Drifted) != 1 || len(report.Dangling) != 0 {
		t.Fatalf("report=%+v, expected exactly the drifted route", report)
	}
	if report.Drifted[0].RouteID != bootstrap.RouteID || report.Drifted[0].DeploymentID != bootstrap.DeploymentID {
		t.Fatalf("withheld the wrong route: %+v", report.Drifted[0])
	}
	if report.Drifted[0].State != domain.CapabilityReviewDrifted {
		t.Fatalf("state=%q", report.Drifted[0].State)
	}

	// Withheld means withheld: the public model must have no candidate, not a
	// candidate that fails later on the request path.
	targets := registry.ResolveCandidatesForEvidence("chat", provider.OperationChat, domain.EvidenceDeclared)
	if len(targets) != 0 {
		t.Fatalf("a drifted deployment was still a routing candidate: %+v", targets)
	}
}

// The whole point of withholding rather than failing: everything else keeps
// working.
func TestDriftDoesNotWithholdUnrelatedRoutes(t *testing.T) {
	runtime, bootstrap := bootstrapForCapabilityTest(t)
	addSecondRoute(t, runtime, bootstrap, "chat-second")
	driftDeployment(t, runtime, bootstrap.DeploymentID)

	registry, report, err := loadProviderRegistry(context.Background(), runtime.config, runtime.store, runtime.vault)
	if err != nil {
		t.Fatalf("registry load failed: %v", err)
	}
	defer registry.Close()

	if len(report.Drifted) != 1 || report.Drifted[0].RouteID != bootstrap.RouteID {
		t.Fatalf("report=%+v", report)
	}
	if targets := registry.ResolveCandidatesForEvidence("chat-second", provider.OperationChat, domain.EvidenceDeclared); len(targets) != 1 {
		t.Fatalf("the unrelated route did not survive the drifted one: %+v", targets)
	}
}

// A hot reload after an admin mutation must not fail because some other
// deployment drifted; that would make an unrelated edit impossible to save.
func TestHotReloadSucceedsWithADriftedDeployment(t *testing.T) {
	runtime, bootstrap := bootstrapForCapabilityTest(t)
	driftDeployment(t, runtime, bootstrap.DeploymentID)

	if err := runtime.reloadProviderRegistry(context.Background()); err != nil {
		t.Fatalf("hot reload refused to complete because a deployment drifted: %v", err)
	}
	if targets := runtime.providers.ResolveCandidatesForEvidence("chat", provider.OperationChat, domain.EvidenceDeclared); len(targets) != 0 {
		t.Fatalf("a drifted deployment stayed routable after reload: %+v", targets)
	}
}

// Opening a runtime is the case an operator actually hits: the process must
// come up rather than refusing to start.
func TestRuntimeOpensWithADriftedDeployment(t *testing.T) {
	runtime, bootstrap, open := openBootstrappedRuntime(t)
	driftDeployment(t, runtime, bootstrap.DeploymentID)
	runtime.Close()

	second := open()
	defer second.Close()

	if targets := second.providers.ResolveCandidatesForEvidence("chat", provider.OperationChat, domain.EvidenceDeclared); len(targets) != 0 {
		t.Fatalf("a drifted deployment was routable after restart: %+v", targets)
	}
}

// switchOffDeploymentBinding disables the Profile Binding the seeded deployment
// runs on, leaving a second binding enabled so the Provider record stays valid.
// It writes straight to the store: the Admin guard against exactly this edit is
// tested separately, and what matters here is what the loader does when the
// state exists anyway.
func switchOffDeploymentBinding(t *testing.T, runtime *Runtime, providerID, deploymentID string) {
	t.Helper()
	instance, err := runtime.store.GetProvider(context.Background(), providerID)
	if err != nil {
		t.Fatal(err)
	}
	deployment, err := runtime.store.GetDeployment(context.Background(), deploymentID)
	if err != nil {
		t.Fatal(err)
	}
	chat := instance.EffectiveProfileBindings()[0]
	if chat.ID != deployment.BindingID {
		t.Fatalf("fixture assumes the deployment runs on the primary binding: %q vs %q", deployment.BindingID, chat.ID)
	}
	chat.Enabled = false
	media := chat
	media.ProfileID = domain.ProfileOpenAIMediaResources
	media.ID = domain.DefaultProviderProfileBindingID(instance.ID, media.ProfileID)
	media.Capabilities = domain.DefaultProviderCapabilitiesForProfile(domain.ProviderOpenAI, media.ProfileID)
	media.CapabilityEvidence = domain.EvidenceForCapabilities(media.Capabilities, domain.EvidenceDeclared)
	media.Enabled = true
	// The disabled binding stays at index 0 so the provider's legacy profile
	// projection still matches its primary binding.
	instance.Bindings = []domain.ProviderProfileBinding{chat, media}
	instance.Capabilities, instance.CapabilityEvidence = domain.BindingsCapabilitiesSummary(instance.Bindings)
	if _, err := runtime.store.PutProvider(context.Background(), instance, instance.Revision, nil); err != nil {
		t.Fatal(err)
	}
}

// A route left pointing at a switched-off Profile Binding used to fail the whole
// registry load. That made one dangling reference worse than switching off an
// entire provider: the durable edit landed, activation failed, every later
// topology edit failed the same way, and the process could not start again.
//
// It is withheld now, for the reason drift already is: one broken record must
// cost its own route and nothing else.
func TestDanglingBindingWithholdsTheRouteAndStillLoadsTheRegistry(t *testing.T) {
	runtime, bootstrap := bootstrapForCapabilityTest(t)
	switchOffDeploymentBinding(t, runtime, bootstrap.ProviderID, bootstrap.DeploymentID)

	registry, report, err := loadProviderRegistry(context.Background(), runtime.config, runtime.store, runtime.vault)
	if err != nil {
		t.Fatalf("a dangling binding stopped the registry from loading: %v", err)
	}
	defer registry.Close()

	if len(report.Dangling) != 1 || len(report.Drifted) != 0 {
		t.Fatalf("report=%+v, expected exactly the dangling route", report)
	}
	item := report.Dangling[0]
	if item.RouteID != bootstrap.RouteID || item.Reason != withheldBindingUnavailable {
		t.Fatalf("withheld=%+v", item)
	}
	// Not counted as drift: the deployment's claim is fine, the topology is not.
	if targets := registry.ResolveCandidatesForEvidence("chat", provider.OperationChat, domain.EvidenceDeclared); len(targets) != 0 {
		t.Fatalf("a route with no adapter was still a candidate: %+v", targets)
	}
}

// The part that made this a P0 rather than a routing bug: the binary has to be
// able to open a data directory it already wrote.
func TestRuntimeOpensWithADanglingBinding(t *testing.T) {
	runtime, bootstrap, open := openBootstrappedRuntime(t)
	switchOffDeploymentBinding(t, runtime, bootstrap.ProviderID, bootstrap.DeploymentID)
	runtime.Close()

	second := open()
	defer second.Close()

	if targets := second.providers.ResolveCandidatesForEvidence("chat", provider.OperationChat, domain.EvidenceDeclared); len(targets) != 0 {
		t.Fatalf("a dangling binding was routable after restart: %+v", targets)
	}
}

// A hot reload must survive it too, or the operator cannot edit their way out:
// every subsequent topology mutation reloads the registry and would fail on a
// state some earlier edit left behind.
func TestHotReloadSucceedsWithADanglingBinding(t *testing.T) {
	runtime, bootstrap := bootstrapForCapabilityTest(t)
	switchOffDeploymentBinding(t, runtime, bootstrap.ProviderID, bootstrap.DeploymentID)

	if err := runtime.reloadProviderRegistry(context.Background()); err != nil {
		t.Fatalf("hot reload refused to complete because a binding was switched off: %v", err)
	}
	if targets := runtime.providers.ResolveCandidatesForEvidence("chat", provider.OperationChat, domain.EvidenceDeclared); len(targets) != 0 {
		t.Fatalf("route stayed routable after reload: %+v", targets)
	}
}

func TestSignedCatalogWithholdingRefreshReturnsAndFallbackRestoresRoute(t *testing.T) {
	runtime, bootstrap, _ := openBootstrappedRuntimeForModel(t, "gpt-4o")
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	current := now
	deployment, err := runtime.store.GetDeployment(context.Background(), bootstrap.DeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	builtinEntry, found := modelcatalog.Builtin().Lookup(modelcatalog.Key{
		ProviderType: domain.ProviderOpenAI, Profile: domain.ProfileOpenAIChatEmbeddings,
		TargetKind: domain.TargetModelID, Model: "gpt-4o",
	})
	if !found {
		t.Fatal("gpt-4o missing from builtin catalog")
	}
	deployment.Capabilities = builtinEntry.Capabilities
	deployment.CapabilityEvidence = builtinEntry.Evidence()
	deployment.ModelCapabilitySnapshot = domain.ModelCapabilitySnapshot{
		ProviderModel: "gpt-4o", ModelRevision: builtinEntry.Revision(),
		CatalogRevision: modelcatalog.Builtin().Revision(), Source: string(modelcatalog.SourceBuiltin),
		Status: string(modelcatalog.StatusKnown), CapturedAt: now, Capabilities: builtinEntry.Capabilities,
	}
	deployment.ModelCapabilitySnapshot.Evidence = domain.SnapshotEvidence(deployment.ModelCapabilitySnapshot)
	if _, err := runtime.store.PutDeployment(context.Background(), deployment, deployment.Revision, nil); err != nil {
		t.Fatal(err)
	}
	payload, root := signedCatalogPayload(t, now, modelcatalog.SnapshotEntry{
		ProviderType: domain.ProviderOpenAI, ProfileID: domain.ProfileOpenAIChatEmbeddings,
		TargetKind: domain.TargetModelID, Model: "gpt-4o", Revoked: true,
	})
	failDownload := false
	manager, err := modelcatalog.NewManager(modelcatalog.ManagerOptions{
		Enabled: true, RefreshInterval: time.Hour, DataDir: t.TempDir(), TrustRoots: []modelcatalog.TrustRoot{root},
		MaxDownloadBytes: 64 << 10, MaxDecodedBytes: 128 << 10, MaxCompressionRatio: 20, MaxEntries: 100,
		Now: func() time.Time { return current }, Endpoint: "https://catalog.test/v1.json",
		HTTPClient: &http.Client{Transport: catalogRoundTripFunc(func(*http.Request) (*http.Response, error) {
			if failDownload {
				return nil, errors.New("offline")
			}
			return &http.Response{
				StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(payload)),
				Header: make(http.Header), ContentLength: int64(len(payload)),
			}, nil
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.SetActivationPreparer(runtime.prepareModelCatalogActivation)
	manager.SetObserver(runtime.observeModelCatalogRefresh)
	runtime.capabilityResolution.catalog = manager

	refreshDone := make(chan error, 1)
	go func() { refreshDone <- manager.Refresh(context.Background()) }()
	select {
	case err := <-refreshDone:
		if err != nil {
			t.Fatalf("signed revoke refresh failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("signed revoke refresh deadlocked while auditing withheld route")
	}
	if targets := runtime.providers.ResolveCandidatesForEvidence("chat", provider.OperationChat, domain.EvidenceDeclared); len(targets) != 0 {
		t.Fatalf("revoked model remained routable: %+v", targets)
	}

	failDownload = true
	if err := manager.Refresh(context.Background()); err == nil {
		t.Fatal("offline refresh unexpectedly succeeded")
	}
	if targets := waitForCapabilityCandidates(t, runtime, "chat", 1); targets[0].DeploymentID != bootstrap.DeploymentID {
		t.Fatalf("catalog failure did not restore immutable deployment snapshot: %+v", targets)
	}

	failDownload = false
	if err := manager.Refresh(context.Background()); err != nil {
		t.Fatalf("reapply signed revoke: %v", err)
	}
	if targets := runtime.providers.ResolveCandidatesForEvidence("chat", provider.OperationChat, domain.EvidenceDeclared); len(targets) != 0 {
		t.Fatalf("reapplied revoke remained routable: %+v", targets)
	}
	current = now.Add(2 * time.Hour)
	_ = manager.Current()
	if targets := waitForCapabilityCandidates(t, runtime, "chat", 1); targets[0].DeploymentID != bootstrap.DeploymentID {
		t.Fatalf("catalog expiry did not restore immutable deployment snapshot: %+v", targets)
	}
}

func TestModelCatalogActivationSerializesTopologyMutation(t *testing.T) {
	runtime, bootstrap := bootstrapForCapabilityTest(t)
	finalize, err := runtime.prepareModelCatalogActivation(modelcatalog.Builtin())
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		runtime.adminTopologyMu.Lock()
		defer runtime.adminTopologyMu.Unlock()
		route, err := runtime.store.GetRoute(context.Background(), bootstrap.RouteID)
		if err != nil {
			done <- err
			return
		}
		route.Enabled = false
		if _, err := runtime.store.PutRoute(context.Background(), route, route.Revision, nil); err != nil {
			done <- err
			return
		}
		done <- runtime.reloadProviderRegistry(context.Background())
	}()
	<-started
	select {
	case err := <-done:
		finalize(false)
		t.Fatalf("topology mutation crossed a prepared catalog activation: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	finalize(true)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("topology mutation did not resume after catalog commit")
	}
	if targets := runtime.providers.ResolveCandidatesForEvidence("chat", provider.OperationChat, domain.EvidenceDeclared); len(targets) != 0 {
		t.Fatalf("catalog commit resurrected disabled route: %+v", targets)
	}
}
