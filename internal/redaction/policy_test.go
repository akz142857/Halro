package redaction

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
)

func TestCompilePolicyRejectsUnboundedStreamingEnforcement(t *testing.T) {
	_, err := CompilePolicy(domain.RedactionPolicy{
		ID: "policy", Name: "Policy", Enabled: true, Mode: "bounded_stream",
		Rules: []domain.RedactionRule{{
			ID: "email", Name: "Email", Kind: "builtin", Builtin: "email",
			Scopes: []string{"outbound"}, Action: "replace", Replacement: "[EMAIL]",
			Enabled: true,
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "bounded streaming width") {
		t.Fatalf("unbounded enforcement was accepted: %v", err)
	}
	policy, err := CompilePolicy(domain.RedactionPolicy{
		ID: "policy", Name: "Policy", Enabled: true, Mode: "bounded_stream",
		Rules: []domain.RedactionRule{{
			ID: "phone", Name: "Phone", Kind: "builtin", Builtin: "china_phone",
			Scopes: []string{"inbound", "outbound"}, Action: "mask", Enabled: true,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if policy.Rules[0].ComputedMaxMatchBytes <= 0 {
		t.Fatalf("finite width was not computed: %#v", policy.Rules[0])
	}
}

func TestBoundedStreamingUsesRollingBuffer(t *testing.T) {
	engine, err := New([]domain.RedactionPolicy{{
		ID: "policy", Name: "Policy", Enabled: true, Mode: "bounded_stream",
		Rules: []domain.RedactionRule{{
			ID: "phone", Name: "Phone", Kind: "builtin", Builtin: "china_phone",
			Scopes: []string{"outbound"}, Action: "mask", Enabled: true,
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !engine.AllowsStreaming("policy") {
		t.Fatal("bounded policy with finite rules should allow streaming")
	}
	if _, err := engine.NewStream("policy"); err != nil {
		t.Fatalf("create rolling redactor: %v", err)
	}
}

func TestPolicyMasksStructuredJSONAndRejectsWithoutReturningOriginal(t *testing.T) {
	policy := domain.RedactionPolicy{
		ID: "policy", Name: "Policy", Enabled: true, Mode: "strict",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
		Rules: []domain.RedactionRule{
			{
				ID: "phone", Name: "Phone", Kind: "builtin", Builtin: "china_phone",
				Scopes: []string{"inbound", "outbound"}, Action: "mask", Enabled: true,
				Priority: 10,
			},
			{
				ID: "deny", Name: "Internal marker", Kind: "dictionary",
				Dictionary: []string{"高度机密"}, Scopes: []string{"inbound"},
				Action: "reject", Enabled: true, Priority: 100,
			},
		},
	}
	engine, err := New([]domain.RedactionPolicy{policy})
	if err != nil {
		t.Fatal(err)
	}
	request := openaiapi.ChatCompletionRequest{
		Model: "chat",
		Messages: []openaiapi.Message{{
			Role: "user", Content: json.RawMessage(`{"contact":"13800138000"}`),
			ToolCalls: []openaiapi.ToolCall{{
				Type: "function", Function: openaiapi.ToolCallFunction{
					Name: "lookup", Arguments: `{"contact":"13800138000"}`,
				},
			}},
		}},
	}
	request, err = engine.ProcessInboundChat(policy.ID, request)
	if err != nil {
		t.Fatal(err)
	}
	if string(request.Messages[0].Content) != `{"contact":"••••8000"}` {
		t.Fatalf("unexpected masked request: %s", request.Messages[0].Content)
	}
	if got := request.Messages[0].ToolCalls[0].Function.Arguments; got != `{"contact":"••••8000"}` ||
		!json.Valid([]byte(got)) {
		t.Fatalf("tool arguments were not structurally redacted: %s", got)
	}
	request.Messages[0].Content = openaiapi.TextContent("这是高度机密内容")
	if _, err := engine.ProcessInboundChat(policy.ID, request); !errors.Is(err, ErrPolicyRejected) {
		t.Fatalf("dictionary reject error=%v", err)
	}
	matches, err := engine.Test(policy.ID, "高度机密和13800138000", "inbound")
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(matches)
	if strings.Contains(string(encoded), "高度机密") || len(matches) != 2 {
		t.Fatalf("test result leaked input or missed matches: %s", encoded)
	}
	if engine.HitCounters()[policy.ID+":phone"] == 0 {
		t.Fatal("hit counter was not updated")
	}
}
