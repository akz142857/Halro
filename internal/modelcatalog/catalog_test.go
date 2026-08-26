package modelcatalog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

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

// DeepSeek's direct profile lists deepseek-v4-flash and deepseek-v4-pro, and
// the two names the catalog used to carry no longer appear in its API
// documentation. They are replaced rather than kept beside the new pair: a
// retired identifier left in the catalog is a pre-checked capability claim for a
// model the upstream will refuse, and its context and output ceilings — 131072
// and 8192 against the current 1M and 384K — are the two numbers Token Guard,
// budget reservation and max_tokens truncation all read.
//
// The compatible profile is a different question and deliberately untouched: it
// serves third-party servers that may still host either name, and it claims
// nothing beyond chat and streaming for any of them.
func TestRetiredDeepSeekModelNamesAreGoneFromTheDirectProfile(t *testing.T) {
	for _, model := range []string{"deepseek-chat", "deepseek-reasoner"} {
		key := Key{ProviderType: domain.ProviderDeepSeek, Profile: domain.ProfileDeepSeekChat, Model: model}
		if entry, ok := Builtin().Lookup(key); ok {
			t.Fatalf("retired model %q still carries built-in capabilities: %#v", model, entry.Capabilities)
		}
	}
	for _, model := range []string{"deepseek-v4-flash", "deepseek-v4-pro"} {
		key := Key{ProviderType: domain.ProviderDeepSeek, Profile: domain.ProfileDeepSeekChat, Model: model}
		entry, ok := Builtin().Lookup(key)
		if !ok {
			t.Fatalf("model %q is not covered", model)
		}
		if entry.Capabilities.MaxContextTokens != 1_000_000 || entry.Capabilities.MaxOutputTokens != 384_000 {
			t.Fatalf("model %q carries stale limits: %#v", model, entry.Capabilities)
		}
		if !entry.Capabilities.Reasoning {
			t.Fatalf("model %q lost reasoning, which is now a switch on both models: %#v", model, entry.Capabilities)
		}
	}
}

func TestLookupDoesNotWidenModelNames(t *testing.T) {
	known := Builtin().Entries()[0].Key
	models := []string{
		known.Model + "-preview",
		strings.ToUpper(known.Model),
		" " + known.Model,
	}
	if withoutVersion := strings.TrimSuffix(known.Model, ":0"); withoutVersion != known.Model {
		models = append(models, withoutVersion)
	}
	for _, model := range models {
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

func TestBuiltinCoversReviewedProviderFamiliesConservatively(t *testing.T) {
	tests := []struct {
		key  Key
		want domain.ProviderCapabilities
	}{
		{
			key:  Key{ProviderType: domain.ProviderAnthropic, Profile: domain.ProfileAnthropicMessages, Model: "claude-sonnet-4-6"},
			want: domain.ProviderCapabilities{Chat: true, Streaming: true, Tools: true, Vision: true, FetchedImage: true, Reasoning: true, StreamUsage: true, MaxContextTokens: 1_000_000, MaxOutputTokens: 128_000},
		},
		{
			key:  Key{ProviderType: domain.ProviderGemini, Profile: domain.ProfileGeminiText, Model: "gemini-2.5-pro"},
			want: domain.ProviderCapabilities{Chat: true, Streaming: true, DeveloperRole: true, MaxContextTokens: 1_048_576, MaxOutputTokens: 65_536},
		},
		{
			key:  Key{ProviderType: domain.ProviderGemini, Profile: domain.ProfileGeminiText, Model: "gemini-embedding-001"},
			want: domain.ProviderCapabilities{Embeddings: true},
		},
		{
			key:  Key{ProviderType: domain.ProviderBedrock, Profile: domain.ProfileBedrockConverseText, Model: "amazon.nova-pro-v1:0"},
			want: domain.ProviderCapabilities{Chat: true, Streaming: true, StreamUsage: true},
		},
		{
			key:  Key{ProviderType: domain.ProviderOpenAICompatible, Profile: domain.ProfileOpenAICompatible, Model: "gpt-5"},
			want: domain.ProviderCapabilities{Chat: true, Streaming: true},
		},
		{
			key:  Key{ProviderType: domain.ProviderDeepSeek, Profile: domain.ProfileDeepSeekChat, Model: "deepseek-v4-flash"},
			want: domain.ProviderCapabilities{Chat: true, Streaming: true, Tools: true, JSONObject: true, Reasoning: true, StreamUsage: true, MaxContextTokens: 1_000_000, MaxOutputTokens: 384_000},
		},
		{
			// The one DeepSeek model that accepts an image. Its two siblings above
			// answer one with a 400, which is why the claim lives per model and not
			// on the profile's defaults.
			key:  Key{ProviderType: domain.ProviderDeepSeek, Profile: domain.ProfileDeepSeekChat, Model: "deepseek-v4-flash-vision-exp"},
			want: domain.ProviderCapabilities{Chat: true, Streaming: true, Tools: true, JSONObject: true, Reasoning: true, Vision: true, FetchedImage: true, StreamUsage: true, MaxContextTokens: 1_000_000, MaxOutputTokens: 384_000},
		},
		{
			key:  Key{ProviderType: domain.ProviderDeepSeek, Profile: domain.ProfileDeepSeekChat, Model: "deepseek-v4-pro"},
			want: domain.ProviderCapabilities{Chat: true, Streaming: true, Tools: true, JSONObject: true, Reasoning: true, StreamUsage: true, MaxContextTokens: 1_000_000, MaxOutputTokens: 384_000},
		},
	}
	for _, test := range tests {
		entry, ok := Builtin().Lookup(test.key)
		if !ok {
			t.Errorf("missing reviewed entry %#v", test.key)
			continue
		}
		if entry.Status != StatusKnown || entry.Source != SourceBuiltin || entry.Capabilities != test.want {
			t.Errorf("entry %#v = %#v", test.key, entry)
		}
		if !domain.ProviderCapabilitiesSubset(entry.Capabilities, test.key.Ceiling()) {
			t.Errorf("entry %#v exceeds its profile", test.key)
		}
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
		Key:          Key{ProviderType: domain.ProviderBedrock, Profile: domain.ProfileBedrockConverseText, Model: "test.unrelated-chat-v1:0"},
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

func TestCatalogKeyEncodingHasNoCrossFieldCollision(t *testing.T) {
	left := Key{ProviderType: domain.ProviderOpenAI, Profile: domain.ProfileOpenAIChatEmbeddings, TargetKind: domain.TargetModelID, Model: "ab", Region: "c"}
	right := Key{ProviderType: domain.ProviderOpenAI, Profile: domain.ProfileOpenAIChatEmbeddings, TargetKind: domain.TargetModelID, Model: "a", Region: "bc"}
	if left.canonical() == right.canonical() {
		t.Fatal("length-prefixed catalog identities collided")
	}
	if err := (Key{ProviderType: domain.ProviderOpenAI, Profile: domain.ProfileOpenAIChatEmbeddings, TargetKind: domain.TargetModelID, Model: "a\x00b", Region: "c"}).Validate(); err == nil {
		t.Fatal("control character in catalog identity was accepted")
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

func TestKeyRejectsProviderProfileTargetKindMismatch(t *testing.T) {
	tests := []Key{
		{ProviderType: domain.ProviderOpenAI, Profile: domain.ProfileOpenAIChatEmbeddings, TargetKind: domain.TargetBedrockInferenceProfile, Model: "gpt"},
		{ProviderType: domain.ProviderAzureOpenAI, Profile: domain.ProfileAzureChatEmbeddings, TargetKind: domain.TargetModelID, Model: "deployment"},
		{ProviderType: domain.ProviderBedrock, Profile: domain.ProfileBedrockInvokeTitanEmbedV2, TargetKind: domain.TargetBedrockProvisionedThroughput, Model: "provisioned-target"},
		{ProviderType: domain.ProviderBedrock, Profile: domain.ProfileBedrockMantleOpenAIChat, TargetKind: domain.TargetBedrockFoundationModel, Model: "model"},
	}
	for _, key := range tests {
		if err := key.Validate(); err == nil || !strings.Contains(err.Error(), "incompatible with profile") {
			t.Fatalf("invalid provider/profile/target-kind identity accepted: %#v err=%v", key, err)
		}
	}
}

func TestBedrockConverseAcceptsEachSupportedTargetKind(t *testing.T) {
	for _, kind := range []domain.DeploymentTargetKind{
		domain.TargetBedrockFoundationModel,
		domain.TargetBedrockInferenceProfile,
		domain.TargetBedrockProvisionedThroughput,
	} {
		key := Key{ProviderType: domain.ProviderBedrock, Profile: domain.ProfileBedrockConverseText, TargetKind: kind, Model: "exact-target"}
		if err := key.Validate(); err != nil {
			t.Fatalf("supported converse target kind %q rejected: %v", kind, err)
		}
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

func TestNewNormalizesOmittedTargetKindToReachableExactIdentity(t *testing.T) {
	key := Key{ProviderType: domain.ProviderOpenAI, Profile: domain.ProfileOpenAIChatEmbeddings, Model: "internal-shorthand"}
	catalog, err := New(Entry{
		Key: key, Status: StatusKnown, Source: SourceBuiltin,
		Capabilities: domain.ProviderCapabilities{Chat: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	entry, found := catalog.Lookup(key)
	if !found || entry.Key.TargetKind != domain.TargetModelID {
		t.Fatalf("normalized entry is unreachable or inexact: found=%v entry=%#v", found, entry)
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

// The publishing runbook tells an operator to sign this exact file as their
// first rehearsal. It is a fixture with a hard expiry, so it rots on a date
// rather than on a code change: past expires_at, prepare and sign still
// succeed and verify fails, which reads as "the procedure is broken" rather
// than "the example is old". Fail here, in the repository, well before that.
func TestTheAuthoringExampleIsNotAboutToExpire(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "catalog", "unsigned-snapshot-v1.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	var example struct {
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(raw, &example); err != nil {
		t.Fatal(err)
	}
	if remaining := time.Until(example.ExpiresAt); remaining < 90*24*time.Hour {
		t.Fatalf("the authoring example expires at %s (%.0f days away); push it out before the runbook stops working",
			example.ExpiresAt.Format(time.RFC3339), remaining.Hours()/24)
	}
}

// The ceiling gained vision so one model could claim it. The other two must not
// have gained it with the ceiling: a catalog that widened by profile rather than
// by model would put vision on models DeepSeek answers with a 400.
// A Mantle deployment used to resolve as "not covered by the catalog", which is
// what forces an operator to declare capabilities by hand — and a hand
// declaration is a widening, which costs a revalidation and a route taken out of
// service to turn one capability on.
//
// The two numbers this entry gets from Bedrock rather than from OpenAI are the
// ones worth pinning: the context window is Bedrock's 272K and not the direct
// API's 1,050,000, and vision is claimed without the fetch, because Bedrock
// reads the bytes a request carries and retrieves nothing.
func TestTheMantleModelIsCoveredWithBedrocksOwnNumbers(t *testing.T) {
	entry, ok := Builtin().Lookup(Key{
		ProviderType: domain.ProviderBedrock,
		Profile:      domain.ProfileBedrockMantleOpenAIResponses,
		Model:        "openai.gpt-5.5",
	})
	if !ok {
		t.Fatal("openai.gpt-5.5 is not covered by the builtin catalog")
	}
	if entry.Capabilities.MaxContextTokens != 272_000 {
		t.Errorf("context window = %d, want Bedrock's 272000 rather than the direct API's", entry.Capabilities.MaxContextTokens)
	}
	if !entry.Capabilities.Vision {
		t.Error("the model card lists image input and the entry does not claim vision")
	}
	if entry.Capabilities.FetchedImage {
		t.Error("Bedrock does not fetch an image, and the Mantle ceiling does not carry fetched_image")
	}
	// Absent from the card, so left to the upstream rather than invented.
	if entry.Capabilities.MaxOutputTokens != 0 {
		t.Errorf("max output = %d, want it undeclared", entry.Capabilities.MaxOutputTokens)
	}
}

// The route is a property of the profile and cannot be read off the identifier,
// so a Mantle model has to appear under its own route's profile and under no
// other. Addressed on the wrong route the upstream refuses it by name; a catalog
// that covered both would be offering a deployment that cannot work.
func TestMantleModelsAreCoveredOnlyOnTheirOwnRoute(t *testing.T) {
	catalog := Builtin()
	covered := func(profile domain.ProviderProfileID, model string) bool {
		_, ok := catalog.Lookup(Key{ProviderType: domain.ProviderBedrock, Profile: profile, Model: model})
		return ok
	}
	for _, test := range []struct {
		model         string
		chat, notChat domain.ProviderProfileID
	}{
		// The identifier points the wrong way in both directions: an openai.*
		// model on the default route, a google.* one on the OpenAI route.
		{"openai.gpt-oss-20b", domain.ProfileBedrockMantleChat, domain.ProfileBedrockMantleOpenAIChat},
		{"google.gemma-3-27b-it", domain.ProfileBedrockMantleChat, domain.ProfileBedrockMantleOpenAIChat},
		{"google.gemma-4-31b", domain.ProfileBedrockMantleOpenAIChat, domain.ProfileBedrockMantleChat},
		{"xai.grok-4.3", domain.ProfileBedrockMantleOpenAIChat, domain.ProfileBedrockMantleChat},
	} {
		t.Run(test.model, func(t *testing.T) {
			if !covered(test.chat, test.model) {
				t.Errorf("%s is not covered on the route that serves it", test.model)
			}
			if covered(test.notChat, test.model) {
				t.Errorf("%s is covered on a route that refuses it", test.model)
			}
		})
	}
	// Responses is measured per model, not inherited from the route: two members
	// of one family serve it and their two safeguard siblings do not.
	for model, serves := range map[string]bool{
		"openai.gpt-oss-20b":            true,
		"openai.gpt-oss-120b":           true,
		"openai.gpt-oss-safeguard-20b":  false,
		"openai.gpt-oss-safeguard-120b": false,
	} {
		if got := covered(domain.ProfileBedrockMantleResponses, model); got != serves {
			t.Errorf("%s covered on the Responses profile = %v, want %v", model, got, serves)
		}
	}
	// Claude reaches Mantle through the Anthropic Messages profile only.
	if !covered(domain.ProfileBedrockMantleAnthropicMessages, "anthropic.claude-haiku-4-5") {
		t.Error("anthropic.claude-haiku-4-5 is not covered on the Anthropic Messages profile")
	}
	if covered(domain.ProfileBedrockMantleChat, "anthropic.claude-haiku-4-5") {
		t.Error("anthropic.claude-haiku-4-5 is covered on an OpenAI-shaped route that does not serve it")
	}
}

// The 50 identifiers Bedrock Mantle listed for a real account on 2026-08-25,
// read from GET /v1/models and sorted. A seeded model that is not on this list
// is a typo — an entry keyed to a name nothing serves, which resolves as
// uncovered exactly like having no entry, and says nothing about why.
var mantleAccountListing = []string{
	"anthropic.claude-haiku-4-5",
	"deepseek.v3.1", "deepseek.v3.2",
	"google.gemma-3-12b-it", "google.gemma-3-27b-it", "google.gemma-3-4b-it",
	"google.gemma-4-26b-a4b", "google.gemma-4-31b", "google.gemma-4-e2b",
	"minimax.minimax-m2", "minimax.minimax-m2.1", "minimax.minimax-m2.5",
	"mistral.devstral-2-123b", "mistral.magistral-small-2509",
	"mistral.ministral-3-14b-instruct", "mistral.ministral-3-3b-instruct", "mistral.ministral-3-8b-instruct",
	"mistral.mistral-large-3-675b-instruct",
	"mistral.voxtral-mini-3b-2507", "mistral.voxtral-small-24b-2507",
	"moonshotai.kimi-k2-thinking", "moonshotai.kimi-k2.5",
	"nvidia.nemotron-nano-12b-v2", "nvidia.nemotron-nano-3-30b", "nvidia.nemotron-nano-9b-v2",
	"nvidia.nemotron-super-3-120b",
	"openai.gpt-5.4", "openai.gpt-5.4-2026-03-05", "openai.gpt-5.5", "openai.gpt-5.5-2026-04-23",
	"openai.gpt-5.6-luna", "openai.gpt-5.6-sol", "openai.gpt-5.6-terra",
	"openai.gpt-oss-120b", "openai.gpt-oss-20b",
	"openai.gpt-oss-safeguard-120b", "openai.gpt-oss-safeguard-20b",
	"qwen.qwen3-235b-a22b-2507", "qwen.qwen3-32b",
	"qwen.qwen3-coder-30b-a3b-instruct", "qwen.qwen3-coder-480b-a35b-instruct", "qwen.qwen3-coder-next",
	"qwen.qwen3-next-80b-a3b-instruct", "qwen.qwen3-vl-235b-a22b-instruct",
	"writer.palmyra-vision-7b",
	"xai.grok-4.3",
	"zai.glm-4.6", "zai.glm-4.7", "zai.glm-4.7-flash", "zai.glm-5",
}

// Both directions of the reconciliation, because each catches a different
// mistake: a seeded name absent from the listing is a typo, and a listed name
// absent from the catalog is a model an operator has to declare by hand. The
// second is allowed to happen — but only on purpose, and only where the reason
// is written down. zai.glm-4.6 is the one such model: the console lists no card
// for it, so it has no window to record.
func TestSeededMantleModelsReconcileWithTheAccountListing(t *testing.T) {
	listed := make(map[string]bool, len(mantleAccountListing))
	for _, model := range mantleAccountListing {
		listed[model] = true
	}
	seeded := map[string]bool{}
	for _, entry := range Builtin().Entries() {
		if !domain.IsBedrockMantleProfile(entry.Key.Profile) {
			continue
		}
		seeded[entry.Key.Model] = true
		if !listed[entry.Key.Model] {
			t.Errorf("%s is seeded but the account lists no such model", entry.Key.Model)
		}
	}
	for _, model := range mantleAccountListing {
		if seeded[model] || model == "zai.glm-4.6" {
			continue
		}
		t.Errorf("%s is served by the account and covered by no entry", model)
	}
	if seeded["zai.glm-4.6"] {
		t.Error("zai.glm-4.6 is seeded; it has no published window, so record one before covering it")
	}
}

// Every seeded Mantle model but gpt-5.5 claims the two operations the matrix run
// measured and nothing else. The ceiling permits tools, JSON mode, vision,
// developer role and reasoning; none of them was exercised per model, and a
// claim nobody checked is the one thing this catalog must not ship.
func TestSeededMantleModelsClaimOnlyWhatWasMeasured(t *testing.T) {
	for _, entry := range Builtin().Entries() {
		if !domain.IsBedrockMantleProfile(entry.Key.Profile) || entry.Key.Model == "openai.gpt-5.5" {
			continue
		}
		capabilities := entry.Capabilities
		if !capabilities.Chat || !capabilities.Streaming {
			t.Errorf("%s claims neither chat nor streaming", entry.Key.Model)
		}
		if capabilities.Tools || capabilities.JSONObject || capabilities.StructuredOutputs ||
			capabilities.Vision || capabilities.DeveloperRole || capabilities.Reasoning || capabilities.StreamUsage {
			t.Errorf("%s on %s claims an unmeasured capability: %#v", entry.Key.Model, entry.Key.Profile, capabilities)
		}
		if capabilities.MaxContextTokens <= 0 {
			t.Errorf("%s has no context window", entry.Key.Model)
		}
	}
}

func TestOnlyTheDeepSeekVisionModelClaimsVision(t *testing.T) {
	catalog := Builtin()
	for model, want := range map[string]bool{
		"deepseek-v4-flash":            false,
		"deepseek-v4-pro":              false,
		"deepseek-v4-flash-vision-exp": true,
	} {
		entry, ok := catalog.Lookup(Key{
			ProviderType: domain.ProviderDeepSeek, Profile: domain.ProfileDeepSeekChat, Model: model,
		})
		if !ok {
			t.Fatalf("%s is not covered by the builtin catalog", model)
		}
		if entry.Capabilities.Vision != want {
			t.Fatalf("%s vision = %v, want %v", model, entry.Capabilities.Vision, want)
		}
	}
}

// The console told an operator that a model the catalog lists is "not in the
// catalogue", because the only question ever asked of it was whether the
// provider's own bindings covered the model. `deepseek.v3.1` is listed under
// the Mantle chat interfaces; a provider bound only to the OpenAI-shaped ones
// resolved it to nothing and offered a billable detection call to decide
// something already written down.
func TestProfilesCoveringAnswersWhichInterfaceListsAModel(t *testing.T) {
	catalog := Builtin()
	profiles := catalog.ProfilesCovering(domain.ProviderBedrock, domain.TargetModelID, "deepseek.v3.1")
	if len(profiles) == 0 {
		t.Fatal("a model the builtin catalog lists was reported as covered by nothing")
	}
	if !slices.Contains(profiles, domain.ProfileBedrockMantleChat) {
		t.Fatalf("the interface serving the model was not named: %v", profiles)
	}
	if slices.Contains(profiles, domain.ProfileBedrockMantleOpenAIChat) {
		t.Fatalf("an interface that does not serve the model was named: %v", profiles)
	}
	if !slices.IsSorted(profiles) {
		t.Fatalf("profiles were not returned in a stable order: %v", profiles)
	}
}

// Exact on the model, like Lookup. A prefix match would promote an unknown
// future model to an interface nobody has claimed serves it — which is the
// guess the whole catalog exists to avoid.
func TestProfilesCoveringDoesNotMatchOnAPrefix(t *testing.T) {
	catalog := Builtin()
	if profiles := catalog.ProfilesCovering(domain.ProviderBedrock, domain.TargetModelID, "deepseek.v3"); len(profiles) != 0 {
		t.Fatalf("a prefix matched a listed model: %v", profiles)
	}
	if profiles := catalog.ProfilesCovering(domain.ProviderBedrock, domain.TargetModelID, "no.such.model"); len(profiles) != 0 {
		t.Fatalf("an unlisted model was reported as covered: %v", profiles)
	}
	// Provider type is part of the question: the same name under a different
	// upstream is a different model.
	if profiles := catalog.ProfilesCovering(domain.ProviderOpenAI, domain.TargetModelID, "deepseek.v3.1"); len(profiles) != 0 {
		t.Fatalf("a model was reported as covered under the wrong provider type: %v", profiles)
	}
}
