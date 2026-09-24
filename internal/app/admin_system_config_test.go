package app

import (
	"bytes"
	"strings"
	"testing"

	referenceconfigs "github.com/akz142857/Halro/configs"
	"github.com/akz142857/Halro/internal/config"
	"gopkg.in/yaml.v3"
)

func TestDescribeSystemConfigUsesReferenceMetadataAndEffectiveValues(t *testing.T) {
	effective := []byte(`version: 2
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

// TestConfigReferenceValuesMatchTheShippedDefaults reads the reference file the
// way the binary would and holds its values to the defaults the binary actually
// has. Nothing else checked this: the console takes only titles and
// descriptions from this file, so a value here can drift from Default() without
// any test noticing, and the operator reading it is told a default that does
// not exist. It has already happened once — max_provider_calls stayed at 8
// after the detection budget moved to 10.
//
// The comparison is on decoded values rather than text, so the reference is
// free to spell 90s where Default() marshals 1m30s.
func TestConfigReferenceValuesMatchTheShippedDefaults(t *testing.T) {
	reference, err := config.Decode(bytes.NewReader(referenceconfigs.ExampleYAML))
	if err != nil {
		t.Fatalf("the reference configuration no longer decodes: %v", err)
	}
	if err := reference.Normalize(); err != nil {
		t.Fatalf("normalize reference: %v", err)
	}
	shipped := config.Default()
	if err := shipped.Normalize(); err != nil {
		t.Fatal(err)
	}
	// The one deliberate exception. "UTC" would not show an operator what an
	// IANA name looks like, and this field is the one place the reference has
	// to teach that. Every other value is a claim about the default.
	if reference.Usage.Timezone == "" {
		t.Fatal("the reference must still name an accounting timezone")
	}
	reference.Usage.Timezone, shipped.Usage.Timezone = "", ""

	referenceYAML, err := yaml.Marshal(reference)
	if err != nil {
		t.Fatal(err)
	}
	shippedYAML, err := yaml.Marshal(shipped)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(referenceYAML, shippedYAML) {
		return
	}
	referenceLines := bytes.Split(referenceYAML, []byte("\n"))
	shippedLines := bytes.Split(shippedYAML, []byte("\n"))
	for index := range max(len(referenceLines), len(shippedLines)) {
		var fromReference, fromShipped []byte
		if index < len(referenceLines) {
			fromReference = referenceLines[index]
		}
		if index < len(shippedLines) {
			fromShipped = shippedLines[index]
		}
		if !bytes.Equal(fromReference, fromShipped) {
			t.Errorf("configs/config.example.yaml claims %q where the binary defaults to %q",
				bytes.TrimSpace(fromReference), bytes.TrimSpace(fromShipped))
		}
	}
}

// A description too long for one line is written as several lines carrying the
// same annotation name. The console has to receive them as one description.
//
// It did not: the parser assigned rather than joined, so every wrapped
// description reached the console as its closing clause alone —
// gateway.max_total_attempts, whose four lines exist to explain that setting it
// below the fallback chain leaves the last target never called, showed only
// "...set it below the longest fallback chain and the last target is configured
// and never called." with nothing before it to say what it was about.
func TestAWrappedDescriptionReachesTheConsoleWhole(t *testing.T) {
	rendered, err := yaml.Marshal(config.Default())
	if err != nil {
		t.Fatal(err)
	}
	var wrapped systemConfigEntry
	for _, entry := range describeSystemConfig(referenceconfigs.ExampleYAML, rendered) {
		if entry.Path == "gateway.max_total_attempts" {
			wrapped = entry
		}
	}
	// The opening clause and the closing one, which the reference writes on
	// different lines. Holding both is what proves they were joined rather than
	// the last one kept.
	for _, want := range []string{
		"Total attempts allowed across route targets",
		"the last target is configured and never called",
	} {
		if !strings.Contains(wrapped.DescriptionEN, want) {
			t.Errorf("gateway.max_total_attempts description lost %q: %q", want, wrapped.DescriptionEN)
		}
	}
	if !strings.Contains(wrapped.DescriptionZH, "一次请求跨路由目标允许的尝试总数") ||
		!strings.Contains(wrapped.DescriptionZH, "永远不会被调用") {
		t.Errorf("gateway.max_total_attempts zh description=%q", wrapped.DescriptionZH)
	}
	// Joined, not glued with the space an English line break stands for:
	// Chinese sets none, so one after its punctuation is a visible gap.
	for _, gap := range []string{"， ", "。 ", "； ", "： "} {
		if strings.Contains(wrapped.DescriptionZH, gap) {
			t.Errorf("gateway.max_total_attempts zh description has a stray space: %q", wrapped.DescriptionZH)
		}
	}
}
