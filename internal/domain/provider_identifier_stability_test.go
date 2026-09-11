package domain

import (
	"slices"
	"testing"
)

// These are persisted and audit-visible identifiers. Both the keys and values
// are literals on purpose: replacing a constant and every production reference
// to it must still fail this compatibility gate. Add rows; never edit or remove
// an existing row.
func TestProviderProfileIdentifiersAndBindingsAreStable(t *testing.T) {
	type contract struct{ group, surface, scheme string }
	want := map[string]contract{
		"openai.chat-embeddings.v1":                         {"openai-api", "openai-api", "bearer.static"},
		"openai.responses.v1":                               {"openai-api", "openai-api", "bearer.static"},
		"anthropic.messages.2023-06-01":                     {"anthropic-api", "anthropic-api", "anthropic.x-api-key"},
		"azure-openai.chat-embeddings.v1":                   {"azure-openai", "azure-openai", "azure.api-key"},
		"deepseek.chat.v1":                                  {"deepseek-api", "deepseek-api", "bearer.static"},
		"openai-compatible.chat-embeddings.v1":              {"openai-compatible", "openai-compatible", "bearer.static"},
		"bigmodel.cn.chat-embeddings.v1":                    {"bigmodel-cn-general", "bigmodel-cn-general-api", "bigmodel.api-key"},
		"bigmodel.global.chat.v1":                           {"bigmodel-global-general", "bigmodel-global-general-api", "bigmodel.api-key"},
		"bigmodel.cn.coding.chat.v1":                        {"bigmodel-cn-coding", "bigmodel-cn-coding-api", "bigmodel.coding-plan-key"},
		"bigmodel.global.coding.chat.v1":                    {"bigmodel-global-coding", "bigmodel-global-coding-api", "bigmodel.coding-plan-key"},
		"gemini.generate-content.text.v1beta":               {"gemini-text", "gemini-generate-content", "google.api-key"},
		"bedrock.runtime.converse.text.v1":                  {"bedrock-runtime", "bedrock-runtime", "aws.sigv4.explicit-session"},
		"bedrock.runtime.invoke.titan-embed-text-v2.v1":     {"bedrock-runtime", "bedrock-runtime", "aws.sigv4.explicit-session"},
		"openai.media-resources.v1":                         {"openai-api", "openai-api", "bearer.static"},
		"bedrock.runtime.invoke.titan-image-v2.v1":          {"bedrock-runtime", "bedrock-runtime", "aws.sigv4.explicit-session"},
		"bedrock.agent-runtime.rerank.cohere-v3-5.v1":       {"bedrock-agent-runtime", "bedrock-agent-runtime", "aws.sigv4.explicit-session"},
		"bedrock.runtime.async.nova-reel-v1.v1":             {"bedrock-runtime", "bedrock-runtime", "aws.sigv4.explicit-session"},
		"bedrock.mantle.chat.v1":                            {"bedrock-mantle", "bedrock-mantle", "aws.bedrock.api-key"},
		"bedrock.mantle.openai.chat.v1":                     {"bedrock-mantle", "bedrock-mantle", "aws.bedrock.api-key"},
		"bedrock.mantle.responses.v1":                       {"bedrock-mantle", "bedrock-mantle", "aws.bedrock.api-key"},
		"bedrock.mantle.openai.responses.v1":                {"bedrock-mantle", "bedrock-mantle", "aws.bedrock.api-key"},
		"bedrock.mantle.anthropic.messages.v1":              {"bedrock-mantle", "bedrock-mantle", "aws.bedrock.api-key"},
		"minimax.anthropic.messages.v1":                     {"minimax-api", "minimax-api", "bearer.static"},
		"minimax.chat.v1":                                   {"minimax-api", "minimax-api", "bearer.static"},
		"minimax.responses.v1":                              {"minimax-api", "minimax-api", "bearer.static"},
		"kimi.chat.v1":                                      {"kimi-api", "kimi-api", "bearer.static"},
		"kimi.anthropic.messages.v1":                        {"kimi-api", "kimi-api", "bearer.static"},
		"kimi.responses.v1":                                 {"kimi-api", "kimi-api", "bearer.static"},
		"kimi.code.openai.chat.v1":                          {"kimi-code-openai", "kimi-code", "kimi.code-key"},
		"kimi.code.anthropic.messages.v1":                   {"kimi-code-anthropic", "kimi-code", "kimi.code-key"},
		"minimax.cn.subscription.openai.chat.v1":            {"minimax-cn-subscription-openai", "minimax-cn-subscription-access", "minimax.subscription-key"},
		"minimax.cn.subscription.anthropic.messages.v1":     {"minimax-cn-subscription-anthropic", "minimax-cn-subscription-access", "minimax.subscription-key"},
		"minimax.global.subscription.openai.chat.v1":        {"minimax-global-subscription-openai", "minimax-global-subscription-access", "minimax.subscription-key"},
		"minimax.global.subscription.anthropic.messages.v1": {"minimax-global-subscription-anthropic", "minimax-global-subscription-access", "minimax.subscription-key"},
	}
	got := make(map[string]contract, len(profileTable))
	for _, row := range profileTable {
		got[string(row.ID)] = contract{string(row.ConnectionGroup), string(row.Surface), string(row.Scheme)}
	}
	if len(got) != len(want) {
		t.Fatalf("profile identifier set has %d rows, stable contract has %d: got=%v", len(got), len(want), got)
	}
	for id, expected := range want {
		if actual, ok := got[id]; !ok || actual != expected {
			t.Errorf("profile literal %q = %+v present=%t, want %+v", id, actual, ok, expected)
		}
	}
}

func TestAccessSurfaceIdentifiersAndProductBindingsAreStable(t *testing.T) {
	type contract struct {
		offering, scope, region string
		hosts                   []string
	}
	want := map[string]contract{
		"openai-api":                         {"openai.api-platform", "none", "", nil},
		"anthropic-api":                      {"anthropic.console-api", "none", "", nil},
		"azure-openai":                       {"azure-openai.resource", "none", "", nil},
		"deepseek-api":                       {"deepseek.api-platform", "none", "", nil},
		"openai-compatible":                  {"openai-compatible.self-declared", "none", "", nil},
		"gemini-generate-content":            {"google.gemini-api", "none", "", nil},
		"bedrock-runtime":                    {"aws.bedrock-runtime", "none", "", nil},
		"bedrock-agent-runtime":              {"aws.bedrock-runtime", "none", "", nil},
		"bedrock-mantle":                     {"aws.bedrock-mantle", "none", "", nil},
		"minimax-api":                        {"minimax.api-platform", "by_endpoint", "", []string{"global=api.minimax.io", "cn=api.minimaxi.com"}},
		"kimi-api":                           {"kimi.open-platform", "by_endpoint", "", []string{"global=api.moonshot.ai", "cn=api.moonshot.cn"}},
		"bigmodel-cn-general-api":            {"bigmodel.general-api", "fixed", "cn", []string{"cn=open.bigmodel.cn"}},
		"bigmodel-global-general-api":        {"bigmodel.general-api", "fixed", "global", []string{"global=api.z.ai"}},
		"bigmodel-cn-coding-api":             {"bigmodel.coding-plan", "fixed", "cn", []string{"cn=open.bigmodel.cn"}},
		"bigmodel-global-coding-api":         {"bigmodel.coding-plan", "fixed", "global", []string{"global=api.z.ai"}},
		"kimi-code":                          {"kimi.code", "none", "", nil},
		"minimax-cn-subscription-access":     {"minimax.subscription-access", "fixed", "cn", []string{"cn=api.minimax.cn"}},
		"minimax-global-subscription-access": {"minimax.subscription-access", "fixed", "global", []string{"global=api.minimax.io"}},
	}
	got := make(map[string]contract, len(surfaceTable))
	for _, row := range surfaceTable {
		hosts := make([]string, 0, len(row.Hosts))
		for _, host := range row.Hosts {
			hosts = append(hosts, string(host.Region)+"="+host.Host)
		}
		got[string(row.Surface)] = contract{string(row.Offering), string(row.RegionScope), string(row.Region), hosts}
	}
	if len(got) != len(want) {
		t.Fatalf("surface identifier set has %d rows, stable contract has %d", len(got), len(want))
	}
	for id, expected := range want {
		actual, ok := got[id]
		if !ok || actual.offering != expected.offering || actual.scope != expected.scope ||
			actual.region != expected.region || !slices.Equal(actual.hosts, expected.hosts) {
			t.Errorf("surface literal %q = %+v present=%t, want %+v", id, actual, ok, expected)
		}
	}
}

func TestProviderOfferingIdentifiersAreStable(t *testing.T) {
	want := map[string]string{
		"openai.api-platform": "metered_api", "anthropic.console-api": "metered_api",
		"azure-openai.resource": "metered_api", "deepseek.api-platform": "metered_api",
		"google.gemini-api": "metered_api", "aws.bedrock-runtime": "metered_api",
		"aws.bedrock-mantle": "metered_api", "openai-compatible.self-declared": "metered_api",
		"kimi.open-platform": "metered_api", "minimax.api-platform": "metered_api",
		"bigmodel.general-api": "metered_api", "bigmodel.coding-plan": "subscription",
		"kimi.code": "subscription", "minimax.subscription-access": "entitlement",
	}
	got := make(map[string]string, len(providerOfferingTable))
	for _, row := range providerOfferingTable {
		got[string(row.ID)] = string(row.Kind)
	}
	if len(got) != len(want) {
		t.Fatalf("offering identifier set has %d rows, stable contract has %d", len(got), len(want))
	}
	for id, expected := range want {
		if actual, ok := got[id]; !ok || actual != expected {
			t.Errorf("offering literal %q kind=%q present=%t, want %q", id, actual, ok, expected)
		}
	}
}
