package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/semantic"
)

func TestUnknownModelCarriesEveryCandidateInterfaceInsteadOfRefusing(t *testing.T) {
	runtime, bootstrap := bootstrapForCapabilityTest(t)
	instance, err := runtime.store.GetProvider(context.Background(), bootstrap.ProviderID)
	if err != nil {
		t.Fatal(err)
	}
	bindings := instance.EffectiveProfileBindings()
	chat := bindings[0]
	chat.ID = "b-chat"
	media := chat
	media.ID = "b-media"
	media.ProfileID = domain.ProfileOpenAIMediaResources
	instance.Bindings = []domain.ProviderProfileBinding{chat, media}

	original := runtime.providers
	registry := provider.NewRegistry()
	chatDetector, mediaDetector := &fixedCapabilityDetector{}, &fixedCapabilityDetector{}
	if err := registry.RegisterBindingAdapter(instance.ID, chat.ID, chatDetector); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterBindingAdapter(instance.ID, media.ID, mediaDetector); err != nil {
		t.Fatal(err)
	}
	runtime.providers = registry
	original.Close()

	// An unlisted model must not be turned into a question the operator cannot
	// answer. Both interfaces stay candidates and identification decides.
	input := modelCapabilityDetectionInput{ProviderModel: "unlisted-image-model", RiskTier: "safe_automatic"}
	catalogCandidates, probeCandidates, err := runtime.capabilityDetectionCandidates(instance, input, "")
	if err != nil || len(catalogCandidates) != 0 || len(probeCandidates) != 2 {
		t.Fatalf("catalog=%d probe=%d err=%v", len(catalogCandidates), len(probeCandidates), err)
	}
	if err := capabilityCandidateError(catalogCandidates, probeCandidates); err != nil {
		t.Fatalf("several probeable interfaces were refused instead of identified: %v", err)
	}

	// An explicit interface still narrows the set to exactly that one, which is
	// how the operator resolves a detection that found several answering.
	input.BindingID = media.ID
	_, probeCandidates, err = runtime.capabilityDetectionCandidates(instance, input, "")
	if err != nil || len(probeCandidates) != 1 || probeCandidates[0].binding.ID != media.ID {
		t.Fatalf("explicit interface was not honored: candidates=%d err=%v", len(probeCandidates), err)
	}
}

// A model that answers on exactly one interface must be bound to it without the
// operator being asked, which is the whole point of pressing "detect".
func TestIdentificationResolvesTheOnlyInterfaceThatAnswers(t *testing.T) {
	runtime, instance, chat, media := twoInterfaceProviderForTest(t)
	chatDetector := &scriptedCapabilityDetector{supported: map[string]bool{}}
	mediaDetector := &scriptedCapabilityDetector{supported: map[string]bool{"moderations": true}}
	registerBindingDetectors(t, runtime, instance, map[string]provider.Adapter{chat.ID: chatDetector, media.ID: mediaDetector})

	completed := runDetectionForTest(t, runtime, instance, "unlisted-moderation-model")
	if completed.Status != domain.DetectionCompleted {
		t.Fatalf("status=%s candidates=%#v", completed.Status, completed.Candidates)
	}
	if completed.BindingID != media.ID {
		t.Fatalf("identification resolved %q, want %q", completed.BindingID, media.ID)
	}
	if !completed.Recommended.Moderations || completed.Recommended.Chat {
		t.Fatalf("recommended=%#v", completed.Recommended)
	}
	// The losing interface costs only its roots — both of them, because failing
	// chat does not rule out embeddings — and stops there rather than walking
	// its dependents. The winner's own identification probe is carried into the
	// results rather than paid for twice, so it adds nothing beyond its root.
	if chatDetector.calls.Load() != 2 || mediaDetector.calls.Load() != 1 {
		t.Fatalf("chat calls=%d media calls=%d", chatDetector.calls.Load(), mediaDetector.calls.Load())
	}
	if completed.ProviderCalls != 3 || len(completed.Calls) != 3 {
		t.Fatalf("provider calls=%d records=%d", completed.ProviderCalls, len(completed.Calls))
	}
	for _, call := range completed.Calls {
		if call.BindingID == "" {
			t.Fatalf("a probe was recorded without the interface it was spent on: %#v", call)
		}
	}
}

// When several interfaces answer, the deployment still runs on one, so the
// choice returns to the operator — but carrying what each one answered.
func TestIdentificationLeavesAGenuineChoiceToTheOperatorWithEvidence(t *testing.T) {
	runtime, instance, chat, media := twoInterfaceProviderForTest(t)
	chatDetector := &scriptedCapabilityDetector{supported: map[string]bool{"chat": true}}
	mediaDetector := &scriptedCapabilityDetector{supported: map[string]bool{"moderations": true}}
	registerBindingDetectors(t, runtime, instance, map[string]provider.Adapter{chat.ID: chatDetector, media.ID: mediaDetector})

	completed := runDetectionForTest(t, runtime, instance, "unlisted-dual-model")
	if completed.Status != domain.DetectionAmbiguous {
		t.Fatalf("status=%s", completed.Status)
	}
	if completed.BindingID != "" {
		t.Fatalf("an ambiguous detection resolved a binding anyway: %q", completed.BindingID)
	}
	answered := map[string]string{}
	for _, candidate := range completed.Candidates {
		if candidate.Answered {
			answered[candidate.BindingID] = candidate.Capability
		}
	}
	if len(answered) != 2 || answered[chat.ID] != "chat" || answered[media.ID] != "moderations" {
		t.Fatalf("the operator was left without evidence: %#v", completed.Candidates)
	}
}

// A model whose real work has no probe — image generation, transcription —
// gets asked only questions it cannot answer. The record has to say what each
// interface could have established at all, or "failed" reads as a fault in the
// model rather than as a question detection was never able to put.
func TestFailedIdentificationReportsWhatEachInterfaceCouldVerify(t *testing.T) {
	runtime, instance, chat, media := twoInterfaceProviderForTest(t)
	// Nothing is supported anywhere, and the chat interface rejects the
	// credential on its FIRST root probe — a fact the operator can act on, and
	// one a rule that simply kept the last outcome would bury behind the "could
	// not tell" that follows it.
	chatDetector := &scriptedCapabilityDetector{supported: map[string]bool{}, unauthorized: map[string]bool{"chat": true}}
	mediaDetector := &scriptedCapabilityDetector{supported: map[string]bool{}}
	registerBindingDetectors(t, runtime, instance, map[string]provider.Adapter{chat.ID: chatDetector, media.ID: mediaDetector})

	completed := runDetectionForTest(t, runtime, instance, "gpt-image-unlisted")
	if completed.Status != domain.DetectionFailed {
		t.Fatalf("status=%s", completed.Status)
	}
	byBinding := map[string]domain.DetectionBindingCandidate{}
	for _, candidate := range completed.Candidates {
		byBinding[candidate.BindingID] = candidate
	}
	if got := byBinding[media.ID].Verifiable; !slices.Equal(got, []string{"moderations"}) {
		t.Fatalf("media interface reports verifiable=%v, want only moderations", got)
	}
	if got := byBinding[chat.ID].Verifiable; !slices.Contains(got, "chat") || !slices.Contains(got, "embeddings") {
		t.Fatalf("chat interface reports verifiable=%v", got)
	}
	// The credential rejection survives, rather than the last probe attempted.
	if candidate := byBinding[chat.ID]; candidate.Status != domain.ProbeUnauthorized || candidate.Capability != "chat" {
		t.Fatalf("chat candidate kept %q/%s instead of the credential rejection", candidate.Capability, candidate.Status)
	}
}

// Nothing answering is a failure, not a silent pick of the first interface.
func TestIdentificationFailsWhenNoInterfaceAnswers(t *testing.T) {
	runtime, instance, chat, media := twoInterfaceProviderForTest(t)
	chatDetector := &scriptedCapabilityDetector{supported: map[string]bool{}}
	mediaDetector := &scriptedCapabilityDetector{supported: map[string]bool{}}
	registerBindingDetectors(t, runtime, instance, map[string]provider.Adapter{chat.ID: chatDetector, media.ID: mediaDetector})

	completed := runDetectionForTest(t, runtime, instance, "model-that-does-not-exist")
	if completed.Status != domain.DetectionFailed || completed.BindingID != "" {
		t.Fatalf("status=%s binding=%q", completed.Status, completed.BindingID)
	}
}

func TestDetectionStopsAtItsBillableCallCeiling(t *testing.T) {
	runtime, instance, chat, media := twoInterfaceProviderForTest(t)
	chatDetector := &scriptedCapabilityDetector{supported: map[string]bool{}}
	mediaDetector := &scriptedCapabilityDetector{supported: map[string]bool{"moderations": true}}
	registerBindingDetectors(t, runtime, instance, map[string]provider.Adapter{chat.ID: chatDetector, media.ID: mediaDetector})
	runtime.config.Admin.ModelCapabilityDetection.MaxProviderCalls = 1

	completed := runDetectionForTest(t, runtime, instance, "call-ceiling-model")
	if completed.ProviderCalls != 1 || len(completed.Calls) != 1 {
		t.Fatalf("provider calls=%d records=%d, want the configured ceiling of one", completed.ProviderCalls, len(completed.Calls))
	}
	if chatDetector.calls.Load()+mediaDetector.calls.Load() != 1 {
		t.Fatalf("detector calls crossed ceiling: chat=%d media=%d", chatDetector.calls.Load(), mediaDetector.calls.Load())
	}
}

// The other half of the ceiling, and the one nothing reached. With a single
// candidate, identification spends nothing and breaks out early, so the test
// above never executes the probe loop's own budget check — deleting that check
// left it green. Here the whole budget is consumed by the first probe, and what
// is asserted is the branch's actual product: the remaining capabilities come
// back not_probed rather than unsupported. Reporting "unsupported" for a probe
// nobody could afford to run would be a claim nothing checked, and this ceiling
// is the only upper bound on what detection can spend.
func TestProbesBeyondTheCallCeilingAreLeftUnprobedRatherThanUnsupported(t *testing.T) {
	runtime, bootstrap := bootstrapForCapabilityTest(t)
	instance, err := runtime.store.GetProvider(context.Background(), bootstrap.ProviderID)
	if err != nil {
		t.Fatal(err)
	}
	bindings := instance.EffectiveProfileBindings()
	if len(bindings) != 1 {
		t.Fatalf("this test needs a single candidate so identification spends nothing; bindings=%d", len(bindings))
	}
	detector := &scriptedCapabilityDetector{supported: map[string]bool{"chat": true, "embeddings": true, "streaming": true}}
	registerBindingDetectors(t, runtime, instance, map[string]provider.Adapter{bindings[0].ID: detector})
	runtime.config.Admin.ModelCapabilityDetection.MaxProviderCalls = 1

	completed := runDetectionForTest(t, runtime, instance, "probe-ceiling-model")

	if completed.ProviderCalls != 1 || detector.calls.Load() != 1 {
		t.Fatalf("ceiling of one was crossed: accounted=%d adapter=%d", completed.ProviderCalls, detector.calls.Load())
	}
	notProbed := 0
	for capability, result := range completed.Results {
		switch result.Status {
		case domain.ProbeNotProbed:
			notProbed++
		case domain.ProbeUnsupported:
			t.Fatalf("%q was reported unsupported although the budget stopped before it ran", capability)
		}
	}
	if notProbed == 0 {
		t.Fatalf("no capability was left unprobed, so the probe-loop ceiling never ran: %#v", completed.Results)
	}
}

// twoInterfaceProviderForTest persists a provider carrying both OpenAI
// interfaces, which is the shape that used to force the operator to choose.
func twoInterfaceProviderForTest(t *testing.T) (*Runtime, domain.ProviderInstance, domain.ProviderProfileBinding, domain.ProviderProfileBinding) {
	t.Helper()
	runtime, bootstrap := bootstrapForCapabilityTest(t)
	instance, err := runtime.store.GetProvider(context.Background(), bootstrap.ProviderID)
	if err != nil {
		t.Fatal(err)
	}
	// The chat binding keeps its bootstrapped ID: the seeded deployment is
	// bound to it, and renaming it would orphan that deployment rather than
	// test anything about identification.
	chat := instance.EffectiveProfileBindings()[0]
	chat.ProfileID = domain.ProfileOpenAIChatEmbeddings
	chat.Capabilities = domain.DefaultProviderCapabilitiesForProfile(domain.ProviderOpenAI, domain.ProfileOpenAIChatEmbeddings)
	chat.CapabilityEvidence = domain.EvidenceForCapabilities(chat.Capabilities, domain.EvidenceDeclared)
	media := chat
	media.ID, media.ProfileID = "b-media", domain.ProfileOpenAIMediaResources
	media.Capabilities = domain.DefaultProviderCapabilitiesForProfile(domain.ProviderOpenAI, domain.ProfileOpenAIMediaResources)
	media.CapabilityEvidence = domain.EvidenceForCapabilities(media.Capabilities, domain.EvidenceDeclared)
	instance.Bindings = []domain.ProviderProfileBinding{chat, media}
	instance.Capabilities, instance.CapabilityEvidence = domain.BindingsCapabilitiesSummary(instance.Bindings)
	stored, err := runtime.store.PutProvider(context.Background(), instance, instance.Revision, nil)
	if err != nil {
		t.Fatal(err)
	}
	return runtime, stored, chat, media
}

func registerBindingDetectors(t *testing.T, runtime *Runtime, instance domain.ProviderInstance, adapters map[string]provider.Adapter) {
	t.Helper()
	original := runtime.providers
	registry := provider.NewRegistry()
	for bindingID, adapter := range adapters {
		if err := registry.RegisterBindingAdapter(instance.ID, bindingID, adapter); err != nil {
			t.Fatal(err)
		}
	}
	runtime.providers = registry
	original.Close()
}

func runDetectionForTest(t *testing.T, runtime *Runtime, instance domain.ProviderInstance, model string) domain.ModelCapabilityDetection {
	t.Helper()
	session := loginTestAdmin(t, runtime, "admin", "correct horse battery staple")
	request := adminMutationRequest(t, http.MethodPost, "/admin/api/v1/providers/"+instance.ID+"/model-capability-detections", session, map[string]any{
		"provider_model": model, "target_kind": "model_id", "risk_tier": "safe_automatic",
		// Detection spends the Provider credential, so it carries step-up.
		"current_password": "correct horse battery staple",
	})
	request.Header.Set("Idempotency-Key", "identify-"+model)
	response := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var created domain.ModelCapabilityDetection
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		current, err := runtime.store.GetModelCapabilityDetection(context.Background(), created.ID)
		if err == nil && current.Status.Terminal() {
			return current
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("capability detection did not finish")
	return domain.ModelCapabilityDetection{}
}

// scriptedCapabilityDetector answers a fixed set of capabilities and derives its
// plan from the profile under test, so a probe against the wrong interface fails
// exactly as an unsupported model would.
type scriptedCapabilityDetector struct {
	fixedCapabilityDetector
	supported    map[string]bool
	unsupported  map[string]bool
	unauthorized map[string]bool
}

func (d *scriptedCapabilityDetector) CapabilityDetectionPlan(target provider.ModelCapabilityDetectionTarget) (provider.CapabilityDetectionPlan, error) {
	if target.ProviderModel == "" || target.BindingID == "" || target.RiskTier != "safe_automatic" {
		return provider.CapabilityDetectionPlan{}, errors.New("capability detection target does not match adapter profile")
	}
	// Two roots and one dependent, mirroring the real chat interface: a model
	// that fails chat is still asked about embeddings before the interface is
	// written off.
	probes := []provider.CapabilityProbe{{Capability: "chat", Kind: "minimal_chat", MaxOutputTokens: 8, MayBill: true},
		{Capability: "embeddings", Kind: "embedding", MaxOutputTokens: 8, MayBill: true},
		{Capability: "streaming", Kind: "minimal_stream", DependsOn: []string{"chat"}, MaxOutputTokens: 8, MayBill: true}}
	if target.ProfileID == domain.ProfileOpenAIMediaResources {
		probes = []provider.CapabilityProbe{{Capability: "moderations", Kind: "moderation", MaxOutputTokens: 8, MayBill: true}}
	}
	return provider.CapabilityDetectionPlan{ContractVersion: provider.CapabilityDetectorContractVersion, Probes: probes, MaxCalls: len(probes)}, nil
}

func (d *scriptedCapabilityDetector) DetectCapability(_ context.Context, target provider.ModelCapabilityDetectionTarget, probe provider.CapabilityProbe) domain.CapabilityProbeResult {
	d.calls.Add(1)
	if d.supported[probe.Capability] {
		return domain.CapabilityProbeResult{Status: domain.ProbeSupported, Evidence: domain.EvidenceVerified, BindingID: target.BindingID, ProbeKind: probe.Kind}
	}
	if d.unsupported[probe.Capability] {
		return domain.CapabilityProbeResult{Status: domain.ProbeUnsupported, ErrorClass: "bad_request", BindingID: target.BindingID, ProbeKind: probe.Kind}
	}
	if d.unauthorized[probe.Capability] {
		return domain.CapabilityProbeResult{Status: domain.ProbeUnauthorized, ErrorClass: "authentication", BindingID: target.BindingID, ProbeKind: probe.Kind}
	}
	return domain.CapabilityProbeResult{Status: domain.ProbeInconclusive, ErrorClass: "bad_request", BindingID: target.BindingID, ProbeKind: probe.Kind}
}

type fixedCapabilityDetector struct{ calls atomic.Int64 }

// budgetRecordingDetector answers instantly and records how much time each
// probe was actually given, which is the property under test. Its plan mirrors
// the real chat interface: one root, and every other capability gated on it.
type budgetRecordingDetector struct {
	fixedCapabilityDetector
	mu      sync.Mutex
	budgets map[string]time.Duration
}

func (*budgetRecordingDetector) CapabilityDetectionPlan(target provider.ModelCapabilityDetectionTarget) (provider.CapabilityDetectionPlan, error) {
	if target.ProviderModel == "" || target.BindingID == "" || target.RiskTier != "safe_automatic" {
		return provider.CapabilityDetectionPlan{}, errors.New("capability detection target does not match adapter profile")
	}
	probes := []provider.CapabilityProbe{{Capability: "chat", Kind: "minimal_chat", MaxOutputTokens: 8, MayBill: true}}
	for _, dependent := range []string{"streaming", "tools", "json_object", "developer_role", "vision"} {
		probes = append(probes, provider.CapabilityProbe{Capability: dependent, Kind: dependent,
			DependsOn: []string{"chat"}, MaxOutputTokens: 8, MayBill: true})
	}
	return provider.CapabilityDetectionPlan{ContractVersion: provider.CapabilityDetectorContractVersion, Probes: probes, MaxCalls: len(probes)}, nil
}

func (d *budgetRecordingDetector) DetectCapability(ctx context.Context, target provider.ModelCapabilityDetectionTarget, probe provider.CapabilityProbe) domain.CapabilityProbeResult {
	d.calls.Add(1)
	d.mu.Lock()
	if d.budgets == nil {
		d.budgets = map[string]time.Duration{}
	}
	if deadline, ok := ctx.Deadline(); ok {
		d.budgets[probe.Capability] = time.Until(deadline)
	}
	d.mu.Unlock()
	return domain.CapabilityProbeResult{Status: domain.ProbeSupported, Evidence: domain.EvidenceVerified, BindingID: target.BindingID, ProbeKind: probe.Kind}
}

func (d *budgetRecordingDetector) budgetFor(capability string) time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.budgets[capability]
}

// The probe every other probe waits on used to be given the smallest share of
// the budget. Every capability in a chat plan depends on chat, so chat failing
// skips the rest without spending a call — yet the budget was divided by the
// number of probes the plan listed, leaving chat a sixth of the total here and
// a seventh on the real Bedrock Mantle plan. A frontier reasoning model needs
// longer than that for one non-streaming completion, so a model that works was
// reported as a timeout on every capability.
func TestRootProbeIsBoundedByTheAttemptTimeoutNotAFractionOfTheBudget(t *testing.T) {
	runtime, instance, chat, _ := twoInterfaceProviderForTest(t)
	detector := &budgetRecordingDetector{}
	registerBindingDetectors(t, runtime, instance, map[string]provider.Adapter{chat.ID: detector})
	runtime.config.Admin.ModelCapabilityDetection.TotalTimeout = config.Duration(90 * time.Second)
	runtime.config.Gateway.AttemptResponseHeaderTimeout = config.Duration(60 * time.Second)

	completed := runDetectionForTest(t, runtime, instance, "slow-reasoning-model")
	if completed.Status != domain.DetectionCompleted {
		t.Fatalf("status=%s", completed.Status)
	}
	// The even split gave the root 90s/6 = 15s. It is now bounded by the
	// attempt timeout, which is the same bound one gateway attempt gets.
	root := detector.budgetFor("chat")
	if root < 45*time.Second {
		t.Fatalf("root probe was given %s of a 90s budget with a 60s attempt timeout", root)
	}
	// The dependents still share what the root left, so this is a reallocation
	// and not a removal of the bound.
	dependent := detector.budgetFor("streaming")
	if dependent <= 0 || dependent >= root {
		t.Fatalf("dependent probe was given %s against a root share of %s", dependent, root)
	}
}

type lateCapabilityDetector struct {
	fixedCapabilityDetector
	started chan struct{}
	release chan struct{}
}

func (d *lateCapabilityDetector) DetectCapability(_ context.Context, target provider.ModelCapabilityDetectionTarget, probe provider.CapabilityProbe) domain.CapabilityProbeResult {
	d.calls.Add(1)
	close(d.started)
	<-d.release // Deliberately ignore cancellation to exercise late-result discard.
	return domain.CapabilityProbeResult{Status: domain.ProbeSupported, Evidence: domain.EvidenceVerified, BindingID: target.BindingID, ProbeKind: probe.Kind}
}

func (*fixedCapabilityDetector) Type() string { return string(domain.ProviderOpenAI) }
func (*fixedCapabilityDetector) Close()       {}
func (*fixedCapabilityDetector) Chat(context.Context, provider.ChatCall) (openaiapi.ChatCompletionResponse, error) {
	return openaiapi.ChatCompletionResponse{}, nil
}
func (*fixedCapabilityDetector) ChatStream(context.Context, provider.ChatCall, func(semantic.Event) error) (*openaiapi.Usage, error) {
	return nil, nil
}
func (*fixedCapabilityDetector) Embed(context.Context, provider.EmbeddingCall) (openaiapi.EmbeddingResponse, error) {
	return openaiapi.EmbeddingResponse{}, nil
}
func (*fixedCapabilityDetector) CapabilityDetectionPlan(target provider.ModelCapabilityDetectionTarget) (provider.CapabilityDetectionPlan, error) {
	return provider.CapabilityDetectionPlan{ContractVersion: provider.CapabilityDetectorContractVersion, MaxCalls: 1,
		Probes: []provider.CapabilityProbe{{Capability: "chat", Kind: "minimal_chat", MaxInputBytes: 64, MaxOutputTokens: 8, MayBill: true}}}, nil
}
func (d *fixedCapabilityDetector) DetectCapability(_ context.Context, target provider.ModelCapabilityDetectionTarget, probe provider.CapabilityProbe) domain.CapabilityProbeResult {
	d.calls.Add(1)
	return domain.CapabilityProbeResult{Status: domain.ProbeSupported, Evidence: domain.EvidenceVerified, BindingID: target.BindingID, ProbeKind: probe.Kind}
}

func TestCapabilityDetectionAPIIsExplicitCachedAndCreatesUntestedSnapshot(t *testing.T) {
	runtime, bootstrap := bootstrapForCapabilityTest(t)
	runtime.config.Admin.ModelCapabilityDetection.CreateRPM = 1
	// The create limiter's window is a wall-clock minute. Seconds of real time
	// pass between the first create and the one that must be throttled — the
	// detection has to finish and its metrics have to appear — so an unpinned
	// clock crossing a minute boundary hands the third request a fresh window
	// and it is accepted instead. Pinning makes the assertion about the limiter
	// rather than about where the run happened to land in the minute.
	pinned := time.Date(2026, time.August, 7, 9, 30, 30, 0, time.UTC)
	runtime.now = func() time.Time { return pinned }
	instance, err := runtime.store.GetProvider(context.Background(), bootstrap.ProviderID)
	if err != nil {
		t.Fatal(err)
	}
	binding := instance.EffectiveProfileBindings()[0]
	original := runtime.providers
	registry := provider.NewRegistry()
	detector := &fixedCapabilityDetector{}
	if err := registry.RegisterBindingAdapter(instance.ID, binding.ID, detector); err != nil {
		t.Fatal(err)
	}
	runtime.providers = registry
	original.Close()
	session := loginTestAdmin(t, runtime, "admin", "correct horse battery staple")

	createDetection := func(key string) *httptest.ResponseRecorder {
		request := adminMutationRequest(t, http.MethodPost, "/admin/api/v1/providers/"+instance.ID+"/model-capability-detections", session, map[string]any{
			"provider_model": "unlisted-model", "target_kind": "model_id", "risk_tier": "safe_automatic", "selection_revision": "selection-one",
			"current_password": "correct horse battery staple",
		})
		request.Header.Set("Idempotency-Key", key)
		response := httptest.NewRecorder()
		runtime.adminRouter().ServeHTTP(response, request)
		return response
	}
	response := createDetection("explicit-one")
	if response.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var created domain.ModelCapabilityDetection
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.TargetFingerprint != "" || created.IdempotencyKeyHash != "" || created.RequestHash != "" || len(created.Calls) != 0 {
		t.Fatalf("private fields leaked: %#v", created)
	}
	var completed domain.ModelCapabilityDetection
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		completed, err = runtime.store.GetModelCapabilityDetection(context.Background(), created.ID)
		if err == nil && completed.Status.Terminal() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if completed.Status != domain.DetectionCompleted || detector.calls.Load() != 1 || !completed.Recommended.Chat {
		t.Fatalf("completed=%#v calls=%d", completed, detector.calls.Load())
	}
	metrics := ""
	metricsDeadline := time.Now().Add(time.Second)
	for time.Now().Before(metricsDeadline) {
		metrics = renderMetricsForTest(t, runtime)
		if strings.Contains(metrics, "halro_model_capability_detection_total{") {
			break
		}
		time.Sleep(time.Millisecond)
	}
	for _, series := range []string{
		`halro_model_capability_detection_total{provider_type="openai",status="completed",source="verified_probe"} 1`,
		`halro_model_capability_probe_total{provider_type="openai",capability="chat",status="supported"} 1`,
		`halro_model_capability_detection_provider_calls_total{provider_type="openai"} 1`,
	} {
		if !strings.Contains(metrics, series) {
			t.Fatalf("missing detection metric %q\n%s", series, grepSeries(metrics, "halro_model_capability"))
		}
	}
	for _, forbidden := range []string{instance.ID, completed.ID, completed.ProviderModel, completed.BindingID} {
		if strings.Contains(grepSeries(metrics, "halro_model_capability"), forbidden) {
			t.Fatalf("detection metrics leaked %q", forbidden)
		}
	}

	cached := createDetection("explicit-two")
	if cached.Code != http.StatusOK || detector.calls.Load() != 1 {
		t.Fatalf("cache status=%d calls=%d body=%s", cached.Code, detector.calls.Load(), cached.Body.String())
	}
	rateLimitedRequest := adminMutationRequest(t, http.MethodPost, "/admin/api/v1/providers/"+instance.ID+"/model-capability-detections", session, map[string]any{
		"provider_model": "another-unlisted-model", "target_kind": "model_id", "risk_tier": "safe_automatic",
		"current_password": "correct horse battery staple",
	})
	rateLimitedRequest.Header.Set("Idempotency-Key", "rate-limited-new-work")
	rateLimited := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(rateLimited, rateLimitedRequest)
	if rateLimited.Code != http.StatusTooManyRequests || detector.calls.Load() != 1 ||
		!strings.Contains(rateLimited.Body.String(), `"code":"capability_detection_rate_limited"`) {
		t.Fatalf("rate limit status=%d calls=%d body=%s", rateLimited.Code, detector.calls.Load(), rateLimited.Body.String())
	}

	deploymentResponse := performAdminMutation(t, runtime, session.cookie, session.csrf, http.MethodPost, "/admin/api/v1/deployments", "", map[string]any{
		"name": "Detected", "provider_id": instance.ID, "provider_model": "unlisted-model", "target_kind": "model_id",
		"capabilities": map[string]any{"chat": true}, "capability_detection_id": completed.ID,
		"capability_detection_revision": completed.Revision, "max_concurrency": 0, "enabled": false,
	})
	if deploymentResponse.Code != http.StatusCreated {
		t.Fatalf("deployment status=%d body=%s", deploymentResponse.Code, deploymentResponse.Body.String())
	}
	var deployment domain.Deployment
	if err := json.Unmarshal(deploymentResponse.Body.Bytes(), &deployment); err != nil {
		t.Fatal(err)
	}
	if deployment.ModelCapabilitySnapshot.Source != "verified_probe" || deployment.ModelCapabilitySnapshot.Evidence["chat"] != domain.EvidenceVerified ||
		deployment.LastTestRevision != 0 || deployment.LastTestStatus != "" {
		t.Fatalf("deployment=%#v", deployment)
	}

	if completed.ExpiresAt == nil {
		t.Fatal("completed detection has no expiry")
	}
	expiredAt := completed.ExpiresAt.Add(time.Minute)
	runtime.now = func() time.Time { return expiredAt }
	stale := performAdminMutation(t, runtime, session.cookie, session.csrf, http.MethodPost, "/admin/api/v1/deployments", "", map[string]any{
		"name": "Expired detection", "provider_id": instance.ID, "provider_model": "unlisted-model", "target_kind": "model_id",
		"capabilities": map[string]any{"chat": true}, "capability_detection_id": completed.ID,
		"capability_detection_revision": completed.Revision, "enabled": false,
	})
	if stale.Code != http.StatusConflict || !strings.Contains(stale.Body.String(), `"code":"capability_detection_stale"`) {
		t.Fatalf("stale status=%d body=%s", stale.Code, stale.Body.String())
	}
}

func TestCapabilityDetectionCancelDiscardsALateSupportedResult(t *testing.T) {
	runtime, bootstrap := bootstrapForCapabilityTest(t)
	instance, err := runtime.store.GetProvider(context.Background(), bootstrap.ProviderID)
	if err != nil {
		t.Fatal(err)
	}
	binding := instance.EffectiveProfileBindings()[0]
	original := runtime.providers
	registry := provider.NewRegistry()
	detector := &lateCapabilityDetector{started: make(chan struct{}), release: make(chan struct{})}
	if err := registry.RegisterBindingAdapter(instance.ID, binding.ID, detector); err != nil {
		t.Fatal(err)
	}
	runtime.providers = registry
	original.Close()
	t.Cleanup(func() {
		select {
		case <-detector.release:
		default:
			close(detector.release)
		}
	})
	session := loginTestAdmin(t, runtime, "admin", "correct horse battery staple")
	request := adminMutationRequest(t, http.MethodPost, "/admin/api/v1/providers/"+instance.ID+"/model-capability-detections", session, map[string]any{
		"provider_model": "late-model", "target_kind": "model_id", "risk_tier": "safe_automatic",
		"current_password": "correct horse battery staple",
	})
	request.Header.Set("Idempotency-Key", "cancel-late")
	response := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("create status=%d body=%s", response.Code, response.Body.String())
	}
	var created domain.ModelCapabilityDetection
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	select {
	case <-detector.started:
	case <-time.After(2 * time.Second):
		t.Fatal("provider probe did not start")
	}
	running, err := runtime.store.GetModelCapabilityDetection(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	canceled := performAdminMutation(t, runtime, session.cookie, session.csrf, http.MethodDelete,
		"/admin/api/v1/model-capability-detections/"+created.ID, revisionETag(running.Revision), nil)
	if canceled.Code != http.StatusOK {
		t.Fatalf("cancel status=%d body=%s", canceled.Code, canceled.Body.String())
	}
	close(detector.release)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		stored, getErr := runtime.store.GetModelCapabilityDetection(context.Background(), created.ID)
		if getErr == nil && stored.Status == domain.DetectionCanceled {
			if stored.Results["chat"].Status == domain.ProbeSupported || stored.Recommended.Chat || stored.Calls[0].Status != "unknown" {
				t.Fatalf("late result survived cancellation: %#v", stored)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("detection did not reach canceled")
}

// A model the catalog covers used to be unverifiable: the catalog answered, the
// handler short-circuited, and no amount of asking would spend a probe on it.
// The catalog is a review of what a model was, and an operator whose account
// behaves otherwise had nowhere to go but a manual declaration.
//
// A refresh is what asks. It declines the answers that already exist — the
// stored detection and the catalog's review alike — and measures instead, while
// the ordinary path still costs nothing.
func TestVerificationProbesAModelTheCatalogAlreadyCovers(t *testing.T) {
	runtime, bootstrap := bootstrapForCapabilityTest(t)
	instance, err := runtime.store.GetProvider(context.Background(), bootstrap.ProviderID)
	if err != nil {
		t.Fatal(err)
	}
	binding := instance.EffectiveProfileBindings()[0]
	original := runtime.providers
	registry := provider.NewRegistry()
	detector := &fixedCapabilityDetector{}
	if err := registry.RegisterBindingAdapter(instance.ID, binding.ID, detector); err != nil {
		t.Fatal(err)
	}
	runtime.providers = registry
	original.Close()
	session := loginTestAdmin(t, runtime, "admin", "correct horse battery staple")

	create := func(key string, refresh bool) *httptest.ResponseRecorder {
		body := map[string]any{
			"provider_model": "gpt-4o-mini", "target_kind": "model_id", "risk_tier": "safe_automatic",
			"current_password": "correct horse battery staple",
		}
		if refresh {
			body["force_refresh"] = true
		}
		request := adminMutationRequest(t, http.MethodPost, "/admin/api/v1/providers/"+instance.ID+"/model-capability-detections", session, body)
		request.Header.Set("Idempotency-Key", key)
		response := httptest.NewRecorder()
		runtime.adminRouter().ServeHTTP(response, request)
		return response
	}

	fromCatalog := create("catalog-answer", false)
	if fromCatalog.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", fromCatalog.Code, fromCatalog.Body.String())
	}
	var answered domain.ModelCapabilityDetection
	if err := json.Unmarshal(fromCatalog.Body.Bytes(), &answered); err != nil {
		t.Fatal(err)
	}
	if answered.Source != "builtin_catalog" || answered.MaxProviderCalls != 0 || detector.calls.Load() != 0 {
		t.Fatalf("the catalog answer spent something: detection=%#v calls=%d", answered, detector.calls.Load())
	}

	verified := create("verify-instead", true)
	if verified.Code != http.StatusAccepted {
		t.Fatalf("verification status=%d body=%s", verified.Code, verified.Body.String())
	}
	var started domain.ModelCapabilityDetection
	if err := json.Unmarshal(verified.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	var completed domain.ModelCapabilityDetection
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		completed, err = runtime.store.GetModelCapabilityDetection(context.Background(), started.ID)
		if err == nil && completed.Status.Terminal() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if completed.Status != domain.DetectionCompleted || completed.Source != "verified_probe" {
		t.Fatalf("verification did not run: %#v", completed)
	}
	if detector.calls.Load() == 0 || completed.ProviderCalls == 0 {
		t.Fatalf("verification spent nothing: calls=%d detection=%#v", detector.calls.Load(), completed)
	}
	// It probes where the catalog already says the model runs, rather than
	// spending calls identifying an interface that is not in question.
	if completed.BindingID != binding.ID {
		t.Fatalf("verification ran on %q, want %q", completed.BindingID, binding.ID)
	}
	if completed.Baseline == nil || !completed.Baseline.Chat || !completed.Baseline.Vision {
		t.Fatalf("verification recorded no baseline to measure against: %#v", completed.Baseline)
	}
	if completed.Results["chat"].Status != domain.ProbeSupported || completed.Results["chat"].Evidence != domain.EvidenceVerified {
		t.Fatalf("chat was not verified: %#v", completed.Results["chat"])
	}
	// The plan reaches chat and nothing else, and what it cannot reach it must
	// not delete: vision stays on the catalog's claim rather than being reported
	// as absent by a run that never asked about it.
	if !completed.Recommended.Chat || !completed.Recommended.Vision {
		t.Fatalf("verification dropped a claim it never measured: %#v", completed.Recommended)
	}
	// And the deployment that adopts it says which half was measured.
	deploymentResponse := performAdminMutation(t, runtime, session.cookie, session.csrf, http.MethodPost, "/admin/api/v1/deployments", "", map[string]any{
		"name": "Verified", "provider_id": instance.ID, "provider_model": "gpt-4o-mini", "target_kind": "model_id",
		"capabilities": map[string]any{"chat": true, "vision": true}, "capability_detection_id": completed.ID,
		"capability_detection_revision": completed.Revision, "max_concurrency": 0, "enabled": false,
	})
	if deploymentResponse.Code != http.StatusCreated {
		t.Fatalf("deployment status=%d body=%s", deploymentResponse.Code, deploymentResponse.Body.String())
	}
	var deployment domain.Deployment
	if err := json.Unmarshal(deploymentResponse.Body.Bytes(), &deployment); err != nil {
		t.Fatal(err)
	}
	if deployment.ModelCapabilitySnapshot.Evidence["chat"] != domain.EvidenceVerified {
		t.Fatalf("probed capability was not recorded as verified: %#v", deployment.ModelCapabilitySnapshot.Evidence)
	}
	if deployment.ModelCapabilitySnapshot.Evidence["vision"] != domain.EvidenceDeclared {
		t.Fatalf("carried claim recorded as %q, want declared", deployment.ModelCapabilitySnapshot.Evidence["vision"])
	}
}

// requestVerification asks for a model the catalog may already answer for to be
// measured anyway. It returns the raw response so a test can assert on a refusal
// as well as on a run.
func requestVerification(t *testing.T, runtime *Runtime, instance domain.ProviderInstance, model, key string) *httptest.ResponseRecorder {
	t.Helper()
	session := loginTestAdmin(t, runtime, "admin", "correct horse battery staple")
	request := adminMutationRequest(t, http.MethodPost, "/admin/api/v1/providers/"+instance.ID+"/model-capability-detections", session, map[string]any{
		"provider_model": model, "target_kind": "model_id", "risk_tier": "safe_automatic", "force_refresh": true,
		"current_password": "correct horse battery staple",
	})
	request.Header.Set("Idempotency-Key", key)
	response := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(response, request)
	return response
}

func awaitDetection(t *testing.T, runtime *Runtime, response *httptest.ResponseRecorder) domain.ModelCapabilityDetection {
	t.Helper()
	if response.Code != http.StatusAccepted {
		t.Fatalf("verification was not accepted: status=%d body=%s", response.Code, response.Body.String())
	}
	var created domain.ModelCapabilityDetection
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		current, err := runtime.store.GetModelCapabilityDetection(context.Background(), created.ID)
		if err == nil && current.Status.Terminal() {
			return current
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("verification did not reach a terminal status")
	return domain.ModelCapabilityDetection{}
}

// The catalog names the interface, so a verification has nothing to identify and
// must not spend anything trying. With one probeable interface this is
// unfalsifiable — there is nothing else to pick — so the provider is given two.
func TestVerificationProbesOnlyTheInterfaceTheCatalogNames(t *testing.T) {
	runtime, instance, chat, media := twoInterfaceProviderForTest(t)
	chatDetector := &scriptedCapabilityDetector{supported: map[string]bool{"chat": true}}
	mediaDetector := &scriptedCapabilityDetector{supported: map[string]bool{"moderations": true}}
	registerBindingDetectors(t, runtime, instance, map[string]provider.Adapter{chat.ID: chatDetector, media.ID: mediaDetector})

	completed := awaitDetection(t, runtime, requestVerification(t, runtime, instance, "gpt-4o-mini", "verify-pinned"))
	if completed.Status != domain.DetectionCompleted || completed.BindingID != chat.ID {
		t.Fatalf("verification ran on %q with status %s", completed.BindingID, completed.Status)
	}
	if mediaDetector.calls.Load() != 0 {
		t.Fatalf("verification spent %d calls identifying an interface the catalog already named", mediaDetector.calls.Load())
	}
}

// The catalog answering is not the same as something being able to probe. When
// the named interface has no detector the request is refused outright, with
// nothing spent — the earlier predicate said "the catalog knows this one" and
// let an empty candidate list through to be indexed.
func TestVerificationIsRefusedWhenTheNamedInterfaceCannotBeProbed(t *testing.T) {
	runtime, bootstrap := bootstrapForCapabilityTest(t)
	instance, err := runtime.store.GetProvider(context.Background(), bootstrap.ProviderID)
	if err != nil {
		t.Fatal(err)
	}
	binding := instance.EffectiveProfileBindings()[0]
	registerBindingDetectors(t, runtime, instance, map[string]provider.Adapter{binding.ID: &detectorlessAdapter{}})

	response := requestVerification(t, runtime, instance, "gpt-4o-mini", "verify-undetectable")
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"no_detectable_binding"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

// A verification that answered nothing is a failed verification, whatever
// baseline it carried. Silence keeps the catalog's claim, so a run whose every
// probe was refused by a revoked credential recommends the whole catalog entry —
// and would otherwise be stored as verified_probe, the one source allowed to
// claim verified evidence and the one that is exempt from catalog drift.
func TestVerificationThatMeasuredNothingFailsInsteadOfAdoptingTheCatalog(t *testing.T) {
	runtime, bootstrap := bootstrapForCapabilityTest(t)
	instance, err := runtime.store.GetProvider(context.Background(), bootstrap.ProviderID)
	if err != nil {
		t.Fatal(err)
	}
	binding := instance.EffectiveProfileBindings()[0]
	detector := &scriptedCapabilityDetector{supported: map[string]bool{},
		unauthorized: map[string]bool{"chat": true, "embeddings": true}}
	registerBindingDetectors(t, runtime, instance, map[string]provider.Adapter{binding.ID: detector})

	finished := awaitDetection(t, runtime, requestVerification(t, runtime, instance, "gpt-4o-mini", "verify-revoked"))
	if finished.Status != domain.DetectionFailed {
		t.Fatalf("status=%s recommended=%#v", finished.Status, finished.Recommended)
	}
	if finished.ExpiresAt != nil || finished.Fresh(time.Now().UTC()) {
		t.Fatalf("a verification that measured nothing became adoptable: %#v", finished)
	}
}

// What the probes disprove still binds what the baseline may carry. The catalog
// claims chat, streaming and vision; the upstream refuses chat; everything that
// needs chat has to go with it, and the record has to remain storable — a
// recommendation whose dependencies do not hold is refused by the validator, and
// a detection that cannot be stored never reaches a terminal status at all.
func TestVerificationClearsBaselineDependentsWhenTheirBaselineIsRefused(t *testing.T) {
	runtime, bootstrap := bootstrapForCapabilityTest(t)
	instance, err := runtime.store.GetProvider(context.Background(), bootstrap.ProviderID)
	if err != nil {
		t.Fatal(err)
	}
	binding := instance.EffectiveProfileBindings()[0]
	detector := &scriptedCapabilityDetector{supported: map[string]bool{"embeddings": true},
		unsupported: map[string]bool{"chat": true}}
	registerBindingDetectors(t, runtime, instance, map[string]provider.Adapter{binding.ID: detector})

	completed := awaitDetection(t, runtime, requestVerification(t, runtime, instance, "gpt-4o-mini", "verify-no-chat"))
	if completed.Status != domain.DetectionCompleted {
		t.Fatalf("status=%s", completed.Status)
	}
	if completed.Baseline == nil || !completed.Baseline.Chat || !completed.Baseline.Vision {
		t.Fatalf("baseline=%#v", completed.Baseline)
	}
	if completed.Recommended.Chat || completed.Recommended.Vision || completed.Recommended.Streaming || completed.Recommended.StreamUsage {
		t.Fatalf("a refused baseline kept its dependents: %#v", completed.Recommended)
	}
	if !completed.Recommended.Embeddings {
		t.Fatalf("an independent verified capability was dropped: %#v", completed.Recommended)
	}
	if err := completed.Validate(); err != nil {
		t.Fatalf("stored record does not validate: %v", err)
	}
}

// The cooldown still holds for what it was written to bound. Only the catalog's
// free answer is exempt, and it is identified by the ceiling it was written with
// rather than by the calls it has made so far — a queued verification has made
// none either.
func TestASecondVerificationInsideTheWindowIsRefused(t *testing.T) {
	runtime, bootstrap := bootstrapForCapabilityTest(t)
	instance, err := runtime.store.GetProvider(context.Background(), bootstrap.ProviderID)
	if err != nil {
		t.Fatal(err)
	}
	binding := instance.EffectiveProfileBindings()[0]
	detector := &fixedCapabilityDetector{}
	registerBindingDetectors(t, runtime, instance, map[string]provider.Adapter{binding.ID: detector})

	first := awaitDetection(t, runtime, requestVerification(t, runtime, instance, "gpt-4o-mini", "verify-once"))
	if first.Status != domain.DetectionCompleted {
		t.Fatalf("status=%s", first.Status)
	}
	spent := detector.calls.Load()
	second := requestVerification(t, runtime, instance, "gpt-4o-mini", "verify-twice")
	if second.Code != http.StatusTooManyRequests || !strings.Contains(second.Body.String(), `"code":"capability_detection_cooldown"`) {
		t.Fatalf("status=%d body=%s", second.Code, second.Body.String())
	}
	if detector.calls.Load() != spent {
		t.Fatalf("the refused verification spent %d more calls", detector.calls.Load()-spent)
	}
}

// An adapter that serves traffic and cannot answer questions about a model. Not
// every profile has a detector, and the difference decides whether a
// verification can be asked for at all.
type detectorlessAdapter struct{}

func (*detectorlessAdapter) Type() string { return string(domain.ProviderOpenAI) }
func (*detectorlessAdapter) Close()       {}
func (*detectorlessAdapter) Chat(context.Context, provider.ChatCall) (openaiapi.ChatCompletionResponse, error) {
	return openaiapi.ChatCompletionResponse{}, nil
}
func (*detectorlessAdapter) ChatStream(context.Context, provider.ChatCall, func(semantic.Event) error) (*openaiapi.Usage, error) {
	return nil, nil
}
func (*detectorlessAdapter) Embed(context.Context, provider.EmbeddingCall) (openaiapi.EmbeddingResponse, error) {
	return openaiapi.EmbeddingResponse{}, nil
}

// The console tells a verification from an ordinary detection by whether the
// record carries a baseline, so absence has to be real on the wire. It was not:
// `omitempty` does not omit a struct, so every detection carried an all-false
// baseline object, which is truthy in a browser — a first-time detection of a
// model the catalog does not cover reported its own findings as capabilities
// established beyond a catalog that never mentioned it.
func TestOnlyAVerificationCarriesABaselineOnTheWire(t *testing.T) {
	ordinary := publicCapabilityDetection(domain.ModelCapabilityDetection{ID: "mcd_ordinary"})
	if _, present := ordinary["baseline_capabilities"]; present {
		t.Fatalf("an ordinary detection carried a baseline: %#v", ordinary["baseline_capabilities"])
	}
	baseline := domain.ProviderCapabilities{Chat: true}
	verification := publicCapabilityDetection(domain.ModelCapabilityDetection{ID: "mcd_verified", Baseline: &baseline})
	if _, present := verification["baseline_capabilities"]; !present {
		t.Fatalf("a verification lost the baseline it was measured against: %#v", verification)
	}
}

// A capability the plan could not fit is recorded under probe_budget, and that
// kind is the whole point: the console filters risk_policy rows out of both the
// unestablished-capability banner and the failed-probe list, because a policy
// decision is not something an operator can act on. A budget ceiling is — they
// can raise it or declare the capability by hand.
//
// The entry was written into the in-memory detection and then thrown away: the
// probe loop opens by re-reading the record from the store and assigning over
// it, so the first iteration replaced the map. finalizeCapabilityDetection
// filled the missing name back in as risk_policy, and the capability vanished
// from the page.
func TestABudgetDeferredCapabilityIsStoredAsSuchRatherThanAsAPolicy(t *testing.T) {
	runtime, bootstrap := bootstrapForCapabilityTest(t)
	instance, err := runtime.store.GetProvider(context.Background(), bootstrap.ProviderID)
	if err != nil {
		t.Fatal(err)
	}
	bindings := instance.EffectiveProfileBindings()
	if len(bindings) != 1 {
		t.Fatalf("this test needs a single candidate so identification spends nothing; bindings=%d", len(bindings))
	}
	detector := &deferringCapabilityDetector{scriptedCapabilityDetector: scriptedCapabilityDetector{
		supported: map[string]bool{"chat": true},
	}}
	registerBindingDetectors(t, runtime, instance, map[string]provider.Adapter{bindings[0].ID: detector})

	completed := runDetectionForTest(t, runtime, instance, "budget-deferred-model")

	result, recorded := completed.Results["reasoning"]
	if !recorded {
		t.Fatalf("reasoning is absent from the stored results entirely: %#v", completed.Results)
	}
	if result.ProbeKind != "probe_budget" {
		t.Fatalf("the budget ceiling was stored as %q, so the console hides it: %#v", result.ProbeKind, result)
	}
	if result.Status != domain.ProbeNotProbed {
		t.Fatalf("a deferred capability was given a verdict: %#v", result)
	}
}

// deferringCapabilityDetector is a plan whose ceiling cut one capability, which
// is what the real OpenAI chat set does: it serves nine probeable capabilities
// and the plan may ask for eight. The scripted detector it embeds returns a plan
// with nothing deferred, so it cannot reach this path at all.
type deferringCapabilityDetector struct {
	scriptedCapabilityDetector
}

func (d *deferringCapabilityDetector) CapabilityDetectionPlan(target provider.ModelCapabilityDetectionTarget) (provider.CapabilityDetectionPlan, error) {
	plan, err := d.scriptedCapabilityDetector.CapabilityDetectionPlan(target)
	if err != nil {
		return plan, err
	}
	plan.Deferred = []string{"reasoning"}
	return plan, nil
}
