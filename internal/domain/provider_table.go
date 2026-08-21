package domain

import "strings"

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
//
// BaseURLTemplate is the endpoint a new connection on this profile is offered.
// It is a prefill, not a bound: an operator may enter any endpoint, and the
// outbound host allowlist is derived from the saved connection rather than from
// this. RegionPlaceholder, where present, is substituted from configuration —
// the region is a deployment choice, unlike everything else in this row.
type profileRow struct {
	ID              ProviderProfileID
	Type            ProviderType
	Surface         AccessSurface
	Scheme          CredentialScheme
	BaseURLTemplate string
	Immutable       bool
	Defaults        ProviderCapabilities
	Ceiling         ProviderCapabilities
}

// RegionPlaceholder is what BaseURLTemplate carries where a deployment's own
// region belongs. ResolveBaseURL replaces it; a template without it is returned
// unchanged.
const RegionPlaceholder = "{region}"

// The three Bedrock access surfaces are addressed by different hosts, which is
// why the endpoint belongs to the profile rather than to the provider type: one
// value per type cannot say all three.
const (
	bedrockRuntimeEndpoint      = "https://bedrock-runtime." + RegionPlaceholder + ".amazonaws.com"
	bedrockAgentRuntimeEndpoint = "https://bedrock-agent-runtime." + RegionPlaceholder + ".amazonaws.com"
	bedrockMantleEndpoint       = "https://bedrock-mantle." + RegionPlaceholder + ".api.aws"
)

// ResolveBaseURL fills a profile's endpoint template in for one deployment.
//
// An unregistered profile has no endpoint to offer and returns "", which the
// caller shows as an empty field rather than guessing — a wrong prefilled
// endpoint is worse than none, because it is the field an operator is least
// likely to check.
func ResolveBaseURL(profileID ProviderProfileID, region string) string {
	row, ok := profileIndex[profileID]
	if !ok || row.BaseURLTemplate == "" {
		return ""
	}
	return strings.ReplaceAll(row.BaseURLTemplate, RegionPlaceholder, region)
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
		BaseURLTemplate: "https://api.openai.com",
		Defaults:        openAIChatSet, Ceiling: openAIChatSet,
	},
	{
		// JSONMode covers Anthropic's schema-backed structured outputs, which the
		// Messages profile carries through output_config.format.
		ID: ProfileAnthropicMessages, Type: ProviderAnthropic,
		Surface: SurfaceAnthropic, Scheme: CredentialAnthropicAPIKey,
		BaseURLTemplate: "https://api.anthropic.com",
		Defaults:        anthropicMessagesSet,
		Ceiling:         withProviderExecutedTools(anthropicMessagesSet),
	},
	{
		// No endpoint to offer: an Azure OpenAI deployment lives on the resource's
		// own host. Prefilling api.openai.com there was inherited from the console
		// and was wrong in the worst way a prefill can be — plausible, and the one
		// field an operator is least likely to re-read before saving.
		ID: ProfileAzureChatEmbeddings, Type: ProviderAzureOpenAI,
		Surface: SurfaceAzureOpenAI, Scheme: CredentialAzureAPIKey,
		Defaults: openAIChatSet, Ceiling: openAIChatSet,
	},
	{
		ID: ProfileDeepSeekChat, Type: ProviderDeepSeek,
		Surface: SurfaceDeepSeek, Scheme: CredentialBearerStatic,
		BaseURLTemplate: "https://api.deepseek.com",
		Defaults:        deepSeekSet, Ceiling: deepSeekSet,
	},
	{
		// Compatibility servers vary. These conservative defaults preserve the two
		// core OpenAI endpoints; anything else has to be established per server,
		// which capability detection does — not asserted in a table.
		// Also no endpoint: a compatibility server is by definition somewhere else,
		// and the whole point of this type is that Halro does not know where.
		ID: ProfileOpenAICompatible, Type: ProviderOpenAICompatible,
		Surface: SurfaceOpenAICompatible, Scheme: CredentialBearerStatic,
		Defaults: openAICompatibleSet, Ceiling: openAICompatibleSet,
	},
	{
		// Beta profile intentionally declares only the translated text subset.
		ID: ProfileGeminiText, Type: ProviderGemini,
		Surface: SurfaceGemini, Scheme: CredentialGoogleAPIKey,
		BaseURLTemplate: "https://generativelanguage.googleapis.com",
		Defaults:        geminiTextSet, Ceiling: geminiTextSet,
	},
	{
		// Beta profile intentionally declares only Converse text chat and usage.
		ID: ProfileBedrockConverseText, Type: ProviderBedrock,
		Surface: SurfaceBedrockRuntime, Scheme: CredentialAWSSigV4Explicit,
		BaseURLTemplate: bedrockRuntimeEndpoint,
		Defaults:        bedrockConverseSet, Ceiling: bedrockConverseSet,
	},
	{
		ID: ProfileBedrockInvokeTitanEmbedV2, Type: ProviderBedrock,
		Surface: SurfaceBedrockRuntime, Scheme: CredentialAWSSigV4Explicit,
		BaseURLTemplate: bedrockRuntimeEndpoint,
		Immutable:       true,
		Defaults:        titanEmbedSet, Ceiling: titanEmbedSet,
	},
	{
		ID: ProfileOpenAIMediaResources, Type: ProviderOpenAI,
		Surface: SurfaceOpenAI, Scheme: CredentialBearerStatic,
		BaseURLTemplate: "https://api.openai.com",
		Immutable:       true,
		Defaults:        openAIMediaSet, Ceiling: openAIMediaSet,
	},
	{
		ID: ProfileBedrockInvokeTitanImageV2, Type: ProviderBedrock,
		Surface: SurfaceBedrockRuntime, Scheme: CredentialAWSSigV4Explicit,
		BaseURLTemplate: bedrockRuntimeEndpoint,
		Immutable:       true,
		Defaults:        ProviderCapabilities{Images: true}, Ceiling: ProviderCapabilities{Images: true},
	},
	{
		ID: ProfileBedrockAgentRerankCohere35, Type: ProviderBedrock,
		Surface: SurfaceBedrockAgentRuntime, Scheme: CredentialAWSSigV4Explicit,
		BaseURLTemplate: bedrockAgentRuntimeEndpoint,
		Immutable:       true,
		Defaults:        ProviderCapabilities{Rerank: true}, Ceiling: ProviderCapabilities{Rerank: true},
	},
	{
		ID: ProfileBedrockAsyncNovaReel, Type: ProviderBedrock,
		Surface: SurfaceBedrockRuntime, Scheme: CredentialAWSSigV4Explicit,
		BaseURLTemplate: bedrockRuntimeEndpoint,
		Immutable:       true,
		Defaults:        ProviderCapabilities{AsyncGenerate: true}, Ceiling: ProviderCapabilities{AsyncGenerate: true},
	},
	{
		// The Bedrock Mantle profiles keep ceiling == defaults on purpose. Their
		// sets are fixed by the build and widening one is a separate contract
		// review.
		//
		// The chat and responses sets are shared by both route profiles: the route
		// decides which models are reachable, not what the wire shape can express.
		// Which models a route carries is recorded in the model catalogue, and a
		// model's own capabilities are narrowed from this ceiling by detection.
		ID: ProfileBedrockMantleChat, Type: ProviderBedrock,
		Surface: SurfaceBedrockMantle, Scheme: CredentialBedrockAPIKey,
		BaseURLTemplate: bedrockMantleEndpoint,
		Immutable:       true,
		Defaults:        mantleOpenAIChatSet, Ceiling: mantleOpenAIChatSet,
	},
	{
		ID: ProfileBedrockMantleOpenAIChat, Type: ProviderBedrock,
		Surface: SurfaceBedrockMantle, Scheme: CredentialBedrockAPIKey,
		BaseURLTemplate: bedrockMantleEndpoint,
		Immutable:       true,
		Defaults:        mantleOpenAIChatSet, Ceiling: mantleOpenAIChatSet,
	},
	{
		// Phase 1C deliberately exposes only the stateless Responses subset. The
		// current canonical response mapper cannot preserve reasoning items, which
		// is the one capability this row does not share with the chat profile.
		ID: ProfileBedrockMantleResponses, Type: ProviderBedrock,
		Surface: SurfaceBedrockMantle, Scheme: CredentialBedrockAPIKey,
		BaseURLTemplate: bedrockMantleEndpoint,
		Immutable:       true,
		Defaults:        mantleOpenAIResponsesSet, Ceiling: mantleOpenAIResponsesSet,
	},
	{
		ID: ProfileBedrockMantleOpenAIResponses, Type: ProviderBedrock,
		Surface: SurfaceBedrockMantle, Scheme: CredentialBedrockAPIKey,
		BaseURLTemplate: bedrockMantleEndpoint,
		Immutable:       true,
		Defaults:        mantleOpenAIResponsesSet, Ceiling: mantleOpenAIResponsesSet,
	},
	{
		ID: ProfileBedrockMantleAnthropicMessages, Type: ProviderBedrock,
		Surface: SurfaceBedrockMantle, Scheme: CredentialBedrockAPIKey,
		BaseURLTemplate: bedrockMantleEndpoint,
		Immutable:       true,
		Defaults:        mantleAnthropicSet, Ceiling: mantleAnthropicSet,
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
	BaseURLTemplate  string
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
			CredentialScheme: row.Scheme, BaseURLTemplate: row.BaseURLTemplate,
			Immutable: row.Immutable,
			Defaults:  row.Defaults, Ceiling: row.Ceiling,
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

// CapabilityDependencies returns, per capability, what has to be on with it.
//
// This is the whole rule, not one half of it. It used to be a flat "these
// require chat" list, which was wrong twice: it claimed to mirror
// ProviderInstance.Validate, which only enforces streaming→chat, and it left out
// stream_usage→streaming, which modelcatalog.ValidateDependencies does enforce —
// so a form built from it could offer stream usage without streaming and have
// the deployment refuse it later.
//
// Dependencies are direct, not transitive: a caller enabling stream_usage has to
// walk to streaming and from there to chat. Keeping them direct is what lets a
// caller present the reason ("streaming needs chat") rather than a flat set.
func CapabilityDependencies() map[string][]string {
	return map[string][]string{
		"streaming":               {"chat"},
		"tools":                   {"chat"},
		"vision":                  {"chat"},
		"json_mode":               {"chat"},
		"developer_role":          {"chat"},
		"reasoning":               {"chat"},
		"provider_executed_tools": {"chat"},
		"stream_usage":            {"streaming"},
	}
}

// CapabilityOptInWarnings returns the capabilities whose consequence a checkbox
// does not show.
//
// provider_executed_tools is the one: enabling it accepts that the upstream may
// run tools of its own and reach the network to do so, and that traffic never
// passes through SafeTransport — no host allowlist, no DNS pinning, nothing in
// the audit trail. Every other capability changes what Halro will relay; this
// one changes who else gets to make requests. A ceiling that permits it and a
// default that leaves it off is only half the answer: the operator turning it on
// has to be told what they are accepting, at the moment they do it.
//
// It is a list of keys, not sentences. What to say about it belongs to whoever
// renders it, in the reader's language.
func CapabilityOptInWarnings() []string {
	return []string{"provider_executed_tools"}
}
