package redaction

import (
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/semantic"
)

func citationResult(url string) semantic.GenerateResult {
	return semantic.GenerateResult{
		ID: "resp_1", Model: "chat", MappingRevision: 1, Translation: semantic.TranslationNone,
		Choices: []semantic.GenerateChoice{{Index: 0, Message: semantic.Message{
			Role: semantic.RoleAssistant,
			Content: []semantic.Content{{
				Kind: semantic.ContentText, Text: "see the docs",
				Citations: []semantic.Citation{{
					URL: url, Title: "Docs", StartIndex: 0, EndIndex: 3,
				}},
			}},
		}}},
	}
}

func replacePolicy(t *testing.T, pattern, replacement string) domain.RedactionPolicy {
	t.Helper()
	return domain.RedactionPolicy{
		ID: "redaction_replace", Name: "Replace", Enabled: true, Mode: "strict",
		Rules: []domain.RedactionRule{{
			ID: "url", Name: "URL", Kind: "regex", Pattern: pattern,
			Scopes: []string{"outbound"}, Action: "replace", Replacement: replacement, Enabled: true,
		}},
	}
}

// A replacement template was stored as free text and never checked against the
// pattern it belongs to: the domain rejects only an empty or over-long string,
// and Expand answers a reference to a group that does not exist with nothing at
// all. So `$1` on a pattern with no capture groups was accepted and then deleted
// what it matched instead of masking it.
//
// A reference to a group the pattern does not have is unambiguously a mistake —
// there is no rule for which it is the intended behaviour — so it is refused
// where it is written rather than at the far end of a request.
func TestReplacementReferringToAMissingGroupIsRefusedAtCompile(t *testing.T) {
	for _, template := range []string{"$1", "${1}", "$2x", "$name"} {
		if _, err := New([]domain.RedactionPolicy{replacePolicy(t, `https://[^\s]+`, template)}); err == nil {
			t.Fatalf("replacement %q was accepted against a pattern with no capture groups", template)
		} else if !strings.Contains(err.Error(), "capture group") {
			t.Fatalf("replacement %q was refused for the wrong reason: %v", template, err)
		}
	}
}

// The templates that stay allowed are the ones that are sometimes right. `$1` on
// a pattern that does have a group is how a rule keeps the part it means to keep
// — the last four digits, the scheme, the domain — and a group that is optional
// expands to nothing only on the inputs where it did not take part.
//
// So a policy can still empty a field that is required to be non-empty, and this
// pins that it can: the compile-time guard narrows the mistake, it does not
// remove the case. What stands behind it is the gateway's check between redaction
// and the ledger — see TestUnrenderableAnswerIsNotRecordedAsSuccess in
// internal/gateway, which pins that such a request is no longer recorded as a
// success while the caller is told it failed.
func TestOptionalGroupReplacementIsAllowedAndCanStillEmptyAField(t *testing.T) {
	policy := replacePolicy(t, `https://[^\s]+?(never-here)?$`, "$1")
	engine, err := New([]domain.RedactionPolicy{policy})
	if err != nil {
		t.Fatalf("a reference to a group the pattern does have was refused: %v", err)
	}
	result := citationResult("https://example.test/page")
	if err := result.Validate(); err != nil {
		t.Fatalf("the fixture was invalid before redaction ran: %v", err)
	}
	processed, err := engine.ProcessOutboundGenerateResult(policy.ID, result)
	if err != nil {
		t.Fatalf("redaction refused the answer outright, which is not the case under test: %v", err)
	}
	if got := processed.Choices[0].Message.Content[0].Citations[0].URL; got != "" {
		t.Fatalf("citation URL = %q, want empty — the optional group did not expand to nothing", got)
	}
	if err := processed.Validate(); err == nil {
		t.Fatal("a result with an empty citation URL validated; the gateway's post-redaction check would have nothing to catch")
	}
}

// Detail and Status carry a string that no other pass on the outbound traversal
// looks at, so a secret in either used to reach the caller beside the fields that
// were rewritten. They are checked rather than masked, because both are read as a
// fixed vocabulary at the other end.
func TestMandatoryBaselineCoversDetailAndStatus(t *testing.T) {
	engine := NewDefault()
	key := "sk-ant-" + strings.Repeat("a", 32)
	for name, result := range map[string]semantic.GenerateResult{
		"status": {
			ID: "resp_1", Model: "chat", MappingRevision: 1, Translation: semantic.TranslationNone,
			Choices: []semantic.GenerateChoice{{Index: 0, Message: semantic.Message{
				Role: semantic.RoleAssistant,
				Content: []semantic.Content{
					{Kind: semantic.ContentText, Text: "done"},
					{Kind: semantic.ContentProviderToolCall, Name: "web_search", CallID: "call_1", Text: "docs", Status: key},
				},
			}}},
		},
	} {
		if _, err := engine.ProcessOutboundGenerateResult("", result); err == nil {
			t.Fatalf("a provider key in %s reached the caller", name)
		}
	}
}
