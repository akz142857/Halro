package redaction

import (
	"encoding/json"
	"os"
	"slices"
	"testing"

	"github.com/akz142857/Heimdall/internal/domain"
)

type piiCorpus struct {
	Positives []struct {
		Category string `json:"category"`
		Value    string `json:"value"`
	} `json:"positives"`
	Negatives []string `json:"negatives"`
}

func TestBuiltinPIICorpus(t *testing.T) {
	payload, err := os.ReadFile("testdata/pii_corpus.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus piiCorpus
	if err := json.Unmarshal(payload, &corpus); err != nil {
		t.Fatal(err)
	}
	categories := []string{
		"china_phone", "email", "china_id", "bank_card_candidate",
		"gateway_key", "openai_key", "anthropic_key", "google_key",
		"aws_access_key", "bearer_token", "private_key",
	}
	rules := make([]domain.RedactionRule, 0, len(categories))
	for _, category := range categories {
		rules = append(rules, domain.RedactionRule{
			ID: "rule_" + category, Name: category, Kind: "builtin", Builtin: category,
			Scopes: []string{"outbound"}, Action: "detect_only", Enabled: true,
		})
	}
	engine, err := New([]domain.RedactionPolicy{{
		ID: "corpus", Name: "Corpus", Enabled: true, Mode: "detect_only_stream",
		Rules: rules,
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, sample := range corpus.Positives {
		t.Run("positive_"+sample.Category, func(t *testing.T) {
			matches, err := engine.Test("corpus", sample.Value, "outbound")
			if err != nil {
				t.Fatal(err)
			}
			categories := make([]string, 0, len(matches))
			for _, match := range matches {
				categories = append(categories, match.Category)
			}
			if !slices.Contains(categories, sample.Category) {
				t.Fatalf("%q did not match %s; matches=%v", sample.Value, sample.Category, categories)
			}
		})
	}
	for _, sample := range corpus.Negatives {
		t.Run("negative", func(t *testing.T) {
			matches, err := engine.Test("corpus", sample, "outbound")
			if err != nil {
				t.Fatal(err)
			}
			if len(matches) != 0 {
				t.Fatalf("false positive for %q: %#v", sample, matches)
			}
		})
	}
}

func TestBankCardAndChinaIDSemanticValidators(t *testing.T) {
	for value, expected := range map[string]bool{
		"4111111111111111":    true,
		"4111-1111-1111-1111": true,
		"4111111111111112":    false,
	} {
		if got := validBankCard(value); got != expected {
			t.Fatalf("validBankCard(%q)=%v want=%v", value, got, expected)
		}
	}
	for value, expected := range map[string]bool{
		"11010519491231002X": true,
		"110105194912310021": false,
		"11010519490231002X": false,
	} {
		if got := validChinaID(value); got != expected {
			t.Fatalf("validChinaID(%q)=%v want=%v", value, got, expected)
		}
	}
}
