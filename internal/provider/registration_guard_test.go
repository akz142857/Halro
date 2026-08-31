package provider

import (
	"os"
	"regexp"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
)

// boundPrimitives is every primitive some registered profile actually binds.
func boundPrimitives(t *testing.T) map[Primitive]domain.ProviderProfileID {
	t.Helper()
	bound := map[Primitive]domain.ProviderProfileID{}
	for _, profile := range domain.AllProviderProfiles() {
		manifest, ok := BuiltinProfile(profile.ID)
		if !ok {
			t.Fatalf("%s has no profile manifest", profile.ID)
		}
		for _, binding := range manifest.PrimitiveBindings {
			bound[binding.Primitive] = profile.ID
		}
	}
	return bound
}

// A Primitive constant that nothing binds is dead code, and ProfileManifest.Validate
// cannot see it: it checks that every binding names a declared operation, not
// that every declared primitive is bound.
//
// It matters more than tidiness. A primitive is the answer to "which provider
// API serves this operation", so an unbound one is a claim nobody makes — and
// the way it usually arrives is a platform declaring the constants it expects
// to need and then binding fewer, which leaves the surplus looking supported to
// anyone reading the list.
func TestEveryPrimitiveConstantIsBoundBySomeProfile(t *testing.T) {
	source, err := os.ReadFile("primitive.go")
	if err != nil {
		t.Fatal(err)
	}
	declared := regexp.MustCompile(`(?m)^\s*(Primitive[A-Za-z0-9]+)\s+Primitive\s*=`).FindAllStringSubmatch(string(source), -1)
	if len(declared) < 20 {
		t.Fatalf("found %d primitive constants; the pattern has stopped matching the file", len(declared))
	}
	bound := boundPrimitives(t)
	for _, match := range declared {
		name := match[1]
		found := false
		for primitive := range bound {
			if primitiveConstantName(string(primitive)) == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s is declared and no profile binds it", name)
		}
	}
}

// Every primitive declared as semantic has to be one a profile binds. The map is
// a declaration that changes how Resolve reaches the adapter — a bound primitive
// missing from it silently takes the legacy Chat path instead, which for the
// Responses branch means the adapter refuses after routing has already chosen
// the target.
func TestSemanticGenerationPrimitivesAreAllBound(t *testing.T) {
	bound := boundPrimitives(t)
	for primitive := range semanticGenerationPrimitives {
		if _, ok := bound[primitive]; !ok {
			t.Errorf("%s is declared as a semantic generation primitive and no profile binds it", primitive)
		}
	}
}

// primitiveConstantName maps a primitive's wire value back to the constant name
// that declares it, so the two lists can be compared without a second table.
// The mapping is by value, so it stays correct when a constant is renamed.
func primitiveConstantName(value string) string {
	source, err := os.ReadFile("primitive.go")
	if err != nil {
		return ""
	}
	pattern := regexp.MustCompile(`(?m)^\s*(Primitive[A-Za-z0-9]+)\s+Primitive\s*=\s*"` + regexp.QuoteMeta(value) + `"`)
	if match := pattern.FindStringSubmatch(string(source)); match != nil {
		return match[1]
	}
	return ""
}

// Reasoning is probed through the portable Chat mapping, which refuses the
// signed thinking blocks an Anthropic-wire upstream answers with. Every profile
// on that wire has to be excluded from the probe for the same reason, and the
// exclusion is a switch that a new Anthropic-wire platform can miss — the
// default branch then hands it an OpenAI effort rung and pays for a probe whose
// answer cannot be read.
func TestEveryAnthropicWireProfileIsExcludedFromTheReasoningProbe(t *testing.T) {
	anthropicWire := map[Primitive]bool{
		PrimitiveAnthropicMessages:              true,
		PrimitiveAnthropicMessagesStream:        true,
		PrimitiveBedrockMantleAnthropicMessages: true,
		PrimitiveMiniMaxAnthropicMessages:       true,
	}
	for _, profile := range domain.AllProviderProfiles() {
		manifest, ok := BuiltinProfile(profile.ID)
		if !ok {
			continue
		}
		onAnthropicWire := false
		for _, binding := range manifest.PrimitiveBindings {
			if anthropicWire[binding.Primitive] {
				onAnthropicWire = true
			}
		}
		if !onAnthropicWire {
			continue
		}
		if _, asks := reasoningProbeEffort(profile.ID); asks {
			t.Errorf("%s speaks the Anthropic wire and is still asked to prove reasoning; the probe cannot read the answer", profile.ID)
		}
	}
}
