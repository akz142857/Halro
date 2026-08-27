package redaction

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/semantic"
)

// leakedKey is a mandatory-baseline match, so these tests need no Project
// policy: the leak they pin down is reachable on a Project that configured
// nothing. A Gateway Key is the apt shape for it — the caller's own credential
// coming back out of a search the model ran on its behalf.
const leakedKey = "gw_abcdefghijklmnopqrstuvwxyz0123456789ABCD"

func searchResult() semantic.GenerateResult {
	return semantic.GenerateResult{
		ID: "resp_1", Created: 1, Model: "provider-model",
		Translation: semantic.TranslationNone, MappingRevision: 1,
		Choices: []semantic.GenerateChoice{{Index: 0, Termination: "complete", Message: semantic.Message{
			Role: semantic.RoleAssistant,
			Content: []semantic.Content{
				{
					Kind: semantic.ContentProviderToolCall, CallID: "ws_1",
					Name: semantic.ProviderToolWebSearch, Status: "completed",
					Text: "acme wiki " + leakedKey,
				},
				{
					Kind: semantic.ContentText,
					Text: "The key " + leakedKey + " is in the dump.",
					Citations: []semantic.Citation{{
						URL:   "https://intranet.test/doc?token=" + leakedKey,
						Title: leakedKey + " dump",
						// The whole answer, which is what a single-source reply
						// normally carries.
						StartIndex: 0, EndIndex: len("The key " + leakedKey + " is in the dump."),
					}},
				},
			},
		}}},
	}
}

// The query the model wrote and the sources it read reach the caller as
// action.query and as url_citation annotations. Nothing else on the outbound
// path rewrites either one, so if this traversal skips them the same secret is
// [REDACTED] in the answer and verbatim beside it.
func TestOutboundRedactionCoversTheProviderToolQueryAndCitationSources(t *testing.T) {
	engine := NewDefault()
	processed, err := engine.ProcessOutboundGenerateResult("", searchResult())
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(processed)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), leakedKey) {
		t.Fatalf("the secret survived somewhere in the result: %s", encoded)
	}
	content := processed.Choices[0].Message.Content
	if !strings.Contains(content[0].Text, "[REDACTED]") {
		t.Fatalf("the provider tool query was not rewritten: %q", content[0].Text)
	}
	if !strings.Contains(content[1].Citations[0].URL, "[REDACTED]") {
		t.Fatalf("the citation URL was not rewritten: %q", content[1].Citations[0].URL)
	}
	if !strings.Contains(content[1].Citations[0].Title, "[REDACTED]") {
		t.Fatalf("the citation title was not rewritten: %q", content[1].Citations[0].Title)
	}
	// The parts the traversal already covered must still behave as before.
	if !strings.Contains(content[1].Text, "[REDACTED]") || !strings.Contains(content[1].Text, "is in the dump") {
		t.Fatalf("the answer text was mangled: %q", content[1].Text)
	}
}

// Redaction changes the length of what it rewrites and leaves no diff behind, so
// a span measured against the old text describes the new text only by accident.
// Out of range, the result fails Validate at render time — after the attempt has
// settled. In range, it validates and silently attributes the source to
// different words. Both are answered by collapsing the span.
func TestOutboundRedactionCollapsesACitationSpanItCanNoLongerPlace(t *testing.T) {
	engine := NewDefault()
	original := searchResult()
	before := original.Choices[0].Message.Content[1].Citations[0]

	processed, err := engine.ProcessOutboundGenerateResult("", original)
	if err != nil {
		t.Fatal(err)
	}
	part := processed.Choices[0].Message.Content[1]
	if len(part.Text) >= before.EndIndex {
		t.Fatalf("this test needs the rewrite to shorten the text: len=%d span end=%d", len(part.Text), before.EndIndex)
	}

	citation := part.Citations[0]
	if citation.StartIndex != 0 || citation.EndIndex != 0 {
		t.Fatalf("a span that no longer describes its text was kept: %#v", citation)
	}
	if citation.URL == "" {
		t.Fatal("the source itself was dropped; only the span should have been")
	}
	// The whole point: what comes out of redaction can still be rendered. Before
	// this fix Validate refused here, after settlement had already committed.
	if err := processed.Validate(); err != nil {
		t.Fatalf("a redacted result can no longer be rendered: %v", err)
	}
}

// The caller's slice is the message that was routed, and on a retry the message
// that will be routed again. Copying the Content header is not enough for a
// member the traversal edits — Citations is a slice of its own.
func TestOutboundRedactionDoesNotMutateTheCallersCitations(t *testing.T) {
	engine := NewDefault()
	original := searchResult()
	if _, err := engine.ProcessOutboundGenerateResult("", original); err != nil {
		t.Fatal(err)
	}
	citation := original.Choices[0].Message.Content[1].Citations[0]
	if !strings.Contains(citation.URL, leakedKey) || citation.EndIndex == 0 {
		t.Fatalf("the traversal edited the caller's own citation: %#v", citation)
	}
}

// A kind added to the vocabulary without a case in the traversal would reach the
// caller with no rule and no baseline applied. On the one pass whose purpose is
// to catch a secret in provider output, silence is the fail-open.
func TestOutboundRedactionRefusesAContentKindItCannotTraverse(t *testing.T) {
	engine := NewDefault()
	result := semantic.GenerateResult{
		ID: "resp_1", Created: 1, Model: "provider-model",
		Choices: []semantic.GenerateChoice{{Index: 0, Termination: "complete", Message: semantic.Message{
			Role:    semantic.RoleAssistant,
			Content: []semantic.Content{{Kind: "input_audio", Text: leakedKey}},
		}}},
	}
	processed, err := engine.ProcessOutboundGenerateResult("", result)
	if err == nil {
		t.Fatal("an untraversable content kind was served instead of refused")
	}
	if !strings.Contains(err.Error(), "input_audio") {
		t.Fatalf("the refusal does not name the kind: %v", err)
	}
	encoded, marshalErr := json.Marshal(processed)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if !strings.Contains(string(encoded), leakedKey) {
		t.Fatal("this test no longer proves the untouched result is returned on refusal")
	}
}

// Reasoning is model output like any other text, and the traversal had no case
// for it either. It is covered here rather than left to the new default, because
// refusing it would refuse every reasoning-capable deployment.
func TestOutboundRedactionCoversReasoningText(t *testing.T) {
	engine := NewDefault()
	result := semantic.GenerateResult{
		ID: "resp_1", Created: 1, Model: "provider-model",
		Choices: []semantic.GenerateChoice{{Index: 0, Termination: "complete", Message: semantic.Message{
			Role:    semantic.RoleAssistant,
			Content: []semantic.Content{{Kind: semantic.ContentReasoning, Text: "the key is " + leakedKey}},
		}}},
	}
	processed, err := engine.ProcessOutboundGenerateResult("", result)
	if err != nil {
		t.Fatal(err)
	}
	if text := processed.Choices[0].Message.Content[0].Text; strings.Contains(text, leakedKey) {
		t.Fatalf("reasoning text was served unredacted: %q", text)
	}
}
