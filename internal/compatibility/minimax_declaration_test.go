package compatibility_test

import (
	"slices"
	"testing"

	"github.com/akz142857/Halro/internal/compatibility"
	openaiwire "github.com/akz142857/Halro/internal/compatibility/openai"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/semantic"
)

// minimaxRendererFields are the members RenderMiniMaxChatRequest decides on.
// The reverse assertion is scoped to them for the reason DeepSeek's is:
// messages[].content[].is_error is declared and the renderer never sees it,
// because that loss is in the shape of an OpenAI tool message rather than in a
// member the renderer inspects.
var minimaxRendererFields = []string{
	"n", "seed", "stop", "parallel_tool_calls", "user", "response_format", "max_tokens",
}

func minimaxBaseSemanticRequest() semantic.GenerateRequest {
	request := semantic.GenerateRequest{
		Operation: semantic.OperationGenerate, Mode: semantic.ModePortable,
		Source:         semantic.Source{ProfileID: string(compatibility.ProfileOpenAIChatCompletions), ProfileRevision: 1},
		RequestedModel: "minimax",
		Messages:       []semantic.Message{{Role: semantic.RoleUser, Content: []semantic.Content{{Kind: semantic.ContentText, Text: "hi"}}}},
	}
	request.Requirements = request.DeriveRequirements()
	return request
}

// TestMiniMaxRendersEveryRequestItsDeclarationAdmits is the guard MiniMax was
// missing, and its absence is why the output-limit refusals reached provider I/O.
//
// The renderer and the field rules describe the same profile from two sides: the
// rules decide whether a request may be routed here at all, the renderer decides
// what goes on the wire once it has been. When only one of them knows a member is
// refused, the router admits the request, the budget is reserved, and the refusal
// arrives as a bad request that cannot fall back. The manifest coverage guard
// cannot see it either — it holds the declaration to the rules, and both were
// silent about the same member.
//
// DeepSeek has carried this test since it was written. MiniMax reuses DeepSeek's
// shape rather than its content: the two upstreams have the mirror image of each
// other's single output bound, so the member each one refuses is the other's.
func TestMiniMaxRendersEveryRequestItsDeclarationAdmits(t *testing.T) {
	seed, completion, visible, candidates := int64(7), int64(64), int64(64), 2
	serial, parallel := false, true
	for _, mutate := range []func(*semantic.GenerateRequest){
		func(r *semantic.GenerateRequest) {},
		func(r *semantic.GenerateRequest) { r.Candidates = &candidates },
		func(r *semantic.GenerateRequest) { r.Seed = &seed },
		func(r *semantic.GenerateRequest) { r.ParallelTools = &serial },
		func(r *semantic.GenerateRequest) { r.ParallelTools = &parallel },
		func(r *semantic.GenerateRequest) { r.EndUserRef = "customer" },
		func(r *semantic.GenerateRequest) { r.Stop = []string{"END"} },
		// The output bounds, on every combination that matters. MiniMax has one
		// bound and it counts reasoning, so the answer-only member is the same
		// quantity only while nothing is thinking.
		func(r *semantic.GenerateRequest) { r.CompletionTokenLimit = &completion },
		func(r *semantic.GenerateRequest) { r.VisibleOutputTokenLimit = &visible },
		func(r *semantic.GenerateRequest) {
			r.VisibleOutputTokenLimit = &visible
			r.ReasoningEffort = "none"
		},
		func(r *semantic.GenerateRequest) {
			r.VisibleOutputTokenLimit = &visible
			r.ReasoningEffort = "high"
		},
		func(r *semantic.GenerateRequest) {
			r.CompletionTokenLimit = &completion
			r.ReasoningEffort = "high"
		},
		func(r *semantic.GenerateRequest) {
			r.VisibleOutputTokenLimit = &visible
			r.CompletionTokenLimit = &completion
		},
		func(r *semantic.GenerateRequest) { r.ReasoningEffort = "none" },
		func(r *semantic.GenerateRequest) { r.ReasoningEffort = "minimal" },
		func(r *semantic.GenerateRequest) { r.ReasoningEffort = "low" },
		func(r *semantic.GenerateRequest) { r.ReasoningEffort = "medium" },
		func(r *semantic.GenerateRequest) { r.ReasoningEffort = "high" },
		func(r *semantic.GenerateRequest) { r.ReasoningEffort = "xhigh" },
		func(r *semantic.GenerateRequest) {
			r.OutputFormat = &semantic.OutputFormat{Kind: semantic.OutputJSONObject}
		},
		func(r *semantic.GenerateRequest) {
			r.OutputFormat = &semantic.OutputFormat{
				Kind: semantic.OutputJSONSchema, Name: "reply",
				Schema: []byte(`{"type":"object"}`), Strict: true,
			}
		},
	} {
		request := minimaxBaseSemanticRequest()
		mutate(&request)
		request.Requirements = request.DeriveRequirements()
		unsupported := compatibility.UnsupportedGenerateFields(domain.ProfileMiniMaxChat, request)
		wire, err := openaiwire.RenderGenerateRequest(request, "MiniMax-M3")
		if err != nil {
			t.Fatalf("portable render failed for %#v: %v", request, err)
		}
		_, renderErr := compatibility.RenderMiniMaxChatRequest(wire)
		if len(unsupported) == 0 && renderErr != nil {
			t.Fatalf("declaration admitted a request the wire cannot carry (%v): %#v", renderErr, wire)
		}
		declaredByRenderer := slices.DeleteFunc(slices.Clone(unsupported), func(field string) bool {
			return !slices.Contains(minimaxRendererFields, field)
		})
		if len(declaredByRenderer) > 0 && renderErr == nil {
			t.Fatalf("declaration rejected %v for a request the wire carries fine: %#v", declaredByRenderer, wire)
		}
	}
}

// TestMiniMaxChatIsRoutedAwayFromAnEffortBearingMessagesRequest states the case
// the field rule exists for, in the shape an operator meets it.
//
// Anthropic Messages requires max_tokens, so DecodePortable always produces the
// answer-only bound — every effort-bearing Messages request routed to this
// profile carried both. Before the rule, the capability filter returned nothing,
// the router kept MiniMax Chat as a candidate, the reservation was written, and
// the renderer then refused with a bad request that is not retryable and so
// cannot reach a second target. The published manifest meanwhile declared the
// effort translation supported on this endpoint.
func TestMiniMaxChatIsRoutedAwayFromAnEffortBearingMessagesRequest(t *testing.T) {
	visible := int64(64)
	request := minimaxBaseSemanticRequest()
	request.VisibleOutputTokenLimit = &visible
	request.ReasoningEffort = "high"
	request.Requirements = request.DeriveRequirements()
	if got := compatibility.UnsupportedGenerateFields(domain.ProfileMiniMaxChat, request); !slices.Contains(got, "max_tokens") {
		t.Fatalf("an effort-bearing request that bounds the answer was admitted: %v", got)
	}
	// The same request without the effort is exactly what this profile serves,
	// and the rule must not take it away: that would route MiniMax Chat off
	// every portable Messages request there is.
	request.ReasoningEffort = ""
	request.Requirements = request.DeriveRequirements()
	if got := compatibility.UnsupportedGenerateFields(domain.ProfileMiniMaxChat, request); slices.Contains(got, "max_tokens") {
		t.Fatalf("a request that only bounds the answer was refused: %v", got)
	}
}
