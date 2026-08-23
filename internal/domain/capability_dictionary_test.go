package domain

import (
	"reflect"
	"testing"
)

// The dictionary is the single declaration everything else reads, so the thing
// that can still go wrong is a member added to the struct and not to the table.
// Nothing else would notice: the capability would exist in Go, be storable and
// serializable, and be invisible to every by-name path — the name list, the
// console's checkbox set, the evidence projection, the subset check and the
// difference report all walk the table.
//
// This is the same failure the table was built to remove, one level up, so it is
// held by reflection rather than by another list to keep in step.
func TestTheDictionaryCoversEveryCapabilityMember(t *testing.T) {
	declared := map[string]struct{}{}
	for _, field := range capabilityFields {
		declared[field.Name] = struct{}{}
	}

	structure := reflect.TypeOf(ProviderCapabilities{})
	for index := 0; index < structure.NumField(); index++ {
		member := structure.Field(index)
		if member.Type.Kind() != reflect.Bool {
			// The two limits are bounds a deployment declares, not capabilities a
			// target either has or lacks, and no by-name path carries them.
			continue
		}
		name := member.Tag.Get("json")
		if _, ok := declared[name]; !ok {
			t.Errorf("ProviderCapabilities.%s (%q) is not in the dictionary, so nothing by-name can see it", member.Name, name)
		}
	}

	// And the other direction: a table row whose member was removed writes
	// through a stale accessor.
	for _, field := range capabilityFields {
		if _, ok := structure.FieldByNameFunc(func(candidate string) bool {
			member, _ := structure.FieldByName(candidate)
			return member.Tag.Get("json") == field.Name
		}); !ok {
			t.Errorf("the dictionary carries %q, which ProviderCapabilities no longer has", field.Name)
		}
	}
}

// Every accessor must reach its own member. A copy-paste that points two names
// at one field is invisible to the test above and to the compiler, and it makes
// one capability silently set another.
func TestEveryAccessorReachesItsOwnMember(t *testing.T) {
	for _, field := range capabilityFields {
		var capabilities ProviderCapabilities
		*field.Value(&capabilities) = true
		if enabled := EnabledCapabilityNames(capabilities); len(enabled) != 1 || enabled[0] != field.Name {
			t.Errorf("setting %q turned on %v", field.Name, enabled)
		}
	}
}
