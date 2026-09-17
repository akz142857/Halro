package config

import (
	"strings"
	"testing"
)

// v0.8.1 wrote this section into the shipped template. Configuration decoding
// is intentionally strict, so retaining the legacy shape is what lets an
// existing installation reach v0.8.2 without editing config.yaml first.
func TestV081BedrockRegionConfigurationStillStarts(t *testing.T) {
	legacy := string(defaultTemplate) + `
providers:
  bedrock:
    region: "  us-east-1  "
`
	cfg, err := Decode(strings.NewReader(legacy))
	if err != nil {
		t.Fatalf("decode v0.8.1 config: %v", err)
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatalf("normalize v0.8.1 config: %v", err)
	}
	if got := cfg.LegacyProviders.Bedrock.Region; got != "us-east-1" {
		t.Fatalf("legacy region = %q, want us-east-1", got)
	}
	if err := cfg.Validate(LoadOptions{}); err != nil {
		t.Fatalf("validate v0.8.1 config: %v", err)
	}
}

func TestLegacyBedrockRegionRetainsItsValidationBoundary(t *testing.T) {
	cfg := Default()
	cfg.LegacyProviders.Bedrock.Region = "evil.example/path"
	err := cfg.Validate(LoadOptions{})
	if err == nil || !strings.Contains(err.Error(), "providers.bedrock.region") {
		t.Fatalf("invalid legacy region was not rejected with its configuration key: %v", err)
	}
}
