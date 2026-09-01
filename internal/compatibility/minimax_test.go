package compatibility

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/openaiapi"
)

func minimaxBaseRequest() openaiapi.ChatCompletionRequest {
	return openaiapi.ChatCompletionRequest{
		Model:    "MiniMax-M3",
		Messages: []openaiapi.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}
}

// An unasked request must still switch thinking off. MiniMax-M3 defaults to
// adaptive, so omitting the member bills every caller for reasoning they never
// requested — the failure DeepSeek was caught by and the reason this renderer
// sends the member in both directions rather than only one.
func TestMiniMaxDisablesThinkingWhenNobodyAsked(t *testing.T) {
	body, err := RenderMiniMaxChatRequest(minimaxBaseRequest())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if body.Thinking == nil || body.Thinking.Type != MiniMaxThinkingOff {
		t.Fatalf("thinking is %+v, want an explicit %q", body.Thinking, MiniMaxThinkingOff)
	}
	if body.ReasoningSplit {
		t.Fatal("reasoning_split was sent with thinking off, where there is nothing to split")
	}
}

func TestMiniMaxCarriesEveryDepthToTheOneOnState(t *testing.T) {
	for _, effort := range []string{"minimal", "low", "medium", "high", "xhigh"} {
		request := minimaxBaseRequest()
		request.ReasoningEffort = effort
		body, err := RenderMiniMaxChatRequest(request)
		if err != nil {
			t.Fatalf("render %q: %v", effort, err)
		}
		if body.Thinking == nil || body.Thinking.Type != MiniMaxThinkingOn {
			t.Fatalf("effort %q produced thinking %+v, want %q", effort, body.Thinking, MiniMaxThinkingOn)
		}
		// Without the split, reasoning comes back inside the answer and the
		// caller reads the model's thinking as part of its reply.
		if !body.ReasoningSplit {
			t.Fatalf("effort %q did not ask for reasoning to be split out", effort)
		}
	}
}

func TestMiniMaxNoneReachesTheOffState(t *testing.T) {
	request := minimaxBaseRequest()
	request.ReasoningEffort = "none"
	body, err := RenderMiniMaxChatRequest(request)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if body.Thinking == nil || body.Thinking.Type != MiniMaxThinkingOff {
		t.Fatalf("thinking is %+v, want %q", body.Thinking, MiniMaxThinkingOff)
	}
}

// The rendered body is the whole point: a member MiniMax ignores must not be on
// the wire, and the top-level reasoning_effort is the one that costs money.
func TestMiniMaxBodyCarriesNoIgnoredMember(t *testing.T) {
	request := minimaxBaseRequest()
	request.ReasoningEffort = "high"
	// response_format=text is the one value that is accepted and still must not
	// reach the wire: it means what omitting the member already means, and only
	// json_object has ever been measured. Sending it would put an undocumented
	// member on a request that gains nothing from it.
	request.ResponseFormat = json.RawMessage(`{"type":"text"}`)
	body, err := RenderMiniMaxChatRequest(request)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Matched as object keys. Bare `"user"` also appears as a message role, which
	// is not what this is looking for.
	for _, member := range []string{`"reasoning_effort":`, `"presence_penalty":`, `"frequency_penalty":`, `"logit_bias":`, `"n":`, `"seed":`, `"stop":`, `"response_format":`, `"user":`, `"max_tokens":`} {
		if strings.Contains(string(encoded), member) {
			t.Fatalf("rendered body carries %s, which MiniMax ignores: %s", member, encoded)
		}
	}
	if !strings.Contains(string(encoded), `"thinking":{"type":"adaptive"}`) {
		t.Fatalf("rendered body does not carry the thinking switch: %s", encoded)
	}
}

func TestMiniMaxRefusesMembersItCannotCarry(t *testing.T) {
	two := 2
	no := false
	seed := int64(7)
	cases := map[string]func(*openaiapi.ChatCompletionRequest){
		"n":    func(r *openaiapi.ChatCompletionRequest) { r.N = &two },
		"seed": func(r *openaiapi.ChatCompletionRequest) { r.Seed = &seed },
		"stop": func(r *openaiapi.ChatCompletionRequest) { r.Stop = json.RawMessage(`["END"]`) },
		"response_format json_schema": func(r *openaiapi.ChatCompletionRequest) {
			r.ResponseFormat = json.RawMessage(`{"type":"json_schema","json_schema":{"name":"x","schema":{}}}`)
		},
		"user":                func(r *openaiapi.ChatCompletionRequest) { r.User = "someone" },
		"parallel_tool_calls": func(r *openaiapi.ChatCompletionRequest) { r.ParallelToolCalls = &no },
	}
	for name, mutate := range cases {
		request := minimaxBaseRequest()
		mutate(&request)
		if _, err := RenderMiniMaxChatRequest(request); err == nil {
			t.Fatalf("%s was accepted; MiniMax ignores it, so it has to be refused rather than sent", name)
		}
	}
}

// parallel_tool_calls=true is what omitting the member already means, so only
// the disable is a loss. Refusing on presence would take MiniMax out of the
// candidate set for every portable Messages request, which always sets the flag.
func TestMiniMaxAcceptsParallelToolsWhenAllowed(t *testing.T) {
	yes := true
	request := minimaxBaseRequest()
	request.ParallelToolCalls = &yes
	if _, err := RenderMiniMaxChatRequest(request); err != nil {
		t.Fatalf("parallel_tool_calls=true was refused: %v", err)
	}
}

// MiniMax has one output bound and it counts reasoning. An answer budget means
// the same thing only while nothing is thinking.
func TestMiniMaxOutputLimitFollowsTheThinkingSwitch(t *testing.T) {
	limit := int64(256)
	request := minimaxBaseRequest()
	request.MaxTokens = &limit
	body, err := RenderMiniMaxChatRequest(request)
	if err != nil {
		t.Fatalf("render with thinking off: %v", err)
	}
	if body.MaxCompletionTokens == nil || *body.MaxCompletionTokens != limit {
		t.Fatalf("max_completion_tokens is %v, want %d", body.MaxCompletionTokens, limit)
	}

	request.ReasoningEffort = "high"
	if _, err := RenderMiniMaxChatRequest(request); err == nil {
		t.Fatal("an answer budget was silently widened into a budget over answer-plus-reasoning")
	}

	request = minimaxBaseRequest()
	request.MaxTokens = &limit
	request.MaxCompletionTokens = &limit
	if _, err := RenderMiniMaxChatRequest(request); err == nil {
		t.Fatal("two output limits were accepted where MiniMax has one member")
	}
}

// json_object was refused until a real account served it. The schema mode was
// never sent, so it stays refused — the silence that turned out to be wrong
// about one half is not evidence about the other.
func TestMiniMaxCarriesJSONObjectAndRefusesASchema(t *testing.T) {
	request := minimaxBaseRequest()
	// A sibling member the facade does not inspect: only response_format.type is
	// validated there, so anything beside it would ride along into a member no
	// MiniMax document describes. What was measured is the bare object, and that
	// is what goes on the wire.
	request.ResponseFormat = json.RawMessage(`{"type":"json_object","unmeasured":true}`)
	body, err := RenderMiniMaxChatRequest(request)
	if err != nil {
		t.Fatalf("json_object was refused: %v", err)
	}
	if string(body.ResponseFormat) != `{"type":"json_object"}` {
		t.Fatalf("json_object reached the wire as %s, want the bare measured object", body.ResponseFormat)
	}
	request.ResponseFormat = json.RawMessage(`{"type":"json_schema","json_schema":{"name":"x","schema":{}}}`)
	if _, err := RenderMiniMaxChatRequest(request); err == nil {
		t.Fatal("a schema was sent to a face that has never been shown to enforce one")
	}
}
