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
			// Exact, not one-directional. It used to check only that everything
			// refused was declared, which left the other side unverified: a
			// declaration no rule produces was simply believed. That is how
			// fourteen rows came to repeat one endpoint-level fact and one of
			// them came to state it incompletely — nothing could see the
			// difference.
			expected := make(map[string]struct{}, len(refused))
			for name := range refused {
				expected[name] = struct{}{}
			}
			if !servedNatively(manifest, coverage.ProfileID) && manifest.ID == "anthropic.messages.2023-06-01" {
				// Refused a layer above the rules: DecodePortable rejects these
				// outright rather than projecting them, so no field rule sees them.
				for _, name := range AnthropicPortableOnlyFields {
					expected[name] = struct{}{}
				}
			}
			for name := range expected {
				if !slices.Contains(coverage.UnsupportedRequestFields, name) {
					t.Errorf("%s: %s refuses %q and the manifest does not declare it",
						manifest.ID, coverage.ProfileID, name)
				}
			}
			for _, name := range coverage.UnsupportedRequestFields {
				if _, ok := expected[name]; !ok {
					t.Errorf("%s: %s declares %q unsupported and nothing refuses it",
						manifest.ID, coverage.ProfileID, name)
				}
			}
		}
	}
}

// servedNatively reports the one pairing where a request reaches the provider
// unprojected: an Anthropic-shaped endpoint answered by the Anthropic Messages
// profile.
func servedNatively(manifest EndpointCompatibilityManifest, profileID domain.ProviderProfileID) bool {
	if manifest.Protocol != "anthropic" {
		return false
	}
	// Every profile that can serve this endpoint natively, not just the first
	// one. A profile reachable in native mode carries the members the portable
	// decoder rejects, so the endpoint-level portable losses are not its losses —
	// which is why all three of these rows declare only what is theirs.
	//
	// Naming one of the three left the other two looking like drift, and the
	// exact check below reported them as such the moment it was turned on.
	switch profileID {
	case domain.ProfileAnthropicMessages, domain.ProfileBedrockMantleAnthropicMessages, domain.ProfileMiniMaxAnthropicMessages:
		return true
	}
	return false
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
		case "stop":
			// The same list under this endpoint's name for it. Missing, this
			// helper could not connect a profile's refusal of stop to the
			// stop_sequences its coverage declares, so every such profile had to
			// hand-write the entry and the guard could not check it.
			return "stop_sequences"
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
