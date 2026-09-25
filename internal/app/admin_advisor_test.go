package app

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/advisor"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/gateway"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/routegate"
)

func advisorRuntime(t *testing.T, gate *routegate.Gate, registry *provider.Registry, now time.Time) *Runtime {
	t.Helper()
	cfg := config.Default()
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	return &Runtime{
		config: cfg, routes: gate, providers: registry,
		now:    func() time.Time { return now },
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func decodeAdvisorFindings(t *testing.T, runtime *Runtime) []advisor.Finding {
	t.Helper()
	recorder := httptest.NewRecorder()
	runtime.listAdminAdvisorFindings(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Items []advisor.Finding `json:"items"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	return payload.Items
}

func advisorFinding(t *testing.T, findings []advisor.Finding, rule string) advisor.Finding {
	t.Helper()
	for _, finding := range findings {
		if finding.Rule == rule {
			return finding
		}
	}
	t.Fatalf("no finding for rule %q", rule)
	return advisor.Finding{}
}

// TestAdvisorUnclassifiedLabelMatchesTheCounter. internal/advisor deliberately
// keeps no dependency on the gateway, so it spells the unclassified label
// itself. That duplication is only safe while something asserts the two agree —
// otherwise a rename in the counter would silently turn the classification rule
// into one that counts nothing and always reports ok.
func TestAdvisorUnclassifiedLabelMatchesTheCounter(t *testing.T) {
	var found bool
	for _, reason := range gateway.KnownFailureReasons() {
		if reason == advisor.UnclassifiedReason {
			found = true
		}
	}
	if !found {
		t.Fatalf("%q is not among the reasons the counter publishes: %v",
			advisor.UnclassifiedReason, gateway.KnownFailureReasons())
	}
	if label := failureReasonLabel(""); label != advisor.UnclassifiedReason {
		t.Fatalf("an unclassified refusal is labelled %q, advisor expects %q", label, advisor.UnclassifiedReason)
	}
}

// TestLiveAdvisorNamesTheSuspendedScope is the whole reason the live view
// exists next to the offline one: the gate's state and the attempt arithmetic
// answer one question together, and reading them in two places is what made the
// original triage take three files.
func TestLiveAdvisorNamesTheSuspendedScope(t *testing.T) {
	gate := routegate.New(routegate.Config{})
	now := time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC)
	gate.Observe(provider.Target{
		ID: "route_1", DeploymentID: "dep_1", ProviderID: "provider_1",
		ProviderModel: "gpt-test", CredentialID: "cred_live", CredentialRevision: 7,
	}, routegate.Observation{
		Reason: provider.FailureReasonInvalidCredential, Status: 401, Code: "invalid_api_key",
	}, now)
	runtime := advisorRuntime(t, gate, provider.NewRegistry(), now)

	finding := advisorFinding(t, decodeAdvisorFindings(t, runtime), advisor.RuleSuspendedScopes)
	if finding.Status != advisor.StatusWarn {
		t.Fatalf("a held suspension reports %q", finding.Status)
	}
	var named bool
	for _, term := range finding.Evidence {
		if term.Name == "credential:cred_live" {
			named = true
		}
	}
	if !named {
		t.Fatalf("the finding does not name the credential: %+v", finding.Evidence)
	}
}

// TestLiveAdvisorReportsRefusalClassificationAsUnknownWithoutAService. The
// counter lives on the gateway service; a runtime assembled without one must
// say it did not look rather than report a clean classification.
func TestLiveAdvisorReportsRefusalClassificationAsUnknownWithoutAService(t *testing.T) {
	runtime := advisorRuntime(t, routegate.New(routegate.Config{}), provider.NewRegistry(), time.Now())
	finding := advisorFinding(t, decodeAdvisorFindings(t, runtime), advisor.RuleUnclassifiedRefusals)
	if finding.Status != advisor.StatusUnknown {
		t.Fatalf("with no gateway service the classification rule reports %q", finding.Status)
	}
}

// TestLiveAdvisorMeasuresTheRegistryFanOut. The registry is the authority on
// what a request walks — a route withheld from it is already unreachable — so
// the live view compares the attempt budget against registered targets rather
// than against the stored route table.
func TestLiveAdvisorMeasuresTheRegistryFanOut(t *testing.T) {
	registry := provider.NewRegistry()
	// Production wraps every adapter in the legacy bridge before registering
	// it, and the registry refuses anything without a profile contract, so the
	// fixture registers what a real route registers.
	manifest, ok := provider.BuiltinProfile(domain.ProfileOpenAIChatEmbeddings)
	if !ok {
		t.Fatal("the built-in OpenAI chat profile is missing")
	}
	adapter, err := provider.NewLegacyAdapterBridge(&advisorStubAdapter{}, manifest,
		domain.EvidenceForCapabilities(domain.ProviderCapabilities{Chat: true, Streaming: true}, domain.EvidenceDeclared))
	if err != nil {
		t.Fatal(err)
	}
	for index, id := range []string{"r1", "r2", "r3", "r4", "r5"} {
		if err := registry.Register(provider.Target{
			ID: id, DeploymentID: "dep_" + id, PublicModel: "busy",
			ProviderModel: "m", Priority: index, Adapter: adapter,
			Capabilities: provider.Capabilities{Chat: true, Streaming: true},
		}); err != nil {
			t.Fatal(err)
		}
	}
	runtime := advisorRuntime(t, routegate.New(routegate.Config{}), registry, time.Now())
	finding := advisorFinding(t, decodeAdvisorFindings(t, runtime), advisor.RuleAttemptBudgetReachesFanOut)
	// Five candidates behind the shipped budget of four.
	if finding.Status != advisor.StatusWarn {
		t.Fatalf("five candidates behind a budget of four reports %q: %s", finding.Status, finding.Comparison)
	}
}

// TestWidestFanOutSkipsUnreachableRoutes is the offline counterpart. A route
// whose deployment is switched off produces no target, so counting it would
// report an attempt-budget problem that disabling the deployment created.
func TestWidestFanOutSkipsUnreachableRoutes(t *testing.T) {
	deleted := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	providers := []domain.ProviderInstance{
		{ID: "p_on", Enabled: true},
		{ID: "p_off", Enabled: false},
	}
	deployments := []domain.Deployment{
		{ID: "d_on", ProviderID: "p_on", Enabled: true},
		{ID: "d_disabled", ProviderID: "p_on", Enabled: false},
		{ID: "d_behind_off_provider", ProviderID: "p_off", Enabled: true},
	}
	routes := []domain.Route{
		{ID: "r1", PublicModel: "busy", DeploymentID: "d_on", Enabled: true},
		{ID: "r2", PublicModel: "busy", DeploymentID: "d_disabled", Enabled: true},
		{ID: "r3", PublicModel: "busy", DeploymentID: "d_behind_off_provider", Enabled: true},
		{ID: "r4", PublicModel: "busy", DeploymentID: "d_on", Enabled: false},
		{ID: "r5", PublicModel: "busy", DeploymentID: "d_on", Enabled: true, DeletedAt: &deleted},
		{ID: "r6", PublicModel: "quiet", DeploymentID: "d_on", Enabled: true},
	}
	widest := widestFanOut(routes, deployments, providers)
	if widest.PublicModel != "busy" || widest.Candidates != 1 {
		t.Fatalf("widest fan-out is %q with %d, want busy with 1", widest.PublicModel, widest.Candidates)
	}
}

// advisorStubAdapter is a route's upstream reduced to the one thing the
// registry checks when a target is registered: which provider type it is. It is
// never called — the fan-out rule counts registered targets and dispatches
// nothing.
type advisorStubAdapter struct{ provider.Adapter }

func (*advisorStubAdapter) Type() string { return "openai" }
