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
//     §6.1 of docs/todo/model-aware-capability-selection.zh-CN.md asks for
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
	key := Key{ProviderType: providerType, Profile: profile, Model: model}
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
// one outward-facing model is the route layer's job (see
// model-aware-capability-selection.v1.1.0.zh-CN.md) — so the answer is a second
// entry on the profile that does carry it, not a quieter first one.
func builtinEntry(providerType domain.ProviderType, profile domain.ProviderProfileID, model string,
	capabilities domain.ProviderCapabilities) Entry {
	return Entry{
		Key:          Key{ProviderType: providerType, Profile: profile, Model: model},
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

func vision(capabilities *domain.ProviderCapabilities)        { capabilities.Vision = true }
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

// deepSeekModels cover the DeepSeek chat profile, whose ceiling carries no
// vision and no embeddings.
//
// deepseek-reasoner is credited with reasoning and deliberately not with tools
// or JSON mode: those are the two the reasoning model has not carried, and
// crediting them would put a pre-checked box in front of an operator that the
// upstream then rejects.
func deepSeekModels() []Entry {
	const provider, profile = domain.ProviderDeepSeek, domain.ProfileDeepSeekChat
	return []Entry{
		builtinEntry(provider, profile, "deepseek-chat", chat(131_072, 8_192)),
		builtinEntry(provider, profile, "deepseek-reasoner", domain.ProviderCapabilities{
			Chat: true, Streaming: true, StreamUsage: true, Reasoning: true,
			MaxContextTokens: 131_072, MaxOutputTokens: 65_536,
		}),
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
func anthropicModels() []Entry {
	const provider, profile = domain.ProviderAnthropic, domain.ProfileAnthropicMessages
	claude := func(contextTokens, outputTokens int64) domain.ProviderCapabilities {
		return domain.ProviderCapabilities{
			Chat: true, Streaming: true, StreamUsage: true, Tools: true, Vision: true, Reasoning: true,
			MaxContextTokens: contextTokens, MaxOutputTokens: outputTokens,
		}
	}
	return []Entry{
		builtinEntry(provider, profile, "claude-opus-4-7", claude(1_000_000, 128_000)),
		builtinEntry(provider, profile, "claude-sonnet-4-6", claude(1_000_000, 64_000)),
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
