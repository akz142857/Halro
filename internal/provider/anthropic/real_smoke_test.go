package anthropic

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/anthropicapi"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/semantic"
)

// TestRealProviderSmoke contacts a real Anthropic account and costs money. It is
// inert unless every variable below is set deliberately.
//
//	HALRO_REAL_PROVIDER_SMOKE=1
//	HALRO_SMOKE_PROFILE=anthropic
//	HALRO_SMOKE_BASE_URL=https://api.anthropic.com
//	HALRO_SMOKE_API_KEY=<Anthropic API key>
//	HALRO_SMOKE_MODEL=<exact upstream model id>
//
// This profile is the one with two execution modes, so one run has to prove
// both. A native request is forwarded verbatim; a portable request is
// re-authored through the canonical model and comes back through a different
// decoder. Passing on one says nothing about the other — every defect this
// package has produced lived in exactly one of the two paths.
//
// The catalog read is here because it is not only a listing: it is what a
// connection test falls back to when no Deployment names a model, so a change
// that breaks it takes the Admin console's provider test with it.
//
// Structure is asserted, never content. The matrix runner captures this
// process's output into an evidence file, and model output is not evidence.
func TestRealProviderSmoke(t *testing.T) {
	if os.Getenv("HALRO_REAL_PROVIDER_SMOKE") != "1" || os.Getenv("HALRO_SMOKE_PROFILE") != "anthropic" {
		t.Skip("set HALRO_REAL_PROVIDER_SMOKE=1 and HALRO_SMOKE_PROFILE=anthropic")
	}
	endpoint, err := url.Parse(os.Getenv("HALRO_SMOKE_BASE_URL"))
	if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" {
		t.Fatal("HALRO_SMOKE_BASE_URL must be an absolute HTTPS URL")
	}
	apiKey := os.Getenv("HALRO_SMOKE_API_KEY")
	model := os.Getenv("HALRO_SMOKE_MODEL")
	if apiKey == "" || model == "" {
		t.Fatal("HALRO_SMOKE_API_KEY and HALRO_SMOKE_MODEL are required")
	}
	authorizer, err := provider.NewStaticHeaderAuthorizer(
		domain.CredentialAnthropicAPIKey, "x-api-key", "", []byte(apiKey), "Authorization")
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 60 * time.Second}
	adapter, err := New(Options{
		Endpoint: endpoint, Authorizer: authorizer, Client: client,
		// The profile's own ceiling, so a smoke cannot exercise a capability the
		// product refuses to declare for this profile.
		Capabilities: smokeCapabilities(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	t.Run("native messages", func(t *testing.T) { smokeNativeMessages(ctx, t, adapter, model) })
	t.Run("native messages stream", func(t *testing.T) { smokeNativeMessagesStream(ctx, t, adapter, model) })
	t.Run("portable chat", func(t *testing.T) { smokePortableChat(ctx, t, adapter, model) })
	t.Run("portable chat stream", func(t *testing.T) { smokePortableStream(ctx, t, adapter, model) })
	t.Run("count tokens", func(t *testing.T) { smokeCountTokens(ctx, t, adapter, model) })
	t.Run("model catalog", func(t *testing.T) { smokeModelCatalog(ctx, t, adapter, model) })
	t.Run("probe without a deployment model", func(t *testing.T) {
		// The credential-only connection test, which is what an operator runs
		// before any Deployment exists.
		if err := adapter.Probe(ctx, ""); err != nil {
			t.Fatalf("catalog probe: %v", err)
		}
	})
}

func smokeNativeMessages(ctx context.Context, t *testing.T, adapter *Adapter, model string) {
	t.Helper()
	result, err := adapter.MessagesNative(ctx, provider.NativeMessageCall{
		RequestID: "anthropic-real-smoke", ProviderModel: model,
		Version: anthropicapi.SupportedVersion, Payload: smokeNativePayload(),
	})
	if err != nil {
		t.Fatalf("native messages: %v", err)
	}
	message, err := anthropicapi.DecodeMessage(result.Payload)
	if err != nil {
		t.Fatalf("native response did not decode: %v", err)
	}
	if message.Role != "assistant" || len(message.Content) == 0 {
		t.Fatal("native response carried no assistant content")
	}
	if message.Usage.InputTokens == 0 && message.Usage.OutputTokens == 0 {
		t.Fatal("native response reported no usage, so this run cannot be accounted")
	}
	if result.ProviderRequestID == "" {
		t.Fatal("native response carried no upstream request id")
	}
}

func smokeNativeMessagesStream(ctx context.Context, t *testing.T, adapter *Adapter, model string) {
	t.Helper()
	events := 0
	usage, err := adapter.MessagesNativeStream(ctx, provider.NativeMessageCall{
		RequestID: "anthropic-real-smoke-stream", ProviderModel: model,
		Version: anthropicapi.SupportedVersion, Payload: smokeNativePayload(),
	}, func(anthropicapi.RawStreamEvent) error {
		events++
		return nil
	})
	if err != nil {
		t.Fatalf("native messages stream: %v", err)
	}
	if events == 0 {
		t.Fatal("native stream produced no events")
	}
	if usage == nil || (usage.InputTokens == 0 && usage.OutputTokens == 0) {
		t.Fatal("native stream reported no usage, so this run cannot be accounted")
	}
}

// smokePortableChat is the path an OpenAI-shaped caller takes. It is also the
// path that has to tell the model not to think: a current model thinks by
// default, and a signed thinking block has nowhere to live in an OpenAI-shaped
// response, so the response would be refused as malformed after being billed.
func smokePortableChat(ctx context.Context, t *testing.T, adapter *Adapter, model string) {
	t.Helper()
	response, err := adapter.Chat(ctx, provider.ChatCall{
		RequestID: "anthropic-real-smoke-portable", ProviderModel: model,
		Request: openaiapi.ChatCompletionRequest{
			Model:     model,
			Messages:  []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("Reply with OK.")}},
			MaxTokens: smokeInt64(16),
		},
	})
	if err != nil {
		t.Fatalf("portable chat: %v", err)
	}
	if len(response.Choices) == 0 || response.Choices[0].Message == nil {
		t.Fatal("portable chat returned no assistant choice")
	}
	if response.Usage == nil || response.Usage.TotalTokens == 0 {
		t.Fatal("portable chat reported no usage, so this run cannot be accounted")
	}
	// finish_reason is an OpenAI enum. Anthropic ends a turn with "end_turn",
	// and letting that word through produced a value no OpenAI client can read
	// — and a 502 from /v1/responses for a turn that completed normally.
	finish := ""
	if response.Choices[0].FinishReason != nil {
		finish = *response.Choices[0].FinishReason
	}
	switch finish {
	case "stop", "length", "tool_calls", "content_filter", "function_call":
	default:
		t.Fatalf("finish_reason %q is not an OpenAI value", finish)
	}
}

func smokePortableStream(ctx context.Context, t *testing.T, adapter *Adapter, model string) {
	t.Helper()
	events := 0
	usage, err := adapter.ChatStream(ctx, provider.ChatCall{
		RequestID: "anthropic-real-smoke-portable-stream", ProviderModel: model,
		Request: openaiapi.ChatCompletionRequest{
			Model:     model,
			Messages:  []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("Reply with OK.")}},
			MaxTokens: smokeInt64(16),
		},
	}, func(semantic.Event) error {
		events++
		return nil
	})
	if err != nil {
		t.Fatalf("portable chat stream: %v", err)
	}
	if events == 0 {
		t.Fatal("portable stream produced no events")
	}
	if usage == nil || usage.TotalTokens == 0 {
		t.Fatal("portable stream reported no usage, so this run cannot be accounted")
	}
}

// smokeCountTokens exercises the endpoint only this profile serves. Anthropic
// does not bill it, but it is a real call on the operator's credential.
func smokeCountTokens(ctx context.Context, t *testing.T, adapter *Adapter, model string) {
	t.Helper()
	result, err := adapter.CountTokensNative(ctx, provider.NativeMessageCall{
		RequestID: "anthropic-real-smoke-count", ProviderModel: model,
		Version: anthropicapi.SupportedVersion, Payload: smokeCountTokensPayload(),
	})
	if err != nil {
		t.Fatalf("count_tokens: %v", err)
	}
	var counted struct {
		InputTokens int64 `json:"input_tokens"`
	}
	if err := json.Unmarshal(result.Payload, &counted); err != nil {
		t.Fatalf("count_tokens response did not decode: %v", err)
	}
	if counted.InputTokens <= 0 {
		t.Fatal("count_tokens reported no input tokens")
	}
}

// smokeModelCatalog asserts the shape the console depends on, and that the
// model this run was pointed at is one the account can actually see. A catalog
// that returns entries with a zero output ceiling is the shape the decoder
// produced while it read a field name the API does not use.
func smokeModelCatalog(ctx context.Context, t *testing.T, adapter *Adapter, model string) {
	t.Helper()
	targets, err := adapter.ListInvocationTargets(ctx, domain.TargetQuery{TargetKind: domain.TargetModelID})
	if err != nil {
		t.Fatalf("model catalog: %v", err)
	}
	if len(targets) == 0 {
		t.Fatal("model catalog returned no targets")
	}
	found := false
	limits := false
	for _, target := range targets {
		if target.TargetID == "" || target.DisplayName == "" {
			t.Fatalf("catalog entry is missing its identity: %#v", target)
		}
		if target.TargetID == strings.TrimSpace(model) {
			found = true
		}
		if target.Metadata.MaxContextTokens > 0 && target.Metadata.MaxOutputTokens > 0 {
			limits = true
		}
	}
	if !found {
		t.Fatalf("the configured model is not in this account's catalog of %d targets", len(targets))
	}
	if !limits {
		t.Fatal("no catalog entry reported both token limits; the decoder is reading a field the API does not send")
	}
}

func smokeNativePayload() []byte {
	return []byte(`{"model":"placeholder","max_tokens":16,` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"Reply with OK."}]}]}`)
}

// smokeCountTokensPayload carries no max_tokens. count_tokens has no such
// parameter — there is no generation to bound — and the upstream rejects the
// member outright: `max_tokens: Extra inputs are not permitted`.
//
// Halro forwards it rather than stripping it, which is what makes this the
// caller's error to see: prepareCountTokensPayload rewrites the upstream model
// and nothing else, so a caller who sends a member this endpoint does not take
// gets the upstream's own refusal instead of a request Halro quietly rewrote.
// The first run of this smoke made the same mistake and was answered the same
// way, which is the behaviour working.
func smokeCountTokensPayload() []byte {
	return []byte(`{"model":"placeholder",` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"Reply with OK."}]}]}`)
}

// smokeCapabilities is the ceiling this profile declares by default, so the
// smoke exercises what the product would actually permit.
func smokeCapabilities() provider.Capabilities {
	declared := domain.DefaultProviderCapabilitiesForProfile(domain.ProviderAnthropic, domain.ProfileAnthropicMessages)
	return provider.Capabilities{
		Chat: declared.Chat, Streaming: declared.Streaming, Embeddings: declared.Embeddings,
		Tools: declared.Tools, Vision: declared.Vision,
		JSONObject: declared.JSONObject, StructuredOutputs: declared.StructuredOutputs,
		DeveloperRole: declared.DeveloperRole, Reasoning: declared.Reasoning,
		StreamUsage:      declared.StreamUsage,
		MaxContextTokens: declared.MaxContextTokens, MaxOutputTokens: declared.MaxOutputTokens,
	}
}

func smokeInt64(value int64) *int64 { return &value }
