package modelcatalog

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
)

func TestBuiltinCatalogValidates(t *testing.T) {
	catalog := Builtin()
	if catalog.Len() == 0 {
		t.Fatal("builtin catalog is empty")
	}
	for _, entry := range catalog.Entries() {
		if err := entry.Validate(); err != nil {
			t.Fatalf("entry %q: %v", entry.Key.Model, err)
		}
		if entry.Source != SourceBuiltin || entry.Status != StatusKnown {
			t.Fatalf("entry %q has source=%q status=%q", entry.Key.Model, entry.Source, entry.Status)
		}
		if entry.Evidence()["chat"] == domain.EvidenceVerified {
			t.Fatalf("entry %q claims verified evidence from the builtin catalog", entry.Key.Model)
		}
	}
}

// No entry may widen its profile. A catalog claim is about a model; the profile
// is what Halro's adapter can actually carry, and a Beta profile's limits are
// pinned deliberately.
func TestBuiltinEntriesStayInsideTheirProfileCeiling(t *testing.T) {
	for _, entry := range Builtin().Entries() {
		if !domain.ProviderCapabilitiesSubset(entry.Capabilities, entry.Key.Ceiling()) {
			t.Fatalf("entry %q capabilities=%#v exceed ceiling=%#v", entry.Key.Model, entry.Capabilities, entry.Key.Ceiling())
		}
		if !entry.Capabilities.AnyOperation() {
			t.Fatalf("entry %q declares no core operation", entry.Key.Model)
		}
	}
}

// A pinned-profile entry is evidence only because the profile rejects every
// other model, which is what makes "the model's capabilities are the profile's"
// sound. If a profile stops pinning its model, the entry stops being evidence
// and has to be re-justified rather than inherited.
func TestPinnedProfileEntriesEqualTheirCeiling(t *testing.T) {
	pinned := map[domain.ProviderProfileID]string{
		domain.ProfileBedrockInvokeTitanEmbedV2:  "amazon.titan-embed-text-v2:0",
		domain.ProfileBedrockInvokeTitanImageV2:  "amazon.titan-image-generator-v2:0",
		domain.ProfileBedrockAgentRerankCohere35: "cohere.rerank-v3-5:0",
		domain.ProfileBedrockAsyncNovaReel:       "amazon.nova-reel-v1:0",
	}
	seen := 0
	for _, entry := range Builtin().Entries() {
		model, isPinned := pinned[entry.Key.Profile]
		if !isPinned {
			continue
		}
		seen++
		if entry.Key.Model != model {
			t.Fatalf("profile %q carries model %q, want the pinned %q", entry.Key.Profile, entry.Key.Model, model)
		}
		if !reflect.DeepEqual(entry.Capabilities, entry.Key.Ceiling()) {
			t.Fatalf("entry %q capabilities=%#v ceiling=%#v", entry.Key.Model, entry.Capabilities, entry.Key.Ceiling())
		}
	}
	if seen != len(pinned) {
		t.Fatalf("covered %d pinned profiles, want %d", seen, len(pinned))
	}
}

// §6.2 forbids a prefix promoting an unknown future model to known capabilities,
// so every key must be an exact identifier. A moving alias is admissible only
// where Halro itself pins it, which today is the moderation default.
func TestBuiltinEntriesUseExactModelIdentifiers(t *testing.T) {
	pinnedAliases := map[string]bool{"omni-moderation-latest": true}
	for _, entry := range Builtin().Entries() {
		model := entry.Key.Model
		if strings.ContainsAny(model, "*?") || strings.HasSuffix(model, "-") {
			t.Fatalf("entry %q is not an exact model identifier", model)
		}
		if strings.HasSuffix(model, "-latest") && !pinnedAliases[model] {
			t.Fatalf("entry %q is a moving alias that Halro does not pin", model)
		}
	}
}

func TestUnknownModelGetsNoCapabilities(t *testing.T) {
	key := Key{ProviderType: domain.ProviderOpenAI, Profile: domain.ProfileOpenAIChatEmbeddings, Model: "not-in-catalog"}
	entry, ok := Builtin().Lookup(key)
	if ok {
		t.Fatal("unknown model resolved")
	}
	if entry.Status != StatusUnknown {
		t.Fatalf("status=%q", entry.Status)
	}
	if entry.Capabilities != (domain.ProviderCapabilities{}) {
		t.Fatalf("unknown model inherited capabilities: %#v", entry.Capabilities)
	}
	// The profile ceiling is wide open for OpenAI; the point is that none of it
	// leaks into a model nobody has established anything about.
	if ceiling := key.Ceiling(); !ceiling.Chat {
		t.Fatal("test no longer exercises a permissive ceiling")
	}
}

func TestLookupDoesNotWidenModelNames(t *testing.T) {
	known := Builtin().Entries()[0].Key
	for _, model := range []string{
		known.Model + "-preview",
		strings.TrimSuffix(known.Model, ":0"),
		strings.ToUpper(known.Model),
		" " + known.Model,
	} {
		key := known
		key.Model = model
		if _, ok := Builtin().Lookup(key); ok {
			t.Fatalf("model %q matched the catalog", model)
		}
	}
}

func TestLookupIsScopedByProfile(t *testing.T) {
	known := Builtin().Entries()[0].Key
	other := known
	other.Profile = domain.ProfileBedrockConverseText
	if _, ok := Builtin().Lookup(other); ok {
		t.Fatal("entry leaked across profiles")
	}
}

func TestRegionAgnosticEntryCoversAnyRegion(t *testing.T) {
	known := Builtin().Entries()[0].Key
	if known.Region != "" {
		t.Skip("seed entry is region scoped")
	}
	scoped := known
	scoped.Region = "us-east-1"
	entry, ok := Builtin().Lookup(scoped)
	if !ok {
		t.Fatal("region-agnostic entry did not cover a regional key")
	}
	// The revision must identify the claim, not the question. A caller that
	// knows the region and one that does not have to agree, or every create
	// carrying a revision read from a region-less listing would conflict.
	agnostic, _ := Builtin().Lookup(known)
	if entry.Revision() != agnostic.Revision() {
		t.Fatalf("revision changed with the caller's region: %q vs %q", entry.Revision(), agnostic.Revision())
	}
	if entry.Key.Region != "" {
		t.Fatalf("entry adopted the caller's region: %q", entry.Key.Region)
	}
}

func TestRevisionsAreDeterministicAndPerModel(t *testing.T) {
	first, second := Builtin(), Builtin()
	if first.Revision() != second.Revision() {
		t.Fatal("catalog revision is not deterministic")
	}
	entries := first.Entries()
	seen := map[string]string{}
	for _, entry := range entries {
		if previous, exists := seen[entry.Revision()]; exists {
			t.Fatalf("entries %q and %q share a revision", previous, entry.Key.Model)
		}
		seen[entry.Revision()] = entry.Key.Model
	}

	// Adding an unrelated model must not disturb the revisions of the others.
	// A catalog-wide digest would, which is why conflict detection uses the
	// per-model one.
	extra := Entry{
		Key:          Key{ProviderType: domain.ProviderBedrock, Profile: domain.ProfileBedrockConverseText, Model: "amazon.nova-pro-v1:0"},
		Status:       StatusKnown,
		Source:       SourceBuiltin,
		Capabilities: domain.DefaultProviderCapabilitiesForProfile(domain.ProviderBedrock, domain.ProfileBedrockConverseText),
	}
	grown, err := New(append(entries, extra)...)
	if err != nil {
		t.Fatal(err)
	}
	if grown.Revision() == first.Revision() {
		t.Fatal("catalog revision ignored a new entry")
	}
	for _, entry := range entries {
		resolved, ok := grown.Lookup(entry.Key)
		if !ok {
			t.Fatalf("entry %q disappeared", entry.Key.Model)
		}
		if resolved.Revision() != entry.Revision() {
			t.Fatalf("entry %q revision changed because an unrelated model was added", entry.Key.Model)
		}
	}
}

func TestMergeFailsClosedOnConflict(t *testing.T) {
	key := Key{ProviderType: domain.ProviderOpenAI, Profile: domain.ProfileOpenAIChatEmbeddings, Model: "conflicted"}
	entry := Merge(key,
		Claim{Source: SourceBuiltin, Supported: domain.ProviderCapabilities{Chat: true, Streaming: true, Tools: true}},
		Claim{Source: SourceProviderMetadata, Unsupported: []string{"tools"}},
	)
	if entry.Status != StatusConflicting {
		t.Fatalf("status=%q", entry.Status)
	}
	if entry.Capabilities.Tools {
		t.Fatal("disputed capability stayed on")
	}
	if !entry.Capabilities.Chat || !entry.Capabilities.Streaming {
		t.Fatal("conflict on one capability disabled the others")
	}
	if !slices.Contains(entry.Conflicts, "tools") {
		t.Fatalf("conflicts=%v", entry.Conflicts)
	}
}

func TestMergeSilenceIsNotDenial(t *testing.T) {
	key := Key{ProviderType: domain.ProviderOpenAI, Profile: domain.ProfileOpenAIChatEmbeddings, Model: "quiet"}
	entry := Merge(key,
		Claim{Source: SourceBuiltin, Supported: domain.ProviderCapabilities{Chat: true, Streaming: true, Tools: true}},
		Claim{Source: SourceProviderMetadata, Supported: domain.ProviderCapabilities{Chat: true}},
	)
	if entry.Status != StatusKnown {
		t.Fatalf("status=%q", entry.Status)
	}
	if !entry.Capabilities.Tools {
		t.Fatal("a source that said nothing about tools switched them off")
	}
}

func TestMergeCannotExceedProfileCeiling(t *testing.T) {
	// The Gemini Beta ceiling is pinned deliberately: no tools, no vision.
	key := Key{ProviderType: domain.ProviderGemini, Profile: domain.ProfileGeminiText, Model: "gemini-x"}
	entry := Merge(key, Claim{
		Source:    SourceProviderMetadata,
		Supported: domain.ProviderCapabilities{Chat: true, Streaming: true, Tools: true, Vision: true},
	})
	if entry.Capabilities.Tools || entry.Capabilities.Vision {
		t.Fatalf("upstream metadata widened a Beta profile: %#v", entry.Capabilities)
	}
	if !entry.Capabilities.Chat || !entry.Capabilities.Streaming {
		t.Fatalf("clamp dropped capabilities the profile allows: %#v", entry.Capabilities)
	}
	if entry.Status != StatusPartial {
		t.Fatalf("status=%q, want partial for a non-builtin source", entry.Status)
	}
	if entry.Source.PreselectsCapabilities() {
		t.Fatal("provider metadata may not pre-select capabilities")
	}
	if entry.Evidence()["chat"] != domain.EvidenceDeclared {
		t.Fatalf("evidence=%q", entry.Evidence()["chat"])
	}
}

func TestMergeWithNoClaimsIsUnknown(t *testing.T) {
	key := Key{ProviderType: domain.ProviderOpenAI, Profile: domain.ProfileOpenAIChatEmbeddings, Model: "silent"}
	if entry := Merge(key); entry.Status != StatusUnknown || entry.Capabilities.AnyOperation() {
		t.Fatalf("entry=%#v", entry)
	}
	// A claim that survives the ceiling as nothing is also unknown, not partial.
	entry := Merge(key, Claim{Source: SourceOperatorDeclared})
	if entry.Status != StatusUnknown {
		t.Fatalf("status=%q", entry.Status)
	}
}

func TestMergeTakesTheNarrowerLimit(t *testing.T) {
	key := Key{ProviderType: domain.ProviderBedrock, Profile: domain.ProfileBedrockInvokeTitanEmbedV2, Model: "amazon.titan-embed-text-v2:0"}
	entry := Merge(key,
		Claim{Source: SourceBuiltin, Supported: domain.ProviderCapabilities{Embeddings: true, MaxContextTokens: 8192}},
		Claim{Source: SourceVerifiedProbe, Supported: domain.ProviderCapabilities{Embeddings: true, MaxContextTokens: 4096}},
	)
	if entry.Capabilities.MaxContextTokens != 4096 {
		t.Fatalf("max context tokens=%d", entry.Capabilities.MaxContextTokens)
	}
	// A zero from one source means "declared nothing", not "no limit".
	widened := Merge(key,
		Claim{Source: SourceBuiltin, Supported: domain.ProviderCapabilities{Embeddings: true, MaxContextTokens: 8192}},
		Claim{Source: SourceVerifiedProbe, Supported: domain.ProviderCapabilities{Embeddings: true}},
	)
	if widened.Capabilities.MaxContextTokens != 8192 {
		t.Fatalf("silence erased a declared limit: %d", widened.Capabilities.MaxContextTokens)
	}
}

func TestEntryRejectsCapabilitiesAboveCeiling(t *testing.T) {
	entry := Entry{
		Key:          Key{ProviderType: domain.ProviderGemini, Profile: domain.ProfileGeminiText, Model: "gemini-x"},
		Status:       StatusKnown,
		Source:       SourceBuiltin,
		Capabilities: domain.ProviderCapabilities{Chat: true, Tools: true},
	}
	if err := entry.Validate(); err == nil {
		t.Fatal("entry above the profile ceiling validated")
	}
}

func TestDependenciesAreEnforced(t *testing.T) {
	for name, capabilities := range map[string]domain.ProviderCapabilities{
		"tools without chat":         {Tools: true},
		"vision without chat":        {Vision: true},
		"reasoning without chat":     {Reasoning: true},
		"stream usage without chat":  {StreamUsage: true},
		"output above context limit": {Chat: true, MaxContextTokens: 100, MaxOutputTokens: 200},
	} {
		if err := ValidateDependencies(capabilities); err == nil {
			t.Fatalf("%s validated", name)
		}
	}
	if err := ValidateDependencies(domain.ProviderCapabilities{Chat: true, Streaming: true, StreamUsage: true, Tools: true}); err != nil {
		t.Fatalf("valid capabilities rejected: %v", err)
	}
}

func TestKeyRejectsUnregisteredOrMismatchedProfile(t *testing.T) {
	if err := (Key{ProviderType: domain.ProviderOpenAI, Profile: "made.up.v1", Model: "m"}).Validate(); err == nil {
		t.Fatal("unregistered profile validated")
	}
	if err := (Key{ProviderType: domain.ProviderOpenAI, Profile: domain.ProfileAnthropicMessages, Model: "m"}).Validate(); err == nil {
		t.Fatal("profile from another provider type validated")
	}
}

func TestNewRejectsDuplicateAndEmptyEntries(t *testing.T) {
	entry := Builtin().Entries()[0]
	if _, err := New(entry, entry); err == nil {
		t.Fatal("duplicate entry accepted")
	}
	empty := entry
	empty.Capabilities = domain.ProviderCapabilities{}
	if _, err := New(empty); err == nil {
		t.Fatal("entry without a core operation accepted")
	}
}

// CapabilityNames drives every digest in this package. If a capability is added
// to the domain struct and not here, entries would hash identically whether it
// is on or off, and drift detection would miss the change.
func TestCapabilityNamesCoverTheDomainStruct(t *testing.T) {
	fields := reflect.VisibleFields(reflect.TypeOf(domain.ProviderCapabilities{}))
	var tags []string
	for _, field := range fields {
		tag := strings.Split(field.Tag.Get("json"), ",")[0]
		if tag == "" || tag == "-" {
			t.Fatalf("field %s has no json tag", field.Name)
		}
		tags = append(tags, tag)
	}
	if !slices.Equal(slices.Sorted(slices.Values(tags)), slices.Sorted(slices.Values(CapabilityNames))) {
		t.Fatalf("CapabilityNames=%v does not cover %v", CapabilityNames, tags)
	}
	// Serialization sanity: the names are the wire names, so an entry's
	// capabilities round-trip through the same identifiers the API uses.
	encoded, err := json.Marshal(domain.ProviderCapabilities{Chat: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"chat":true`) {
		t.Fatalf("unexpected encoding %s", encoded)
	}
}

func TestGainedAndLostReportBooleanNamesOnly(t *testing.T) {
	before := domain.ProviderCapabilities{
		Chat: true, Tools: true, MaxContextTokens: 8000, MaxOutputTokens: 2000,
	}
	after := domain.ProviderCapabilities{
		Chat: true, Vision: true, MaxContextTokens: 4000, MaxOutputTokens: 1000,
	}

	lost := LostCapabilities(before, after)
	if !slices.Equal(lost, []string{"tools"}) {
		t.Fatalf("lost=%v", lost)
	}
	gained := GainedCapabilities(before, after)
	if !slices.Equal(gained, []string{"vision"}) {
		t.Fatalf("gained=%v", gained)
	}
	// The limits halved, but a narrowed limit does not remove a candidate from a
	// route; it narrows which requests fit. Reporting it as a lost capability
	// would make every limit edit look like it strands routes.
	for _, name := range append(lost, gained...) {
		if name == "max_context_tokens" || name == "max_output_tokens" {
			t.Fatalf("a token limit was reported as a capability change: %v", name)
		}
	}
}

func TestGainedAndLostFollowCapabilityNameOrder(t *testing.T) {
	// Both names are set, so the result order comes from CapabilityNames rather
	// than from the struct field order or map iteration.
	gained := GainedCapabilities(domain.ProviderCapabilities{},
		domain.ProviderCapabilities{Vision: true, Chat: true})
	if !slices.Equal(gained, []string{"chat", "vision"}) {
		t.Fatalf("gained=%v", gained)
	}
}

func TestHasCapabilityAnswersFalseForLimits(t *testing.T) {
	capabilities := domain.ProviderCapabilities{Tools: true, MaxContextTokens: 8000}
	if !HasCapability(capabilities, "tools") {
		t.Fatal("a held capability answered false")
	}
	if HasCapability(capabilities, "max_context_tokens") {
		t.Fatal("a token limit answered true; it is not something a candidate has or lacks")
	}
}

// Union is the dual of Clamp. It exists only to describe a combined claim when
// reporting what exceeds a ceiling, so it must never be narrower than either side.
func TestUnionIsWiderThanBothSides(t *testing.T) {
	first := domain.ProviderCapabilities{Chat: true, MaxContextTokens: 8000, MaxOutputTokens: 2000}
	second := domain.ProviderCapabilities{Tools: true, MaxContextTokens: 4000, MaxOutputTokens: 1000}

	union := Union(first, second)

	if !union.Chat || !union.Tools {
		t.Fatalf("union dropped a boolean: %+v", union)
	}
	if union.MaxContextTokens != 8000 || union.MaxOutputTokens != 2000 {
		t.Fatalf("union took the narrower limit: %+v", union)
	}
	// An undeclared limit is the widest claim of all, so it must win.
	widest := Union(first, domain.ProviderCapabilities{Chat: true})
	if widest.MaxContextTokens != 0 || widest.MaxOutputTokens != 0 {
		t.Fatalf("an undeclared limit was bounded by the other side: %+v", widest)
	}
}

// Conflict detection compares the per-model digest, and the reason is that the
// catalog-wide one rotates whenever any unrelated model appears. The existing
// coverage only showed a stale revision being caught; this shows the other
// direction, which is what keeps operators from learning to retry through 409s
// until the code means nothing.
func TestAnUnrelatedModelChangingDoesNotMoveThisModelsRevision(t *testing.T) {
	key := Key{ProviderType: domain.ProviderOpenAI, Profile: domain.ProfileOpenAIChatEmbeddings, Model: "subject"}
	subject := Entry{
		Key: key, Status: StatusKnown, Source: SourceBuiltin,
		Capabilities: domain.ProviderCapabilities{Chat: true, Streaming: true},
	}
	unrelated := Entry{
		Key:          Key{ProviderType: domain.ProviderOpenAI, Profile: domain.ProfileOpenAIChatEmbeddings, Model: "other"},
		Status:       StatusKnown,
		Source:       SourceBuiltin,
		Capabilities: domain.ProviderCapabilities{Embeddings: true},
	}

	before, err := New(subject)
	if err != nil {
		t.Fatal(err)
	}
	after, err := New(subject, unrelated)
	if err != nil {
		t.Fatal(err)
	}

	first, ok := before.Lookup(key)
	if !ok {
		t.Fatal("subject missing")
	}
	second, ok := after.Lookup(key)
	if !ok {
		t.Fatal("subject missing after the unrelated model appeared")
	}
	if first.Revision() != second.Revision() {
		t.Fatalf("an unrelated model moved this model's revision: %s -> %s", first.Revision(), second.Revision())
	}
	// And the catalog-wide digest does move, which is exactly why it is not what
	// a create request echoes back.
	if before.Revision() == after.Revision() {
		t.Fatal("the catalog digest did not change, so this proves nothing about why it is unused for conflicts")
	}

	// Changing the subject itself does move its revision.
	widened := subject
	widened.Capabilities.Tools = true
	changed, err := New(widened, unrelated)
	if err != nil {
		t.Fatal(err)
	}
	third, _ := changed.Lookup(key)
	if third.Revision() == second.Revision() {
		t.Fatal("changing what the catalog establishes for a model left its revision alone")
	}
}

// An entry that claims what its profile cannot carry has to fail loudly. It
// used to be trimmed on the way in and then validate cleanly, so the console
// would show a model missing a capability somebody had written down, with
// nothing anywhere saying why.
//
// The failure is also the signal that matters most: a model whose own
// capabilities do not fit inside one profile means Halro's profile split does
// not match a real model, and the answer is a second entry on the profile that
// carries it — not a quieter first one.
func TestAnEntryExceedingItsProfileIsRefusedRatherThanTrimmed(t *testing.T) {
	// The OpenAI chat profile carries no image generation; the media profile does.
	overreaching := builtinEntry(domain.ProviderOpenAI, domain.ProfileOpenAIChatEmbeddings, "chat-and-images",
		domain.ProviderCapabilities{Chat: true, Images: true})
	if !overreaching.Capabilities.Images {
		t.Fatal("the entry was trimmed on construction, so nothing downstream can object to it")
	}

	catalog, err := New(overreaching)
	if err == nil {
		t.Fatal("a catalog accepted an entry claiming what its profile cannot carry")
	}
	if catalog != nil {
		t.Fatal("a rejected catalog was still returned")
	}
	if !strings.Contains(err.Error(), "ceiling") {
		t.Fatalf("the refusal does not say the profile is what refused it: %v", err)
	}

	// The same claim on the profile that does carry it is fine, which is what
	// makes the refusal actionable rather than a dead end.
	onTheRightProfile := builtinEntry(domain.ProviderOpenAI, domain.ProfileOpenAIMediaResources, "chat-and-images",
		domain.ProviderCapabilities{Images: true})
	if _, err := New(onTheRightProfile); err != nil {
		t.Fatalf("the profile that carries images refused it: %v", err)
	}
}
