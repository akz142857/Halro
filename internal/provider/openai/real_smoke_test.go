package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/semantic"
)

// TestRealProviderSmoke is intentionally inert unless the operator explicitly
// opts in. It never logs credentials, request content, or provider responses.
//
// Required:
//
//	HALRO_REAL_PROVIDER_SMOKE=1
//	HALRO_SMOKE_PROFILE=openai|azure_openai|deepseek|openai_compatible
//	HALRO_SMOKE_BASE_URL=...
//	HALRO_SMOKE_API_KEY=...
//	HALRO_SMOKE_MODEL=...
//
// Azure also requires HALRO_SMOKE_API_VERSION. Embeddings run only when
// HALRO_SMOKE_EMBEDDING_MODEL is set.
func TestRealProviderSmoke(t *testing.T) {
	if os.Getenv("HALRO_REAL_PROVIDER_SMOKE") != "1" {
		t.Skip("real provider smoke is opt-in")
	}
	profile := os.Getenv("HALRO_SMOKE_PROFILE")
	baseURL := os.Getenv("HALRO_SMOKE_BASE_URL")
	apiKey := os.Getenv("HALRO_SMOKE_API_KEY")
	model := os.Getenv("HALRO_SMOKE_MODEL")
	if profile == "" || baseURL == "" || apiKey == "" || model == "" {
		t.Fatal("real smoke requires profile, base URL, API key, and model")
	}
	switch profile {
	case "openai", "azure_openai", "deepseek", "openai_compatible":
	default:
		t.Fatal("unsupported smoke profile")
	}
	endpoint, err := url.Parse(baseURL)
	if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" {
		t.Fatal("smoke base URL must be an absolute HTTPS URL")
	}
	capabilities := provider.Capabilities{
		Chat: true, Streaming: true, Embeddings: true, StreamUsage: true,
	}
	if profile == "deepseek" {
		capabilities.Embeddings = false
	}
	if profile == "openai_compatible" {
		capabilities.StreamUsage = false
	}
	client := &http.Client{Timeout: 45 * time.Second}
	adapter, err := NewWithOptions(Options{
		Endpoint: endpoint, APIKey: []byte(apiKey), Client: client,
		ProviderType: profile, APIVersion: os.Getenv("HALRO_SMOKE_API_VERSION"),
		Azure: profile == "azure_openai", Capabilities: capabilities,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	// Which output-limit parameter to send is a property of the upstream, not a
	// preference. OpenAI's current models reject max_tokens outright —
	// `unsupported_parameter:max_tokens`, HTTP 400 — and take
	// max_completion_tokens instead; Azure follows the same API. DeepSeek and the
	// reviewed compatible endpoints are on the older parameter, and sending both
	// is rejected everywhere, so this picks one per profile.
	//
	// This smoke sent max_tokens to every profile, which made the GA gate's
	// OpenAI cell fail against any current model. The capability-detection path
	// beside it was already on max_completion_tokens
	// (internal/provider/capability_detection.go:113); only this file was stale.
	request := openaiapi.ChatCompletionRequest{
		Model: model,
		Messages: []openaiapi.Message{{
			Role: "user", Content: openaiapi.TextContent("Reply with OK."),
		}},
	}
	switch profile {
	case "openai", "azure_openai":
		request.MaxCompletionTokens = int64Pointer(16)
	default:
		request.MaxTokens = int64Pointer(16)
	}
	response, err := adapter.Chat(ctx, provider.ChatCall{
		RequestID: "smoke_nonstream", ProviderModel: model, Request: request,
	})
	if err != nil {
		t.Fatalf("non-stream chat failed: %s", smokeErrorClass(err))
	}
	if response.ID == "" || len(response.Choices) == 0 {
		t.Fatal("non-stream chat returned an incomplete envelope")
	}

	streamRequest := request
	streamRequest.Stream = true
	chunks := 0
	_, err = adapter.ChatStream(ctx, provider.ChatCall{
		RequestID: "smoke_stream", ProviderModel: model, Request: streamRequest,
	}, func(chunk semantic.Event) error {
		if len(chunk.Outputs) > 0 {
			chunks++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("stream chat failed: %s", smokeErrorClass(err))
	}
	if chunks == 0 {
		t.Fatal("stream chat returned no semantic chunks")
	}

	embeddingModel := os.Getenv("HALRO_SMOKE_EMBEDDING_MODEL")
	if embeddingModel != "" {
		if !capabilities.Embeddings {
			t.Fatal("embedding model was configured for a profile without embeddings")
		}
		embedding, err := adapter.Embed(ctx, provider.EmbeddingCall{
			RequestID: "smoke_embedding", ProviderModel: embeddingModel,
			Request: openaiapi.EmbeddingRequest{
				Model: embeddingModel, Input: json.RawMessage(`["smoke"]`),
			},
		})
		if err != nil {
			t.Fatalf("embedding failed: %s", smokeErrorClass(err))
		}
		if len(embedding.Data) == 0 {
			t.Fatal("embedding returned no vectors")
		}
	}
	if os.Getenv("HALRO_SMOKE_CAPABILITY_DETECTION") == "1" {
		runRealCapabilityDetection(t, adapter, profile, model)
	}
}

func runRealCapabilityDetection(t *testing.T, adapter *Adapter, profile, model string) {
	providerType := domain.ProviderType(profile)
	profileID := map[string]domain.ProviderProfileID{
		"openai": domain.ProfileOpenAIChatEmbeddings, "azure_openai": domain.ProfileAzureChatEmbeddings,
		"deepseek": domain.ProfileDeepSeekChat, "openai_compatible": domain.ProfileOpenAICompatible,
	}[profile]
	manifest, ok := provider.BuiltinProfile(profileID)
	if !ok {
		t.Fatal("capability detection profile is not registered")
	}
	bridge, err := provider.NewLegacyAdapterBridge(adapter, manifest,
		domain.EvidenceForCapabilities(domain.DefaultProviderCapabilitiesForProfile(providerType, profileID), domain.EvidenceDeclared))
	if err != nil {
		t.Fatal("capability detector construction failed")
	}
	target := provider.ModelCapabilityDetectionTarget{ProviderModel: model, BindingID: "real-matrix", ProfileID: profileID, RiskTier: "safe_automatic"}
	plan, err := bridge.CapabilityDetectionPlan(target)
	if err != nil || plan.MaxCalls > 8 || len(plan.Probes) > 8 {
		t.Fatal("capability detection plan is invalid")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	supported, chatSupported := 0, false
	for _, probe := range plan.Probes {
		if probe.PersistentEffect || !probe.MayBill {
			t.Fatal("capability detection plan contains an unsafe probe")
		}
		result := bridge.DetectCapability(ctx, target, probe)
		if !result.Status.Valid() {
			t.Fatal("capability detection returned an invalid classification")
		}
		if result.Status == domain.ProbeSupported {
			supported++
			chatSupported = chatSupported || probe.Capability == "chat"
		}
	}
	if !chatSupported {
		t.Fatal("capability detection did not verify chat for the configured model")
	}
	// This is the only detection evidence written to test output: bounded counts
	// and an explicit explanation for the absent stable negative model. No model,
	// prompt, output, provider error body, request ID, or credential is logged.
	t.Logf("capability detection passed: calls=%d supported=%d stable_negative=not_configured", len(plan.Probes), supported)
}

func int64Pointer(value int64) *int64 {
	return &value
}

// smokeErrorClass names a failure as narrowly as the evidence contract allows.
//
// The class alone was not enough to act on: a run that answered "bad_request"
// said nothing about which parameter the upstream refused, and the operator had
// to bisect a request they did not write. The status and the upstream's own
// error code are added because they are identifiers rather than prose — the same
// two fields a failed connection test already logs — while the message stays
// out, since that is where an upstream quotes back what it was sent.
//
// The code is admitted only in the shape a real error code takes. Anything else
// is dropped rather than trimmed: a value that is not an identifier is prose,
// and prose is what this must not print.
func smokeErrorClass(err error) string {
	var classified *provider.Error
	if !errorsAsProvider(err, &classified) {
		return "unknown"
	}
	description := string(classified.Class)
	if classified.StatusCode > 0 {
		description += fmt.Sprintf(" http=%d", classified.StatusCode)
	}
	if code := smokeIdentifier(classified.ProviderCode); code != "" {
		description += " code=" + code
	}
	return description
}

// smokeIdentifier mirrors probeIdentifier in the Admin provider tests: bounded
// length, and only the characters a provider error code is made of.
func smokeIdentifier(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || len(trimmed) > 120 {
		return ""
	}
	for _, r := range trimmed {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == '-' || r == ':':
		default:
			return ""
		}
	}
	return trimmed
}

// Kept local so failure messages cannot accidentally format a wrapped error
// containing a provider response body.
func errorsAsProvider(err error, target **provider.Error) bool {
	for err != nil {
		if classified, ok := err.(*provider.Error); ok {
			*target = classified
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}
