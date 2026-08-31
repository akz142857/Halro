package openai

import (
	"context"
	"encoding/json"
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

// The MiniMax adaptation was written from documentation and one unauthenticated
// probe. This is the test that turns its assumptions into measurements.
//
// It is opt-in and billable, and it skips without a key. Run it once per region:
// MiniMax splits by account, api.minimax.io for international and
// api.minimaxi.com for mainland, with keys that are not interchangeable. Halro
// serves both from one profile group on the strength of the two contracts being
// identical, which is only established when both have been run.
//
// See docs/verification/provider-real-matrix.md for what each assertion decides
// and docs/prd/minimax-adaptation-plan.zh-CN.md §7 for the assumptions the
// implementation rests on.
func TestRealMiniMaxSmoke(t *testing.T) {
	apiKey := strings.TrimSpace(os.Getenv("HALRO_MINIMAX_API_KEY"))
	if apiKey == "" {
		t.Skip("set HALRO_MINIMAX_API_KEY to run the billable MiniMax smoke")
	}
	rawEndpoint := strings.TrimSpace(os.Getenv("HALRO_MINIMAX_BASE_URL"))
	if rawEndpoint == "" {
		rawEndpoint = "https://api.minimax.io"
	}
	endpoint, err := url.Parse(rawEndpoint)
	if err != nil {
		t.Fatalf("HALRO_MINIMAX_BASE_URL is not a URL: %v", err)
	}
	model := strings.TrimSpace(os.Getenv("HALRO_MINIMAX_MODEL"))
	if model == "" {
		model = "MiniMax-M3"
	}
	client := &http.Client{Timeout: 120 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()

	newAdapter := func(t *testing.T, responses bool) *Adapter {
		t.Helper()
		authorizer, err := provider.NewStaticHeaderAuthorizer(domain.CredentialBearerStatic, "Authorization", "Bearer ", []byte(apiKey), "x-api-key")
		if err != nil {
			t.Fatalf("authorizer: %v", err)
		}
		adapter, err := NewWithOptions(Options{
			Endpoint: endpoint, Authorizer: authorizer, Client: client,
			ProviderType: string(domain.ProviderMiniMax), CredentialScheme: domain.CredentialBearerStatic,
			Capabilities: domain.ProviderCapabilities{Chat: true, Streaming: true, Tools: true, StreamUsage: true},
			Responses:    responses,
		})
		if err != nil {
			t.Fatalf("adapter: %v", err)
		}
		t.Cleanup(adapter.Close)
		return adapter
	}
	ask := func(model string) openaiapi.ChatCompletionRequest {
		return openaiapi.ChatCompletionRequest{
			Model:    model,
			Messages: []openaiapi.Message{{Role: "user", Content: json.RawMessage(`"Reply with the single word: ok"`)}},
		}
	}

	// Assertion 2 in the matrix: whether input and output can be settled at
	// their own rates, or whether only a total comes back.
	t.Run("chat usage carries the input and output split", func(t *testing.T) {
		response, err := newAdapter(t, false).Chat(ctx, provider.ChatCall{RequestID: "smoke_chat", ProviderModel: model, Request: ask(model)})
		if err != nil {
			t.Fatalf("chat: %v", err)
		}
		if response.Usage == nil {
			t.Fatal("no usage at all; a settled attempt would carry no cost")
		}
		t.Logf("usage: prompt=%d completion=%d total=%d", response.Usage.PromptTokens, response.Usage.CompletionTokens, response.Usage.TotalTokens)
		if response.Usage.PromptTokens == 0 || response.Usage.CompletionTokens == 0 {
			t.Fatal("MiniMax reported only a total; input and output cannot be settled at their own rates, and the plan's §3.2 fallback applies")
		}
	})

	// Assertion 4: without usage on the final chunk a streamed call has no
	// measured cost, and StreamUsage is claimed by both streaming profiles.
	t.Run("stream reports usage on the final chunk", func(t *testing.T) {
		request := ask(model)
		request.Stream = true
		var events int
		usage, err := newAdapter(t, false).ChatStream(ctx, provider.ChatCall{RequestID: "smoke_stream", ProviderModel: model, Request: request}, func(semantic.Event) error {
			events++
			return nil
		})
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		if events == 0 {
			t.Fatal("the stream produced no events")
		}
		if usage == nil || usage.TotalTokens == 0 {
			t.Fatal("stream_options.include_usage produced no usage; a streamed attempt would settle at the reservation with nothing measured")
		}
	})

	// Assertion 1, and the most expensive one to be wrong about: a failure
	// wearing a 200 is settled as a success unless the guard sees it.
	t.Run("a controlled error is reported as a failure", func(t *testing.T) {
		request := ask(model)
		request.Model = model + "-does-not-exist"
		_, err := newAdapter(t, false).Chat(ctx, provider.ChatCall{RequestID: "smoke_error", ProviderModel: request.Model, Request: request})
		if err == nil {
			t.Fatal("an unknown model was accepted as a success; either the upstream really served it, or a 200-wrapped refusal slipped past the base_resp guard")
		}
		providerErr, ok := err.(*provider.Error)
		if !ok {
			t.Fatalf("the refusal is not a provider error, so retry bounding and failover cannot read it: %T", err)
		}
		t.Logf("refusal: class=%s status=%d code=%s retryable=%v ambiguous=%v",
			providerErr.Class, providerErr.StatusCode, providerErr.ProviderCode, providerErr.Retryable, providerErr.Ambiguous)
		if providerErr.Class == provider.ErrorMalformed {
			t.Fatal("the refusal arrived unclassified; a rate limit reaching failover as malformed keeps a throttled route looking healthy")
		}
	})

	// Assertion 5: Halro sends the switch on every request that did not ask to
	// think, because M3 thinks by default. M2.x cannot switch it off, and which
	// way it answers decides whether that default is safe.
	t.Run("thinking disabled is accepted", func(t *testing.T) {
		request := ask(model)
		request.ReasoningEffort = "none"
		if _, err := newAdapter(t, false).Chat(ctx, provider.ChatCall{RequestID: "smoke_thinking", ProviderModel: model, Request: request}); err != nil {
			t.Fatalf("model %q refused an explicitly disabled thinking switch: %v", model, err)
		}
	})

	// The Responses face is a separate route on the same key. It binds no stream
	// primitive and claims no reasoning, so one unary call is the whole surface.
	t.Run("responses serves a unary generation", func(t *testing.T) {
		_, err := newAdapter(t, true).GenerateSemantic(ctx, provider.GenerateCall{
			RequestID: "smoke_responses", ProviderModel: model,
			Request: semantic.GenerateRequest{
				Operation: semantic.OperationGenerate, Mode: semantic.ModePortable,
				Source:         semantic.Source{ProfileID: "smoke", ProfileRevision: 1},
				RequestedModel: model,
				Messages:       []semantic.Message{{Role: semantic.RoleUser, Content: []semantic.Content{{Kind: semantic.ContentText, Text: "Reply with the single word: ok"}}}},
			},
		})
		if err != nil {
			t.Fatalf("responses: %v", err)
		}
	})

	// The catalogue route the connection test and target enumeration both read.
	// Measured as reachable without a credential (401, not 404) on 2026-08-31;
	// this establishes it answers with one.
	t.Run("model catalogue is readable", func(t *testing.T) {
		if err := newAdapter(t, false).Probe(ctx, ""); err != nil {
			t.Fatalf("probe: %v", err)
		}
	})
}
