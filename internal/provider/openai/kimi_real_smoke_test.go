package openai

import (
	"context"
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

// TestRealKimiSmoke is opt-in because it contacts the metered Kimi platform and
// is billable. It logs only stable classes and counts, never credentials, the
// probe input, or a response body.
//
//	HALRO_REAL_PROVIDER_SMOKE=1
//	HALRO_SMOKE_PROFILE=kimi
//	HALRO_SMOKE_BASE_URL=https://api.moonshot.cn   (or https://api.moonshot.ai)
//	HALRO_SMOKE_API_KEY=...
//	HALRO_SMOKE_MODEL=...    optional; without it the test enumerates and says
//	                         which identifiers the account actually serves
//
// Why this exists. `internal/compatibility/kimi.go` carries a set of facts
// measured against a real mainland account on 2026-09-01, and nothing has
// re-checked any of them since. Two of them are the reason that file exists at
// all, and one of those two costs money when it drifts:
//
//   - Kimi's request schema has no temperature, top_p or n. Halro renders them
//     away, and a 200 here is what says the rendered shape is still accepted.
//     Without the renderer the request fails upstream *after* the budget is
//     reserved, on a request that cannot fall back because a bad request is not
//     retryable.
//   - The reasoning switch is spelled differently per model family, and the
//     wrong spelling answers 200 and does nothing. An ignored member is worse
//     than a refused one: a caller who explicitly declined reasoning is billed
//     for it on every request, silently, for as long as nobody looks.
//
// So the assertion below is about the *effect* of the switch, never its
// spelling — choosing the spelling is Halro's job, and a test that pinned the
// spelling would pass while the caller was being billed.
//
// What it deliberately does not attempt: the 402 entitlement path. That belongs
// to the Kimi Code subscription product, not this one, and
// docs/verification/kimi-code-subscription-evidence.md already records why no
// smoke can reach it — a healthy subscription cannot produce quota exhaustion
// on demand.
func TestRealKimiSmoke(t *testing.T) {
	if os.Getenv("HALRO_REAL_PROVIDER_SMOKE") != "1" || os.Getenv("HALRO_SMOKE_PROFILE") != "kimi" {
		t.Skip("set HALRO_REAL_PROVIDER_SMOKE=1 and HALRO_SMOKE_PROFILE=kimi to run the billable metered-Kimi smoke")
	}
	apiKey := strings.TrimSpace(os.Getenv("HALRO_SMOKE_API_KEY"))
	if apiKey == "" {
		t.Fatal("HALRO_SMOKE_API_KEY is required")
	}
	rawEndpoint := strings.TrimSpace(os.Getenv("HALRO_SMOKE_BASE_URL"))
	if rawEndpoint == "" {
		t.Fatal("HALRO_SMOKE_BASE_URL is required: api.moonshot.cn and api.moonshot.ai " +
			"are different accounts on keys that are not interchangeable, so the host is " +
			"what says which one this run measured")
	}
	endpoint, err := url.Parse(rawEndpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" {
		t.Fatal("HALRO_SMOKE_BASE_URL must be an absolute HTTPS URL")
	}

	adapter, err := NewWithOptions(Options{
		Endpoint: endpoint, APIKey: []byte(apiKey),
		Client:       &http.Client{Timeout: 45 * time.Second},
		ProviderType: string(domain.ProviderKimi),
		Capabilities: provider.Capabilities{Chat: true, Streaming: true, StreamUsage: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	model := resolveKimiSmokeModel(ctx, t, adapter)

	// 1. The rendered shape is still accepted. Halro strips members Kimi's
	//    schema does not carry; this call is what says the result still loads.
	base := openaiapi.ChatCompletionRequest{
		Model:     model,
		Messages:  []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("Reply with OK.")}},
		MaxTokens: int64Pointer(16),
	}
	response, err := kimiChatWithBackoff(ctx, t, adapter, provider.ChatCall{
		RequestID: "smoke_kimi_nonstream", ProviderModel: model, Request: base,
	})
	if err != nil {
		t.Fatalf("non-stream chat failed, which is how a rendered member Kimi no longer accepts shows up: %s",
			smokeErrorClass(err))
	}
	if response.ID == "" || len(response.Choices) == 0 {
		t.Fatal("non-stream chat returned an incomplete envelope")
	}

	// 2. Streaming, because the semantic decoder is a separate path.
	streamRequest := base
	streamRequest.Stream = true
	chunks := 0
	if _, err := adapter.ChatStream(ctx, provider.ChatCall{
		RequestID: "smoke_kimi_stream", ProviderModel: model, Request: streamRequest,
	}, func(chunk semantic.Event) error {
		if len(chunk.Outputs) > 0 {
			chunks++
		}
		return nil
	}); err != nil {
		t.Fatalf("stream chat failed: %s", smokeErrorClass(err))
	}
	if chunks == 0 {
		t.Fatal("stream chat returned no semantic chunks")
	}

	runRealKimiReasoningContract(ctx, t, adapter, model)
}

// resolveKimiSmokeModel takes the model from the upstream's own enumeration
// rather than from a guess.
//
// "Halro does not enumerate this" is a fact about Halro, not the upstream's
// answer — the rule this repository learned from MiniMax, whose bundled model
// list shipped on those grounds while the upstream served a list all along. So
// a configured identifier is checked against what the account actually serves,
// and an unconfigured one is answered with the real list instead of a default
// that might be retired, might be priced differently, or might not exist on
// this account at all.
func resolveKimiSmokeModel(ctx context.Context, t *testing.T, adapter *Adapter) string {
	t.Helper()
	targets, err := adapter.ListInvocationTargets(ctx, domain.TargetQuery{})
	if err != nil {
		t.Fatalf("model enumeration failed, so nothing here can say which identifiers this "+
			"account serves: %s", smokeErrorClass(err))
	}
	served := make([]string, 0, len(targets))
	for _, target := range targets {
		served = append(served, target.TargetID)
	}
	if len(served) == 0 {
		t.Fatal("the enumeration route answered with no models")
	}
	t.Logf("kimi enumeration: %d models served: %s", len(served), strings.Join(served, " "))

	configured := strings.TrimSpace(os.Getenv("HALRO_SMOKE_MODEL"))
	if configured == "" {
		t.Fatalf("HALRO_SMOKE_MODEL is not set. This account serves: %s\n"+
			"Name one rather than letting the test pick: the identifiers differ in price and "+
			"in which reasoning spelling they read, and choosing by the shape of a name is the "+
			"inference the model catalogue exists to prevent.", strings.Join(served, " "))
	}
	for _, id := range served {
		if id == configured {
			return configured
		}
	}
	t.Fatalf("HALRO_SMOKE_MODEL %q is not in what this account serves: %s\n"+
		"A retired identifier answers 404 resource_not_found_error, which would read as a "+
		"transport failure rather than as a model that is gone.", configured, strings.Join(served, " "))
	return ""
}

// kimiChatWithBackoff separates a transient refusal from a contract one.
//
// The first run of this smoke hit `429 rate_limit_reached_error` on its third
// call and reported it as a model that refuses to be quietened — a rate limit
// dressed up as a fact about Kimi's schema. A transient error is never evidence
// about a contract, the same rule the capability matrix already follows when it
// refuses to record a transient failure as `unsupported`.
//
// So a rate limit is waited out, a bounded number of times, and if it persists
// the test says the account is too tightly limited to measure rather than
// inventing a finding.
func kimiChatWithBackoff(ctx context.Context, t *testing.T, adapter *Adapter, call provider.ChatCall) (openaiapi.ChatCompletionResponse, error) {
	t.Helper()
	var lastErr error
	for attempt := range 4 {
		if attempt > 0 {
			pause := time.Duration(attempt*attempt) * 5 * time.Second
			t.Logf("%s: rate limited, waiting %s before retrying", call.RequestID, pause)
			select {
			case <-ctx.Done():
				return openaiapi.ChatCompletionResponse{}, ctx.Err()
			case <-time.After(pause):
			}
		}
		response, err := adapter.Chat(ctx, call)
		if err == nil {
			return response, nil
		}
		lastErr = err
		var classified *provider.Error
		if !errorsAsProvider(err, &classified) || classified.Class != provider.ErrorRateLimit {
			return openaiapi.ChatCompletionResponse{}, err
		}
	}
	t.Skipf("%s stayed rate limited across four attempts (%s). This account is too tightly "+
		"limited to measure the reasoning contract; nothing here is evidence about Kimi's "+
		"schema either way.", call.RequestID, smokeErrorClass(lastErr))
	return openaiapi.ChatCompletionResponse{}, lastErr
}

// runRealKimiReasoningContract is the assertion that costs money when it drifts.
//
// Kimi spells the reasoning switch differently depending on the model, its own
// OpenAPI document discriminates on the exact `model` string, and the wrong
// spelling is accepted and ignored rather than refused. Halro picks the
// spelling; this checks only that the request the caller made was honoured,
// because that is the part an operator's bill depends on.
//
// It needs a positive control and the first version did not have one. That
// version asked for the default depth and then for none, saw zero reasoning
// tokens both times, and passed — but zero-then-zero is exactly what a switch
// that does nothing looks like on a model that never reasons. A pass has to
// mean the switch was observed working, so the deep rung is asked for first and
// the off-switch assertion is only made once reasoning has actually been seen.
//
// The zero at "default" was not upstream drift, which is worth writing down
// because it read like it. RenderKimiChatRequest switches reasoning *off* on a
// request that asks for nothing, deliberately, so an unasked request showing no
// reasoning is Halro doing what it says rather than Kimi changing its default.
// That is also why this asks for a depth explicitly instead of inferring one.
func runRealKimiReasoningContract(ctx context.Context, t *testing.T, adapter *Adapter, model string) {
	t.Helper()
	// max_completion_tokens, not max_tokens, and the difference is the renderer's
	// rule rather than a style choice: Kimi has one output bound and it counts
	// reasoning, so an answer-only max_tokens is a different quantity once
	// anything is thinking. RenderKimiChatRequest refuses that combination
	// locally — which is what the first version of this test tripped over, and
	// it was right to.
	ask := openaiapi.ChatCompletionRequest{
		Model:               model,
		Messages:            []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("What is 17 times 23? Reply with the number.")}},
		MaxCompletionTokens: int64Pointer(512),
	}

	// The positive control. `high` is a rung both ladders have, so it is
	// reachable from a portable request on any Kimi model that reasons at all.
	deep := ask
	deep.ReasoningEffort = "high"
	loud, err := kimiChatWithBackoff(ctx, t, adapter, provider.ChatCall{
		RequestID: "smoke_kimi_reasoning_high", ProviderModel: model, Request: deep,
	})
	if err != nil {
		t.Fatalf("deep-reasoning chat was refused: %s\n"+
			"`invalid thinking: only type=enabled is allowed for this model` is one known "+
			"answer here and is a fact worth recording; any other refusal is not. Read the "+
			"class before widening this test.", smokeErrorClass(err))
	}
	loudTokens, loudContent := kimiReasoningObserved(loud)
	t.Logf("kimi depth=high: reasoning_tokens=%d reasoning_content_present=%t", loudTokens, loudContent)

	if loudTokens == 0 && !loudContent {
		// Not a failure and not a pass. Three things produce this and they are
		// not distinguishable from here: the model does not reason, the
		// spelling Halro sent was accepted and ignored, or the counter is
		// published under a member the decoder does not read. Saying "the off
		// switch works" on this evidence would be the vacuous pass this test
		// was rewritten to stop reporting.
		t.Skipf("%s produced no reasoning even at depth=high, so the off switch cannot be "+
			"measured on it. Either this model does not reason, or the spelling was accepted "+
			"and ignored, or the reasoning counter is not the member being read — worth a "+
			"finding, not a green test. Try a model this account serves that does reason.", model)
	}

	// Now the assertion means something: reasoning has been observed on this
	// model, so zero after declining it is the switch being honoured rather
	// than a model that never reasoned.
	off := ask
	off.ReasoningEffort = "none"
	quiet, err := kimiChatWithBackoff(ctx, t, adapter, provider.ChatCall{
		RequestID: "smoke_kimi_reasoning_off", ProviderModel: model, Request: off,
	})
	if err != nil {
		t.Fatalf("reasoning-disabled chat was refused after depth=high was accepted: %s",
			smokeErrorClass(err))
	}
	quietTokens, quietContent := kimiReasoningObserved(quiet)
	t.Logf("kimi depth=none: reasoning_tokens=%d reasoning_content_present=%t", quietTokens, quietContent)

	if quietTokens != 0 || quietContent {
		t.Fatalf("reasoning stayed on after being declined: %d reasoning tokens billed, "+
			"reasoning_content_present=%t, on a model that reasoned %d tokens at depth=high. "+
			"Either the spelling Halro sends is no longer the one this model reads, or the "+
			"model stopped accepting an off switch — both bill the caller for depth they "+
			"refused", quietTokens, quietContent, loudTokens)
	}
}

// kimiReasoningObserved reads both signals, because a model can publish one
// without the other and either is enough to say reasoning happened.
func kimiReasoningObserved(response openaiapi.ChatCompletionResponse) (int64, bool) {
	tokens := int64(0)
	if response.Usage != nil {
		tokens = response.Usage.ReasoningTokens()
	}
	content := len(response.Choices) > 0 && response.Choices[0].Message != nil &&
		response.Choices[0].Message.ReasoningContent != ""
	return tokens, content
}
