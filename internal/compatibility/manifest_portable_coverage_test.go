package compatibility_test

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/akz142857/Halro/internal/anthropicapi"
	"github.com/akz142857/Halro/internal/compatibility"
	anthropicwire "github.com/akz142857/Halro/internal/compatibility/anthropic"
	openaiwire "github.com/akz142857/Halro/internal/compatibility/openai"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/semantic"
)

// The manifest is the machine-readable answer to "what happens to this field on
// this profile", and output_config is the member where it had drifted: Gemini,
// Bedrock Converse, Mantle Responses and Mantle Anthropic all route a portable
// Messages request away when it carries an effort or a format, and none of them
// said so. The declaration was right and the published claim was silent, which
// is the failure that looks like success — an operator reading the manifest sees
// a profile that serves the field.
//
// This walks the real portable chain rather than calling the declaration with a
// hand-built semantic request: a Messages request reaches routing through
// DecodePortable, the OpenAI wire form, and DecodeGenerate, and it is the value
// that survives all three that routing judges. Anthropic's `max` effort, for
// one, never arrives — it fails at the OpenAI ladder — so a profile is not asked
// to declare it.
func TestPortableMessagesCoverageDeclaresEveryOutputConfigLoss(t *testing.T) {
	var messages compatibility.EndpointCompatibilityManifest
	for _, manifest := range compatibility.BuiltinEndpointManifests() {
		if manifest.ID == "anthropic.messages.2023-06-01" {
			messages = manifest
			break
		}
	}
	if messages.ID == "" {
		t.Fatal("the Messages endpoint manifest went missing")
	}

	// Each probe is one output_config member at one value, in the northbound
	// spelling the manifest documents.
	probes := []struct {
		field string
		body  string
	}{}
	for _, effort := range anthropicapi.EffortLevels {
		probes = append(probes, struct {
			field string
			body  string
		}{"output_config.effort", `{"effort":"` + effort + `"}`})
	}
	probes = append(probes,
		struct {
			field string
			body  string
		}{"output_config.format", `{"format":{"type":"text"}}`},
		struct {
			field string
			body  string
		}{"output_config.format", `{"format":{"type":"json_schema","name":"reply","schema":{"type":"object"}}}`},
	)

	declared := map[domain.ProviderProfileID]map[string]bool{}
	for _, coverage := range messages.ProfileCoverage {
		declared[coverage.ProfileID] = map[string]bool{}
	}
	for _, probe := range probes {
		canonical, reachable := portableMessagesRequest(t, probe.body)
		if !reachable {
			// The value cannot survive the portable projection, so no profile is
			// ever offered it and none of them has anything to declare.
			continue
		}
		for _, coverage := range messages.ProfileCoverage {
			fields := compatibility.UnsupportedGenerateFields(coverage.ProfileID, canonical)
			if slices.Contains(fields, semanticSpellingOf(probe.field)) {
				declared[coverage.ProfileID][probe.field] = true
			}
		}
	}

	for _, coverage := range messages.ProfileCoverage {
		for _, field := range []string{"output_config.effort", "output_config.format"} {
			listed := slices.Contains(coverage.UnsupportedRequestFields, field)
			switch {
			case declared[coverage.ProfileID][field] && !listed:
				t.Errorf("%s routes a portable Messages request away over %s and the manifest does not say so", coverage.ProfileID, field)
			case !declared[coverage.ProfileID][field] && listed:
				t.Errorf("%s serves every portable %s and the manifest claims it does not", coverage.ProfileID, field)
			}
		}
	}
}

// The same both-ways check on /v1/responses, for the two members whose support
// is value-dependent there. max_output_tokens is the one that moved: it decodes
// into the completion budget, DeepSeek declared that budget unsupported at every
// value, and every Responses request that bounded its output was therefore
// routed away from DeepSeek. Nothing on this endpoint thinks — it rejects the
// reasoning request field — so the budget bounds the answer and DeepSeek carries
// it as max_tokens.
func TestResponsesCoverageDeclaresEveryOutputBudgetAndFormatLoss(t *testing.T) {
	var responses compatibility.EndpointCompatibilityManifest
	for _, manifest := range compatibility.BuiltinEndpointManifests() {
		if manifest.ID == "openai.responses.stateless.v1" {
			responses = manifest
			break
		}
	}
	if responses.ID == "" {
		t.Fatal("the Responses endpoint manifest went missing")
	}

	limit := int64(256)
	probes := []struct {
		field   string
		request openaiapi.ResponseRequest
	}{
		{"max_output_tokens", openaiapi.ResponseRequest{Model: "probe", Input: json.RawMessage(`"hi"`), MaxOutputTokens: &limit}},
		{"text.format", openaiapi.ResponseRequest{Model: "probe", Input: json.RawMessage(`"hi"`), Text: &openaiapi.ResponseTextConfig{Format: openaiapi.ResponseTextFormat{Type: "text"}}}},
		{"text.format", openaiapi.ResponseRequest{Model: "probe", Input: json.RawMessage(`"hi"`), Text: &openaiapi.ResponseTextConfig{Format: openaiapi.ResponseTextFormat{Type: "json_object"}}}},
		{"text.format", openaiapi.ResponseRequest{Model: "probe", Input: json.RawMessage(`"hi"`), Text: &openaiapi.ResponseTextConfig{Format: openaiapi.ResponseTextFormat{Type: "json_schema", Name: "reply", Schema: json.RawMessage(`{"type":"object"}`), Strict: true}}}},
	}

	declared := map[domain.ProviderProfileID]map[string]bool{}
	for _, coverage := range responses.ProfileCoverage {
		declared[coverage.ProfileID] = map[string]bool{}
	}
	for _, probe := range probes {
		canonical, err := openaiwire.DecodeResponseGenerate(probe.request)
		if err != nil {
			// A body this endpoint refuses never reaches routing, so no profile is
			// asked to declare anything about it.
			continue
		}
		for _, coverage := range responses.ProfileCoverage {
			if slices.Contains(compatibility.UnsupportedGenerateFields(coverage.ProfileID, canonical), responsesSpellingOf(probe.field)) {
				declared[coverage.ProfileID][probe.field] = true
			}
		}
	}

	for _, coverage := range responses.ProfileCoverage {
		for _, field := range []string{"max_output_tokens", "text.format"} {
			listed := slices.Contains(coverage.UnsupportedRequestFields, field)
			switch {
			case declared[coverage.ProfileID][field] && !listed:
				t.Errorf("%s routes a Responses request away over %s and the manifest does not say so", coverage.ProfileID, field)
			case !declared[coverage.ProfileID][field] && listed:
				t.Errorf("%s serves every Responses %s and the manifest claims it does not", coverage.ProfileID, field)
			}
		}
	}
}

func responsesSpellingOf(field string) string {
	switch field {
	case "max_output_tokens":
		return "max_completion_tokens"
	case "text.format":
		return "response_format"
	default:
		return field
	}
}

// semanticSpellingOf maps the northbound member to the name the profile
// declaration uses. The two ladders are the same semantic field under two
// protocol spellings, which is exactly why the manifest coverage had to be
// written by hand and could drift.
func semanticSpellingOf(field string) string {
	switch field {
	case "output_config.effort":
		return "reasoning_effort"
	case "output_config.format":
		return "response_format"
	default:
		return field
	}
}

// portableMessagesRequest walks a Messages body through the same three steps the
// portable path takes before routing sees it. It reports whether the request
// reached routing at all: a body that dies in the projection is refused with a
// 400 naming the field, which is a different contract from being routed away.
func portableMessagesRequest(t *testing.T, outputConfig string) (semantic.GenerateRequest, bool) {
	t.Helper()
	body := `{"model":"probe","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"output_config":` + outputConfig + `}`
	var request anthropicapi.MessageRequest
	if err := json.Unmarshal([]byte(body), &request); err != nil {
		t.Fatalf("probe body is not a Messages request: %v", err)
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("probe body failed northbound validation: %v", err)
	}
	canonical, err := anthropicwire.DecodePortable(request)
	if err != nil {
		return semantic.GenerateRequest{}, false
	}
	wire, err := openaiwire.RenderGenerateRequest(canonical, request.Model)
	if err != nil {
		return semantic.GenerateRequest{}, false
	}
	decoded, err := openaiwire.DecodeGenerate(wire)
	if err != nil {
		return semantic.GenerateRequest{}, false
	}
	return decoded, true
}
