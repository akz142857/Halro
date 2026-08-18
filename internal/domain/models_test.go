package domain

import (
	"strings"
	"testing"
)

// The allowlist is only ever read by the native Anthropic Messages path, which
// is a property of the profile, not the surface. Bedrock Mantle also carries
// OpenAI chat and responses profiles, and a token stored on one of those is the
// stored-but-never-sent setting this check exists to prevent.
func TestAnthropicBetasAreValidatedAgainstTheProfileNotTheSurface(t *testing.T) {
	for _, testCase := range []struct {
		profile ProviderProfileID
		allowed bool
	}{
		{ProfileAnthropicMessages, true},
		{ProfileBedrockMantleAnthropicMessages, true},
		{ProfileBedrockMantleOpenAIChat, false},
		{ProfileBedrockMantleOpenAIResponses, false},
		{ProfileOpenAIChatEmbeddings, false},
	} {
		if got := ProfileSendsAnthropicBetas(testCase.profile); got != testCase.allowed {
			t.Fatalf("%s: sends betas=%v, want %v", testCase.profile, got, testCase.allowed)
		}
	}
}

// Defaults are what a new connection starts with; the ceiling is what an
// operator may turn on. Collapsing them made every optional capability either
// always-on or unreachable.
func TestProviderExecutedToolsIsReachableButNotADefault(t *testing.T) {
	if DefaultProviderCapabilitiesForProfile(ProviderAnthropic, ProfileAnthropicMessages).ProviderExecutedTools {
		t.Fatal("upstream egress must not be on by default")
	}
	if !MaxProviderCapabilitiesForProfile(ProviderAnthropic, ProfileAnthropicMessages).ProviderExecutedTools {
		t.Fatal("the Anthropic profile must allow the operator to enable it")
	}
	// The Mantle ceiling is fixed by the build; widening it is a separate review.
	if MaxProviderCapabilitiesForProfile(ProviderBedrock, ProfileBedrockMantleAnthropicMessages).ProviderExecutedTools {
		t.Fatal("the Mantle ceiling was widened without a contract review")
	}
}

// The alias list decides which public models a project may call, and the
// Gateway scans it on every request. Its shape used to be checked only by the
// Admin handler that writes it, which left the Admin API — a public write
// surface — able to store a list the domain model would never have accepted.
func TestProjectValidatesTheShapeOfItsAllowedModels(t *testing.T) {
	valid := func(aliases []string) Project {
		return Project{ID: "project_1", Name: "www", AllowedModels: aliases}
	}
	for _, testCase := range []struct {
		name    string
		aliases []string
		wantErr string
	}{
		{name: "distinct aliases", aliases: []string{"chat", "gpt-chat"}},
		{name: "no aliases", aliases: nil},
		{name: "repeated alias", aliases: []string{"chat", "gpt-chat", "chat"}, wantErr: "repeats alias chat"},
		{name: "empty alias", aliases: []string{"chat", ""}, wantErr: "must not contain an empty alias"},
		{name: "blank alias", aliases: []string{"   "}, wantErr: "must not contain an empty alias"},
		{name: "padded alias", aliases: []string{" chat"}, wantErr: "must not be padded with whitespace"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := valid(testCase.aliases).Validate()
			if testCase.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("accepted %v", testCase.aliases)
			}
			if !strings.Contains(err.Error(), testCase.wantErr) {
				t.Fatalf("error %q does not name the fault %q", err, testCase.wantErr)
			}
		})
	}
}

// Bounded because the list is scanned per request, so an Admin write should not
// be able to make every Gateway request arbitrarily more expensive.
func TestProjectRejectsAnUnboundedAliasList(t *testing.T) {
	aliases := make([]string, 0, MaxProjectAllowedModels+1)
	for index := range MaxProjectAllowedModels + 1 {
		aliases = append(aliases, "alias_"+string(rune('a'+index%26))+string(rune('a'+index/26)))
	}
	project := Project{ID: "project_1", Name: "www", AllowedModels: aliases}
	if err := project.Validate(); err == nil {
		t.Fatalf("accepted %d aliases", len(aliases))
	}
	if err := (Project{ID: "project_1", Name: "www", AllowedModels: aliases[:MaxProjectAllowedModels]}).Validate(); err != nil {
		t.Fatalf("rejected the maximum permitted list: %v", err)
	}
}
