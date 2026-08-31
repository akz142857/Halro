package app

import (
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/modelcatalog"
)

// The built-in catalog was seeded so an operator would not have to type an
// identifier, and on a profile that lists nothing it was unreachable from the
// create form: the aggregation builds its items only from what a provider
// enumerated, so entries could enrich a listed target and never be one.
//
// MiniMax's Anthropic face is the case. It deliberately does not enumerate —
// MiniMax's model list is OpenAI-shaped and carries an identifier and an owner,
// so building targets from it would credit every id on the account, speech and
// video models included, with chat and streaming on declared evidence.
func TestBuiltinCatalogOffersFillAProfileThatCannotEnumerate(t *testing.T) {
	offers := builtinCatalogOffers(domain.ProviderMiniMax, domain.ProfileMiniMaxAnthropicMessages, modelcatalog.Builtin())
	if len(offers) == 0 {
		t.Fatal("a profile with catalog entries offered nothing; the create form is back to a blank field")
	}
	byID := map[string]domain.InvocationTargetDescriptor{}
	for _, offer := range offers {
		byID[offer.TargetID] = offer
	}
	m3, ok := byID["MiniMax-M3"]
	if !ok {
		t.Fatalf("MiniMax-M3 is in the catalog and was not offered; offers were %v", byID)
	}
	if m3.Metadata.MaxContextTokens != 1_000_000 || m3.Metadata.MaxOutputTokens != 524_288 {
		t.Errorf("M3 offered with context=%d output=%d, want the catalog's 1000000 and 524288",
			m3.Metadata.MaxContextTokens, m3.Metadata.MaxOutputTokens)
	}
}

// The label is the point. A provider listing a model is evidence the account
// reaches it; a built-in entry is evidence only that Halro pre-checked the
// identifier. Presenting the second as the first would tell an operator their
// account has a model it may not be entitled to.
func TestBuiltinCatalogOffersTravelAsOffersNotFindings(t *testing.T) {
	for _, offer := range builtinCatalogOffers(domain.ProviderMiniMax, domain.ProfileMiniMaxChat, modelcatalog.Builtin()) {
		if offer.MetadataSource != domain.MetadataSourceBuiltinCatalog {
			t.Errorf("%s carries source %q, want builtin_catalog", offer.TargetID, offer.MetadataSource)
		}
		if offer.Availability != domain.AvailabilityUnverified {
			t.Errorf("%s claims availability %q; the catalog cannot know what this account reaches", offer.TargetID, offer.Availability)
		}
		if offer.Lifecycle != domain.TargetLifecycleUnknown {
			t.Errorf("%s claims lifecycle %q; the catalog does not track retirement", offer.TargetID, offer.Lifecycle)
		}
		if offer.FetchedAt != (offer.FetchedAt.UTC()) || !offer.FetchedAt.IsZero() {
			t.Errorf("%s carries a fetch time; nothing was fetched", offer.TargetID)
		}
	}
}

// A profile with no catalog entries offers nothing rather than something
// borrowed from a neighbour.
func TestBuiltinCatalogOffersAreScopedToTheProfile(t *testing.T) {
	if offers := builtinCatalogOffers(domain.ProviderMiniMax, domain.ProfileAnthropicMessages, modelcatalog.Builtin()); len(offers) != 0 {
		t.Fatalf("offers leaked across the (type, profile) key: %d returned", len(offers))
	}
	azure := builtinCatalogOffers(domain.ProviderAzureOpenAI, domain.ProfileAzureChatEmbeddings, modelcatalog.Builtin())
	for _, offer := range azure {
		if offer.TargetKind != domain.TargetAzureDeployment && offer.TargetKind != domain.TargetModelID {
			t.Errorf("unexpected target kind %q for an Azure offer", offer.TargetKind)
		}
	}
}
