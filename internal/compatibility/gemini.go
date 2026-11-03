package compatibility

import "github.com/akz142857/Halro/internal/semantic"

// Exact Generate Content contracts reviewed on 2026-11-03. Unknown identifiers
// do not inherit restrictions from a family prefix. Legacy 2.5 identifiers keep
// their existing contract until Google's announced 2026-12-01 migration.
func GeminiTargetUnsupportedGenerateFields(model string, request semantic.GenerateRequest) []string {
	switch model {
	case "gemini-4-argon", "gemini-3.8-flash", "gemini-3.7-flash", "gemini-3.6-flash", "gemini-3.5-flash", "gemini-3.5-flash-lite":
	default:
		return nil
	}
	var fields []string
	if request.Temperature != nil {
		fields = append(fields, "temperature")
	}
	if request.TopP != nil {
		fields = append(fields, "top_p")
	}
	if request.Candidates != nil {
		fields = append(fields, "n")
	}
	if len(request.Messages) > 0 && request.Messages[len(request.Messages)-1].Role == semantic.RoleAssistant {
		fields = append(fields, "messages[].role=assistant")
	}
	return fields
}
