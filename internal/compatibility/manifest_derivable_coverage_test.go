package compatibility

import (
	"slices"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/semantic"
)

// The published coverage is 65 hand-written rows, and nothing held them to the
// code that does the refusing. A field rule added without its manifest entry
// left a profile refusing a member the contract said it carried — which is what
// this test found on the two Mantle Responses profiles, for messages[].name and
// tools.
//
// It is a containment check rather than a generated list, because the coverage
// is not a function of the field rules. Some declarations are about endpoint
// members that never reach the semantic model at all — Anthropic's top_k,
// thinking, metadata and service_tier are dropped by the portable projection
// before any rule sees them — so the manifest legitimately says more than the
// rules can derive. What it must never do is say less.
func TestTheManifestDeclaresEverythingTheRulesRefuse(t *testing.T) {
	for _, manifest := range BuiltinEndpointManifests() {
		if manifest.SemanticOperation != semantic.OperationGenerate {
			continue
		}
		modelled := make(map[string]struct{}, len(manifest.RequestFields))
		for _, field := range manifest.RequestFields {
			modelled[field] = struct{}{}
		}
		for _, coverage := range manifest.ProfileCoverage {
			if servedNatively(manifest, coverage.ProfileID) {
				// Native mode forwards the caller's own bytes after schema
				// validation. No projection runs, so no field rule applies —
				// asking what this profile cannot carry answers about the
				// portable path, which this pairing never takes.
				continue
			}
			refused := map[string]struct{}{}
			for _, probe := range coverageProbes() {
				for _, field := range UnsupportedGenerateFields(coverage.ProfileID, probe) {
					name := endpointSpelling(manifest.ID, field)
					// A field this endpoint does not model has nowhere to be
					// declared: Validate refuses a coverage entry naming one.
					if _, ok := modelled[name]; ok {
						refused[name] = struct{}{}
					}
				}
			}
			for name := range refused {
				if slices.Contains(coverage.UnsupportedRequestFields, name) {
					continue
				}
				t.Errorf("%s: %s refuses %q and the manifest does not declare it",
					manifest.ID, coverage.ProfileID, name)
			}
		}
	}
}

// servedNatively reports the one pairing where a request reaches the provider
// unprojected: an Anthropic-shaped endpoint answered by the Anthropic Messages
// profile.
func servedNatively(manifest EndpointCompatibilityManifest, profileID domain.ProviderProfileID) bool {
	return manifest.Protocol == "anthropic" && profileID == domain.ProfileAnthropicMessages
}

// endpointSpelling maps the chat-shaped name a rule returns to the name the
// endpoint publishes for the same member. The rules speak one dialect so that
// routing can compare them; the manifest speaks each endpoint's own.
func endpointSpelling(endpointID, field string) string {
	switch endpointID {
	case "openai.responses.stateless.v1":
		switch field {
		case "messages[].content[].detail":
			return "input[].content[].detail"
		case "messages[].content[].image_url":
			return "input[].content[].image_url"
		case "response_format":
			return "text.format"
		}
	case "anthropic.messages.2023-06-01":
		switch field {
		case "response_format":
			return "output_config.format"
		case "reasoning_effort":
			return "output_config.effort"
		}
	}
	return field
}

// coverageProbes trips one rule at a time, and trips the value-dependent ones at
// each value that matters. A single maximal request cannot find them: a profile
// that carries reasoning_effort at "high" and refuses it at "minimal" declares
// the field, and a probe that only ever sends "high" would call that a clean
// carry.
func coverageProbes() []semantic.GenerateRequest {
	seed := int64(1)
	two := 2
	noParallel := false
	base := func() semantic.GenerateRequest {
		return semantic.GenerateRequest{Messages: []semantic.Message{{
			Role: semantic.RoleUser, Content: []semantic.Content{{Kind: semantic.ContentText, Text: "x"}},
		}}}
	}
	with := func(mutate func(*semantic.GenerateRequest)) semantic.GenerateRequest {
		request := base()
		mutate(&request)
		return request
	}
	probes := []semantic.GenerateRequest{
		with(func(r *semantic.GenerateRequest) { r.Messages[0].Name = "named" }),
		with(func(r *semantic.GenerateRequest) { r.Messages[0].Role = semantic.RoleDeveloper }),
		with(func(r *semantic.GenerateRequest) { r.Seed = &seed }),
		with(func(r *semantic.GenerateRequest) { r.Candidates = &two }),
		with(func(r *semantic.GenerateRequest) { r.Stop = []string{"s"} }),
		with(func(r *semantic.GenerateRequest) { r.EndUserRef = "u" }),
		with(func(r *semantic.GenerateRequest) { r.ParallelTools = &noParallel }),
		with(func(r *semantic.GenerateRequest) { r.IncludeUsage = true; r.Stream = true }),
		with(func(r *semantic.GenerateRequest) {
			r.Tools = []semantic.Tool{{Name: "f"}}
			r.ToolChoice = &semantic.ToolChoice{Mode: "auto"}
		}),
		with(func(r *semantic.GenerateRequest) {
			r.Tools = []semantic.Tool{{Name: "f"}}
			r.Stream = true
		}),
		with(func(r *semantic.GenerateRequest) {
			r.Messages[0].Content = []semantic.Content{{
				Kind: semantic.ContentInputImage, URL: "https://example.test/a.png", Detail: "high",
			}}
		}),
		with(func(r *semantic.GenerateRequest) {
			r.Messages = append(r.Messages,
				semantic.Message{Role: semantic.RoleAssistant, Content: []semantic.Content{{
					Kind: semantic.ContentToolCall, CallID: "c", Name: "f", Arguments: "{}",
				}}},
				semantic.Message{Role: semantic.RoleTool, Content: []semantic.Content{{
					Kind: semantic.ContentToolResult, CallID: "c", Text: "x", ToolError: true,
				}}})
		}),
	}
	for _, kind := range []semantic.OutputFormatKind{semantic.OutputJSONObject, semantic.OutputJSONSchema} {
		format := kind
		probes = append(probes, with(func(r *semantic.GenerateRequest) {
			r.OutputFormat = &semantic.OutputFormat{Kind: format, Schema: []byte(`{}`)}
		}))
	}
	for _, level := range []string{"minimal", "low", "medium", "high", "xhigh", "max"} {
		effort := level
		probes = append(probes, with(func(r *semantic.GenerateRequest) { r.ReasoningEffort = effort }))
	}
	for _, budget := range []int64{1, 0} {
		limit := budget
		probes = append(probes, with(func(r *semantic.GenerateRequest) {
			r.CompletionTokenLimit = &limit
			r.ReasoningEffort = "high"
		}))
	}
	return probes
}

// The probes have to be worth something: a battery that trips no rule on a
// profile known to declare several is a battery that would pass whatever the
// manifest said.
func TestTheProbesTripTheRulesTheyAreMeantTo(t *testing.T) {
	tripped := map[string]struct{}{}
	for _, probe := range coverageProbes() {
		for _, field := range UnsupportedGenerateFields(domain.ProfileGeminiText, probe) {
			tripped[field] = struct{}{}
		}
	}
	for _, expected := range []string{
		"messages[].name", "seed", "tools", "tool_choice",
		"parallel_tool_calls", "response_format", "reasoning_effort", "user",
	} {
		if _, ok := tripped[expected]; !ok {
			t.Errorf("no probe trips %q on the Gemini profile", expected)
		}
	}
}
