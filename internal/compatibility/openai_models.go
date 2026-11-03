package compatibility

import (
	"slices"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/semantic"
)

// Exact IDs reviewed against the OpenAI model pages and GPT-6 migration guide
// on 2026-11-03. An unknown model never inherits a contract from its prefix.
func OpenAIModelEfforts(model string) []string {
	switch model {
	case "gpt-6-astra", "gpt-6.1-sol":
		return []string{"low", "medium", "high", "xhigh", "max"}
	case "gpt-6.1-terra", "gpt-6-sol", "gpt-6-luna":
		return []string{"none", "low", "medium", "high", "xhigh", "max"}
	default:
		return nil
	}
}

func OpenAITargetUnsupportedGenerateFields(profile domain.ProviderProfileID, model string, r semantic.GenerateRequest) []string {
	efforts := OpenAIModelEfforts(model)
	if len(efforts) == 0 {
		return nil
	}
	var fields []string
	if r.ReasoningEffort != "" && !slices.Contains(efforts, r.ReasoningEffort) {
		fields = append(fields, "reasoning_effort")
	}
	thinking := r.ReasoningEffort != "none"
	if thinking {
		if r.Temperature != nil {
			fields = append(fields, "temperature")
		}
		if r.TopP != nil {
			fields = append(fields, "top_p")
		}
	}
	// These models replaced the deprecated answer-only max_tokens parameter with
	// max_completion_tokens (Chat) / max_output_tokens (Responses). Do not silently
	// reinterpret an answer-only bound as one that also includes reasoning.
	if r.VisibleOutputTokenLimit != nil {
		fields = append(fields, "max_tokens")
	}
	if profile == domain.ProfileOpenAIChatEmbeddings && len(r.Tools) > 0 {
		if model == "gpt-6-astra" || model == "gpt-6.1-sol" || model == "gpt-6.1-terra" || thinking {
			fields = append(fields, "tools")
		}
	}
	return fields
}
