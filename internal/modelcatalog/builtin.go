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

// builtinEntry declares one reviewed model. Capabilities are clamped to the
// profile ceiling on the way in, so an entry can never be the thing that widens
// a profile — including a Beta profile whose limits are pinned deliberately.
func builtinEntry(providerType domain.ProviderType, profile domain.ProviderProfileID, model string,
	capabilities domain.ProviderCapabilities) Entry {
	key := Key{ProviderType: providerType, Profile: profile, Model: model}
	return Entry{
		Key:          key,
		Status:       StatusKnown,
		Source:       SourceBuiltin,
		Capabilities: Clamp(capabilities, key.Ceiling()),
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
