package openai

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
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
	// The same environment every other profile's smoke reads. It was
	// HALRO_MINIMAX_* until this was wired into the matrix runner, which
	// translates HALRO_MATRIX_<PREFIX>_<SUFFIX> into HALRO_SMOKE_<SUFFIX> — so a
	// private naming scheme meant the runner would set variables this test could
	// not see, which is the two-lists-that-cannot-see-each-other shape this
	// adaptation has already been bitten by twice.
	if os.Getenv("HALRO_REAL_PROVIDER_SMOKE") != "1" || os.Getenv("HALRO_SMOKE_PROFILE") != "minimax" {
		t.Skip("set HALRO_REAL_PROVIDER_SMOKE=1 and HALRO_SMOKE_PROFILE=minimax to run the billable MiniMax smoke")
	}
	apiKey := strings.TrimSpace(os.Getenv("HALRO_SMOKE_API_KEY"))
	if apiKey == "" {
		t.Skip("HALRO_SMOKE_API_KEY is required")
	}
	rawEndpoint := strings.TrimSpace(os.Getenv("HALRO_SMOKE_BASE_URL"))
	if rawEndpoint == "" {
		rawEndpoint = "https://api.minimax.io"
	}
	endpoint, err := url.Parse(rawEndpoint)
	if err != nil {
		t.Fatalf("HALRO_SMOKE_BASE_URL is not a URL: %v", err)
	}
	model := strings.TrimSpace(os.Getenv("HALRO_SMOKE_MODEL"))
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

	// The stream subtest above reports only that the adapter refused; this says
	// why, by reading the bytes. It is a raw request rather than an adapter call
	// because what is in question is exactly what the adapter's SSE contract
	// rejects, and an adapter cannot report bytes it declined to accept.
	//
	// The MiniMax stream ended before `data: [DONE]`, which the OpenAI-family
	// stream loop treats as a truncated response — correctly for every other
	// upstream, because a stream that stops early is how a partial generation
	// looks. Whether MiniMax simply never sends the sentinel, spells it
	// differently, or stopped for its own reason decides whether the fix is
	// scoped to this provider or is not a fix at all.
	t.Run("diagnose how the stream terminates", func(t *testing.T) {
		body := `{"model":"` + model + `","messages":[{"role":"user","content":"say ok"}],"stream":true,"stream_options":{"include_usage":true},"thinking":{"type":"disabled"}}`
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(endpoint.String(), "/")+"/v1/chat/completions", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+apiKey)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "text/event-stream")
		response, err := client.Do(request)
		if err != nil {
			t.Fatalf("raw stream: %v", err)
		}
		defer response.Body.Close()
		t.Logf("status=%d content-type=%q", response.StatusCode, response.Header.Get("Content-Type"))
		scanner := bufio.NewScanner(io.LimitReader(response.Body, 1<<20))
		scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
		var lines []string
		for scanner.Scan() {
			lines = append(lines, scanner.Text())
		}
		if err := scanner.Err(); err != nil {
			t.Logf("read error after %d lines: %v", len(lines), err)
		}
		t.Logf("total lines: %d", len(lines))
		from := len(lines) - 8
		if from < 0 {
			from = 0
		}
		for _, line := range lines[from:] {
			// Truncated: a content chunk can be long and only its shape matters.
			if len(line) > 300 {
				line = line[:300] + "…"
			}
			t.Logf("  tail | %s", line)
		}
		sawDone := false
		for _, line := range lines {
			if strings.Contains(line, "[DONE]") {
				sawDone = true
			}
		}
		t.Logf("carries a [DONE] sentinel anywhere: %v", sawDone)
	})

	// Not tested until now, and it was listed as an assertion of this smoke from
	// the start — every subtest above builds the OpenAI adapter, so nothing had
	// ever established what MiniMax does with a member no document mentions.
	// Both JSON capabilities are declared absent on the strength of that silence.
	t.Run("response_format is refused or supported", func(t *testing.T) {
		encoded, err := json.Marshal(map[string]any{
			"model": model, "messages": []map[string]string{{"role": "user", "content": "Reply with the JSON object {\"ok\":true}"}},
			"response_format": map[string]string{"type": "json_object"},
			"thinking":        map[string]string{"type": "disabled"},
		})
		if err != nil {
			t.Fatal(err)
		}
		// Sent raw: the renderer refuses response_format by design, so asking the
		// adapter would only re-read Halro's own decision.
		httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(endpoint.String(), "/")+"/v1/chat/completions", strings.NewReader(string(encoded)))
		if err != nil {
			t.Fatal(err)
		}
		httpRequest.Header.Set("Authorization", "Bearer "+apiKey)
		httpRequest.Header.Set("Content-Type", "application/json")
		response, err := client.Do(httpRequest)
		if err != nil {
			t.Fatalf("raw response_format request: %v", err)
		}
		defer response.Body.Close()
		payload, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		t.Logf("status=%d body=%s", response.StatusCode, strings.TrimSpace(string(payload)))
		t.Log("read this: a 2xx whose content is valid JSON means the capability is real and both profiles under-declare it; " +
			"a 4xx, or a 2xx that ignored the member, means the fail-closed declaration was right")
	})

	// The loudest open risk, and the model it concerns was never the one under
	// test: everything above ran on M3. MiniMax documents the M2.x line as unable
	// to switch thinking off, and Halro sends the disabled switch on every request
	// that did not ask to think. If M2.x refuses it, every M2.x request fails.
	t.Run("M2.x accepts or refuses a disabled thinking switch", func(t *testing.T) {
		m2 := strings.TrimSpace(os.Getenv("HALRO_SMOKE_M2_MODEL"))
		if m2 == "" {
			m2 = "MiniMax-M2.7"
		}
		request := ask(m2)
		request.ReasoningEffort = "none"
		_, err := newAdapter(t, false).Chat(ctx, provider.ChatCall{RequestID: "smoke_m2", ProviderModel: m2, Request: request})
		if err != nil {
			t.Errorf("model %q refused an explicitly disabled thinking switch: %v\n"+
				"this is the failure the plan predicted; the fix is a catalogue-driven distinction, not a model-name prefix", m2, err)
			return
		}
		t.Logf("%q accepted a disabled thinking switch", m2)
	})

	// The catalogue route the connection test and target enumeration both read.
	// Measured as reachable without a credential (401, not 404) on 2026-08-31;
	// this establishes it answers with one.
	t.Run("model catalogue is readable", func(t *testing.T) {
		if err := newAdapter(t, false).Probe(ctx, ""); err != nil {
			t.Fatalf("probe: %v", err)
		}
	})

	// What the probe above deliberately does not read. Its check is "the reply is
	// a JSON object" and nothing more, because asserting a member name Halro only
	// guessed would turn a wrong guess into a failed credential test. So a pass
	// there establishes the route answers a credential — never that what it
	// answers with is a model list.
	//
	// Three decisions rest on the shape and none has evidence:
	//
	//   - Whether MiniMax's Anthropic-faced profile can enumerate at all. It
	//     ships a bundled list today because the Anthropic catalogue decoder
	//     reads Anthropic's shape and this route does not serve it. If the body
	//     is OpenAI-shaped, that profile can read it the way the Chat profile
	//     already does, and the console gets a Refresh button that reaches the
	//     account instead of a list frozen into the binary.
	//   - Whether "the list would credit speech and video models with chat" is
	//     true. It is the stated reason the Anthropic face does not enumerate,
	//     and it was inferred from documentation, not read off a response.
	//   - Whether the Chat profile's enumeration, which does run against this
	//     route, is already returning those models. The same inference says it
	//     must be, and nothing has checked.
	//
	// Raw rather than through the adapter: the adapter decodes into a fixed
	// struct, so it can only ever report the members Halro already expected to
	// find. The point here is the members it did not.
	t.Run("what the model catalogue actually returns", func(t *testing.T) {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(endpoint.String(), "/")+"/v1/models", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+apiKey)
		request.Header.Set("Accept", "application/json")
		response, err := client.Do(request)
		if err != nil {
			t.Fatalf("raw model catalogue: %v", err)
		}
		defer response.Body.Close()
		payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		if err != nil {
			t.Fatalf("read model catalogue: %v", err)
		}
		t.Logf("status=%d content-type=%q bytes=%d",
			response.StatusCode, response.Header.Get("Content-Type"), len(payload))

		// Top-level members first, because that is the question the probe
		// declined to ask: is there a `data` at all, or does MiniMax wrap its
		// list under a name of its own with base_resp beside it.
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(payload, &envelope); err != nil {
			t.Fatalf("the catalogue reply is not a JSON object: %v\nbody: %s", err, truncateForLog(payload, 2000))
		}
		members := make([]string, 0, len(envelope))
		for name := range envelope {
			members = append(members, name)
		}
		sort.Strings(members)
		t.Logf("top-level members: %v", members)

		// Then the OpenAI shape specifically, because that is what the fix would
		// be written against. Zero entries with a 200 is itself an answer: it
		// would mean the route exists and lists nothing this key may reach.
		var openAIShaped struct {
			Object string `json:"object"`
			Data   []struct {
				ID      string `json:"id"`
				Object  string `json:"object"`
				OwnedBy string `json:"owned_by"`
			} `json:"data"`
		}
		if err := json.Unmarshal(payload, &openAIShaped); err != nil {
			t.Logf("not OpenAI-shaped: %v", err)
		} else {
			t.Logf("object=%q data entries=%d", openAIShaped.Object, len(openAIShaped.Data))
			for _, entry := range openAIShaped.Data {
				t.Logf("  model | id=%q object=%q owned_by=%q", entry.ID, entry.Object, entry.OwnedBy)
			}
		}

		// The body verbatim, bounded. Every member above was one Halro thought to
		// look for; this is the only line that can show a member nobody predicted,
		// which is the whole reason the subtest is here. It is a public model
		// list, and the matrix runner scrubs the credential out of captured
		// output either way.
		t.Logf("body: %s", truncateForLog(payload, 8000))
		t.Log("read this: a `data` array of `{id, object, owned_by}` means the Anthropic face can " +
			"enumerate from this route with an OpenAI-shaped decoder; the ids present decide whether " +
			"speech and video models really are in it, which is the claim the no-enumeration decision rests on")
	})
}

// truncateForLog bounds a captured body without hiding that it was cut.
func truncateForLog(payload []byte, limit int) string {
	text := strings.TrimSpace(string(payload))
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "… (truncated, " + strconv.Itoa(len(text)) + " bytes total)"
}
