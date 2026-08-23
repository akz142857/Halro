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
		openAIEmbeddingModels(),
		openAIMediaModels(),
		deepSeekModels(),
		anthropicModels(),
		geminiModels(),
		bedrockConverseModels(),
		bedrockMantleModels(),
		openAICompatibleModels(),
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
func chat(contextTokens, outputTokens int64) domain.ProviderCapabilities {
	return domain.ProviderCapabilities{
		Chat: true, Streaming: true, StreamUsage: true, Tools: true, JSONMode: true,
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
		builtinEntry(provider, profile, "gpt-4o", with(chat(128_000, 16_384), vision)),
		builtinEntry(provider, profile, "gpt-4o-mini", with(chat(128_000, 16_384), vision)),
		builtinEntry(provider, profile, "gpt-4.1", with(chat(1_047_576, 32_768), vision, developerRole)),
		builtinEntry(provider, profile, "gpt-4.1-mini", with(chat(1_047_576, 32_768), vision, developerRole)),
		builtinEntry(provider, profile, "gpt-4.1-nano", with(chat(1_047_576, 32_768), vision, developerRole)),
		builtinEntry(provider, profile, "o3", with(chat(200_000, 100_000), vision, developerRole, reasoning)),
		builtinEntry(provider, profile, "o4-mini", with(chat(200_000, 100_000), vision, developerRole, reasoning)),
		builtinEntry(provider, profile, "gpt-5", with(chat(400_000, 128_000), vision, developerRole, reasoning)),
		builtinEntry(provider, profile, "gpt-5-mini", with(chat(400_000, 128_000), vision, developerRole, reasoning)),
		builtinEntry(provider, profile, "gpt-5-nano", with(chat(400_000, 128_000), vision, developerRole, reasoning)),
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
		builtinEntry(provider, profile, "gpt-5.4", with(chat(1_050_000, 128_000), vision, reasoning)),
		builtinEntry(provider, profile, "gpt-5.5", with(chat(1_050_000, 128_000), vision, reasoning)),
		builtinEntry(provider, profile, "gpt-5.6-sol", with(chat(1_050_000, 128_000), vision, reasoning)),
		builtinEntry(provider, profile, "gpt-5.6-terra", with(chat(1_050_000, 128_000), vision, reasoning)),
		builtinEntry(provider, profile, "gpt-5.6-luna", with(chat(1_050_000, 128_000), vision, reasoning)),
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
// bedrockMantleModels covers the OpenAI-family models Bedrock serves on its
// Mantle endpoint. The catalog had no entry for any of them, so every Mantle
// deployment resolved as "not covered" and its operator had to declare the
// capabilities by hand — which is what forces a widening, a revalidation, and a
// route taken out of service to turn one capability on.
//
// Two things about these entries are not guesses and are easy to get wrong:
//
//   - The context window is Bedrock's, not OpenAI's. GPT-5.5 is 272K here and
//     1,050,000 on the direct API. Carrying the direct number over would let a
//     request four times too large through the deployment's own ceiling.
//   - Vision without the fetch. Bedrock reads an image from the bytes a request
//     carries and retrieves nothing, so these claim vision and not fetched_image
//     — which is also all the Mantle profile ceilings permit.
//
// Max output is absent from the model card ("N/A"), so it is left undeclared
// rather than invented; zero means the upstream limit applies.
//
// The profile is the Responses one because the card says so: Chat Completions,
// Converse and Invoke are all marked unsupported for this model, and it answers
// on the openai/v1 path that ProfileBedrockMantleOpenAIResponses addresses.
//
// Sources reviewed 2026-08-23, documentation only — no live account:
//   - https://docs.aws.amazon.com/bedrock/latest/userguide/model-cards.html
//   - https://docs.aws.amazon.com/bedrock/latest/userguide/model-card-openai-gpt-55.html
func bedrockMantleModels() []Entry {
	const provider, profile = domain.ProviderBedrock, domain.ProfileBedrockMantleOpenAIResponses
	return []Entry{
		builtinEntry(provider, profile, "openai.gpt-5.5", with(chat(272_000, 0), visionInline)),
	}
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
