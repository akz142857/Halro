package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
	if d.unauthorized[probe.Capability] {
		return domain.CapabilityProbeResult{Status: domain.ProbeUnauthorized, ErrorClass: "authentication", BindingID: target.BindingID, ProbeKind: probe.Kind}
	}
	return domain.CapabilityProbeResult{Status: domain.ProbeInconclusive, ErrorClass: "bad_request", BindingID: target.BindingID, ProbeKind: probe.Kind}
}

type fixedCapabilityDetector struct{ calls atomic.Int64 }

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
