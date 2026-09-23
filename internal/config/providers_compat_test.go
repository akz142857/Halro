package config

import (
	"errors"
	"strings"
	"testing"
)

// v0.8.1 wrote a `providers.bedrock.region` into the shipped template. When
// provider connections moved into the Admin-managed credential workflow the
// section was kept in the decoding contract so an existing installation could
// reach v0.8.2 without editing config.yaml first — accepted, trimmed and
// validated, while nothing read it.
//
// That is the worst of the three possible answers. An operator who still has
// the section believes it is choosing the region their Bedrock calls use; the
// value is checked, so it even looks like it is being acted on. A refusal that
// names where the setting went is what tells them otherwise, and it is what
// every other retired key now gets.
func TestTheV081ProvidersSectionIsRefusedRatherThanAccepted(t *testing.T) {
	legacy := string(defaultTemplate) + `
providers:
  bedrock:
    region: "  us-east-1  "
`
	_, err := Decode(strings.NewReader(legacy))
	var retired *RetiredKeyError
	if !errors.As(err, &retired) {
		t.Fatalf("a v0.8.1 providers section was not answered by the retirement table: %v", err)
	}
	if !strings.Contains(retired.Error(), "providers") {
		t.Errorf("the refusal does not name the section: %v", retired)
	}
	if !retired.Migratable {
		t.Error("removing a section nothing reads needs no decision from the operator")
	}
}

// TestTheV081ProvidersSectionMigratesAway pairs with it: the refusal has a way
// out that costs the operator nothing, because there is no value to carry —
// the region now belongs to the credential the console holds.
func TestTheV081ProvidersSectionMigratesAway(t *testing.T) {
	legacy := string(defaultTemplate) + `
providers:
  bedrock:
    region: us-east-1
`
	result, err := Migrate([]byte(legacy))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Refusals) > 0 {
		t.Fatalf("refused: %v", result.Refusals)
	}
	if strings.Contains(string(result.Output), "bedrock") {
		t.Error("the retired section survived the migration")
	}
	if _, err := Load(writeTemp(t, result.Output), LoadOptions{}); err != nil {
		t.Fatalf("the migrated configuration does not load: %v", err)
	}
}
