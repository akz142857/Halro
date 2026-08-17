package domain

import (
	"slices"
	"testing"
)

// The split is the rule the console used to own. These tests are the reason it
// can stop owning it: they pin what one flat capability set turns into for every
// shape of connection the matrix has — one profile, several with disjoint
// ceilings, and several whose ceilings overlap.

func TestOpenAIConnectionSplitsChatFromMedia(t *testing.T) {
	requested := ProviderCapabilities{
		Chat: true, Streaming: true, Embeddings: true, Tools: true,
		Moderations: true, Images: true, Files: true, Batches: true,
	}
	result := AssignConnectionCapabilities(ProviderOpenAI, ProfileOpenAIChatEmbeddings, requested)
	if len(result.Unservable) != 0 || len(result.Ambiguous) != 0 {
		t.Fatalf("openai defaults were not servable: %+v", result)
	}
	if len(result.Assignments) != 2 {
		t.Fatalf("want a chat binding and a media binding, got %d", len(result.Assignments))
	}
	chat, media := result.Assignments[0], result.Assignments[1]
	if chat.ProfileID != ProfileOpenAIChatEmbeddings || media.ProfileID != ProfileOpenAIMediaResources {
		t.Fatalf("wrong profiles: %s then %s", chat.ProfileID, media.ProfileID)
	}
	if !chat.Capabilities.Chat || !chat.Capabilities.Embeddings || chat.Capabilities.Images {
		t.Errorf("chat binding carries the wrong set: %+v", chat.Capabilities)
	}
	if !media.Capabilities.Images || !media.Capabilities.Moderations || media.Capabilities.Chat {
		t.Errorf("media binding carries the wrong set: %+v", media.Capabilities)
	}
	// Files and batches are OpenAI's media profile alone, so they leave the
	// anchor: the two ceilings are disjoint and every capability has one home.
	if chat.Capabilities.Files || !media.Capabilities.Files || !media.Capabilities.Batches {
		t.Errorf("files and batches did not land on the media profile: %+v / %+v", chat, media)
	}
}

// The anchor is the operator's answer to "which implementation", so it takes
// everything it can serve. Before this rule the first row in the table did, and
// selecting Bedrock Mantle's Responses implementation produced a connection
// bound to its Chat one.
func TestAnchorTakesWhatItCanServeBeforeAnyPeer(t *testing.T) {
	requested := ProviderCapabilities{Chat: true, Streaming: true, Tools: true, Vision: true}
	for _, anchor := range []ProviderProfileID{
		ProfileBedrockMantleOpenAIChat,
		ProfileBedrockMantleOpenAIResponses,
		ProfileBedrockMantleAnthropicMessages,
	} {
		result := AssignConnectionCapabilities(ProviderBedrock, anchor, requested)
		if len(result.Assignments) != 1 || result.Assignments[0].ProfileID != anchor {
			t.Fatalf("%s: want one binding on the selected profile, got %+v", anchor, result.Assignments)
		}
	}
}

// Where the anchor cannot serve a capability and more than one peer can, there
// is no answer in a flat set. Refusing names the capability; choosing silently
// would bind the connection to a protocol nobody picked.
func TestOverlappingPeersRefuseRatherThanPick(t *testing.T) {
	result := AssignConnectionCapabilities(
		ProviderBedrock, ProfileBedrockMantleOpenAIResponses,
		ProviderCapabilities{Chat: true, Reasoning: true},
	)
	if !slices.Contains(result.Ambiguous, "reasoning") {
		t.Fatalf("reasoning is served by two Mantle peers and was not refused: %+v", result)
	}
	if len(result.Assignments) != 1 || result.Assignments[0].Capabilities.Reasoning {
		t.Fatalf("the ambiguous capability was assigned anyway: %+v", result.Assignments)
	}
	// And the connection ceiling never offers it, so a form built from the
	// endpoint cannot produce this request in the first place.
	if ConnectionCeiling(ProviderBedrock, ProfileBedrockMantleOpenAIResponses).Reasoning {
		t.Error("the ceiling offers a capability the assignment refuses")
	}
}

func TestCapabilityNoProfileServesIsRefusedNotDropped(t *testing.T) {
	result := AssignConnectionCapabilities(
		ProviderGemini, ProfileGeminiText,
		ProviderCapabilities{Chat: true, Images: true, Rerank: true},
	)
	if !slices.Contains(result.Unservable, "images") || !slices.Contains(result.Unservable, "rerank") {
		t.Fatalf("unservable capabilities were not named: %+v", result)
	}
	for _, assigned := range result.Assignments {
		if assigned.Capabilities.Images || assigned.Capabilities.Rerank {
			t.Fatalf("a refused capability was stored anyway: %+v", assigned)
		}
	}
}

// Bedrock's runtime profiles have disjoint ceilings, so one connection can carry
// Converse and Titan and Nova at once — the case that made the flat contract
// possible without losing what the console could already express.
func TestBedrockRuntimeSpreadsAcrossItsProfiles(t *testing.T) {
	result := AssignConnectionCapabilities(
		ProviderBedrock, ProfileBedrockConverseText,
		ProviderCapabilities{Chat: true, Streaming: true, Embeddings: true, Images: true, AsyncGenerate: true},
	)
	if len(result.Unservable) != 0 || len(result.Ambiguous) != 0 {
		t.Fatalf("bedrock runtime refused its own group: %+v", result)
	}
	got := make(map[ProviderProfileID]ProviderCapabilities, len(result.Assignments))
	for _, assigned := range result.Assignments {
		got[assigned.ProfileID] = assigned.Capabilities
	}
	if !got[ProfileBedrockConverseText].Chat || !got[ProfileBedrockInvokeTitanEmbedV2].Embeddings ||
		!got[ProfileBedrockInvokeTitanImageV2].Images || !got[ProfileBedrockAsyncNovaReel].AsyncGenerate {
		t.Fatalf("the runtime group did not split by ceiling: %+v", got)
	}
	// The rerank profile is on another access surface, so it is not on this
	// connection at all — the credential could not sign for it.
	if _, present := got[ProfileBedrockAgentRerankCohere35]; present {
		t.Error("a profile from another access surface joined the connection")
	}
}

// The numeric bounds belong to the profile, not to the request. Titan Embed is
// the only profile that bounds one, and a chat binding on the same connection
// used to inherit its 8192 because the console carried the union of the group's
// defaults in the flat set and handed the numbers to whoever had chat.
func TestNumericLimitsComeFromTheProfileThatServesTheCapability(t *testing.T) {
	result := AssignConnectionCapabilities(
		ProviderBedrock, ProfileBedrockConverseText,
		ProviderCapabilities{Chat: true, Embeddings: true},
	)
	for _, assigned := range result.Assignments {
		switch assigned.ProfileID {
		case ProfileBedrockConverseText:
			if assigned.Capabilities.MaxContextTokens != 0 {
				t.Errorf("chat binding inherited a context bound: %d", assigned.Capabilities.MaxContextTokens)
			}
		case ProfileBedrockInvokeTitanEmbedV2:
			if assigned.Capabilities.MaxContextTokens != 8192 {
				t.Errorf("titan binding lost its own bound: %d", assigned.Capabilities.MaxContextTokens)
			}
		}
	}
}

func TestNumericLimitsNarrowButNeverWiden(t *testing.T) {
	narrowed := AssignConnectionCapabilities(
		ProviderBedrock, ProfileBedrockInvokeTitanEmbedV2,
		ProviderCapabilities{Embeddings: true, MaxContextTokens: 4096},
	)
	if len(narrowed.Assignments) != 1 || narrowed.Assignments[0].Capabilities.MaxContextTokens != 4096 {
		t.Fatalf("a lower bound was not honoured: %+v", narrowed.Assignments)
	}
	widened := AssignConnectionCapabilities(
		ProviderBedrock, ProfileBedrockInvokeTitanEmbedV2,
		ProviderCapabilities{Embeddings: true, MaxContextTokens: 100000},
	)
	if !slices.Contains(widened.Exceeded, "max_context_tokens") {
		t.Fatalf("a request above the profile bound was not refused: %+v", widened)
	}
}

// Reading a connection back and saving it unchanged must store what was already
// there. It did not: the stored summary reports the loosest bound across the
// bindings — 8192, from Titan Embed — and re-submitting that handed Titan's
// bound to the Converse chat binding, capping chat context at 8192 on an edit
// nobody meant as a change. The console does this round trip on every save.
func TestSavingAConnectionUnchangedDoesNotMoveABoundBetweenProfiles(t *testing.T) {
	// Chat and embeddings together: two bindings, only one of which declares a
	// bound. That is the shape the summary loses information about.
	first := AssignConnectionCapabilities(ProviderBedrock, ProfileBedrockConverseText,
		ProviderCapabilities{Chat: true, Streaming: true, StreamUsage: true, Embeddings: true})
	bindings := make([]ProviderProfileBinding, 0, len(first.Assignments))
	for _, assigned := range first.Assignments {
		bindings = append(bindings, ProviderProfileBinding{
			ProfileID: assigned.ProfileID, Capabilities: assigned.Capabilities, Enabled: true,
		})
	}
	summary, _ := BindingsCapabilitiesSummary(bindings)
	if summary.MaxContextTokens != 8192 {
		t.Fatalf("this test is only meaningful while the summary carries Titan's bound: %d", summary.MaxContextTokens)
	}

	second := AssignConnectionCapabilities(ProviderBedrock, ProfileBedrockConverseText, summary)
	if len(second.Unservable) != 0 || len(second.Ambiguous) != 0 || len(second.Unboundable) != 0 {
		t.Fatalf("saving a connection unchanged was refused: %+v", second)
	}
	for _, assigned := range second.Assignments {
		want := int64(0)
		if assigned.ProfileID == ProfileBedrockInvokeTitanEmbedV2 {
			want = 8192
		}
		if assigned.Capabilities.MaxContextTokens != want {
			t.Errorf("%s context bound is %d, want %d", assigned.ProfileID, assigned.Capabilities.MaxContextTokens, want)
		}
	}
}

// A bound no profile here declares has nowhere to be stored. Refused by name:
// accepting it would answer 201 to a caller who then believes the connection is
// bounded, and the connection would not be.
func TestABoundNoProfileHoldsIsRefused(t *testing.T) {
	result := AssignConnectionCapabilities(
		ProviderBedrock, ProfileBedrockConverseText,
		ProviderCapabilities{Chat: true, MaxOutputTokens: 16000},
	)
	if !slices.Contains(result.Unboundable, "max_output_tokens") {
		t.Fatalf("an unholdable bound was accepted: %+v", result)
	}
	// And the same request against a profile that does bound it is fine.
	held := AssignConnectionCapabilities(
		ProviderBedrock, ProfileBedrockInvokeTitanEmbedV2,
		ProviderCapabilities{Embeddings: true, MaxContextTokens: 4096},
	)
	if len(held.Unboundable) != 0 {
		t.Fatalf("a bound the profile holds was refused: %+v", held)
	}
}

// Whether a token bound can be declared at all depends on which capabilities are
// on, because the profile that serves them is the one that holds it. The
// connection-level ceiling therefore states no number: a Bedrock connection
// accepts 8192 once embeddings are on and refuses it otherwise, and a single
// figure would be wrong in one of those two states. Each profile's own bound is
// served next to it.
func TestConnectionCeilingMakesNoClaimAboutTokenBounds(t *testing.T) {
	ceiling := ConnectionCeiling(ProviderBedrock, ProfileBedrockConverseText)
	if ceiling.MaxContextTokens != 0 || ceiling.MaxOutputTokens != 0 {
		t.Fatalf("the connection ceiling stated a token bound: %+v", ceiling)
	}
	withEmbeddings := AssignConnectionCapabilities(ProviderBedrock, ProfileBedrockConverseText,
		ProviderCapabilities{Chat: true, Embeddings: true, MaxContextTokens: 8192})
	if len(withEmbeddings.Unservable) != 0 || len(withEmbeddings.Unboundable) != 0 || len(withEmbeddings.Exceeded) != 0 {
		t.Fatalf("Titan's own bound was refused on a connection that carries it: %+v", withEmbeddings)
	}
	withoutEmbeddings := AssignConnectionCapabilities(ProviderBedrock, ProfileBedrockConverseText,
		ProviderCapabilities{Chat: true, MaxContextTokens: 8192})
	if !slices.Contains(withoutEmbeddings.Unboundable, "max_context_tokens") {
		t.Fatalf("a bound was accepted with nothing to hold it: %+v", withoutEmbeddings)
	}
	// Too large is its own refusal: the fix is a smaller number, not a dropped
	// field, and "cannot serve maximum context tokens" says neither.
	above := AssignConnectionCapabilities(ProviderBedrock, ProfileBedrockConverseText,
		ProviderCapabilities{Chat: true, Embeddings: true, MaxContextTokens: 8193})
	if !slices.Contains(above.Exceeded, "max_context_tokens") || len(above.Unservable) != 0 {
		t.Fatalf("a value above Titan's bound was not refused as too large: %+v", above)
	}
}

// A new connection declares what the profile the operator picked declares, and
// nothing its neighbours declare. The console's union default meant a new
// Bedrock connection advertised embeddings, image generation and async video to
// the router before anyone chose them — on an account that may not have access
// to those models at all.
func TestNewConnectionDefaultsToTheProfileThatWasPicked(t *testing.T) {
	converse := ConnectionDefaults(ProviderBedrock, ProfileBedrockConverseText)
	if !converse.Chat || !converse.Streaming || !converse.StreamUsage {
		t.Fatalf("the Converse defaults lost their own capabilities: %+v", converse)
	}
	if converse.Embeddings || converse.Images || converse.AsyncGenerate {
		t.Fatalf("a new Bedrock connection declared a neighbour's capabilities: %+v", converse)
	}
	// The neighbours stay reachable — they are offered, not declared.
	ceiling := ConnectionCeiling(ProviderBedrock, ProfileBedrockConverseText)
	if !ceiling.Embeddings || !ceiling.Images || !ceiling.AsyncGenerate {
		t.Fatalf("the ceiling stopped offering the group: %+v", ceiling)
	}
	// And picking a different implementation means something again.
	titan := ConnectionDefaults(ProviderBedrock, ProfileBedrockInvokeTitanEmbedV2)
	if !titan.Embeddings || titan.Chat {
		t.Fatalf("the Titan Embed defaults are not its own: %+v", titan)
	}
}

// Everything the connection defaults offer must be assignable, or a new
// connection would be refused the moment it was saved unchanged — the failure
// this whole change exists to make impossible.
func TestConnectionDefaultsAreAlwaysAssignable(t *testing.T) {
	for _, profile := range AllProviderProfiles() {
		defaults := ConnectionDefaults(profile.Type, profile.ID)
		result := AssignConnectionCapabilities(profile.Type, profile.ID, defaults)
		if len(result.Unservable) != 0 || len(result.Ambiguous) != 0 {
			t.Errorf("%s: defaults are not assignable: %+v", profile.ID, result)
		}
		if len(result.Assignments) == 0 {
			t.Errorf("%s: defaults produce no binding at all", profile.ID)
		}
	}
}

// The ceiling is what a form offers. Anything inside it must survive assignment,
// and each binding it produces must stay within the ceiling the domain write
// boundary enforces — otherwise the console can tick something the save refuses.
func TestConnectionCeilingIsAssignableAndBounded(t *testing.T) {
	for _, profile := range AllProviderProfiles() {
		ceiling := ConnectionCeiling(profile.Type, profile.ID)
		result := AssignConnectionCapabilities(profile.Type, profile.ID, ceiling)
		if len(result.Unservable) != 0 || len(result.Ambiguous) != 0 {
			t.Errorf("%s: its own ceiling is not assignable: %+v", profile.ID, result)
			continue
		}
		for _, assigned := range result.Assignments {
			bound := MaxProviderCapabilitiesForProfile(profile.Type, assigned.ProfileID)
			if !ProviderCapabilitiesSubset(assigned.Capabilities, bound) {
				t.Errorf("%s: binding on %s exceeds its profile ceiling: %+v",
					profile.ID, assigned.ProfileID, assigned.Capabilities)
			}
		}
	}
}

// A connection carries only profiles its credential can sign for, which is the
// same rule the Admin handler enforces per binding. If this drifted, the handler
// would refuse connections the assignment had just produced.
func TestConnectionProfilesShareSurfaceAndScheme(t *testing.T) {
	for _, profile := range AllProviderProfiles() {
		group := ConnectionProfiles(profile.Type, profile.ID)
		if len(group) == 0 || group[0].ID != profile.ID {
			t.Fatalf("%s: the anchor is not first in its own group: %+v", profile.ID, group)
		}
		for _, peer := range group {
			if peer.Type != profile.Type || peer.AccessSurface != profile.AccessSurface ||
				peer.CredentialScheme != profile.CredentialScheme {
				t.Errorf("%s: peer %s does not share the connection's credential shape", profile.ID, peer.ID)
			}
		}
	}
	if group := ConnectionProfiles(ProviderOpenAI, ProfileAnthropicMessages); group != nil {
		t.Errorf("a profile from another type produced a group: %+v", group)
	}
	if group := ConnectionProfiles(ProviderOpenAI, "no.such.profile.v1"); group != nil {
		t.Errorf("an unregistered anchor produced a group: %+v", group)
	}
}

// setCapabilityEnabled is a switch over capability keys, which is the kind of
// thing that goes stale silently: a capability added to capabilityNames and not
// to the switch would simply never be assigned, and every set built here would
// quietly lack it.
func TestEveryCapabilityNameCanBeSetAndRead(t *testing.T) {
	for _, name := range CapabilityNames() {
		if name == "max_context_tokens" || name == "max_output_tokens" {
			continue
		}
		var capabilities ProviderCapabilities
		setCapabilityEnabled(&capabilities, name, true)
		if !capabilityEnabled(capabilities, name) {
			t.Errorf("%s cannot be set through setCapabilityEnabled", name)
		}
		if names := enabledCapabilityNames(capabilities); len(names) != 1 || names[0] != name {
			t.Errorf("%s set something else: %+v", name, names)
		}
	}
}

// Warned capabilities must be real capability keys, or the console would look up
// a name that does not exist and show nothing where a warning belongs.
func TestOptInWarningsNameRealCapabilities(t *testing.T) {
	for _, name := range CapabilityOptInWarnings() {
		if !slices.Contains(CapabilityNames(), name) {
			t.Errorf("%s is warned about but is not a capability", name)
		}
	}
	if !slices.Contains(CapabilityOptInWarnings(), "provider_executed_tools") {
		t.Error("provider_executed_tools accepts upstream egress outside SafeTransport and must be warned about")
	}
}
