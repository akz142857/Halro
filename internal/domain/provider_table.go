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
//
// Withheld says this build does not offer the profile: it is absent from the
// served matrix and refused on every write, so no connection or credential can
// be created on it. The row stays because the profile is still implemented and
// still bound by the invariant tests that walk this table — withholding scopes
// what an operator may reach, it does not retire the profile. It is deliberately
// not a read-path gate: an install that already holds a withheld connection has
// to start in order for the operator to delete it, and refusing to resolve a
// stored row would take that away.
type profileRow struct {
	ID              ProviderProfileID
	Type            ProviderType
	Surface         AccessSurface
	Scheme          CredentialScheme
	BaseURLTemplate string
	Immutable       bool
	Withheld        bool
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
	// Both JSON halves are at the OpenAI ceiling and both are prefilled, because
	// which of them a given OpenAI model serves is a per-model fact rather than a
	// per-connection one: everything from gpt-4o-2024-08-06 onward enforces a
	// schema, and the models before it take json_object only. The catalogue
	// carries the per-model claim and detection verifies it; the connection-level
	// answer is "this account reaches models that do both".
	openAIChatSet = ProviderCapabilities{
		Chat: true, Streaming: true, Embeddings: true, Tools: true,
		Vision: true, FetchedImage: true, JSONObject: true, StructuredOutputs: true,
		DeveloperRole: true, Reasoning: true,
		StreamUsage: true,
	}
	// Files and batches ride with the Anthropic connection because Anthropic
	// serves both on the same credential. The file half is stored by Halro and
	// never uploaded — Anthropic batches carry their requests inline — so the
	// capability says the deployment can be given a file, not that Anthropic will
	// hold one. See ADR 0021.
	//
	// No JSONObject: Anthropic has no schema-less JSON mode. Messages carries a
	// schema through output_config.format and enforces it, which is exactly
	// structured_outputs and nothing else. Declaring the absent half used to be a
	// per-profile request-field rule; it is a capability now, so a json_object
	// request is refused by the same filter that refuses a target with no chat.
	anthropicMessagesSet = ProviderCapabilities{
		Chat: true, Streaming: true, Tools: true, Vision: true, FetchedImage: true,
		StructuredOutputs: true,
		Reasoning:         true, StreamUsage: true, Files: true, Batches: true,
	}
	// The Responses profile is the same account reached through a different
	// endpoint, and the difference in what it can do is the point of it being a
	// separate profile rather than a flag on the one above.
	//
	// Provider-executed tools are here and nowhere else on OpenAI: web search is
	// a Responses tool, and running it means the upstream originates network
	// calls that never pass through SafeTransport — so it sits at the ceiling and
	// not in the defaults, the same shape the Anthropic profile uses.
	//
	// Absent on purpose: streaming, because this profile binds no stream
	// primitive; embeddings, which live on the chat profile; and reasoning,
	// because the canonical response mapper cannot preserve reasoning items and a
	// claim it cannot carry is a request that fails after the budget is reserved.
	openAIResponsesSet = ProviderCapabilities{
		Chat: true, Tools: true, Vision: true, FetchedImage: true,
		JSONObject: true, StructuredOutputs: true, DeveloperRole: true,
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
		// Ordered after the chat profile deliberately: the two share one (type,
		// surface, scheme), and a stored credential resolves to the first match.
		// Moving this row above it would silently re-point every existing OpenAI
		// connection at a different endpoint.
		ID: ProfileOpenAIResponses, Type: ProviderOpenAI,
		Surface: SurfaceOpenAI, Scheme: CredentialBearerStatic,
		BaseURLTemplate: "https://api.openai.com",
		Defaults:        openAIResponsesSet,
		Ceiling:         withProviderExecutedTools(openAIResponsesSet),
	},
	{
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
		// Vision is reachable but not assumed. DeepSeek serves images on one model
		// and answers every other one with a 400, so the ceiling admits it and the
		// defaults do not: a connection claims what its models do, and the model
		// catalogue is where that per-model claim is recorded. Widening the
		// defaults instead would have every DeepSeek connection assert vision on
		// the strength of a single experimental model.
		ID: ProfileDeepSeekChat, Type: ProviderDeepSeek,
		Surface: SurfaceDeepSeek, Scheme: CredentialBearerStatic,
		BaseURLTemplate: "https://api.deepseek.com",
		Defaults:        deepSeekSet,
		Ceiling:         withVision(deepSeekSet),
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
		//
		// Withheld with the four rows below it: this build supports Bedrock
		// through the Mantle surface only. The five Runtime and Agent Runtime
		// profiles reach two other hosts on a different credential scheme, and
		// offering them means offering three Bedrock connection shapes that share
		// nothing but a name. Removing the withholding is one field per row.
		ID: ProfileBedrockConverseText, Type: ProviderBedrock,
		Surface: SurfaceBedrockRuntime, Scheme: CredentialAWSSigV4Explicit,
		BaseURLTemplate: bedrockRuntimeEndpoint,
		Withheld:        true,
		Defaults:        bedrockConverseSet, Ceiling: bedrockConverseSet,
	},
	{
		ID: ProfileBedrockInvokeTitanEmbedV2, Type: ProviderBedrock,
		Surface: SurfaceBedrockRuntime, Scheme: CredentialAWSSigV4Explicit,
		BaseURLTemplate: bedrockRuntimeEndpoint,
		Immutable:       true,
		Withheld:        true,
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
		Withheld:        true,
		Defaults:        ProviderCapabilities{Images: true}, Ceiling: ProviderCapabilities{Images: true},
	},
	{
		ID: ProfileBedrockAgentRerankCohere35, Type: ProviderBedrock,
		Surface: SurfaceBedrockAgentRuntime, Scheme: CredentialAWSSigV4Explicit,
		BaseURLTemplate: bedrockAgentRuntimeEndpoint,
		Immutable:       true,
		Withheld:        true,
		Defaults:        ProviderCapabilities{Rerank: true}, Ceiling: ProviderCapabilities{Rerank: true},
	},
	{
		ID: ProfileBedrockAsyncNovaReel, Type: ProviderBedrock,
		Surface: SurfaceBedrockRuntime, Scheme: CredentialAWSSigV4Explicit,
		BaseURLTemplate: bedrockRuntimeEndpoint,
		Immutable:       true,
		Withheld:        true,
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
	{
		// The three MiniMax rows share one surface and one scheme, so they form a
		// single connection group and one key binds all three. The Anthropic row
		// leads because it is the face MiniMax itself recommends and the one whose
		// usage reporting is complete — input, output and both cache tiers. The
		// Chat face documents only total_tokens, which is the worse anchor for a
		// connection whose settlement depends on the split.
		//
		// The endpoint prefill is the international host. Mainland accounts live
		// on https://api.minimaxi.com with the same paths, the same headers and
		// the same bodies; only the address differs, and keys are not
		// interchangeable between the two. That is a base URL an operator edits,
		// not a second profile: splitting one contract into two rows would create
		// two truths, and one of them would go stale first.
		ID: ProfileMiniMaxAnthropicMessages, Type: ProviderMiniMax,
		Surface: SurfaceMiniMax, Scheme: CredentialBearerStatic,
		BaseURLTemplate: "https://api.minimax.io",
		Defaults:        minimaxAnthropicSet, Ceiling: minimaxAnthropicSet,
	},
	{
		ID: ProfileMiniMaxChat, Type: ProviderMiniMax,
		Surface: SurfaceMiniMax, Scheme: CredentialBearerStatic,
		BaseURLTemplate: "https://api.minimax.io",
		Defaults:        minimaxChatSet, Ceiling: minimaxChatSet,
	},
	{
		ID: ProfileMiniMaxResponses, Type: ProviderMiniMax,
		Surface: SurfaceMiniMax, Scheme: CredentialBearerStatic,
		BaseURLTemplate: "https://api.minimax.io",
		Defaults:        minimaxResponsesSet, Ceiling: minimaxResponsesSet,
	},
	{
		// The three Kimi rows share one surface and one scheme, so they form a
		// single connection group and one key binds all three.
		//
		// The Chat row leads, which is the opposite of MiniMax's choice and for a
		// reason MiniMax did not have: Kimi's Responses and Messages schemas pin
		// `model` to kimi-k3 alone, so a connection defaulting to either of them
		// reaches one of the four published models. Chat is the only face that
		// reaches all four. The cost is real and is recorded here rather than
		// hidden — Chat is also the only face that reports neither a cache-write
		// tier nor reasoning tokens, so the default connection is the least
		// observable one. Neither gap moves money: Kimi publishes no cache-write
		// rate, and reasoning tokens are a display split of output tokens.
		//
		// The endpoint prefill is the international host. Mainland accounts live
		// on https://api.moonshot.cn with the same paths, the same headers and
		// the same bodies; only the address differs, and keys are not
		// interchangeable between the two — Kimi's own error page says a mixed
		// pair answers 401.
		ID: ProfileKimiChat, Type: ProviderKimi,
		Surface: SurfaceKimi, Scheme: CredentialBearerStatic,
		BaseURLTemplate: "https://api.moonshot.ai",
		Defaults:        kimiChatSet, Ceiling: kimiChatSet,
	},
	{
		ID: ProfileKimiAnthropicMessages, Type: ProviderKimi,
		Surface: SurfaceKimi, Scheme: CredentialBearerStatic,
		BaseURLTemplate: "https://api.moonshot.ai",
		Defaults:        kimiAnthropicSet, Ceiling: kimiAnthropicSet,
	},
	{
		// Withheld, and the first row withheld out of the middle of an offered
		// connection group rather than as a whole group of its own. What it
		// scopes is narrow on purpose: a Kimi connection still carries Chat and
		// Anthropic Messages, so no northbound endpoint loses Kimi and no public
		// alias changes meaning. Only this one upstream face is unreachable.
		//
		// The reason is a pairing rather than a missing feature, and it is the
		// shape docs/contracts/adding-a-northbound-endpoint.md names as the third
		// step with no mechanical guard: what this face returns unasked, the
		// mapper that reads it cannot represent.
		//
		//	Kimi's /v1/responses reasons by default — its reasoning.effort ladder
		//	is low/high/max with no off value — and the portable renderer sends
		//	no reasoning member at all, because this profile's field rules refuse
		//	reasoning_effort at every value. Kimi therefore reasons on every
		//	request, returns a `reasoning` output item, and
		//	compatibility/openai.DecodeProviderResponse has no case for one: it
		//	returns an error, the attempt ends 502, and the upstream has already
		//	been paid.
		//
		// Note what that corrects. The comment on kimiResponsesSet used to say
		// the canonical mapper "drops" a reasoning item; it does not, it refuses
		// one, and the whole argument for serving this face rested on the wrong
		// verb.
		//
		// The house rule the Chat renderer and the portable Anthropic mapper both
		// follow — unspecified means off — would be the fix, and it cannot be
		// applied here. That is measured, not assumed. Both spellings were tried
		// against a real mainland account on 2026-09-02:
		//
		//	reasoning:{"effort":"none"}   -> 400, `reasoning.effort value "none"
		//	                                 is not supported`
		//	thinking:{"type":"disabled"}  -> 200, and it reasoned anyway
		//
		// The second is the one worth remembering. That exact member does switch
		// reasoning off on Kimi's Messages face — undocumented, measured, and the
		// reason this platform's Anthropic row exists at all. Here the same
		// upstream takes it and ignores it. Extrapolating from the other face
		// would have shipped a 200, a bill, and a caller who believed they had
		// turned reasoning off.
		//
		// So this row is not "unmeasured, therefore withheld". It is "measured,
		// and this face cannot be told to stop". Offering it again needs one of
		// two things, neither of them here: Kimi adding an off switch that works,
		// or the "this target always reasons" fact reaching the router, at which
		// point the profile can be offered carrying that mark rather than
		// pretending it can be switched off. See
		// docs/prd/kimi-adaptation-plan.zh-CN.md §14.5.
		ID: ProfileKimiResponses, Type: ProviderKimi,
		Surface: SurfaceKimi, Scheme: CredentialBearerStatic,
		BaseURLTemplate: "https://api.moonshot.ai",
		Withheld:        true,
		Defaults:        kimiResponsesSet, Ceiling: kimiResponsesSet,
	},
}

var (
	// No StructuredOutputs: DeepSeek serves json_object and has no schema mode.
	// This too was a per-profile request-field rule before the split.
	deepSeekSet = ProviderCapabilities{
		Chat: true, Streaming: true, Tools: true, JSONObject: true,
		Reasoning: true, StreamUsage: true,
	}
	openAICompatibleSet = ProviderCapabilities{Chat: true, Streaming: true, Embeddings: true}
	geminiTextSet       = ProviderCapabilities{Chat: true, Streaming: true, Embeddings: true, DeveloperRole: true}
	bedrockConverseSet  = ProviderCapabilities{Chat: true, Streaming: true, StreamUsage: true}
	titanEmbedSet       = ProviderCapabilities{Embeddings: true, MaxContextTokens: 8192}

	// No FetchedImage on any Mantle set: Bedrock reads an image from the bytes a
	// request carries and never retrieves one on the caller's behalf. Halro must
	// not close that gap by fetching the address itself — that would make the
	// gateway retrieve a caller-supplied URL, which is the request forgery
	// SafeTransport's allowlists exist to prevent.
	// Both JSON halves, because both is what the single json_mode bit these sets
	// carried before the split already routed here. Splitting it is a change to
	// how a request is described, not to which requests a Mantle profile accepts,
	// and narrowing either half would be a widening's mirror image — a Beta
	// ceiling moving without the contract review that pins it.
	mantleOpenAIChatSet = ProviderCapabilities{
		Chat: true, Streaming: true, Tools: true, Vision: true,
		JSONObject: true, StructuredOutputs: true,
		DeveloperRole: true, Reasoning: true, StreamUsage: true,
	}
	mantleOpenAIResponsesSet = ProviderCapabilities{
		Chat: true, Streaming: true, Tools: true, Vision: true,
		JSONObject: true, StructuredOutputs: true,
		DeveloperRole: true, StreamUsage: true,
	}
	mantleAnthropicSet = ProviderCapabilities{
		Chat: true, Streaming: true, Tools: true, Vision: true,
		Reasoning: true, StreamUsage: true,
	}

	// The MiniMax sets keep ceiling == defaults: there is no opt-in an operator
	// could reach for that Halro would be able to stand behind. Most of what is
	// here is read from published documentation; one capability — json_object on
	// the Chat face — was measured against a real account on 2026-08-31 and moved
	// on that evidence, which is what the note above minimaxChatSet records.
	//
	// Absent on purpose, and each absence is a claim about the upstream rather
	// than an oversight:
	//
	//   - Embeddings. MiniMax serves POST /v1/embeddings, but in its own shape —
	//     `texts` and `type` in, a top-level `vectors` array out, errors in
	//     `base_resp`. Declaring it would bind the OpenAI embedding primitive to
	//     a body that cannot parse it.
	//   - StructuredOutputs, and JSONObject on two of the three faces. No MiniMax
	//     document lists response_format anywhere, which is documentation being
	//     silent rather than documentation refusing, so it stays absent until a
	//     real request proves otherwise. One did, for one half on one face: the
	//     Chat face was measured serving json_object and now declares it. The
	//     schema mode was never sent, and the other two faces are different
	//     endpoints — the same silence that turned out to be wrong about one half
	//     is no evidence about the rest.
	//   - DeveloperRole. The OpenAI developer role appears nowhere in MiniMax's
	//     request schemas.
	//   - ProviderExecutedTools. MiniMax offers a server-side web search. Turning
	//     it on accepts upstream egress that never passes through SafeTransport,
	//     which is a contract review rather than a table edit.
	//
	// Vision and FetchedImage sit at the connection ceiling because only
	// MiniMax-M3 sees an image at all; the M2.x line accepts text and tool blocks
	// only. Which models see is a per-model fact, so it is recorded in the model
	// catalogue and not asserted here for the whole connection — the same shape
	// DeepSeek's row uses.
	minimaxAnthropicSet = ProviderCapabilities{
		Chat: true, Streaming: true, Tools: true, Vision: true, FetchedImage: true,
		Reasoning: true, StreamUsage: true,
	}
	// JSONObject is measured, not documented. No MiniMax page mentions
	// response_format, which is why every profile here started without it — the
	// fail-closed reading of silence. A real request on 2026-08-31 sent
	// {"type":"json_object"} and came back 200 with a valid JSON body, so the
	// capability is real on this face and withholding it was denying an operator
	// something their account does.
	//
	// StructuredOutputs stays absent: the schema mode was not sent, and the same
	// silence that turned out to be wrong about one half is no evidence about the
	// other.
	minimaxChatSet = ProviderCapabilities{
		Chat: true, Streaming: true, Tools: true, Vision: true, FetchedImage: true,
		JSONObject: true,
		Reasoning:  true, StreamUsage: true,
	}
	// Two absences here that the other two MiniMax rows do not share, and both
	// are inherited from the profile this one is served by rather than from
	// MiniMax:
	//
	//   - No Streaming. MiniMax documents `stream` on /v1/responses, so this is a
	//     Halro-side scope decision: the OpenAI adapter's Responses branch binds
	//     no stream primitive, and CapabilityDependencies requires stream_usage
	//     over streaming over chat. Reaching it means reusing the Bedrock Mantle
	//     Responses adapter, which is welded to that host's endpoint, project
	//     header and credential scheme.
	//   - No JSONObject, and this one is a scope decision rather than an upstream
	//     limit. The Chat face was measured serving json_object; this face was
	//     not, and the two are different endpoints. Declaring it here on the
	//     strength of the other face's result would be the same guess that made
	//     the Chat row wrong in the first place, pointed the other way.
	//   - No Reasoning, for the same reason ProfileOpenAIResponses has none: the
	//     canonical response mapper cannot preserve reasoning items, and a claim
	//     it cannot carry is a request that fails after the budget is reserved.
	//     MiniMax returns reasoning as output items with a summary, and the
	//     mapper **refuses** one rather than dropping it — the same wrong verb
	//     that was carried on the Kimi row until its face was measured.
	//
	//     Whether this row has Kimi's problem as well is unestablished and is
	//     recorded that way rather than assumed either direction. The chain is
	//     identical on paper: this profile refuses reasoning_effort at every
	//     value, so nothing ever sends MiniMax an off switch, and MiniMax-M3's
	//     documented default is thinking on. What is missing is a measurement of
	//     what /v1/responses actually returns on an unasked request. Kimi's
	//     equivalent turned out to reason on every call; it took a real account
	//     to know that, and no MiniMax Responses call has been made.
	minimaxResponsesSet = ProviderCapabilities{
		Chat: true, Tools: true, Vision: true, FetchedImage: true,
	}

	// The Kimi sets keep ceiling == defaults, for the same reason MiniMax's do:
	// there is no opt-in an operator could reach for that Halro would be able to
	// stand behind. These sets started from Kimi's published OpenAPI document and
	// were then corrected against a real mainland account on 2026-09-01 — the
	// rows below say which claim came from which, because on this platform the
	// two disagreed more than once.
	//
	// Absent from all three, each absence a claim about the upstream:
	//
	//   - Embeddings. Kimi publishes no embedding endpoint at all.
	//   - FetchedImage. Every image member on every face accepts exactly two
	//     forms, a data: URL or ms://<file_id>. There is no http address to
	//     fetch, and Halro must not close that gap by retrieving one itself —
	//     that is the request forgery SafeTransport's allowlists exist to
	//     prevent.
	//   - DeveloperRole. The OpenAI developer role appears in no Kimi schema.
	//   - ProviderExecutedTools. Kimi offers an official web search tool.
	//     Turning it on accepts upstream egress that never passes through
	//     SafeTransport, which is a contract review rather than a table edit.
	//
	// MaxContextTokens and MaxOutputTokens are absent here on purpose: the four
	// models differ (1M against 256K), so the bound is a per-model fact and lives
	// in the model catalogue, the same shape DeepSeek's row uses.
	//
	// Vision sits at the connection ceiling because all four published models
	// accept images; which ones also accept video is a per-model fact Halro does
	// not model at all, since the semantic content model has no video block.
	kimiChatSet = ProviderCapabilities{
		Chat: true, Streaming: true, Tools: true, Vision: true,
		JSONObject: true, StructuredOutputs: true,
		Reasoning: true, StreamUsage: true,
	}
	// The Anthropic face. StructuredOutputs because output_config.format takes a
	// json_schema and nothing else; no JSONObject for the same reason the direct
	// Anthropic profile has none.
	//
	// Reasoning is claimed and is reachable only in native mode, which is the
	// same shape MiniMax's Anthropic row has and for a related reason. Measured
	// 2026-09-01: a request carrying output_config.effort answers 200 and returns
	// a thinking block, and the portable decoder refuses a thinking block. So the
	// portable path must never ask for depth — the field rules declare
	// reasoning_effort unsupported at every value — while the capability stays
	// true, because native mode forwards the caller's own bytes and reads the
	// answer back the same way.
	kimiAnthropicSet = ProviderCapabilities{
		Chat: true, Streaming: true, Tools: true, Vision: true,
		StructuredOutputs: true,
		Reasoning:         true, StreamUsage: true,
	}
	// JSONObject is absent here and that is a difference in the upstream rather
	// than a difference in confidence: the Responses face models structured
	// output as text.format, which accepts the json_schema type alone. There is
	// no schema-less JSON mode to declare.
	//
	// Two further absences are inherited from the profile this one is served by
	// rather than from Kimi:
	//
	//   - No Streaming. Kimi documents `stream` on /v1/responses, so this is a
	//     Halro-side scope decision: the OpenAI adapter's Responses branch binds
	//     no stream primitive, and CapabilityDependencies requires stream_usage
	//     over streaming over chat.
	//   - No Reasoning, for the same reason ProfileOpenAIResponses has none: the
	//     canonical response mapper cannot preserve reasoning items, and a claim
	//     it cannot carry is a request that fails after the budget is reserved.
	//     Kimi returns reasoning as an output item, and the mapper **refuses**
	//     one — it does not drop it. The verb is the whole difference and this
	//     line had it wrong: not declaring the capability keeps a caller from
	//     asking for reasoning, and it does nothing at all about reasoning that
	//     arrives unasked, which on this face is every single response. That is
	//     why the profile row above is withheld rather than merely unadorned.
	kimiResponsesSet = ProviderCapabilities{
		Chat: true, Tools: true, Vision: true,
		StructuredOutputs: true,
	}
)

func withProviderExecutedTools(base ProviderCapabilities) ProviderCapabilities {
	base.ProviderExecutedTools = true
	return base
}

// withVision is the DeepSeek ceiling opt-in. It carries both halves because the
// one DeepSeek model that sees an image accepts it either way — inline as a data
// URL, or as an https address it retrieves itself.
func withVision(base ProviderCapabilities) ProviderCapabilities {
	base.Vision = true
	base.FetchedImage = true
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
		StructuredOutputs: true, Reasoning: true, StreamUsage: true,
	}},
	{ProviderAzureOpenAI, ProfileAzureChatEmbeddings, openAIChatSet},
	{ProviderDeepSeek, ProfileDeepSeekChat, deepSeekSet},
	{ProviderOpenAICompatible, ProfileOpenAICompatible, openAICompatibleSet},
	{ProviderGemini, ProfileGeminiText, geminiTextSet},
	// Mantle Chat leads Bedrock because the Runtime profiles are withheld: the
	// default has to be a profile a new connection can actually be created on.
	// LegacyDefaults stays the narrower Converse set — it is a floor for the two
	// callers that start from the type alone, and TestTypeDefaultsWithinDefaultProfile
	// only requires it to sit inside the default profile's own defaults.
	{ProviderBedrock, ProfileBedrockMantleChat, bedrockConverseSet},
	{ProviderMiniMax, ProfileMiniMaxAnthropicMessages, minimaxAnthropicSet},
	// LegacyDefaults equals the default profile's own defaults. The two callers
	// that start from the type alone get exactly what a new Kimi connection
	// gets, which is the simplest way to satisfy
	// TestTypeDefaultsWithinDefaultProfile — that test only requires the
	// type-level set to be no wider.
	{ProviderKimi, ProfileKimiChat, kimiChatSet},
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
	// Withheld travels with the row rather than being a second list a caller
	// keeps: AllProviderProfiles stays the one enumeration, and whoever presents
	// the matrix decides what to do with a withheld row. See profileRow.
	Withheld bool
	Defaults ProviderCapabilities
	Ceiling  ProviderCapabilities
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
			Immutable: row.Immutable, Withheld: row.Withheld,
			Defaults: row.Defaults, Ceiling: row.Ceiling,
		})
	}
	return summaries
}

// IsRegisteredProviderType reports whether this build has a provider type.
//
// It reads the table rather than a list of its own, which is the whole point.
// The Admin write path used to answer this from a hand-written switch, and that
// switch was the third copy of the type list — after the profile table and
// ProviderInstance.Validate. A third copy cannot be told when it falls behind,
// and it did: MiniMax was registered everywhere else, offered by the console's
// own metadata endpoint, and refused on save with "provider type is not
// implemented". The console listed a type its server would not accept.
//
// Withholding is a separate question and stays separate: it scopes profiles,
// not types, and the write path checks IsWithheldProfile on the resolved
// profile. A type whose default profile is withheld is refused there, where the
// reason can be stated.
func IsRegisteredProviderType(value ProviderType) bool {
	_, ok := providerTypeIndex[value]
	return ok
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
		"streaming": {"chat"},
		"tools":     {"chat"},
		"vision":    {"chat"},
		// Fetching is a mode of seeing: a target that cannot read an image has
		// nothing to fetch one for. Same shape as stream_usage over streaming.
		"fetched_image": {"vision"},
		// Two capabilities, not one over the other: Anthropic enforces a schema
		// and has no schema-less mode, so structured_outputs cannot be made to
		// depend on json_object without describing Anthropic wrongly.
		"json_object":             {"chat"},
		"structured_outputs":      {"chat"},
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
