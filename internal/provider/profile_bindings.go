package provider

import (
	"github.com/akz142857/Halro/internal/domain"
)

// What a profile serves, declared once.
//
// This used to be one truth written four times: the operation list, the
// primitive bindings, the profile's identity (type, surface, scheme), and a
// second operation→primitive map that Validate compared the bindings against.
// Three of the four were already machine-checked against each other, which is
// how they stayed consistent — and also the sign that only one of them was
// carrying information.
//
// What is derived and no longer written:
//
//   - Identity comes from the domain profile table, which is where an operator's
//     connection is validated against it anyway. Repeating it here meant a row
//     could disagree with the table until Validate caught it.
//   - Operations are the bindings' operations, in the order declared.
//   - A binding's semantic operation is a property of its legacy operation, so
//     semanticOperationFor answers it.
//
// Revision stays, because it is the one thing here that is not derivable: it
// records that a profile's meaning changed under a stable identifier — the two
// Mantle rows moved routes, the Anthropic row grew files and batches — and
// nothing else in the tree remembers that.
//
// What the merge costs, stated plainly: ProfileManifest.Validate now checks a
// builtin manifest against the table it was built from, which is a tautology.
// It stays meaningful for a manifest supplied by a caller — NewLegacyAdapterBridge
// takes one — and that is the case it was written for.
//
// No protection is lost by that, and the reason is worth following. The old
// arrangement could only detect the two lists *disagreeing*. A binding that was
// simply wrong, written the same way in both, passed then and passes now. So
// what the merge removes is the possibility of disagreement, not a check on
// correctness — there never was one here.
//
// A wrong primitive in this table is caught only where it leaves a primitive
// constant unbound (TestEveryPrimitiveConstantIsBoundBySomeProfile) or where a
// platform has a wiring test that asserts the route it addresses. Swapping two
// bound primitives between profiles is caught by neither. That is a gap, and it
// is recorded in docs/contracts/adding-a-platform.md rather than papered over.
type operationBinding struct {
	Operation Operation
	Primitive Primitive
}

type profileOperations struct {
	Revision uint64
	Bindings []operationBinding
}

// chatPair and messagesPair are the two shapes that repeat. Written out they
// were four lines that had to agree; named, a reader sees a profile serves the
// ordinary chat pair rather than checking that it does.
func chatPair(unary, stream Primitive) []operationBinding {
	return []operationBinding{{OperationChat, unary}, {OperationChatStream, stream}}
}

// anthropicWire is the chat pair plus the northbound Messages operations that
// let /v1/messages land on the profile, which is what makes an Anthropic SDK
// reach it at all.
func anthropicWire(unary, stream Primitive) []operationBinding {
	return []operationBinding{
		{OperationChat, unary}, {OperationChatStream, stream},
		{OperationMessages, unary}, {OperationMessagesStream, stream},
	}
}

var profileOperationTable = map[domain.ProviderProfileID]profileOperations{
	domain.ProfileOpenAIChatEmbeddings: {Revision: 1, Bindings: append(
		chatPair(PrimitiveOpenAIChatCompletions, PrimitiveOpenAIChatStream),
		operationBinding{OperationEmbeddings, PrimitiveOpenAIEmbeddings})},
	domain.ProfileOpenAIResponses: {Revision: 1, Bindings: []operationBinding{
		{OperationChat, PrimitiveOpenAIResponses}}},
	// Revision 2: files and batches joined this profile. Anthropic serves both on
	// the same credential, and the file half is stored by Halro rather than
	// uploaded, because Anthropic batches carry their requests inline.
	domain.ProfileAnthropicMessages: {Revision: 2, Bindings: append(
		anthropicWire(PrimitiveAnthropicMessages, PrimitiveAnthropicMessagesStream),
		operationBinding{OperationFiles, PrimitiveHalroLocalFiles},
		operationBinding{OperationBatches, PrimitiveAnthropicMessageBatches})},
	domain.ProfileAzureChatEmbeddings: {Revision: 1, Bindings: append(
		chatPair(PrimitiveAzureChatCompletions, PrimitiveAzureChatStream),
		operationBinding{OperationEmbeddings, PrimitiveAzureEmbeddings})},
	domain.ProfileDeepSeekChat: {Revision: 1, Bindings: chatPair(
		PrimitiveDeepSeekChat, PrimitiveDeepSeekChatStream)},
	domain.ProfileOpenAICompatible: {Revision: 1, Bindings: append(
		chatPair(PrimitiveCompatibleChat, PrimitiveCompatibleChatStream),
		operationBinding{OperationEmbeddings, PrimitiveCompatibleEmbeddings})},
	domain.ProfileGeminiText: {Revision: 1, Bindings: append(
		chatPair(PrimitiveGeminiGenerateContent, PrimitiveGeminiStreamGenerateContent),
		operationBinding{OperationEmbeddings, PrimitiveGeminiEmbedContent})},
	domain.ProfileBedrockConverseText: {Revision: 1, Bindings: chatPair(
		PrimitiveBedrockConverse, PrimitiveBedrockConverseStream)},
	domain.ProfileBedrockInvokeTitanEmbedV2: {Revision: 1, Bindings: []operationBinding{
		{OperationEmbeddings, PrimitiveBedrockInvokeTitanEmbedV2}}},
	domain.ProfileOpenAIMediaResources: {Revision: 1, Bindings: []operationBinding{
		{OperationModerations, PrimitiveOpenAIModerations},
		{OperationImages, PrimitiveOpenAIImages},
		{OperationTranscriptions, PrimitiveOpenAIAudioTranscriptions},
		{OperationSpeech, PrimitiveOpenAIAudioSpeech},
		{OperationFiles, PrimitiveOpenAIFiles},
		{OperationBatches, PrimitiveOpenAIBatches}}},
	domain.ProfileBedrockInvokeTitanImageV2: {Revision: 1, Bindings: []operationBinding{
		{OperationImages, PrimitiveBedrockTitanImageV2}}},
	domain.ProfileBedrockAgentRerankCohere35: {Revision: 1, Bindings: []operationBinding{
		{OperationRerank, PrimitiveBedrockAgentRerankCohere35}}},
	domain.ProfileBedrockAsyncNovaReel: {Revision: 1, Bindings: []operationBinding{
		{OperationAsyncInvoke, PrimitiveBedrockAsyncNovaReel}}},
	// The Mantle chat and responses pairs each serve one route. Revision 2 on the
	// /openai/v1 halves records the route split: they addressed /v1 until the
	// models moved.
	domain.ProfileBedrockMantleChat: {Revision: 1, Bindings: chatPair(
		PrimitiveBedrockMantleOpenAIChat, PrimitiveBedrockMantleOpenAIChatStream)},
	domain.ProfileBedrockMantleOpenAIChat: {Revision: 2, Bindings: chatPair(
		PrimitiveBedrockMantleOpenAIChat, PrimitiveBedrockMantleOpenAIChatStream)},
	domain.ProfileBedrockMantleResponses: {Revision: 1, Bindings: chatPair(
		PrimitiveBedrockMantleOpenAIResponses, PrimitiveBedrockMantleOpenAIResponsesStream)},
	domain.ProfileBedrockMantleOpenAIResponses: {Revision: 2, Bindings: chatPair(
		PrimitiveBedrockMantleOpenAIResponses, PrimitiveBedrockMantleOpenAIResponsesStream)},
	domain.ProfileBedrockMantleAnthropicMessages: {Revision: 1, Bindings: anthropicWire(
		PrimitiveBedrockMantleAnthropicMessages, PrimitiveBedrockMantleAnthropicMessagesStream)},
	domain.ProfileMiniMaxAnthropicMessages: {Revision: 1, Bindings: anthropicWire(
		PrimitiveMiniMaxAnthropicMessages, PrimitiveMiniMaxAnthropicMessagesStream)},
	domain.ProfileMiniMaxChat: {Revision: 1, Bindings: chatPair(
		PrimitiveMiniMaxChat, PrimitiveMiniMaxChatStream)},
	// One operation: this profile serves the unary Responses call and binds no
	// stream primitive.
	domain.ProfileMiniMaxResponses: {Revision: 1, Bindings: []operationBinding{
		{OperationChat, PrimitiveMiniMaxResponses}}},
}

// builtinProfileDerived assembles a manifest from the table above plus the
// domain profile table, which owns the identity half.
//
// The replacement was established rather than assumed: a temporary equivalence
// test compared this against the four hand-written declarations across every
// registered profile and 12,298 (profile, operation, primitive) combinations
// before they were deleted. One difference was found and kept, because it goes
// the safe way — see profileAllowsPrimitiveDerived.
func builtinProfileDerived(id domain.ProviderProfileID) (ProfileManifest, bool) {
	row, ok := profileOperationTable[id]
	if !ok {
		return ProfileManifest{}, false
	}
	providerType, defaults, ok := domain.RegisteredProviderProfile(id)
	if !ok {
		return ProfileManifest{}, false
	}
	manifest := ProfileManifest{
		ID: id, Revision: row.Revision, ProviderType: providerType,
		AccessSurface: defaults.AccessSurface, CredentialScheme: defaults.CredentialScheme,
		Operations:        make([]Operation, 0, len(row.Bindings)),
		PrimitiveBindings: make([]PrimitiveBinding, 0, len(row.Bindings)),
	}
	for _, binding := range row.Bindings {
		manifest.Operations = append(manifest.Operations, binding.Operation)
		manifest.PrimitiveBindings = append(manifest.PrimitiveBindings, PrimitiveBinding{
			LegacyOperation:   binding.Operation,
			SemanticOperation: semanticOperationFor(binding.Operation),
			Primitive:         binding.Primitive,
		})
	}
	return manifest, true
}

// profileAllowsPrimitiveDerived answers the same question the second map used
// to, from the one table.
//
// One answer changed, deliberately. The old map answered by indexing, so an
// operation a profile does not serve returned the zero Primitive — and comparing
// that to an empty primitive said yes, which reads as "an unbound operation
// permits an empty binding". Nothing could reach it, because Validate rejects an
// empty primitive before asking. This says no, which is what the sentence should
// have said.
func profileAllowsPrimitiveDerived(profileID domain.ProviderProfileID, operation Operation, primitive Primitive) bool {
	row, ok := profileOperationTable[profileID]
	if !ok {
		return false
	}
	for _, binding := range row.Bindings {
		if binding.Operation == operation {
			return binding.Primitive == primitive
		}
	}
	return false
}
