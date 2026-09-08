package modelcatalog

import (
	"testing"

	"github.com/akz142857/Halro/internal/domain"
)

func TestBigModelCatalogKeepsRegionalAvailabilitySeparate(t *testing.T) {
	catalog := Builtin()
	lookup := func(profile domain.ProviderProfileID, model string) (Entry, bool) {
		return catalog.Lookup(Key{ProviderType: domain.ProviderBigModel, Profile: profile, Model: model})
	}
	if _, ok := lookup(domain.ProfileBigModelCNChatEmbeddings, "glm-5-turbo"); !ok {
		t.Error("the mainland-only glm-5-turbo identifier is not covered on the mainland profile")
	}
	if _, ok := lookup(domain.ProfileBigModelGlobalChat, "glm-5-turbo"); ok {
		t.Error("the mainland-only glm-5-turbo identifier leaked into the global profile")
	}
	if _, ok := lookup(domain.ProfileBigModelGlobalChat, "glm-4.5-x"); !ok {
		t.Error("the global-only glm-4.5-x identifier is not covered on the global profile")
	}
	if _, ok := lookup(domain.ProfileBigModelCNChatEmbeddings, "glm-4.5-x"); ok {
		t.Error("the global-only glm-4.5-x identifier leaked into the mainland profile")
	}
	if _, ok := lookup(domain.ProfileBigModelGlobalChat, "embedding-3"); ok {
		t.Error("mainland Embeddings leaked into the global Chat-only profile")
	}
}

func TestBigModelCatalogClaimsVisionAndUnaskedReasoningOnlyForExactModels(t *testing.T) {
	catalog := Builtin()
	for _, test := range []struct {
		profile                    domain.ProviderProfileID
		model                      string
		vision, reasoning, unasked bool
	}{
		{domain.ProfileBigModelCNChatEmbeddings, "glm-4.6", false, false, false},
		{domain.ProfileBigModelCNChatEmbeddings, "glm-4.6v", true, false, false},
		{domain.ProfileBigModelCNChatEmbeddings, "glm-5.2", false, true, false},
		{domain.ProfileBigModelCNChatEmbeddings, "glm-5.3", false, true, true},
		{domain.ProfileBigModelGlobalChat, "glm-5.3-flash", true, true, true},
	} {
		entry, ok := catalog.Lookup(Key{ProviderType: domain.ProviderBigModel, Profile: test.profile, Model: test.model})
		if !ok {
			t.Errorf("%s/%s is not covered", test.profile, test.model)
			continue
		}
		if entry.Capabilities.Vision != test.vision || entry.Capabilities.FetchedImage != test.vision ||
			entry.Capabilities.Reasoning != test.reasoning || entry.ReasonsUnasked != test.unasked {
			t.Errorf("%s/%s claims %#v, reasons_unasked=%v", test.profile, test.model, entry.Capabilities, entry.ReasonsUnasked)
		}
	}
}
