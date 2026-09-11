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

func TestZAIInternationalCodingCatalogUsesOnlyPublishedPlanModels(t *testing.T) {
	catalog := Builtin()
	for _, model := range []string{"glm-5.3", "glm-5.3-flash"} {
		entry, ok := catalog.Lookup(Key{ProviderType: domain.ProviderBigModel, Profile: domain.ProfileBigModelGlobalCodingChat, Model: model})
		if !ok {
			t.Errorf("published international Coding Plan model %q is missing", model)
			continue
		}
		if !entry.Capabilities.Chat || !entry.Capabilities.Streaming || !entry.Capabilities.Tools || !entry.Capabilities.Reasoning {
			t.Errorf("%s has incomplete documented coding-agent capabilities: %#v", model, entry.Capabilities)
		}
		if entry.Capabilities.JSONObject || entry.Capabilities.StreamUsage || !entry.ReasonsUnasked {
			t.Errorf("%s carries unverified international Coding Plan behaviour: %#v, reasons_unasked=%v", model, entry.Capabilities, entry.ReasonsUnasked)
		}
	}
	for _, alias := range []string{"glm-5.2", "glm-5.1", "glm-4.7"} {
		if _, ok := catalog.Lookup(Key{ProviderType: domain.ProviderBigModel, Profile: domain.ProfileBigModelGlobalCodingChat, Model: alias}); ok {
			t.Errorf("documented routing alias %q was seeded as an actual international plan model", alias)
		}
	}
}

func TestBigModelCatalogClaimsVisionAndUnaskedReasoningOnlyForExactModels(t *testing.T) {
	catalog := Builtin()
	for _, test := range []struct {
		profile                    domain.ProviderProfileID
		model                      string
		vision, reasoning, unasked bool
	}{
		{domain.ProfileBigModelCNChatEmbeddings, "glm-4.6", false, false, true},
		{domain.ProfileBigModelCNChatEmbeddings, "glm-4.6v", true, false, true},
		{domain.ProfileBigModelCNChatEmbeddings, "glm-5.2", false, true, true},
		{domain.ProfileBigModelCNChatEmbeddings, "glm-5.3", false, true, true},
		{domain.ProfileBigModelGlobalChat, "glm-4.7", false, false, true},
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

func TestBigModelCatalogDoesNotOverclaimVisionToolsOrJSONMode(t *testing.T) {
	catalog := Builtin()
	for _, test := range []struct {
		profile domain.ProviderProfileID
		model   string
		tools   bool
	}{
		{domain.ProfileBigModelCNChatEmbeddings, "glm-4.6v", true},
		{domain.ProfileBigModelCNChatEmbeddings, "glm-4v-flash", false},
		{domain.ProfileBigModelGlobalChat, "autoglm-phone-multilingual", true},
		{domain.ProfileBigModelGlobalChat, "glm-4.5v", false},
	} {
		entry, ok := catalog.Lookup(Key{ProviderType: domain.ProviderBigModel, Profile: test.profile, Model: test.model})
		if !ok {
			t.Fatalf("%s/%s is not covered", test.profile, test.model)
		}
		if entry.Capabilities.Tools != test.tools {
			t.Errorf("%s/%s tools=%v want=%v", test.profile, test.model, entry.Capabilities.Tools, test.tools)
		}
		if entry.Capabilities.JSONObject {
			t.Errorf("%s/%s incorrectly claims JSON object support", test.profile, test.model)
		}
	}
}
