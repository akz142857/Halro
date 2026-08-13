package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
)

func TestRenderBatchRequestsCarriesEachLineThroughTheCanonicalModel(t *testing.T) {
	input := `{"custom_id":"a","method":"POST","url":"/v1/chat/completions","body":{"model":"public","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}}
{"custom_id":"b","body":{"model":"public","max_tokens":8,"messages":[{"role":"user","content":"there"}]}}
`
	rendered, err := renderBatchRequests(domain.ProfileAnthropicMessages, "claude-provider", []byte(input))
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Requests []struct {
			CustomID string          `json:"custom_id"`
			Params   json.RawMessage `json:"params"`
		} `json:"requests"`
	}
	if err := json.Unmarshal(rendered, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Requests) != 2 || body.Requests[0].CustomID != "a" || body.Requests[1].CustomID != "b" {
		t.Fatalf("requests=%#v", body.Requests)
	}
	var params struct {
		Model     string          `json:"model"`
		MaxTokens int64           `json:"max_tokens"`
		Thinking  json.RawMessage `json:"thinking"`
	}
	if err := json.Unmarshal(body.Requests[0].Params, &params); err != nil {
		t.Fatal(err)
	}
	// The upstream model, not the public alias the caller wrote.
	if params.Model != "claude-provider" {
		t.Fatalf("params.model=%q", params.Model)
	}
	if params.MaxTokens != 16 {
		t.Fatalf("params.max_tokens=%d", params.MaxTokens)
	}
	// Rendering goes through the same function a live request uses, so the
	// decisions made there apply here too — including telling a model that
	// thinks by default not to, since a batched result has nowhere to carry a
	// signed thinking block either.
	if string(params.Thinking) != `{"type":"disabled"}` {
		t.Fatalf("params.thinking=%s, want the same treatment a live portable request gets", params.Thinking)
	}
}

// A line the target cannot represent fails the batch and says which line.
// Capability filtering runs when a request is routed, and a batch is routed once
// for many requests, so this is the only place an unrepresentable field is
// caught. Submitting the rest would send a batch the caller did not write.
func TestRenderBatchRequestsRefusesAnUnrepresentableLineByNumber(t *testing.T) {
	input := `{"custom_id":"a","body":{"model":"public","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}}
{"custom_id":"b","body":{"model":"public","max_tokens":8,"n":3,"messages":[{"role":"user","content":"hi"}]}}
`
	_, err := renderBatchRequests(domain.ProfileAnthropicMessages, "claude-provider", []byte(input))
	if err == nil {
		t.Fatal("a line Anthropic cannot carry was accepted")
	}
	if !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("error does not name the offending line: %v", err)
	}
}

func TestRenderBatchRequestsRefusesWhatWouldSubmitSomethingElse(t *testing.T) {
	valid := `{"model":"public","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`
	for _, test := range []struct{ name, line, wants string }{
		{"a missing custom_id", `{"body":` + valid + `}`, "custom_id"},
		{"another endpoint", `{"custom_id":"a","url":"/v1/embeddings","body":` + valid + `}`, "not served"},
		{"a non-POST method", `{"custom_id":"a","method":"GET","body":` + valid + `}`, "not POST"},
		{"a streaming request", `{"custom_id":"a","body":{"model":"public","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}}`, "cannot stream"},
		{"an unknown member", `{"custom_id":"a","surprise":1,"body":` + valid + `}`, "batch input line"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := renderBatchRequests(domain.ProfileAnthropicMessages, "claude-provider", []byte(test.line+"\n")); err == nil {
				t.Fatal("accepted")
			} else if !strings.Contains(err.Error(), test.wants) {
				t.Fatalf("error=%v, want it to mention %q", err, test.wants)
			}
		})
	}
}

// custom_id is how a caller matches a result to the request that produced it.
// Two lines sharing one makes that matching ambiguous, and the ambiguity would
// only be discovered when the results came back.
func TestRenderBatchRequestsRefusesADuplicateCustomID(t *testing.T) {
	line := `{"custom_id":"same","body":{"model":"public","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}}`
	_, err := renderBatchRequests(domain.ProfileAnthropicMessages, "claude-provider", []byte(line+"\n"+line+"\n"))
	if err == nil || !strings.Contains(err.Error(), "more than once") {
		t.Fatalf("err=%v", err)
	}
}

func TestRenderBatchRequestsRefusesAnEmptyInput(t *testing.T) {
	for _, input := range []string{"", "   \n\n"} {
		if _, err := renderBatchRequests(domain.ProfileAnthropicMessages, "claude-provider", []byte(input)); err == nil {
			t.Fatalf("an empty batch was accepted for input %q", input)
		}
	}
}

// `ended` says a batch stopped, not that it succeeded. Reporting a batch whose
// every request errored as completed would tell the caller their work is ready
// when none of it is.
func TestEndedBatchesAreClassifiedByTheirCounts(t *testing.T) {
	for _, test := range []struct {
		name     string
		status   string
		counts   batchRequestCounts
		expected string
	}{
		{"still running", "in_progress", batchRequestCounts{Processing: 3}, "in_progress"},
		{"cancelling", "canceling", batchRequestCounts{Processing: 3}, "cancelling"},
		{"all succeeded", "ended", batchRequestCounts{Succeeded: 3}, "completed"},
		{"partly succeeded", "ended", batchRequestCounts{Succeeded: 2, Errored: 1}, "completed"},
		{"none succeeded", "ended", batchRequestCounts{Errored: 3}, "failed"},
		{"cancelled", "ended", batchRequestCounts{Cancelled: 3}, "cancelled"},
		{"expired", "ended", batchRequestCounts{Expired: 3}, "expired"},
		{"a lifecycle nothing here knows", "reticulating", batchRequestCounts{}, "reticulating"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := decodeBatchProcessingStatus(test.status, test.counts); got != test.expected {
				t.Fatalf("status=%q counts=%#v gave %q, want %q", test.status, test.counts, got, test.expected)
			}
		})
	}
}
