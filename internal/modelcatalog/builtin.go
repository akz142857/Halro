package modelcatalog

import (
	"sync"

	"github.com/akz142857/Halro/internal/domain"
)

// The seeding policy, stated so later additions are held to it.
//
// An entry may be added only when Halro can point at evidence for it, and there
// are two admissible kinds:
//
//  1. A profile that already pins its model. invoke_titan_embedding.go and
//     inference_resources.go reject any other model for four Bedrock profiles,
//     so "profile P implies model M" is enforced code, not a recollection about
//     a vendor's line-up. For those, the model's capabilities are the profile's.
//
//  2. An exact model identifier whose operation support is reviewed here the
//     way code is, recorded with the operations it claims and nothing else.
//     §6.1 of docs/prd/model-aware-capability-selection.zh-CN.md asks for
//     exactly this: a versioned built-in list of precise model IDs. §6.2 bounds
//     it — exact IDs only, no prefix ever promotes an unknown future model, and
//     a moving alias qualifies only where Halro itself pins it.
//
// What the second kind is not: a guess from a model's name, and not a claim
// that anyone executed. Every entry here carries `declared` evidence, because
// SourceBuiltin.MaxEvidence() caps it there — only a real probe produces
// `verified`. An entry says "Halro is willing to offer this pre-checked", not
// "Halro watched it work".
//
// Two rules keep a wrong entry from becoming a trap:
//
//   - Claim operations, not the ceiling. An entry lists the operations the model
//     performs. Everything the profile could carry but this model does not do is
//     simply absent, and Validate() already refuses an entry that exceeds its
//     profile.
//   - An operator can still say otherwise. A model this catalog covers can be
//     narrowed freely, and widened with an explicit `operator_declared` — which
//     is then recorded as the operator's claim, not as this catalog's. An entry
//     that under-claims costs an operator one deliberate declaration; it does
//     not brick their deployment.
//
// Growing this file is the point; guessing at it is not.
var builtinOnce = sync.OnceValues(func() (*Catalog, error) {
	return New(concat(
		pinnedBedrockProfiles(),
		openAIChatModels(),
		openAIResponsesModels(),
		openAIEmbeddingModels(),
		openAIMediaModels(),
		deepSeekModels(),
		anthropicModels(),
		geminiModels(),
		bedrockConverseModels(),
		bedrockMantleModels(),
		openAICompatibleModels(),
		minimaxModels(),
		kimiModels(),
	)...)
})

func concat(groups ...[]Entry) []Entry {
	var all []Entry
	for _, group := range groups {
		all = append(all, group...)
	}
	return all
}

// Builtin returns the shipped catalog. It panics only if the catalog fails its
// own validation, which is a build-time defect rather than an operating
// condition; TestBuiltinCatalogValidates covers it.
func Builtin() *Catalog {
	catalog, err := builtinOnce()
	if err != nil {
		panic("modelcatalog: builtin catalog is invalid: " + err.Error())
	}
	return catalog
}

// pinnedBedrockProfiles are the four profiles that accept exactly one model.
// Capabilities come from the profile ceiling rather than being restated, so
// narrowing the profile cannot leave a wider claim behind in the catalog.
func pinnedBedrockProfiles() []Entry {
	return []Entry{
		pinnedProfileEntry(domain.ProviderBedrock, domain.ProfileBedrockInvokeTitanEmbedV2, "amazon.titan-embed-text-v2:0"),
		pinnedProfileEntry(domain.ProviderBedrock, domain.ProfileBedrockInvokeTitanImageV2, "amazon.titan-image-generator-v2:0"),
		pinnedProfileEntry(domain.ProviderBedrock, domain.ProfileBedrockAgentRerankCohere35, "cohere.rerank-v3-5:0"),
		pinnedProfileEntry(domain.ProviderBedrock, domain.ProfileBedrockAsyncNovaReel, "amazon.nova-reel-v1:0"),
	}
}

func pinnedProfileEntry(providerType domain.ProviderType, profile domain.ProviderProfileID, model string) Entry {
	key := Key{ProviderType: providerType, Profile: profile, TargetKind: defaultTargetKind(providerType, profile), Model: model}
	return Entry{
		Key:          key,
		Status:       StatusKnown,
		Source:       SourceBuiltin,
		Capabilities: key.Ceiling(),
	}
}

// builtinEntry declares one reviewed model, exactly as written. Capabilities are
// deliberately *not* clamped to the profile ceiling here.
//
// Clamping looked like the safe choice and was the opposite. An entry claiming
// something its profile cannot carry would have been silently trimmed and then
// validated cleanly, so the console would show a model missing a capability
// somebody had written down, with nothing anywhere saying why. Validate()
// refuses the entry instead, which makes it a build failure.
//
// What that failure means is worth stating, because the reflex is to delete the
// offending capability: a model whose own capabilities do not fit inside one
// profile is Halro's profile split failing to match a real model. A deployment
// carries one model's own capabilities and nothing else — composing several into
// one outward-facing model is the route layer's job, since several routes may
// share one public alias — so the answer is a second entry on the profile that
// does carry it, not a quieter first one.
func builtinEntry(providerType domain.ProviderType, profile domain.ProviderProfileID, model string,
	capabilities domain.ProviderCapabilities) Entry {
	return Entry{
		Key:          Key{ProviderType: providerType, Profile: profile, TargetKind: defaultTargetKind(providerType, profile), Model: model},
		Status:       StatusKnown,
		Source:       SourceBuiltin,
		Capabilities: capabilities,
	}
}

// chat is the shared shape of an OpenAI-family text model: the core operation
// plus the enhancements every current chat model on this profile carries.
// Anything a specific model does beyond this is added at its entry, and
// anything it does not do is left off there.
// json_object is in the shared shape and structured_outputs is not, because the
// schema-less mode is the half every model on this profile family serves —
// OpenAI, DeepSeek and Mantle alike — while an enforced schema is a per-model
// claim. Adding it here would credit DeepSeek, whose profile has no schema mode
// at its ceiling at all.
func chat(contextTokens, outputTokens int64) domain.ProviderCapabilities {
	return domain.ProviderCapabilities{
		Chat: true, Streaming: true, StreamUsage: true, Tools: true, JSONObject: true,
		MaxContextTokens: contextTokens, MaxOutputTokens: outputTokens,
	}
}

func with(base domain.ProviderCapabilities, apply ...func(*domain.ProviderCapabilities)) domain.ProviderCapabilities {
	for _, mutate := range apply {
		mutate(&base)
	}
	return base
}

// vision credits both halves. Every model this catalog covers with vision is on
// a platform that retrieves an address as readily as it reads bytes — OpenAI,
// Anthropic direct, DeepSeek. A platform that reads only bytes has no
// fetched_image at its ceiling, and Clamp takes it back off there, so the entry
// does not have to know which platform it will be resolved against.
func vision(capabilities *domain.ProviderCapabilities) {
	capabilities.Vision = true
	capabilities.FetchedImage = true
}

// visionInline is vision without the fetch. Bedrock reads an image from the
// bytes a request carries and retrieves nothing, so an entry resolved against a
// Mantle profile may not claim fetched_image — the profile's ceiling does not
// carry it, and Validate refuses an entry that exceeds its ceiling.
func visionInline(capabilities *domain.ProviderCapabilities)  { capabilities.Vision = true }
func reasoning(capabilities *domain.ProviderCapabilities)     { capabilities.Reasoning = true }
func developerRole(capabilities *domain.ProviderCapabilities) { capabilities.DeveloperRole = true }

// structuredOutputs is the schema half. Every OpenAI model this catalog covers
// is gpt-4o or later and OpenAI documents an enforced json_schema from
// gpt-4o-2024-08-06 onward, which is what the `gpt-4o` alias resolves to; the
// models that took json_object and nothing else are older than every identifier
// seeded here. It is a modifier rather than part of chat() because the same
// shared shape describes DeepSeek, which has no schema mode.
func structuredOutputs(capabilities *domain.ProviderCapabilities) {
	capabilities.StructuredOutputs = true
}

// openAIChatModels covers the text models reachable through the OpenAI chat and
// embeddings profile.
//
// Where the reviewed claim is narrower than the profile: gpt-4o and gpt-4o-mini
// are not reasoning models and are not credited with `reasoning`, and they use
// the system role rather than the developer role, so `developer_role` is absent
// from both. An operator whose account behaves otherwise declares it.
func openAIChatModels() []Entry {
	const provider, profile = domain.ProviderOpenAI, domain.ProfileOpenAIChatEmbeddings
	return []Entry{
		builtinEntry(provider, profile, "gpt-4o", with(chat(128_000, 16_384), vision, structuredOutputs)),
		builtinEntry(provider, profile, "gpt-4o-mini", with(chat(128_000, 16_384), vision, structuredOutputs)),
		builtinEntry(provider, profile, "gpt-4.1", with(chat(1_047_576, 32_768), vision, structuredOutputs, developerRole)),
		builtinEntry(provider, profile, "gpt-4.1-mini", with(chat(1_047_576, 32_768), vision, structuredOutputs, developerRole)),
		builtinEntry(provider, profile, "gpt-4.1-nano", with(chat(1_047_576, 32_768), vision, structuredOutputs, developerRole)),
		builtinEntry(provider, profile, "o3", with(chat(200_000, 100_000), vision, structuredOutputs, developerRole, reasoning)),
		builtinEntry(provider, profile, "o4-mini", with(chat(200_000, 100_000), vision, structuredOutputs, developerRole, reasoning)),
		builtinEntry(provider, profile, "gpt-5", with(chat(400_000, 128_000), vision, structuredOutputs, developerRole, reasoning)),
		builtinEntry(provider, profile, "gpt-5-mini", with(chat(400_000, 128_000), vision, structuredOutputs, developerRole, reasoning)),
		builtinEntry(provider, profile, "gpt-5-nano", with(chat(400_000, 128_000), vision, structuredOutputs, developerRole, reasoning)),
		// The 5.4 generation onward. Each model's own page states 1,050,000
		// context and 128,000 max output, text and image input, reasoning with an
		// effort ladder, function calling and structured outputs.
		//
		// developer_role is deliberately absent: these pages list the supported
		// features and none of them names it. That is an absence of evidence
		// rather than evidence of absence, and the seeding policy resolves it the
		// same way either way — an entry that under-claims costs an operator one
		// deliberate declaration, an entry that over-claims routes a request to a
		// provider that refuses it.
		builtinEntry(provider, profile, "gpt-5.4", with(chat(1_050_000, 128_000), vision, structuredOutputs, reasoning)),
		builtinEntry(provider, profile, "gpt-5.5", with(chat(1_050_000, 128_000), vision, structuredOutputs, reasoning)),
		builtinEntry(provider, profile, "gpt-5.6-sol", with(chat(1_050_000, 128_000), vision, structuredOutputs, reasoning)),
		builtinEntry(provider, profile, "gpt-5.6-terra", with(chat(1_050_000, 128_000), vision, structuredOutputs, reasoning)),
		builtinEntry(provider, profile, "gpt-5.6-luna", with(chat(1_050_000, 128_000), vision, structuredOutputs, reasoning)),
	}
}

// openAIResponsesModels covers the same OpenAI text models reached through the
// Responses endpoint.
//
// They are separate entries rather than the chat entries reused, because a
// model's capabilities are a property of the surface it is reached on as much as
// of the model: nothing here claims streaming or stream usage, since this
// profile binds no stream primitive, and nothing claims reasoning, since the
// canonical response mapper cannot preserve reasoning items.
//
// provider_executed_tools is claimed by no entry. It is at the profile ceiling
// and off in its defaults, which is where an operator accepts the upstream
// egress it implies; a catalogue entry asserting it would make that acceptance
// automatic for every model.
func openAIResponsesModels() []Entry {
	const provider, profile = domain.ProviderOpenAI, domain.ProfileOpenAIResponses
	return []Entry{
		builtinEntry(provider, profile, "gpt-4o", with(responses(128_000, 16_384), vision, structuredOutputs)),
		builtinEntry(provider, profile, "gpt-4o-mini", with(responses(128_000, 16_384), vision, structuredOutputs)),
		builtinEntry(provider, profile, "gpt-4.1", with(responses(1_047_576, 32_768), vision, structuredOutputs, developerRole)),
		builtinEntry(provider, profile, "gpt-4.1-mini", with(responses(1_047_576, 32_768), vision, structuredOutputs, developerRole)),
		builtinEntry(provider, profile, "gpt-4.1-nano", with(responses(1_047_576, 32_768), vision, structuredOutputs, developerRole)),
		builtinEntry(provider, profile, "o3", with(responses(200_000, 100_000), vision, structuredOutputs, developerRole)),
		builtinEntry(provider, profile, "o4-mini", with(responses(200_000, 100_000), vision, structuredOutputs, developerRole)),
		builtinEntry(provider, profile, "gpt-5", with(responses(400_000, 128_000), vision, structuredOutputs, developerRole)),
		builtinEntry(provider, profile, "gpt-5-mini", with(responses(400_000, 128_000), vision, structuredOutputs, developerRole)),
		builtinEntry(provider, profile, "gpt-5-nano", with(responses(400_000, 128_000), vision, structuredOutputs, developerRole)),
		builtinEntry(provider, profile, "gpt-5.4", with(responses(1_050_000, 128_000), vision, structuredOutputs)),
		builtinEntry(provider, profile, "gpt-5.5", with(responses(1_050_000, 128_000), vision, structuredOutputs)),
		builtinEntry(provider, profile, "gpt-5.6-sol", with(responses(1_050_000, 128_000), vision, structuredOutputs)),
		builtinEntry(provider, profile, "gpt-5.6-terra", with(responses(1_050_000, 128_000), vision, structuredOutputs)),
		builtinEntry(provider, profile, "gpt-5.6-luna", with(responses(1_050_000, 128_000), vision, structuredOutputs)),
	}
}

// responses is chat() without the two streaming claims.
func responses(contextTokens, outputTokens int64) domain.ProviderCapabilities {
	return domain.ProviderCapabilities{
		Chat: true, Tools: true, JSONObject: true,
		MaxContextTokens: contextTokens, MaxOutputTokens: outputTokens,
	}
}

// openAIEmbeddingModels share the chat profile because that profile carries
// both operations. An embedding model claims embeddings and nothing else: it
// has no chat, and its context limit is an input limit with no output side.
func openAIEmbeddingModels() []Entry {
	const provider, profile = domain.ProviderOpenAI, domain.ProfileOpenAIChatEmbeddings
	embeddings := domain.ProviderCapabilities{Embeddings: true, MaxContextTokens: 8_191}
	return []Entry{
		builtinEntry(provider, profile, "text-embedding-3-small", embeddings),
		builtinEntry(provider, profile, "text-embedding-3-large", embeddings),
	}
}

// openAIMediaModels cover the media and moderation profile. Each model performs
// exactly one operation, which is what makes them worth seeding: the profile
// ceiling carries six operations, and offering all six for a text-to-speech
// model is the guess this catalog exists to replace.
//
// `omni-moderation-latest` is a moving alias, admissible under §6.2 only
// because Halro pins it: openaiapi.DecodeModerationRequest substitutes exactly
// this identifier when a client omits the model.
//
// Files and batches are absent on purpose. They are account-level resources
// rather than model-level operations, so no model identifier establishes them.
func openAIMediaModels() []Entry {
	const provider, profile = domain.ProviderOpenAI, domain.ProfileOpenAIMediaResources
	moderations := domain.ProviderCapabilities{Moderations: true}
	images := domain.ProviderCapabilities{Images: true}
	transcriptions := domain.ProviderCapabilities{Transcriptions: true}
	speech := domain.ProviderCapabilities{Speech: true}
	return []Entry{
		builtinEntry(provider, profile, "omni-moderation-latest", moderations),
		builtinEntry(provider, profile, "gpt-image-1", images),
		builtinEntry(provider, profile, "dall-e-3", images),
		builtinEntry(provider, profile, "whisper-1", transcriptions),
		builtinEntry(provider, profile, "gpt-4o-transcribe", transcriptions),
		builtinEntry(provider, profile, "gpt-4o-mini-transcribe", transcriptions),
		builtinEntry(provider, profile, "tts-1", speech),
		builtinEntry(provider, profile, "tts-1-hd", speech),
		builtinEntry(provider, profile, "gpt-4o-mini-tts", speech),
	}
}

// deepSeekModels cover the DeepSeek chat profile, whose ceiling carries vision
// but no embeddings.
//
// Both entries carry reasoning, which is the whole difference from the previous
// pair. DeepSeek used to split reasoning off into its own model — deepseek-chat
// beside deepseek-reasoner, the second credited with reasoning and deliberately
// not with tools or JSON mode — and it no longer does: thinking is a switch on
// the same model, so there is no longer a model whose capabilities have to be
// narrowed to describe it.
//
// The two names DeepSeek's GET /models listed before this revision no longer
// appear in its documentation. They are replaced rather than kept alongside, so
// a Deployment still pointing at one will read as "not covered by the catalog"
// and ask its operator to declare or re-detect its capabilities. That is the
// capability review working, not a failure, and it needs no data-directory
// re-initialisation.
//
// Vision is on exactly one of the three. DeepSeek documents that only the vision
// model accepts images and that every other model answers one with a 400, which
// is the whole reason the profile admits vision at its ceiling and withholds it
// from the defaults: this list is where the per-model claim lives, so a
// Deployment on either text model cannot pick vision up by association.
//
// The vision model carries `-exp` in its own published identifier. That is
// DeepSeek's name for it, not a hedge added here, and the seeding policy asks
// for the exact identifier documentation gives — an entry keyed on anything
// looser would promote whatever DeepSeek ships under that prefix next.
//
// Sources reviewed 2026-08-23, documentation only — no live account:
//   - https://api-docs.deepseek.com/api/list-models
//   - https://api-docs.deepseek.com/api/create-chat-completion
//   - https://api-docs.deepseek.com/guides/reasoning_model
//   - https://api-docs.deepseek.com/guides/vision
//   - https://api-docs.deepseek.com/quick_start/pricing
func deepSeekModels() []Entry {
	const provider, profile = domain.ProviderDeepSeek, domain.ProfileDeepSeekChat
	return []Entry{
		builtinEntry(provider, profile, "deepseek-v4-flash", with(chat(1_000_000, 384_000), reasoning)),
		builtinEntry(provider, profile, "deepseek-v4-pro", with(chat(1_000_000, 384_000), reasoning)),
		builtinEntry(provider, profile, "deepseek-v4-flash-vision-exp", with(chat(1_000_000, 384_000), reasoning, vision)),
	}
}

// anthropicModels contains pinned Claude API identifiers rather than the
// convenience aliases used by pre-4.6 releases. Anthropic documents current
// Claude models as accepting text and image input and supporting tool use; the
// reasoning flag is limited to models whose current specification exposes
// thinking. The catalog records reviewed protocol capabilities, not a probe.
//
// Sources reviewed 2026-08-09:
//   - https://platform.claude.com/docs/en/about-claude/models/overview
//   - https://platform.claude.com/docs/en/about-claude/models/model-ids-and-versions
//
// Refreshed 2026-08-23 from the models overview: the 5 generation is added, and
// claude-sonnet-4-6's max output corrected from 64k to the 128k the table
// states. Every current Claude model takes text and image input, and the direct
// API accepts a url image source as readily as base64, so `vision` credits both
// halves here.
func anthropicModels() []Entry {
	const provider, profile = domain.ProviderAnthropic, domain.ProfileAnthropicMessages
	claude := func(contextTokens, outputTokens int64) domain.ProviderCapabilities {
		return domain.ProviderCapabilities{
			Chat: true, Streaming: true, StreamUsage: true, Tools: true,
			Vision: true, FetchedImage: true, Reasoning: true,
			MaxContextTokens: contextTokens, MaxOutputTokens: outputTokens,
		}
	}
	return []Entry{
		builtinEntry(provider, profile, "claude-fable-5", claude(1_000_000, 128_000)),
		builtinEntry(provider, profile, "claude-opus-5", claude(1_000_000, 128_000)),
		builtinEntry(provider, profile, "claude-sonnet-5", claude(1_000_000, 128_000)),
		builtinEntry(provider, profile, "claude-opus-4-8", claude(1_000_000, 128_000)),
		builtinEntry(provider, profile, "claude-opus-4-7", claude(1_000_000, 128_000)),
		builtinEntry(provider, profile, "claude-sonnet-4-6", claude(1_000_000, 128_000)),
		builtinEntry(provider, profile, "claude-haiku-4-5-20251001", claude(200_000, 64_000)),
	}
}

// minimaxModels covers the eight exact identifiers MiniMax documents. The same
// eight are served on both of MiniMax's hosts — api.minimax.io for international
// accounts and api.minimaxi.com for mainland ones — so the entries are not
// region-scoped; only the connection's base URL is.
//
// Two deliberate narrowings, both on the conservative side of what the
// documentation says:
//
//   - Only MiniMax-M3 gets vision. The M2.x line is documented as accepting
//     "text and tool-call content blocks only", so an image request routed there
//     would be refused after the budget was reserved.
//   - M2.x carries no output bound. The documentation records its max_tokens
//     ceiling as 204,800, which is also its context window — that reads as a
//     missing row rather than a model that can emit its whole window, and a
//     wrong bound is enforced by budget and routing while a missing one only
//     costs a layer of protection. M3's 524,288 is listed separately from its
//     1,000,000 window, so it is recorded.
//
// One thing this catalogue records that no capability set can: the M2.x line
// cannot switch thinking off. That is why reasoning is claimed for M3 alone —
// an M2.x deployment cannot honour a request not to think, and claiming the
// capability would let a caller ask for something the model will bill them for
// anyway.
//
// Sources reviewed 2026-08-31:
//   - https://platform.minimax.io/docs/api-reference/text-anthropic-api
//   - https://platform.minimax.io/docs/api-reference/text-chat-anthropic
//   - https://platform.minimax.io/docs/api-reference/text-chat-openai
//   - https://platform.minimax.cn/docs/api-reference/text-anthropic-api
func minimaxModels() []Entry {
	const provider = domain.ProviderMiniMax
	// The Anthropic face is the anchor profile, so it is the one a connection
	// serves chat on by default. Chat and Responses carry the same models; a
	// deployment on either declares them from this same list, which is why the
	// entries are written per profile rather than once per model.
	anthropicChat := func(contextTokens, outputTokens int64) domain.ProviderCapabilities {
		return domain.ProviderCapabilities{
			Chat: true, Streaming: true, StreamUsage: true, Tools: true,
			MaxContextTokens: contextTokens, MaxOutputTokens: outputTokens,
		}
	}
	openAIChat := anthropicChat
	// Responses binds no stream primitive and cannot carry reasoning items, so
	// its entries claim neither.
	responses := func(contextTokens, outputTokens int64) domain.ProviderCapabilities {
		return domain.ProviderCapabilities{
			Chat: true, Tools: true,
			MaxContextTokens: contextTokens, MaxOutputTokens: outputTokens,
		}
	}
	const m3Context, m3Output int64 = 1_000_000, 524_288
	const m2Context int64 = 204_800
	m2Models := []string{
		"MiniMax-M2.7", "MiniMax-M2.7-highspeed",
		"MiniMax-M2.5", "MiniMax-M2.5-highspeed",
		"MiniMax-M2.1", "MiniMax-M2.1-highspeed",
		"MiniMax-M2",
	}
	entries := []Entry{
		// M3 alone sees images and alone can be asked not to think.
		builtinEntry(provider, domain.ProfileMiniMaxAnthropicMessages, "MiniMax-M3", with(anthropicChat(m3Context, m3Output), reasoning, vision)),
		builtinEntry(provider, domain.ProfileMiniMaxChat, "MiniMax-M3", with(openAIChat(m3Context, m3Output), reasoning, vision)),
		builtinEntry(provider, domain.ProfileMiniMaxResponses, "MiniMax-M3", with(responses(m3Context, m3Output), vision)),
	}
	for _, model := range m2Models {
		entries = append(entries,
			builtinEntry(provider, domain.ProfileMiniMaxAnthropicMessages, model, anthropicChat(m2Context, 0)),
			builtinEntry(provider, domain.ProfileMiniMaxChat, model, openAIChat(m2Context, 0)),
			builtinEntry(provider, domain.ProfileMiniMaxResponses, model, responses(m2Context, 0)),
		)
	}
	return entries
}

// kimiModels covers the four exact identifiers Kimi publishes as in-service on
// 2026-09-01. The same four are served on both of Kimi's hosts —
// api.moonshot.ai for international accounts and api.moonshot.cn for mainland
// ones — so the entries are not region-scoped; only the connection's base URL
// and the price list are.
//
// Two things this catalogue records that no capability set can, and they are the
// reason the entries are written per model rather than once per profile:
//
//   - Which models each face serves. Kimi's published OpenAPI pins `model` to
//     kimi-k3 on /v1/responses and /anthropic/v1/messages, and the measurement
//     contradicts it: kimi-k2.6 answers 200 on both. So the entries follow what
//     was measured, not what was published — kimi-k3 and kimi-k2.6 on all three
//     faces, and the two k2.7-code identifiers on Chat alone, which is the only
//     face they have been driven on.
//   - Which models can be told not to reason. kimi-k3 and kimi-k2.6 can; the
//     k2.7-code pair cannot, and always reasons with Preserved Thinking on.
//     kimi-k3's off switch was measured after both its documentation and its own
//     /v1/models metadata said it had none — this list follows the measurement.
//     No capability bit says "reasons, but not optionally", so the pair that
//     cannot be switched off is recorded in this comment and in the endpoint
//     manifest's declared transforms, which is honest about the gap rather than
//     hiding it behind a bit that means something else.
//
// Two deliberate narrowings:
//
//   - No output ceiling on the K2.x line. Kimi documents an output default and
//     maximum for kimi-k3 and gives none for the others, and a wrong bound is
//     enforced by budget and routing while a missing one only costs a layer of
//     protection.
//   - No fetched_image anywhere. Kimi's image members accept a data URL or an
//     ms://<file_id> reference and nothing else, so vision here is the inline
//     half alone — the same shape Bedrock's entries use.
//
// Sources reviewed 2026-09-01 (the .cn and .ai documents were compared and are
// the same contract):
//   - https://platform.kimi.com/docs/models.md
//   - https://platform.kimi.com/docs/api/models-overview.md
//   - https://platform.kimi.com/docs/openapi.json
func kimiModels() []Entry {
	const provider = domain.ProviderKimi
	// Kimi's Chat face documents both JSON halves, so chat() plus
	// structuredOutputs is the right shared shape here.
	kimiChat := func(contextTokens, outputTokens int64) domain.ProviderCapabilities {
		return with(chat(contextTokens, outputTokens), structuredOutputs, visionInline, reasoning)
	}
	// Responses binds no stream primitive and cannot carry reasoning items, so
	// its entries claim neither. It also has no schema-less JSON mode.
	kimiResponses := func(contextTokens, outputTokens int64) domain.ProviderCapabilities {
		return domain.ProviderCapabilities{
			Chat: true, Tools: true, Vision: true, StructuredOutputs: true,
			MaxContextTokens: contextTokens, MaxOutputTokens: outputTokens,
		}
	}
	// The Anthropic face carries no schema-less JSON mode either, and its
	// reasoning is reachable in native mode alone — the profile's field rules
	// route every portable request that asks for depth away, so an entry claiming
	// reasoning here is claiming what native mode can do.
	kimiAnthropic := func(contextTokens, outputTokens int64) domain.ProviderCapabilities {
		return domain.ProviderCapabilities{
			Chat: true, Streaming: true, StreamUsage: true, Tools: true,
			Vision: true, StructuredOutputs: true, Reasoning: true,
			MaxContextTokens: contextTokens, MaxOutputTokens: outputTokens,
		}
	}
	const k3Context, k3Output int64 = 1_048_576, 1_048_576
	const k2Context int64 = 262_144
	// The Responses face reasons on every model it serves and has no off value
	// on its ladder, so every entry on it is marked. That is why the profile is
	// withheld (see internal/domain/provider_table.go), and marking the entries
	// is what keeps the two facts attached: offering the profile again without
	// finding an off switch fails
	// TestNoEndpointIsServedByATargetThatReasonsUnasked rather than reaching an
	// operator.
	reasonsUnasked := func(entry Entry) Entry {
		entry.ReasonsUnasked = true
		return entry
	}
	entries := []Entry{
		builtinEntry(provider, domain.ProfileKimiChat, "kimi-k3", kimiChat(k3Context, k3Output)),
		builtinEntry(provider, domain.ProfileKimiAnthropicMessages, "kimi-k3", kimiAnthropic(k3Context, k3Output)),
		reasonsUnasked(builtinEntry(provider, domain.ProfileKimiResponses, "kimi-k3", kimiResponses(k3Context, k3Output))),
		// Measured answering on all three faces on 2026-09-01, which is what the
		// published schemas say does not happen.
		builtinEntry(provider, domain.ProfileKimiChat, "kimi-k2.6", kimiChat(k2Context, 0)),
		builtinEntry(provider, domain.ProfileKimiAnthropicMessages, "kimi-k2.6", kimiAnthropic(k2Context, 0)),
		reasonsUnasked(builtinEntry(provider, domain.ProfileKimiResponses, "kimi-k2.6", kimiResponses(k2Context, 0))),
	}
	// Driven on Chat alone. Nothing establishes what the other two faces do with
	// them, and an entry that guesses costs an operator a deployment that fails
	// every call.
	//
	// Both reason unasked wherever they are served: `invalid thinking: only
	// type=enabled is allowed for this model`, so the renderer sends no off
	// switch because there is none to send. On the Chat northbound face the
	// answer comes back as reasoning_content and is rendered; on the Responses
	// and Messages faces it cannot be, and that pair is the residue the guard
	// names.
	for _, model := range []string{"kimi-k2.7-code", "kimi-k2.7-code-highspeed"} {
		entries = append(entries, reasonsUnasked(builtinEntry(provider, domain.ProfileKimiChat, model, kimiChat(k2Context, 0))))
	}
	return entries
}

// geminiModels uses stable model codes only; preview, latest and experimental
// aliases are deliberately absent. The Gemini profile currently exposes chat,
// streaming, embeddings and developer-role translation, so multimodal and
// function-calling claims stay out until the profile itself can represent them.
//
// Sources reviewed 2026-08-09:
//   - https://ai.google.dev/gemini-api/docs/models
//   - https://ai.google.dev/api/models
func geminiModels() []Entry {
	const provider, profile = domain.ProviderGemini, domain.ProfileGeminiText
	generate := func(contextTokens, outputTokens int64) domain.ProviderCapabilities {
		return domain.ProviderCapabilities{
			Chat: true, Streaming: true, DeveloperRole: true,
			MaxContextTokens: contextTokens, MaxOutputTokens: outputTokens,
		}
	}
	return []Entry{
		builtinEntry(provider, profile, "gemini-3.6-flash", generate(0, 0)),
		builtinEntry(provider, profile, "gemini-3.5-flash", generate(0, 0)),
		builtinEntry(provider, profile, "gemini-3.5-flash-lite", generate(0, 0)),
		builtinEntry(provider, profile, "gemini-3.1-flash-lite", generate(0, 0)),
		builtinEntry(provider, profile, "gemini-2.5-pro", generate(1_048_576, 65_536)),
		builtinEntry(provider, profile, "gemini-2.5-flash", generate(1_048_576, 65_536)),
		builtinEntry(provider, profile, "gemini-2.5-flash-lite", generate(1_048_576, 65_536)),
		builtinEntry(provider, profile, "gemini-embedding-001", domain.ProviderCapabilities{Embeddings: true}),
	}
}

// bedrockConverseModels seeds a deliberately small set of exact, commonly
// deployed Bedrock foundation-model IDs that AWS lists as Converse-capable.
// Region availability is still resolved by the provider control plane; the
// region-agnostic entry says only that this model speaks the Converse profile.
//
// Source reviewed 2026-08-09:
// https://docs.aws.amazon.com/bedrock/latest/userguide/models-api-compatibility.html
func bedrockConverseModels() []Entry {
	const provider, profile = domain.ProviderBedrock, domain.ProfileBedrockConverseText
	converse := domain.ProviderCapabilities{Chat: true, Streaming: true, StreamUsage: true}
	return []Entry{
		builtinEntry(provider, profile, "amazon.nova-premier-v1:0", converse),
		builtinEntry(provider, profile, "amazon.nova-pro-v1:0", converse),
		builtinEntry(provider, profile, "amazon.nova-lite-v1:0", converse),
		builtinEntry(provider, profile, "amazon.nova-micro-v1:0", converse),
		builtinEntry(provider, profile, "anthropic.claude-sonnet-4-5-20250929-v1:0", converse),
		builtinEntry(provider, profile, "anthropic.claude-haiku-4-5-20251001-v1:0", converse),
	}
}

// openAICompatibleModels is intentionally more conservative than either the
// OpenAI or DeepSeek native catalog. An OpenAI-compatible endpoint establishes
// only the operations implemented by Halro's compatibility profile; identical
// model names do not inherit native-only tools, JSON, reasoning or token limits.
// These exact IDs are also the choices offered when an operator maps a custom
// endpoint alias to a reviewed underlying model.
// mantleModel is one row of the measured Mantle matrix: an exact model
// identifier, the window Bedrock gives it, and whether the route it sits on
// also serves it over Responses.
type mantleModel struct {
	model         string
	contextTokens int64
	outputTokens  int64
	responses     bool
}

// mantleChat is what the matrix run established for a Mantle chat model and
// nothing beyond it: the operation and its stream, both measured on every model
// the account lists. Tools, JSON mode, vision, developer role and reasoning are
// all inside the Mantle ceiling and none of them was exercised per model, so no
// entry claims them — the seeding policy at the top of this file rules out
// reading a capability off a model's name, and the asymmetry is not close: an
// under-claiming entry costs an operator one deliberate declaration, while a
// wrong one costs them a refusal they cannot explain. Capability detection
// fills these in per account, with verified evidence rather than declared.
func mantleChat(contextTokens, outputTokens int64) domain.ProviderCapabilities {
	return domain.ProviderCapabilities{
		Chat: true, Streaming: true,
		MaxContextTokens: contextTokens, MaxOutputTokens: outputTokens,
	}
}

// bedrockMantleModels covers the models Bedrock serves on its Mantle endpoint.
// Until this grew past one entry, every Mantle deployment but `openai.gpt-5.5`
// resolved as "not covered" and its operator had to declare the capabilities by
// hand — which is what forces a widening, a revalidation, and a route taken out
// of service to turn one capability on.
//
// Three things about these entries are not guesses and are easy to get wrong:
//
//   - The route is a property of the profile, never of the identifier. Mantle
//     serves each model from exactly one of /v1, /openai/v1 and /anthropic/v1,
//     and there are four counterexamples to every rule an identifier suggests:
//     openai.gpt-oss-* answer on /v1 while every openai.gpt-5.x answers on
//     /openai/v1, and google.gemma-3-* and google.gemma-4-* split the same way.
//     So a model appears under the chat profile for its own route and nowhere
//     else; addressed on the other one it is refused, never served.
//   - Responses is not the chat set minus something. It was reachable on 13 of
//     49 models, and the sharpest counterexample sits inside one family:
//     openai.gpt-oss-20b and -120b serve it while openai.gpt-oss-safeguard-20b
//     and -120b — same vendor, same route, adjacent names — do not. Only a
//     measured yes puts a model under a Responses profile here.
//   - The context window is Bedrock's, not the vendor's. GPT-5.5 is 272K here
//     and 1,050,000 on the direct API. Carrying the direct number over would
//     let a request four times too large through the deployment's own ceiling.
//     Where the console gives no maximum output, zero is recorded and the
//     upstream limit applies; nothing is invented to fill the column.
//
// One earlier claim here was wrong and is corrected rather than kept beside its
// replacement: this file used to offer gpt-5.5 on the Responses profile only,
// because its model card marks Chat Completions unsupported. The live run
// reached chat on all 49 chat models including that one. The card is the weaker
// evidence and the model is now offered on both, with the vision claim its card
// does support — vision without the fetch, since Bedrock reads image bytes a
// request carries and retrieves nothing, which is also all the Mantle ceilings
// permit.
//
// The dated snapshots — openai.gpt-5.4-2026-03-05 and openai.gpt-5.5-2026-04-23
// — take the window of the model they are a snapshot of. Bedrock's catalogue
// gives them no card of their own; it lists one card per model line and the
// dated identifiers resolve to it, which is the same relationship Halro records
// here. If a snapshot ever ships a different window, its card will say so and
// this is the line to change.
//
// One identifier the account lists is deliberately absent: zai.glm-4.6 has no
// card in the console's model list — only 4.7, 4.7-flash and 5 appear there — so
// there is no window to record for it, and a window is the one number an entry
// cannot leave to the upstream. It resolves as uncovered until a card exists,
// which costs an operator a declaration and costs nobody a wrong ceiling.
//
// Routes and Responses support: measured against a real account 2026-08-21.
// Windows and maximum output: the account's own model list, read 2026-08-25.
// The 50 identifiers were re-read from GET /v1/models the same day and every
// one seeded here matched exactly. Both are recorded in
// docs/verification/provider-real-matrix.md.
func bedrockMantleModels() []Entry {
	const provider = domain.ProviderBedrock
	defaultRoute := []mantleModel{
		{model: "deepseek.v3.1", contextTokens: 128_000, outputTokens: 8_000},
		{model: "deepseek.v3.2", contextTokens: 164_000, outputTokens: 8_000},
		{model: "google.gemma-3-4b-it", contextTokens: 128_000, outputTokens: 8_000},
		{model: "google.gemma-3-12b-it", contextTokens: 128_000, outputTokens: 8_000},
		{model: "google.gemma-3-27b-it", contextTokens: 128_000, outputTokens: 8_000},
		{model: "minimax.minimax-m2", contextTokens: 1_000_000, outputTokens: 8_000},
		{model: "minimax.minimax-m2.1", contextTokens: 196_000, outputTokens: 8_000},
		{model: "minimax.minimax-m2.5", contextTokens: 196_000, outputTokens: 8_000},
		{model: "mistral.devstral-2-123b", contextTokens: 256_000, outputTokens: 32_000},
		{model: "mistral.magistral-small-2509", contextTokens: 128_000, outputTokens: 40_000},
		{model: "mistral.ministral-3-3b-instruct", contextTokens: 128_000, outputTokens: 8_000},
		{model: "mistral.ministral-3-8b-instruct", contextTokens: 128_000, outputTokens: 8_000},
		{model: "mistral.ministral-3-14b-instruct", contextTokens: 128_000, outputTokens: 8_000},
		{model: "mistral.mistral-large-3-675b-instruct", contextTokens: 256_000, outputTokens: 32_000},
		{model: "mistral.voxtral-mini-3b-2507", contextTokens: 32_000, outputTokens: 0},
		{model: "mistral.voxtral-small-24b-2507", contextTokens: 32_000, outputTokens: 0},
		{model: "moonshotai.kimi-k2-thinking", contextTokens: 256_000, outputTokens: 16_000},
		{model: "moonshotai.kimi-k2.5", contextTokens: 256_000, outputTokens: 16_000},
		{model: "nvidia.nemotron-nano-9b-v2", contextTokens: 128_000, outputTokens: 8_000},
		{model: "nvidia.nemotron-nano-12b-v2", contextTokens: 128_000, outputTokens: 8_000},
		{model: "nvidia.nemotron-nano-3-30b", contextTokens: 256_000, outputTokens: 8_000},
		{model: "nvidia.nemotron-super-3-120b", contextTokens: 256_000, outputTokens: 32_000},
		{model: "openai.gpt-oss-20b", contextTokens: 128_000, outputTokens: 16_000, responses: true},
		{model: "openai.gpt-oss-120b", contextTokens: 128_000, outputTokens: 16_000, responses: true},
		{model: "openai.gpt-oss-safeguard-20b", contextTokens: 128_000, outputTokens: 16_000},
		{model: "openai.gpt-oss-safeguard-120b", contextTokens: 128_000, outputTokens: 16_000},
		{model: "qwen.qwen3-32b", contextTokens: 32_000, outputTokens: 8_000},
		{model: "qwen.qwen3-235b-a22b-2507", contextTokens: 256_000, outputTokens: 8_000},
		{model: "qwen.qwen3-coder-30b-a3b-instruct", contextTokens: 256_000, outputTokens: 16_000},
		{model: "qwen.qwen3-coder-480b-a35b-instruct", contextTokens: 128_000, outputTokens: 16_000},
		{model: "qwen.qwen3-coder-next", contextTokens: 256_000, outputTokens: 16_000},
		{model: "qwen.qwen3-next-80b-a3b-instruct", contextTokens: 256_000, outputTokens: 8_000},
		{model: "qwen.qwen3-vl-235b-a22b-instruct", contextTokens: 256_000, outputTokens: 8_000},
		{model: "writer.palmyra-vision-7b", contextTokens: 4_000, outputTokens: 4_000},
		{model: "zai.glm-4.7", contextTokens: 203_000, outputTokens: 4_000},
		{model: "zai.glm-4.7-flash", contextTokens: 203_000, outputTokens: 4_000},
		{model: "zai.glm-5", contextTokens: 200_000, outputTokens: 128_000},
	}
	openAIRoute := []mantleModel{
		{model: "google.gemma-4-26b-a4b", contextTokens: 256_000, outputTokens: 0, responses: true},
		{model: "google.gemma-4-31b", contextTokens: 256_000, outputTokens: 0, responses: true},
		{model: "google.gemma-4-e2b", contextTokens: 128_000, outputTokens: 0, responses: true},
		{model: "openai.gpt-5.4", contextTokens: 272_000, outputTokens: 0, responses: true},
		{model: "openai.gpt-5.4-2026-03-05", contextTokens: 272_000, outputTokens: 0, responses: true},
		{model: "openai.gpt-5.5-2026-04-23", contextTokens: 272_000, outputTokens: 0, responses: true},
		{model: "openai.gpt-5.6-luna", contextTokens: 272_000, outputTokens: 0, responses: true},
		{model: "openai.gpt-5.6-sol", contextTokens: 272_000, outputTokens: 0, responses: true},
		{model: "openai.gpt-5.6-terra", contextTokens: 272_000, outputTokens: 0, responses: true},
		{model: "xai.grok-4.3", contextTokens: 1_000_000, outputTokens: 0, responses: true},
	}
	entries := make([]Entry, 0, 2*(len(defaultRoute)+len(openAIRoute))+3)
	addRoute := func(models []mantleModel, chatProfile, responsesProfile domain.ProviderProfileID) {
		for _, model := range models {
			capabilities := mantleChat(model.contextTokens, model.outputTokens)
			entries = append(entries, builtinEntry(provider, chatProfile, model.model, capabilities))
			if model.responses {
				entries = append(entries, builtinEntry(provider, responsesProfile, model.model, capabilities))
			}
		}
	}
	addRoute(defaultRoute, domain.ProfileBedrockMantleChat, domain.ProfileBedrockMantleResponses)
	addRoute(openAIRoute, domain.ProfileBedrockMantleOpenAIChat, domain.ProfileBedrockMantleOpenAIResponses)
	// gpt-5.5 is the one model whose card was read, so it is the one model
	// carrying claims the matrix run did not measure — vision, and the chat
	// enhancements chat() covers. Its card is left standing rather than narrowed
	// to match the rest: this catalog under-claims where it has no evidence, not
	// where it has some.
	gpt55 := with(chat(272_000, 0), visionInline)
	entries = append(entries,
		builtinEntry(provider, domain.ProfileBedrockMantleOpenAIChat, "openai.gpt-5.5", gpt55),
		builtinEntry(provider, domain.ProfileBedrockMantleOpenAIResponses, "openai.gpt-5.5", gpt55),
		// Claude on Mantle reaches Halro through the Anthropic Messages profile,
		// which is the only route that serves it. Responses refuses it by name.
		builtinEntry(provider, domain.ProfileBedrockMantleAnthropicMessages, "anthropic.claude-haiku-4-5", mantleChat(200_000, 64_000)),
	)
	return entries
}

func openAICompatibleModels() []Entry {
	const provider, profile = domain.ProviderOpenAICompatible, domain.ProfileOpenAICompatible
	compatibleChat := domain.ProviderCapabilities{Chat: true, Streaming: true}
	embeddings := domain.ProviderCapabilities{Embeddings: true}
	return []Entry{
		builtinEntry(provider, profile, "gpt-4o", compatibleChat),
		builtinEntry(provider, profile, "gpt-4o-mini", compatibleChat),
		builtinEntry(provider, profile, "gpt-4.1", compatibleChat),
		builtinEntry(provider, profile, "gpt-4.1-mini", compatibleChat),
		builtinEntry(provider, profile, "gpt-4.1-nano", compatibleChat),
		builtinEntry(provider, profile, "gpt-5", compatibleChat),
		builtinEntry(provider, profile, "gpt-5-mini", compatibleChat),
		builtinEntry(provider, profile, "gpt-5-nano", compatibleChat),
		builtinEntry(provider, profile, "deepseek-chat", compatibleChat),
		builtinEntry(provider, profile, "deepseek-reasoner", compatibleChat),
		builtinEntry(provider, profile, "text-embedding-3-small", embeddings),
		builtinEntry(provider, profile, "text-embedding-3-large", embeddings),
	}
}
