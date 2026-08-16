package config

import (
	"strings"
	"testing"
)

// A configuration file written before providers.bedrock.region existed must
// still load, and must behave exactly as it did — the region the console used to
// hardcode. This is the whole compatibility claim of the change.
//
// The subject is the real shipped template with the section cut back out, not a
// config hand-built here: a fixture written from what this change expects to
// find would test the expectation rather than the file operators actually have.
func TestConfigWithoutProvidersSectionKeepsTheHardcodedRegion(t *testing.T) {
	predating := templateWithoutProvidersSection(t)
	if strings.Contains(predating, "providers:") {
		t.Fatal("the providers section was not removed; the test would prove nothing")
	}
	cfg, err := Decode(strings.NewReader(predating))
	if err != nil {
		t.Fatalf("a config predating the section failed to decode: %v", err)
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if cfg.Providers.Bedrock.Region != DefaultBedrockRegion {
		t.Fatalf("omitted region became %q, want %q", cfg.Providers.Bedrock.Region, DefaultBedrockRegion)
	}
	if cfg.Version != SchemaVersion {
		t.Fatalf("adding the section moved the schema version to %d", cfg.Version)
	}
	if err := cfg.Validate(LoadOptions{}); err != nil {
		t.Fatalf("a config predating the section no longer validates: %v", err)
	}
}

func TestBedrockRegionIsCarriedThroughAndTrimmed(t *testing.T) {
	cfg, err := Decode(strings.NewReader(templateWithoutProvidersSection(t) +
		"\nproviders:\n  bedrock:\n    region: \"  eu-central-1  \"\n"))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if cfg.Providers.Bedrock.Region != "eu-central-1" {
		t.Fatalf("region is %q, want eu-central-1", cfg.Providers.Bedrock.Region)
	}
	if err := cfg.Validate(LoadOptions{}); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

// The region is substituted into an endpoint host, so a value that could move
// the host has to be refused rather than prefilled into a form.
func TestBedrockRegionRejectsAnythingThatIsNotARegionName(t *testing.T) {
	rejected := []string{
		"evil.com", "us-east-1/../x", "us east 1", "US-EAST-1", "-us-east-1",
		"us-east-1-", "us--east-1", "us-east-1:443", "us_east_1",
		strings.Repeat("a", maxBedrockRegionLength+1),
	}
	for _, region := range rejected {
		if validBedrockRegion(region) {
			t.Errorf("%q was accepted as a region", region)
		}
	}
	for _, region := range []string{"us-east-1", "eu-central-1", "ap-northeast-3", "us-gov-west-1", "cn-north-1"} {
		if !validBedrockRegion(region) {
			t.Errorf("%q was refused as a region", region)
		}
	}
}

// A rejected region must stop the process rather than reach a form, and the
// refusal has to name the key an operator would go and fix.
func TestInvalidBedrockRegionFailsValidation(t *testing.T) {
	cfg := Default()
	cfg.Providers.Bedrock.Region = "evil.com"
	err := cfg.Validate(LoadOptions{})
	if err == nil {
		t.Fatal("an endpoint-moving region was accepted")
	}
	if !strings.Contains(err.Error(), "providers.bedrock.region") {
		t.Fatalf("refusal does not name the key: %v", err)
	}
}

// templateWithoutProvidersSection is the shipped default template with the
// providers block and its comment removed — what an operator's config.yaml
// looked like before this change.
func templateWithoutProvidersSection(t *testing.T) string {
	t.Helper()
	lines := strings.Split(string(defaultTemplate), "\n")
	kept := make([]string, 0, len(lines))
	dropping := false
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "providers:"):
			dropping = true
			// Drop the comment block introducing it too, so what is left reads like
			// a file that never had the section.
			for len(kept) > 0 {
				last := kept[len(kept)-1]
				if strings.HasPrefix(last, "#") || strings.TrimSpace(last) == "" {
					kept = kept[:len(kept)-1]
					if strings.TrimSpace(last) == "" {
						break
					}
					continue
				}
				break
			}
		case dropping && (strings.HasPrefix(line, " ") || strings.TrimSpace(line) == ""):
			// still inside the block
		default:
			dropping = false
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}
