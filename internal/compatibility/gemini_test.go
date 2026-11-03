package compatibility

import (
	"slices"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/semantic"
)

func TestGeminiPrefillAdmissionUsesLastNonemptyConversationTurn(t *testing.T) {
	message := func(role semantic.Role, text string) semantic.Message {
		return semantic.Message{Role: role, Content: []semantic.Content{{Kind: semantic.ContentText, Text: text}}}
	}
	for _, test := range []struct {
		name    string
		tail    []semantic.Message
		blocked bool
	}{
		{"developer after prefill", []semantic.Message{message(semantic.RoleDeveloper, "instruction")}, true},
		{"system after prefill", []semantic.Message{message(semantic.RoleSystem, "instruction")}, true},
		{"empty user after prefill", []semantic.Message{message(semantic.RoleUser, "")}, true},
		{"nonempty user after assistant", []semantic.Message{message(semantic.RoleUser, "continue"), message(semantic.RoleDeveloper, "instruction")}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := semantic.GenerateRequest{Messages: append([]semantic.Message{message(semantic.RoleUser, "hi"), message(semantic.RoleAssistant, "prefill")}, test.tail...)}
			fields := UnsupportedGenerateFieldsForTarget(domain.ProfileGeminiText, "gemini-3.8-flash", request)
			if slices.Contains(fields, "messages[].role=assistant") != test.blocked {
				t.Fatalf("fields=%v, blocked=%v", fields, test.blocked)
			}
			if fields := GeminiTargetUnsupportedGenerateFields("gemini-2.5-pro", request); len(fields) != 0 {
				t.Fatalf("legacy contract changed: %v", fields)
			}
		})
	}
}
