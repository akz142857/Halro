package modelcatalog

import (
	"sync"

	"github.com/akz142857/Halro/internal/domain"
)

// The seeding policy, stated so later additions are held to it:
//
// An entry may be added only when Halro can point at evidence for it, and the
// strongest evidence available today is a profile that already pins its model.
// invoke_titan_embedding.go and inference_resources.go reject any other model
// for these four profiles, so "profile P implies model M" is enforced code, not
// a recollection about a vendor's line-up. For those, the model's capabilities
// are the profile's capabilities.
//
// Everything else ships empty on purpose. A chat model's feature set is not
// derivable from its name, and a catalog that guessed would hand operators a
// pre-checked list that Halro cannot stand behind — the exact failure the
// proposal exists to prevent. OpenAI, Anthropic, Azure, DeepSeek, Gemini and
// Bedrock Converse models therefore resolve to StatusUnknown until an entry is
// added with real evidence, or the operator declares one. Growing this file is
// the point; guessing at it is not.
var builtinOnce = sync.OnceValues(func() (*Catalog, error) {
	return New(
		pinnedProfileEntry(domain.ProviderBedrock, domain.ProfileBedrockInvokeTitanEmbedV2, "amazon.titan-embed-text-v2:0"),
		pinnedProfileEntry(domain.ProviderBedrock, domain.ProfileBedrockInvokeTitanImageV2, "amazon.titan-image-generator-v2:0"),
		pinnedProfileEntry(domain.ProviderBedrock, domain.ProfileBedrockAgentRerankCohere35, "cohere.rerank-v3-5:0"),
		pinnedProfileEntry(domain.ProviderBedrock, domain.ProfileBedrockAsyncNovaReel, "amazon.nova-reel-v1:0"),
	)
})

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

// pinnedProfileEntry builds the entry for a profile that accepts exactly one
// model. Capabilities come from the profile ceiling rather than being restated,
// so narrowing the profile cannot leave a wider claim behind in the catalog.
func pinnedProfileEntry(providerType domain.ProviderType, profile domain.ProviderProfileID, model string) Entry {
	key := Key{ProviderType: providerType, Profile: profile, Model: model}
	return Entry{
		Key:          key,
		Status:       StatusKnown,
		Source:       SourceBuiltin,
		Capabilities: key.Ceiling(),
	}
}
