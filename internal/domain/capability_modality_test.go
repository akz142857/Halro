package domain

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

// Every capability the dictionary carries is placed deliberately: it either
// expresses a modality or is named as expressing none. A capability that is in
// neither list is one somebody added without deciding, and the modality view
// would quietly stop describing it.
func TestEveryCapabilityIsEitherModalOrDeclaredNonModal(t *testing.T) {
	// The dictionary lives in modelcatalog, which imports domain, so the field
	// set is read from the struct here rather than imported back.
	value := reflect.TypeOf(ProviderCapabilities{})
	placed := map[string]int{}
	for _, row := range CapabilityModalities() {
		for _, capability := range row.Capabilities {
			placed[capability]++
		}
	}
	for _, capability := range NonModalCapabilities() {
		placed[capability] += 100
	}
	for index := 0; index < value.NumField(); index++ {
		name := strings.Split(value.Field(index).Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		count, ok := placed[name]
		if !ok {
			t.Errorf("capability %q is neither mapped to a modality nor declared non-modal", name)
			continue
		}
		if count >= 100 && count != 100 {
			t.Errorf("capability %q is both modal and declared non-modal", name)
		}
		delete(placed, name)
	}
	for name := range placed {
		t.Errorf("modality mapping names %q, which is not a capability", name)
	}
}

// The rows are a view, not a second dictionary: nothing here may invent a
// direction or repeat a modality on one side, because a renderer walks them in
// order and two rows with the same key would draw the same fact twice.
func TestModalityRowsAreWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, row := range CapabilityModalities() {
		if row.Direction != ModalityInput && row.Direction != ModalityOutput {
			t.Fatalf("row %q has direction %q", row.Modality, row.Direction)
		}
		key := string(row.Direction) + ":" + row.Modality
		if seen[key] {
			t.Fatalf("modality %q is listed twice", key)
		}
		seen[key] = true
		if len(row.Capabilities) == 0 {
			t.Fatalf("modality %q is expressed by nothing", key)
		}
	}
	// A row that can only ever be unknown is not a row. Video and speech-as-input
	// were exactly that and were removed; this pins the decision so that
	// restoring them needs an argument rather than an oversight.
	for _, absent := range []string{"input:video", "output:video", "input:speech", "output:speech"} {
		if seen[absent] {
			t.Fatalf("%q is back without evidence to fill it", absent)
		}
	}
	// The returned slices are copies; a caller must not be able to edit the map.
	rows := CapabilityModalities()
	rows[0].Capabilities[0] = "mutated"
	if slices.Contains(CapabilityModalities()[0].Capabilities, "mutated") {
		t.Fatal("the mapping handed out its backing array")
	}
}
