package compatibility

import (
	"slices"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/semantic"
)

func TestOpenAIReviewedModelParameterContracts(t *testing.T) {
	temperature := 0.5
	limit := int64(10)
	for _, test := range []struct {
		model, effort string
		tools         bool
		want          []string
	}{
		{"gpt-6.1-sol", "none", true, []string{"reasoning_effort", "tools"}},
		{"gpt-6.1-sol", "max", true, []string{"tools"}},
		{"gpt-6-sol", "none", true, nil},
		{"gpt-6-sol", "", true, []string{"tools"}},
		{"gpt-6-luna", "low", true, []string{"tools"}},
		{"gpt-6.1-terra", "none", true, []string{"tools"}},
		{"gpt-6-future", "none", true, nil},
	} {
		request := semantic.GenerateRequest{ReasoningEffort: test.effort}
		if test.tools {
			request.Tools = []semantic.Tool{{Name: "ping"}}
		}
		fields := OpenAITargetUnsupportedGenerateFields(domain.ProfileOpenAIChatEmbeddings, test.model, request)
		if !slices.Equal(fields, test.want) {
			t.Errorf("%s/%s: %v want %v", test.model, test.effort, fields, test.want)
		}
	}
	request := semantic.GenerateRequest{ReasoningEffort: "high", Tools: []semantic.Tool{{Name: "ping"}}}
	if fields := OpenAITargetUnsupportedGenerateFields(domain.ProfileOpenAIResponses, "gpt-6.1-sol", request); len(fields) > 0 {
		t.Fatal(fields)
	}
	request.Temperature = &temperature
	request.TopP = &temperature
	request.VisibleOutputTokenLimit = &limit
	fields := OpenAITargetUnsupportedGenerateFields(domain.ProfileOpenAIResponses, "gpt-6.1-sol", request)
	if !slices.Equal(fields, []string{"temperature", "top_p", "max_tokens"}) {
		t.Fatal(fields)
	}
}
