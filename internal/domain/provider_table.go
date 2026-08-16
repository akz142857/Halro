package domain

// The provider matrix, in one place.
//
// What a profile is, what it starts with, and what an operator may turn on used
// to be six switch statements and an inline slice spread across two files:
// capabilities by type, capabilities by profile, the ceiling, profile identity,
// the immutable set, and the credential-resolution order. Nothing could be read
// as a matrix, and nothing could enumerate it — the only full list of profiles
// lived inside ResolveCredentialProfile's body, so anything that needed to walk
// them (the Admin metadata endpoint, an invariant test) had to write its own
// second list and could not be told when it fell behind.
//
// This is that matrix. The functions below are lookups into it.
//
// Order is significant and is the credential-resolution precedence: several
// profiles share one (type, surface, scheme) — OpenAI chat and media, the four
// Bedrock runtime profiles, the three Mantle profiles — and resolving a stored
// credential must land on the same one every time. A slice fixes that; a map's
// iteration order would not. Within each group the primary profile comes first.

// profileRow is one profile: who it belongs to, how it authenticates, and the
// two capability sets that bound it.
//
// Defaults and Ceiling are deliberately separate fields rather than one with a
// modifier. Defaults answers "what does a new connection start with"; Ceiling
// answers "what may an operator turn on at all". Collapsing them makes every
// optional capability either always-on or unreachable, and ProviderExecutedTools
// has to be neither: the Anthropic Messages profile supports it, and enabling it
// accepts upstream egress that never passes through SafeTransport, so the
// operator opts in. Where the two are equal the row repeats the set, which is
// the readable form — a reader should not have to compute a ceiling.
type profileRow struct {
	ID        ProviderProfileID
	Type      ProviderType
	Surface   AccessSurface
	Scheme    CredentialScheme
	Immutable bool
	Defaults  ProviderCapabilities
	Ceiling   ProviderCapabilities
}

var (
	openAIChatSet = ProviderCapabilities{
		Chat: true, Streaming: true, Embeddings: true, Tools: true,
		Vision: true, JSONMode: true, DeveloperRole: true, Reasoning: true,
		StreamUsage: true,
	}
	// Files and batches ride with the Anthropic connection because Anthropic
	// serves both on the same credential. The file half is stored by Halro and
	// never uploaded — Anthropic batches carry their requests inline — so the
	// capability says the deployment can be given a file, not that Anthropic will
	// hold one. See ADR 0021.
	anthropicMessagesSet = ProviderCapabilities{
		Chat: true, Streaming: true, Tools: true, Vision: true, JSONMode: true,
		Reasoning: true, StreamUsage: true, Files: true, Batches: true,
	}
	openAIMediaSet = ProviderCapabilities{
		Moderations: true, Images: true, Transcriptions: true,
		Speech: true, Files: true, Batches: true,
	}
)

// profileTable is the authority. Adding a profile means adding a row here and a
// manifest in internal/provider; TestCeilingWithinProfileManifestOperations
// refuses a row whose ceiling claims an operation no primitive is bound to.
var profileTable = []profileRow{
	{
		ID: ProfileOpenAIChatEmbeddings, Type: ProviderOpenAI,
		Surface: SurfaceOpenAI, Scheme: CredentialBearerStatic,
		Defaults: openAIChatSet, Ceiling: openAIChatSet,
	},
	{
		// JSONMode covers Anthropic's schema-backed structured outputs, which the
		// Messages profile carries through output_config.format.
		ID: ProfileAnthropicMessages, Type: ProviderAnthropic,
		Surface: SurfaceAnthropic, Scheme: CredentialAnthropicAPIKey,
		Defaults: anthropicMessagesSet,
		Ceiling:  withProviderExecutedTools(anthropicMessagesSet),
	},
	{
		ID: ProfileAzureChatEmbeddings, Type: ProviderAzureOpenAI,
		Surface: SurfaceAzureOpenAI, Scheme: CredentialAzureAPIKey,
		Defaults: openAIChatSet, Ceiling: openAIChatSet,
	},
	{
		ID: ProfileDeepSeekChat, Type: ProviderDeepSeek,
		Surface: SurfaceDeepSeek, Scheme: CredentialBearerStatic,
		Defaults: deepSeekSet, Ceiling: deepSeekSet,
	},
	{
		// Compatibility servers vary. These conservative defaults preserve the two
		// core OpenAI endpoints; anything else has to be established per server,
		// which capability detection does — not asserted in a table.
		ID: ProfileOpenAICompatible, Type: ProviderOpenAICompatible,
		Surface: SurfaceOpenAICompatible, Scheme: CredentialBearerStatic,
		Defaults: openAICompatibleSet, Ceiling: openAICompatibleSet,
	},
	{
		// Beta profile intentionally declares only the translated text subset.
		ID: ProfileGeminiText, Type: ProviderGemini,
		Surface: SurfaceGemini, Scheme: CredentialGoogleAPIKey,
		Defaults: geminiTextSet, Ceiling: geminiTextSet,
	},
	{
		// Beta profile intentionally declares only Converse text chat and usage.
		ID: ProfileBedrockConverseText, Type: ProviderBedrock,
		Surface: SurfaceBedrockRuntime, Scheme: CredentialAWSSigV4Explicit,
		Defaults: bedrockConverseSet, Ceiling: bedrockConverseSet,
	},
	{
		ID: ProfileBedrockInvokeTitanEmbedV2, Type: ProviderBedrock,
		Surface: SurfaceBedrockRuntime, Scheme: CredentialAWSSigV4Explicit,
		Immutable: true,
		Defaults:  titanEmbedSet, Ceiling: titanEmbedSet,
	},
	{
		ID: ProfileOpenAIMediaResources, Type: ProviderOpenAI,
		Surface: SurfaceOpenAI, Scheme: CredentialBearerStatic,
		Immutable: true,
		Defaults:  openAIMediaSet, Ceiling: openAIMediaSet,
	},
	{
		ID: ProfileBedrockInvokeTitanImageV2, Type: ProviderBedrock,
		Surface: SurfaceBedrockRuntime, Scheme: CredentialAWSSigV4Explicit,
		Immutable: true,
		Defaults:  ProviderCapabilities{Images: true}, Ceiling: ProviderCapabilities{Images: true},
	},
	{
		ID: ProfileBedrockAgentRerankCohere35, Type: ProviderBedrock,
		Surface: SurfaceBedrockAgentRuntime, Scheme: CredentialAWSSigV4Explicit,
		Immutable: true,
		Defaults:  ProviderCapabilities{Rerank: true}, Ceiling: ProviderCapabilities{Rerank: true},
	},
	{
		ID: ProfileBedrockAsyncNovaReel, Type: ProviderBedrock,
		Surface: SurfaceBedrockRuntime, Scheme: CredentialAWSSigV4Explicit,
		Immutable: true,
		Defaults:  ProviderCapabilities{AsyncGenerate: true}, Ceiling: ProviderCapabilities{AsyncGenerate: true},
	},
	{
		// The Bedrock Mantle profiles keep ceiling == defaults on purpose. Their
		// sets are fixed by the build and widening one is a separate contract
		// review.
		ID: ProfileBedrockMantleOpenAIChat, Type: ProviderBedrock,
		Surface: SurfaceBedrockMantle, Scheme: CredentialBedrockAPIKey,
		Immutable: true,
		Defaults:  mantleOpenAIChatSet, Ceiling: mantleOpenAIChatSet,
	},
	{
		// Phase 1C deliberately exposes only the stateless Responses subset. The
		// current canonical response mapper cannot preserve reasoning items, which
		// is the one capability this row does not share with the chat profile.
		ID: ProfileBedrockMantleOpenAIResponses, Type: ProviderBedrock,
		Surface: SurfaceBedrockMantle, Scheme: CredentialBedrockAPIKey,
		Immutable: true,
		Defaults:  mantleOpenAIResponsesSet, Ceiling: mantleOpenAIResponsesSet,
	},
	{
		ID: ProfileBedrockMantleAnthropicMessages, Type: ProviderBedrock,
		Surface: SurfaceBedrockMantle, Scheme: CredentialBedrockAPIKey,
		Immutable: true,
		Defaults:  mantleAnthropicSet, Ceiling: mantleAnthropicSet,
	},
}

var (
	deepSeekSet = ProviderCapabilities{
		Chat: true, Streaming: true, Tools: true, JSONMode: true,
		Reasoning: true, StreamUsage: true,
	}
	openAICompatibleSet = ProviderCapabilities{Chat: true, Streaming: true, Embeddings: true}
	geminiTextSet       = ProviderCapabilities{Chat: true, Streaming: true, Embeddings: true, DeveloperRole: true}
	bedrockConverseSet  = ProviderCapabilities{Chat: true, Streaming: true, StreamUsage: true}
	titanEmbedSet       = ProviderCapabilities{Embeddings: true, MaxContextTokens: 8192}

	mantleOpenAIChatSet = ProviderCapabilities{
		Chat: true, Streaming: true, Tools: true, Vision: true, JSONMode: true,
		DeveloperRole: true, Reasoning: true, StreamUsage: true,
	}
	mantleOpenAIResponsesSet = ProviderCapabilities{
		Chat: true, Streaming: true, Tools: true, Vision: true, JSONMode: true,
		DeveloperRole: true, StreamUsage: true,
	}
	mantleAnthropicSet = ProviderCapabilities{
		Chat: true, Streaming: true, Tools: true, Vision: true,
		Reasoning: true, StreamUsage: true,
	}
)

func withProviderExecutedTools(base ProviderCapabilities) ProviderCapabilities {
	base.ProviderExecutedTools = true
	return base
}

// providerTypeRow is what a provider type implies before a profile is chosen.
//
// LegacyDefaults is not the default profile's Defaults, and the difference is
// deliberate rather than an oversight: Anthropic's profile row adds files and
// batches, this one does not. Two callers still start a connection from the type
// alone — bootstrap and the store's fill-in for a stored instance carrying no
// capabilities — and widening what they produce is a behaviour change, not a
// refactor. TestTypeDefaultsWithinDefaultProfile pins the relationship that must
// hold either way: the type-level set may be narrower than the profile's, never
// wider.
type providerTypeRow struct {
	Type           ProviderType
	DefaultProfile ProviderProfileID
	LegacyDefaults ProviderCapabilities
}

var providerTypeTable = []providerTypeRow{
	{ProviderOpenAI, ProfileOpenAIChatEmbeddings, openAIChatSet},
	{ProviderAnthropic, ProfileAnthropicMessages, ProviderCapabilities{
		Chat: true, Streaming: true, Tools: true, Vision: true,
		JSONMode: true, Reasoning: true, StreamUsage: true,
	}},
	{ProviderAzureOpenAI, ProfileAzureChatEmbeddings, openAIChatSet},
	{ProviderDeepSeek, ProfileDeepSeekChat, deepSeekSet},
	{ProviderOpenAICompatible, ProfileOpenAICompatible, openAICompatibleSet},
	{ProviderGemini, ProfileGeminiText, geminiTextSet},
	{ProviderBedrock, ProfileBedrockConverseText, bedrockConverseSet},
}

var profileIndex = func() map[ProviderProfileID]profileRow {
	index := make(map[ProviderProfileID]profileRow, len(profileTable))
	for _, row := range profileTable {
		index[row.ID] = row
	}
	return index
}()

var providerTypeIndex = func() map[ProviderType]providerTypeRow {
	index := make(map[ProviderType]providerTypeRow, len(providerTypeTable))
	for _, row := range providerTypeTable {
		index[row.Type] = row
	}
	return index
}()

// ProviderProfileSummary is one row of the matrix, for callers outside this
// package that need to present or walk it rather than resolve a single profile.
type ProviderProfileSummary struct {
	ID               ProviderProfileID
	Type             ProviderType
	AccessSurface    AccessSurface
	CredentialScheme CredentialScheme
	Immutable        bool
	Defaults         ProviderCapabilities
	Ceiling          ProviderCapabilities
}

// AllProviderProfiles returns every registered profile in table order.
//
// Callers that build something per profile — the Admin metadata endpoint, an
// invariant test — must walk this rather than keep a list of their own, because
// a private list cannot be told when a profile is added.
func AllProviderProfiles() []ProviderProfileSummary {
	summaries := make([]ProviderProfileSummary, 0, len(profileTable))
	for _, row := range profileTable {
		summaries = append(summaries, ProviderProfileSummary{
			ID: row.ID, Type: row.Type, AccessSurface: row.Surface,
			CredentialScheme: row.Scheme, Immutable: row.Immutable,
			Defaults: row.Defaults, Ceiling: row.Ceiling,
		})
	}
	return summaries
}

// AllProviderTypes returns every provider type in table order, each with the
// profile a connection of that type starts on.
func AllProviderTypes() []ProviderType {
	types := make([]ProviderType, 0, len(providerTypeTable))
	for _, row := range providerTypeTable {
		types = append(types, row.Type)
	}
	return types
}

// The capability key list is already exported as CapabilityNames in
// invocation_target.go; this file does not add a second one.

// CapabilityRequiresChat returns the capabilities that cannot be enabled without
// chat. It is the same rule ProviderCapabilities.Validate enforces; exporting it
// lets a caller present the dependency instead of discovering it on refusal.
func CapabilityRequiresChat() []string {
	return []string{
		"streaming", "tools", "vision", "json_mode",
		"developer_role", "reasoning", "stream_usage", "provider_executed_tools",
	}
}
