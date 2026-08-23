package gateway

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/semantic"
)

// unpairedRequirements names the requirements that deliberately have no
// capability to be matched against, with the reason each one does not.
//
// It is written out rather than inferred because "no capability pairs with this"
// and "somebody forgot to pair it" look identical from the outside, and the
// second is the failure this file exists to prevent: an unpaired requirement is
// a request Halro will hand to a target that cannot serve it.
var unpairedRequirements = map[string]string{
	// Answered by the operation, not by a capability: a streaming request is
	// routed with OperationChatStream and filterPrimitiveTargets drops any target
	// whose profile binds no streaming primitive.
	"streaming": "filtered as an operation",
	// These four have no capability anywhere in the dictionary. They are members
	// of a request that some profiles cannot carry, which is a field-level fact,
	// and compatibility.UnsupportedGenerateFields declares them per profile.
	"parallel_tools":      "declared as the parallel_tool_calls field",
	"seed":                "declared as the seed field",
	"multiple_candidates": "declared as the n field",
	"end_user_reference":  "declared as the user field",
}

// A requirement named after a capability has to be paired with it.
//
// The pairing used to live in two hand-written lists whose halves were spelled
// differently on each side, so a new requirement could be added, derived from
// real requests, and never checked against any target — routing would send it
// somewhere that cannot serve it and the caller would meet the provider's
// refusal instead of Halro's. One table fixed the duplication; this fixes the
// omission, which duplication was only one way of causing.
func TestEveryCapabilityShapedRequirementIsPaired(t *testing.T) {
	paired := make(map[string]struct{}, len(capabilityRequirements))
	for _, pairing := range capabilityRequirements {
		if slices.Contains(domain.CapabilityNames(), pairing.name) {
			paired[pairing.name] = struct{}{}
			continue
		}
		t.Errorf("the table pairs %q, which is not a capability the dictionary carries", pairing.name)
	}

	requirements := reflect.TypeOf(semantic.Requirements{})
	for index := 0; index < requirements.NumField(); index++ {
		name := jsonName(requirements.Field(index))
		if _, ok := paired[name]; ok {
			continue
		}
		if reason, ok := unpairedRequirements[name]; ok {
			// A requirement excused here must really have nowhere to be paired,
			// or the excuse is hiding the omission it was written to expose.
			if slices.Contains(domain.CapabilityNames(), name) && reason != "filtered as an operation" {
				t.Errorf("%q is excused as %q but the dictionary does carry it", name, reason)
			}
			continue
		}
		t.Errorf("requirement %q is neither paired with a capability nor listed as deliberately unpaired", name)
	}

	// And nothing may be excused that no longer exists: a stale entry is an
	// excuse waiting to cover a future field that happens to reuse the name.
	for name := range unpairedRequirements {
		if !hasRequirement(requirements, name) {
			t.Errorf("%q is listed as unpaired but is not a requirement", name)
		}
	}
}

func hasRequirement(requirements reflect.Type, name string) bool {
	for index := 0; index < requirements.NumField(); index++ {
		if jsonName(requirements.Field(index)) == name {
			return true
		}
	}
	return false
}

func jsonName(field reflect.StructField) string {
	tag := field.Tag.Get("json")
	if tag == "" {
		return field.Name
	}
	return strings.Split(tag, ",")[0]
}
