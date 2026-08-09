package app

import (
	"testing"

	referenceconfigs "github.com/akz142857/Halro/configs"
	"github.com/akz142857/Halro/internal/config"
	"gopkg.in/yaml.v3"
)

func TestDescribeSystemConfigUsesReferenceMetadataAndEffectiveValues(t *testing.T) {
	effective := []byte(`version: 1
server:
  gateway_listen: 0.0.0.0:8080
tls:
  enabled: true
security:
  trusted_proxy_cidrs: []
`)
	entries := describeSystemConfig(referenceconfigs.ExampleYAML, effective)
	byPath := make(map[string]systemConfigEntry, len(entries))
	for _, entry := range entries {
		byPath[entry.Path] = entry
	}

	gateway := byPath["server.gateway_listen"]
	if gateway.Value != "0.0.0.0:8080" || gateway.TitleZH != "Gateway 监听地址" || gateway.TitleEN != "Gateway listen address" {
		t.Fatalf("gateway entry=%#v", gateway)
	}
	tls := byPath["tls.enabled"]
	if tls.Kind != "boolean" || tls.Value != "true" || tls.DescriptionZH == "" || tls.DescriptionEN == "" {
		t.Fatalf("tls entry=%#v", tls)
	}
	trusted := byPath["security.trusted_proxy_cidrs"]
	if trusted.Kind != "collection" || trusted.Value != "[]" {
		t.Fatalf("trusted proxy entry=%#v", trusted)
	}
}

func TestConfigReferenceDescribesEveryDefaultEffectiveField(t *testing.T) {
	rendered, err := yaml.Marshal(config.Default())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range describeSystemConfig(referenceconfigs.ExampleYAML, rendered) {
		if entry.TitleZH == "" || entry.TitleEN == "" || entry.DescriptionZH == "" || entry.DescriptionEN == "" {
			t.Errorf("%s is missing bilingual reference metadata: %#v", entry.Path, entry)
		}
	}
}

func TestDescribeSystemConfigKeepsUnannotatedEffectiveFieldsVisible(t *testing.T) {
	entries := describeSystemConfig([]byte("known:\n  value: 1\n"), []byte("known:\n  value: 2\nextra: true\n"))
	if len(entries) != 2 {
		t.Fatalf("entries=%#v", entries)
	}
	if entries[0].Path != "known.value" || entries[0].Value != "2" || entries[1].Path != "extra" || entries[1].Value != "true" {
		t.Fatalf("entries=%#v", entries)
	}
}
