package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/semantic"
)

func TestResolveCapabilityDetectorRequiresExplicitBindingForUnknownModel(t *testing.T) {
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

	input := modelCapabilityDetectionInput{ProviderModel: "unlisted-image-model", RiskTier: "safe_automatic"}
	if _, _, _, _, _, err := runtime.resolveCapabilityDetector(instance, input, ""); !errors.Is(err, errAmbiguousCapabilityBinding) {
		t.Fatalf("unqualified unknown model resolved an arbitrary interface: %v", err)
	}

	input.BindingID = media.ID
	binding, _, detector, _, known, err := runtime.resolveCapabilityDetector(instance, input, "")
	if err != nil || binding.ID != media.ID || detector == nil || known {
		t.Fatalf("explicit interface was not honored: binding=%q detector=%T known=%t err=%v", binding.ID, detector, known, err)
	}
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
		"capability_detection_revision": completed.Revision, "max_concurrency": 0, "priority": 0, "weight": 1, "enabled": false,
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
		"capability_detection_revision": completed.Revision, "weight": 1, "enabled": false,
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
