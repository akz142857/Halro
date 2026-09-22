package compatibility

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/openaiapi"
)

func kimiCodeBaseRequest() openaiapi.ChatCompletionRequest {
	return openaiapi.ChatCompletionRequest{
		Model:    "k3",
		Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("ok")}},
	}
}

// The measurement this profile was withheld for, held as a test: a request that
// says nothing about reasoning must leave carrying the off switch. Omitting the
// member is what the plain OpenAI marshaller did, and on this product that is a
// request billed for reasoning nobody asked for.
func TestKimiCodeSwitchesReasoningOffWhenNobodyAsked(t *testing.T) {
	for _, effort := range []string{"", "none"} {
		request := kimiCodeBaseRequest()
		request.ReasoningEffort = effort
		body, err := RenderKimiCodeChatRequest(request)
		if err != nil {
			t.Fatalf("effort %q: %v", effort, err)
		}
		if body.ReasoningEffort != "none" {
			t.Fatalf("effort %q rendered reasoning_effort %q", effort, body.ReasoningEffort)
		}
		// Written, not omitted: the JSON has to carry it for the upstream to read
		// it, and an omitempty tag on that field would silently undo this.
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(encoded), `"reasoning_effort":"none"`) {
			t.Fatalf("effort %q encoded without the off switch: %s", effort, encoded)
		}
	}
}

func TestKimiCodeCarriesThePublishedDepthsAndRefusesTheRest(t *testing.T) {
	for _, effort := range KimiCodeEffortLevels {
		request := kimiCodeBaseRequest()
		request.ReasoningEffort = effort
		body, err := RenderKimiCodeChatRequest(request)
		if err != nil {
			t.Fatalf("effort %q: %v", effort, err)
		}
		if body.ReasoningEffort != effort {
			t.Fatalf("effort %q rendered as %q", effort, body.ReasoningEffort)
		}
	}
	// The upstream answers 200 to every one of these and reasons at its own
	// depth, so a renderer that forwarded them would bill a caller for a depth
	// they did not name.
	for _, effort := range []string{"minimal", "medium", "xhigh", "not-a-depth"} {
		request := kimiCodeBaseRequest()
		request.ReasoningEffort = effort
		if _, err := RenderKimiCodeChatRequest(request); err == nil {
			t.Fatalf("effort %q was rendered rather than refused", effort)
		}
	}
}

func TestKimiCodeRefusesTheMembersTheUpstreamPinsOrIgnores(t *testing.T) {
	value := 0.5
	count := 2
	seed := int64(42)
	parallel := false
	for name, mutate := range map[string]func(*openaiapi.ChatCompletionRequest){
		"temperature":         func(r *openaiapi.ChatCompletionRequest) { r.Temperature = &value },
		"top_p":               func(r *openaiapi.ChatCompletionRequest) { r.TopP = &value },
		"n":                   func(r *openaiapi.ChatCompletionRequest) { r.N = &count },
		"seed":                func(r *openaiapi.ChatCompletionRequest) { r.Seed = &seed },
		"user":                func(r *openaiapi.ChatCompletionRequest) { r.User = "someone" },
		"parallel_tool_calls": func(r *openaiapi.ChatCompletionRequest) { r.ParallelToolCalls = &parallel },
		"response_format": func(r *openaiapi.ChatCompletionRequest) {
			r.ResponseFormat = json.RawMessage(`{"type":"json_object"}`)
		},
	} {
		request := kimiCodeBaseRequest()
		mutate(&request)
		if _, err := RenderKimiCodeChatRequest(request); err == nil {
			t.Errorf("%s reached the upstream", name)
		}
	}
}

// The two output bounds are the same tokens only while nothing is thinking, and
// this renderer is what makes that the common case.
func TestKimiCodeCarriesAnAnswerBoundOnlyWhileNothingThinks(t *testing.T) {
	limit := int64(64)
	request := kimiCodeBaseRequest()
	request.MaxTokens = &limit
	body, err := RenderKimiCodeChatRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if body.MaxCompletionTokens == nil || *body.MaxCompletionTokens != limit {
		t.Fatalf("max_completion_tokens = %v", body.MaxCompletionTokens)
	}
	request.ReasoningEffort = "high"
	if _, err := RenderKimiCodeChatRequest(request); err == nil {
		t.Fatal("an answer-only bound was carried into a request that asked for depth")
	}
	request.ReasoningEffort = ""
	request.MaxCompletionTokens = &limit
	if _, err := RenderKimiCodeChatRequest(request); err == nil {
		t.Fatal("two output bounds were accepted")
	}
}

func TestKimiCodeToolChoiceConflictsOnlyWithANamedFunction(t *testing.T) {
	request := kimiCodeBaseRequest()
	request.ReasoningEffort = "high"
	request.ToolChoice = json.RawMessage(`{"type":"function","function":{"name":"get_weather"}}`)
	if _, err := RenderKimiCodeChatRequest(request); err == nil {
		t.Fatal("a named tool call with a depth was rendered")
	}
	// Measured 200 with a tool call and a reasoning span in one response, so
	// refusing it would route away a request that works.
	request.ToolChoice = json.RawMessage(`"required"`)
	if _, err := RenderKimiCodeChatRequest(request); err != nil {
		t.Fatalf("required with a depth was refused: %v", err)
	}
}

func TestKimiCodeAppliesThePublishedStopBounds(t *testing.T) {
	request := kimiCodeBaseRequest()
	request.Stop = json.RawMessage(`["a","b","c","d","e","f"]`)
	if _, err := RenderKimiCodeChatRequest(request); err == nil {
		t.Fatal("six stop sequences were rendered")
	}
	request.Stop = json.RawMessage(`["0123456789012345678901234567890123456789"]`)
	if _, err := RenderKimiCodeChatRequest(request); err == nil {
		t.Fatal("an over-long stop sequence was rendered")
	}
	request.Stop = json.RawMessage(`["THREE"]`)
	body, err := RenderKimiCodeChatRequest(request)
	if err != nil {
		t.Fatalf("a stop sequence within the bounds was refused: %v", err)
	}
	if string(body.Stop) != `["THREE"]` {
		t.Fatalf("stop = %s", body.Stop)
	}
}

// The Chat face shares OpenAI's wire shape and not its member list, which is the
// whole reason this renderer exists rather than the default marshaller.
func TestKimiCodeSendsNeitherPinnedSamplingMemberOnAnOrdinaryRequest(t *testing.T) {
	request := kimiCodeBaseRequest()
	request.Stream = true
	request.StreamOptions = &openaiapi.StreamOptions{IncludeUsage: true}
	body, err := RenderKimiCodeChatRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	// Spelled with the colon, because "user" is also a message role and the
	// first version of this test caught the request's own message instead.
	for _, member := range []string{`"temperature":`, `"top_p":`, `"n":`, `"seed":`, `"user":`, `"response_format":`, `"parallel_tool_calls":`} {
		if strings.Contains(string(encoded), member) {
			t.Fatalf("%s reached the wire: %s", member, encoded)
		}
	}
	if !strings.Contains(string(encoded), `"include_usage":true`) {
		t.Fatalf("stream usage was dropped: %s", encoded)
	}
}
