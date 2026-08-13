package anthropic

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/provider"
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

// The three batch calls have to land on the paths and methods Anthropic
// documents, and the batch object has to arrive in the northbound shape. A fake
// upstream is the only way to see what actually left the adapter.
func TestBatchCallsAddressTheDocumentedEndpoints(t *testing.T) {
	type seen struct{ method, path string }
	var observed []seen
	adapter := newTestAdapter(t, func(writer http.ResponseWriter, request *http.Request) {
		observed = append(observed, seen{request.Method, request.URL.Path})
		if request.Method == http.MethodPost && request.URL.Path == "/v1/messages/batches" {
			var body struct {
				Requests []struct {
					CustomID string `json:"custom_id"`
				} `json:"requests"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if len(body.Requests) != 1 || body.Requests[0].CustomID != "a" {
				t.Errorf("batch body=%#v", body.Requests)
			}
		}
		writer.Header().Set("content-type", "application/json")
		_, _ = writer.Write([]byte(`{"id":"msgbatch_1","type":"message_batch","processing_status":"ended",` +
			`"request_counts":{"succeeded":1,"errored":0,"canceled":0,"expired":0,"processing":0},` +
			`"results_url":"https://api.anthropic.com/v1/messages/batches/msgbatch_1/results",` +
			`"created_at":"2026-08-13T10:00:00Z","ended_at":"2026-08-13T10:05:00Z","expires_at":"2026-08-14T10:00:00Z"}`))
	})

	created, err := adapter.CreateBatch(context.Background(), provider.BatchCreateCall{
		RequestID: "req", ProviderModel: "claude-provider", CompletionWindow: "24h",
		InputRequests: []byte(`{"custom_id":"a","body":{"model":"public","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}}` + "\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != "msgbatch_1" || created.Status != "completed" {
		t.Fatalf("created=%#v", created)
	}
	// RFC 3339 in, seconds out. Passing the string through would have been read
	// as zero by everything downstream.
	if created.CreatedAt == 0 || created.ExpiresAt == 0 || created.CompletedAt == 0 {
		t.Fatalf("timestamps did not survive: %#v", created)
	}
	if _, err := adapter.GetBatch(context.Background(), "req", "msgbatch_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.CancelBatch(context.Background(), "req", "msgbatch_1"); err != nil {
		t.Fatal(err)
	}
	expected := []seen{
		{http.MethodPost, "/v1/messages/batches"},
		{http.MethodGet, "/v1/messages/batches/msgbatch_1"},
		{http.MethodPost, "/v1/messages/batches/msgbatch_1/cancel"},
	}
	if len(observed) != len(expected) {
		t.Fatalf("observed=%#v", observed)
	}
	for index, want := range expected {
		if observed[index] != want {
			t.Fatalf("call %d was %v, want %v", index, observed[index], want)
		}
	}
}

// Anthropic expires a batch 24 hours after creation and takes no parameter for
// it. Accepting another window would report one the upstream will not honour.
func TestBatchRefusesACompletionWindowAnthropicCannotHonour(t *testing.T) {
	adapter := newTestAdapter(t, func(writer http.ResponseWriter, _ *http.Request) {
		t.Error("a batch with an unhonourable window reached the upstream")
		writer.WriteHeader(http.StatusInternalServerError)
	})
	_, err := adapter.CreateBatch(context.Background(), provider.BatchCreateCall{
		RequestID: "req", ProviderModel: "claude-provider", CompletionWindow: "7d",
		InputRequests: []byte(`{"custom_id":"a","body":{"model":"public","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}}` + "\n"),
	})
	if err == nil || !strings.Contains(err.Error(), "24h") {
		t.Fatalf("err=%v", err)
	}
}

// Results are collected in the OpenAI shape because the endpoint that serves
// them is not provider-specific. A caller who moves a batch between providers
// reads the same file either way.
func TestBatchResultLinesAreRenderedInTheCollectableShape(t *testing.T) {
	succeeded := `{"custom_id":"a","result":{"type":"succeeded","message":{"id":"msg_1","type":"message","role":"assistant",` +
		`"content":[{"type":"text","text":"ok"}],"model":"claude-provider","stop_reason":"end_turn","stop_sequence":null,` +
		`"usage":{"input_tokens":4,"output_tokens":1}}}}`
	rendered, err := renderBatchResultLine("batch_1", []byte(succeeded))
	if err != nil {
		t.Fatal(err)
	}
	var line struct {
		ID       string `json:"id"`
		CustomID string `json:"custom_id"`
		Response *struct {
			StatusCode int             `json:"status_code"`
			Body       json.RawMessage `json:"body"`
		} `json:"response"`
		Error *struct{ Code string } `json:"error"`
	}
	if err := json.Unmarshal(rendered, &line); err != nil {
		t.Fatal(err)
	}
	if line.CustomID != "a" || line.Error != nil || line.Response == nil || line.Response.StatusCode != 200 {
		t.Fatalf("line=%s", rendered)
	}
	var body struct {
		Object  string `json:"object"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(line.Response.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body.Object != "chat.completion" {
		t.Fatalf("body is not a chat completion: %s", line.Response.Body)
	}
	// The same translation a live response takes, so the same vocabulary comes
	// out — end_turn does not reach a caller here either.
	if len(body.Choices) == 0 || body.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish_reason=%#v", body.Choices)
	}
}

// A request that never ran produced no answer. Reporting one with an empty body
// would let a caller process nothing as though it were something.
func TestUnsuccessfulBatchResultsBecomeErrorsRatherThanEmptyAnswers(t *testing.T) {
	for _, test := range []struct{ name, line, code string }{
		{"cancelled", `{"custom_id":"a","result":{"type":"canceled"}}`, "cancelled"},
		{"expired", `{"custom_id":"a","result":{"type":"expired"}}`, "expired"},
		{"errored", `{"custom_id":"a","result":{"type":"errored","error":{"type":"error","error":{"type":"invalid_request_error","message":"nope"}}}}`, "invalid_request_error"},
		{"a kind this build does not know", `{"custom_id":"a","result":{"type":"reticulated"}}`, "unknown_result"},
	} {
		t.Run(test.name, func(t *testing.T) {
			rendered, err := renderBatchResultLine("batch_1", []byte(test.line))
			if err != nil {
				t.Fatal(err)
			}
			var line struct {
				Response *json.RawMessage `json:"response"`
				Error    *struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rendered, &line); err != nil {
				t.Fatal(err)
			}
			if line.Response != nil {
				t.Fatalf("an unsuccessful result carried a response: %s", rendered)
			}
			if line.Error == nil || line.Error.Code != test.code {
				t.Fatalf("line=%s, want code %q", rendered, test.code)
			}
			// The upstream's own sentence is not copied into a file on disk.
			if strings.Contains(string(rendered), "nope") {
				t.Fatalf("an upstream message reached the results file: %s", rendered)
			}
		})
	}
}
