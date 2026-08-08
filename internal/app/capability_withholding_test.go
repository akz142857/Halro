package app

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/provider"
)

func bootstrapForCapabilityTest(t *testing.T) (*Runtime, BootstrapResult) {
	t.Helper()
	runtime, bootstrap, _ := openBootstrappedRuntime(t)
	return runtime, bootstrap
}

func openBootstrappedRuntime(t *testing.T) (*Runtime, BootstrapResult, func() *Runtime) {
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
		ProviderBaseURL: "https://api.openai.com", ProviderModel: "gpt-test", PublicModel: "chat",
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
	if _, err := runtime.store.PutDeployment(context.Background(), deployment, deployment.Revision); err != nil {
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
	if _, err := runtime.store.PutDeployment(context.Background(), second, 0); err != nil {
		t.Fatal(err)
	}
	route := domain.Route{
		ID: "rte-second", PublicModel: publicModel, DeploymentID: second.ID,
		Enabled: true, CreatedAt: source.CreatedAt, UpdatedAt: source.UpdatedAt,
	}
	if _, err := runtime.store.PutRoute(context.Background(), route, 0); err != nil {
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

	registry, withheld, err := loadProviderRegistry(context.Background(), runtime.config, runtime.store, runtime.vault)
	if err != nil {
		t.Fatalf("a drifted deployment stopped the registry from loading: %v", err)
	}
	defer registry.Close()

	if len(withheld) != 1 {
		t.Fatalf("withheld=%+v, expected exactly the drifted route", withheld)
	}
	if withheld[0].RouteID != bootstrap.RouteID || withheld[0].DeploymentID != bootstrap.DeploymentID {
		t.Fatalf("withheld the wrong route: %+v", withheld[0])
	}
	if withheld[0].State != domain.CapabilityReviewDrifted {
		t.Fatalf("state=%q", withheld[0].State)
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

	registry, withheld, err := loadProviderRegistry(context.Background(), runtime.config, runtime.store, runtime.vault)
	if err != nil {
		t.Fatalf("registry load failed: %v", err)
	}
	defer registry.Close()

	if len(withheld) != 1 || withheld[0].RouteID != bootstrap.RouteID {
		t.Fatalf("withheld=%+v", withheld)
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
