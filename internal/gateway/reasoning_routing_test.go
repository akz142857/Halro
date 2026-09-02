package gateway

import (
	"testing"

	"github.com/akz142857/Halro/internal/compatibility"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/semantic"
)

// A target that reasons whether or not the request asked is routed away from an
// endpoint that cannot return what it produces, and left alone everywhere else.
//
// This is the filter both production incidents needed and neither had. It is the
// only one here keyed on something the request does not contain: the others ask
// whether a target can serve what was asked for, and this one asks what arrives
// anyway. Everything it drops would otherwise have reached the upstream, been
// billed, and come back as a 502 the caller could not have avoided.
func TestATargetThatReasonsUnaskedIsRoutedAwayOnlyWhereItCannotBeRendered(t *testing.T) {
	request := func(northbound compatibility.NorthboundProfileID) semantic.GenerateRequest {
		return semantic.GenerateRequest{
			Source:   semantic.Source{ProfileID: string(northbound), ProfileRevision: 1},
			Messages: []semantic.Message{{Role: semantic.RoleUser, Content: []semantic.Content{{Kind: semantic.ContentText, Text: "hi"}}}},
		}
	}
	for _, test := range []struct {
		name       string
		northbound compatibility.NorthboundProfileID
		target     provider.Target
		dropped    bool
	}{
		{
			// kimi-k2.7-code has no off switch at all. The Chat wire carries its
			// answer in reasoning_content and this endpoint renders one, so it is
			// served — the residue this filter was written to remove does not
			// include the face that works.
			name:       "always reasons, Chat wire, Chat endpoint",
			northbound: compatibility.ProfileOpenAIChatCompletions,
			target:     provider.Target{ProfileID: domain.ProfileKimiChat, ReasonsUnasked: true},
		},
		{
			name:       "always reasons, Chat wire, Responses endpoint",
			northbound: compatibility.ProfileOpenAIResponses,
			target:     provider.Target{ProfileID: domain.ProfileKimiChat, ReasonsUnasked: true},
			dropped:    true,
		},
		{
			name:       "always reasons, Chat wire, Messages endpoint",
			northbound: compatibility.ProfileAnthropicMessages,
			target:     provider.Target{ProfileID: domain.ProfileKimiChat, ReasonsUnasked: true},
			dropped:    true,
		},
		{
			// The provider decoder refuses first, so no endpoint helps — including
			// the one that can render a reasoning part. Modelling only the
			// endpoint half would have called this one servable.
			name:       "always reasons, Responses wire, Chat endpoint",
			northbound: compatibility.ProfileOpenAIChatCompletions,
			target:     provider.Target{ProfileID: domain.ProfileMiniMaxResponses, ReasonsUnasked: true},
			dropped:    true,
		},
		{
			// Unmarked targets are untouched on every endpoint. A filter that
			// dropped these would take away working deployments for a property
			// they do not have.
			name:       "does not reason unasked, Responses wire, Messages endpoint",
			northbound: compatibility.ProfileAnthropicMessages,
			target:     provider.Target{ProfileID: domain.ProfileMiniMaxResponses},
		},
		{
			name:       "does not reason unasked, Chat wire, Responses endpoint",
			northbound: compatibility.ProfileOpenAIResponses,
			target:     provider.Target{ProfileID: domain.ProfileKimiChat},
		},
	} {
		kept := filterUnrenderableReasoning([]provider.Target{test.target}, request(test.northbound))
		if dropped := len(kept) == 0; dropped != test.dropped {
			t.Errorf("%s: dropped = %v, want %v", test.name, dropped, test.dropped)
		}
	}
}

// The refusal has to say which property cost the route. An operator told only
// "no route supports this" is left bisecting a request that asked for nothing
// unusual — the shape of both incidents.
func TestTheRefusalNamesATargetThatReasonsUnasked(t *testing.T) {
	canonical := semantic.GenerateRequest{
		Source:   semantic.Source{ProfileID: string(compatibility.ProfileOpenAIResponses), ProfileRevision: 1},
		Messages: []semantic.Message{{Role: semantic.RoleUser, Content: []semantic.Content{{Kind: semantic.ContentText, Text: "hi"}}}},
	}
	target := provider.Target{ProfileID: domain.ProfileKimiChat, ReasonsUnasked: true}
	reasons := unservableReasons([]provider.Target{target}, canonical, provider.OperationChat)
	found := false
	for _, reason := range reasons {
		if reason == "target_reasons_unasked" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the refusal did not name the property that cost the route: %v", reasons)
	}
	// And it must not appear when it is not the problem, or it becomes noise on
	// every refusal an operator reads.
	clean := unservableReasons([]provider.Target{{ProfileID: domain.ProfileKimiChat}}, canonical, provider.OperationChat)
	for _, reason := range clean {
		if reason == "target_reasons_unasked" {
			t.Fatalf("a target that does not reason unasked was blamed for it: %v", clean)
		}
	}
}
