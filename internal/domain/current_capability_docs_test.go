package domain

import (
	"os"
	"strings"
	"testing"
)

// Current operator-facing documents must say what the served profile table
// says. Implementation evidence is intentionally broader, so a future release
// that offers Runtime again has to update both the table and these summaries in
// the same change instead of leaving users with two current answers.
func TestCurrentDocsDescribeWithheldBedrockRuntimeProfiles(t *testing.T) {
	runtimeProfiles := []ProviderProfileID{
		ProfileBedrockConverseText,
		ProfileBedrockInvokeTitanEmbedV2,
		ProfileBedrockInvokeTitanImageV2,
		ProfileBedrockAgentRerankCohere35,
		ProfileBedrockAsyncNovaReel,
	}
	for _, profileID := range runtimeProfiles {
		if !IsWithheldProfile(profileID) {
			t.Skip("Bedrock Runtime is offered by this build; update the current capability summaries")
		}
	}

	want := map[string][]string{
		"../../README.md": {
			"Bedrock is offered through Mantle alone",
			"five Bedrock Runtime and Agent\nRuntime Profiles",
		},
		"../../docs/guides/user-guide.md": {
			"Bedrock Runtime and Agent Runtime implementations are **withheld in this\nbuild**",
		},
		"../../docs/guides/user-guide.zh-CN.md": {
			"Bedrock Runtime 与 Agent Runtime 的实现当前处于 **withheld** 状态",
		},
		"../../docs/guides/operator-guide.md": {
			"**Withheld in this build**; implemented profiles cannot be created or routed",
			"they are not setup instructions for this build",
		},
		"../../docs/architecture/api-provider-realtime-architecture.zh-CN.md": {
			"当前构建只通过 Mantle 提供 Bedrock",
		},
		"../../docs/milestones/implementation-status.md": {
			"Bedrock Runtime adapter (withheld)",
			"Bedrock Invoke Phase 2A (withheld)",
		},
	}
	for path, phrases := range want {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, phrase := range phrases {
			if !strings.Contains(string(body), phrase) {
				t.Errorf("%s does not carry current capability marker %q", path, phrase)
			}
		}
	}
}
