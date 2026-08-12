package main

import (
	"strings"
	"testing"
)

func TestScrubOutputRemovesEveryConfiguredValue(t *testing.T) {
	values := map[string]string{"API_KEY": "secret-key", "BASE_URL": "https://provider.example"}
	output := scrubOutput("secret-key at https://provider.example", values)
	if strings.Contains(output, "secret-key") || strings.Contains(output, "provider.example") {
		t.Fatalf("configured value leaked: %q", output)
	}
}

func TestSmokeEnvironmentDoesNotExposeOtherMatrixProfiles(t *testing.T) {
	t.Setenv("HALRO_MATRIX_DEEPSEEK_API_KEY", "other-secret")
	environment := smokeEnvironment(gaProfiles[0], map[string]string{
		"BASE_URL": "https://api.openai.com/v1", "API_KEY": "selected-secret", "MODEL": "model", "EMBEDDING_MODEL": "embedding",
	})
	joined := strings.Join(environment, "\n")
	if strings.Contains(joined, "other-secret") || !strings.Contains(joined, "HALRO_SMOKE_API_KEY=selected-secret") ||
		!strings.Contains(joined, "HALRO_SMOKE_CAPABILITY_DETECTION=1") {
		t.Fatalf("unexpected child environment: %s", scrubOutput(joined, map[string]string{"API_KEY": "selected-secret"}))
	}
}

// A Bedrock Mantle result is only valid along the axes it was produced on. The
// evidence has to carry them, or a later reader will generalise one model in
// one region under one project mode into "Mantle works".
func TestBedrockMantleEvidenceRecordsTheCellItCovers(t *testing.T) {
	current := result{Profile: "bedrock_mantle", Status: "passed"}
	describeCell(&current, betaProfiles[0], map[string]string{
		"BASE_URL":           "https://bedrock-mantle.ap-southeast-1.api.aws",
		"MANTLE_PROFILE":     "messages",
		"MODEL":              "anthropic.claude-secret-entitlement",
		"BEDROCK_PROJECT_ID": "proj_abc123",
	})
	if current.Region != "ap-southeast-1" || current.WireProfile != "messages" ||
		current.Authentication != "bedrock_api_key" || current.ProjectMode != "explicit_project" {
		t.Fatalf("the cell was not recorded: %#v", current)
	}
	if !strings.HasPrefix(current.TargetDigest, "sha256:") || len(current.TargetDigest) != len("sha256:")+64 {
		t.Fatalf("target digest is not a sha256: %q", current.TargetDigest)
	}
	if strings.Contains(current.TargetDigest, "claude-secret-entitlement") {
		t.Fatal("the exact model reached shared evidence")
	}
}

// The digest exists to tell two cells apart, so it must actually change when
// the cell does. A constant would be worse than no digest at all.
func TestBedrockMantleTargetDigestSeparatesCells(t *testing.T) {
	base := map[string]string{
		"BASE_URL": "https://bedrock-mantle.us-east-1.api.aws", "MANTLE_PROFILE": "chat", "MODEL": "openai.gpt-test",
	}
	digest := func(overrides map[string]string) string {
		values := map[string]string{}
		for key, value := range base {
			values[key] = value
		}
		for key, value := range overrides {
			values[key] = value
		}
		current := result{}
		describeCell(&current, betaProfiles[0], values)
		return current.TargetDigest
	}
	original := digest(nil)
	for name, overrides := range map[string]map[string]string{
		"region":       {"BASE_URL": "https://bedrock-mantle.eu-west-1.api.aws"},
		"wire profile": {"MANTLE_PROFILE": "responses"},
		"model":        {"MODEL": "openai.gpt-other"},
		"project mode": {"BEDROCK_PROJECT_ID": "proj_abc123"},
	} {
		if digest(overrides) == original {
			t.Fatalf("changing the %s did not change the target digest", name)
		}
	}
}

// An unknown host must not produce a plausible-looking region. A wrong region
// on an evidence record is worse than a missing one.
func TestMantleRegionRefusesToGuess(t *testing.T) {
	for _, raw := range []string{
		"https://bedrock-runtime.us-east-1.amazonaws.com",
		"https://bedrock-mantle.us-east-1.api.aws.evil.example",
		"not a url at all",
		"",
	} {
		if region := mantleRegion(raw); region != "" {
			t.Fatalf("%q produced region %q", raw, region)
		}
	}
	if region := mantleRegion("https://bedrock-mantle.us-east-2.api.aws"); region != "us-east-2" {
		t.Fatalf("a valid Mantle endpoint produced region %q", region)
	}
}

// Beta profiles must be present in the evidence whether or not they ran, and
// must never be able to move the GA release gate.
func TestBetaProfilesAreNeverPartOfTheReleaseGate(t *testing.T) {
	for _, item := range betaProfiles {
		for _, ga := range gaProfiles {
			if item.Name == ga.Name || item.Prefix == ga.Prefix {
				t.Fatalf("%s is listed as both GA and Beta", item.Name)
			}
		}
		if item.Package == "" {
			t.Fatalf("%s has no smoke package, so -include-beta would run the OpenAI smoke against it", item.Name)
		}
	}
}
