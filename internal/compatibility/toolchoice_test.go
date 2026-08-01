package compatibility

import (
	"testing"

	"github.com/akz142857/Heimdall/internal/semantic"
)

func TestToolChoiceGoldenMatrix(t *testing.T) {
	tests := []struct {
		name  string
		wire  ToolChoiceWire
		mode  semantic.ToolChoiceMode
		named string
	}{
		{"openai auto", ToolChoiceWire{Protocol: ToolProtocolOpenAI, Mode: "auto", ParallelAllowed: true}, semantic.ToolChoiceAuto, ""},
		{"openai required", ToolChoiceWire{Protocol: ToolProtocolOpenAI, Mode: "required", ParallelAllowed: true}, semantic.ToolChoiceRequired, ""},
		{"anthropic any", ToolChoiceWire{Protocol: ToolProtocolAnthropic, Mode: "any", ParallelAllowed: false}, semantic.ToolChoiceRequired, ""},
		{"anthropic tool", ToolChoiceWire{Protocol: ToolProtocolAnthropic, Mode: "tool", NamedTool: "lookup", ParallelAllowed: false}, semantic.ToolChoiceNamed, "lookup"},
		{"gemini any", ToolChoiceWire{Protocol: ToolProtocolGemini, Mode: "ANY", ParallelAllowed: true}, semantic.ToolChoiceRequired, ""},
		{"gemini named", ToolChoiceWire{Protocol: ToolProtocolGemini, Mode: "ANY", AllowedTools: []string{"lookup"}, ParallelAllowed: true}, semantic.ToolChoiceNamed, "lookup"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			choice, _, err := DecodeToolChoice(test.wire)
			if err != nil {
				t.Fatal(err)
			}
			if choice.Mode != test.mode || choice.Name != test.named {
				t.Fatalf("unexpected choice: %#v", choice)
			}
		})
	}
}

func TestToolChoiceMatrixRejectsNonEquivalentGeminiModes(t *testing.T) {
	for _, wire := range []ToolChoiceWire{
		{Protocol: ToolProtocolGemini, Mode: "VALIDATED", StrictValidated: true},
		{Protocol: ToolProtocolGemini, Mode: "ANY", AllowedTools: []string{"one", "two"}},
	} {
		if _, _, err := DecodeToolChoice(wire); err == nil {
			t.Fatalf("expected rejection for %#v", wire)
		}
	}
}

func TestPortableToolChoiceRendersAllProtocols(t *testing.T) {
	choice := semantic.ToolChoice{Mode: semantic.ToolChoiceNamed, Name: "lookup"}
	for _, protocol := range []ToolChoiceProtocol{ToolProtocolOpenAI, ToolProtocolAnthropic, ToolProtocolGemini} {
		wire, err := RenderToolChoice(choice, false, protocol)
		if err != nil {
			t.Fatal(err)
		}
		decoded, parallel, err := DecodeToolChoice(wire)
		if err != nil {
			t.Fatal(err)
		}
		if decoded != choice || parallel == nil || *parallel {
			t.Fatalf("%s lost semantics: %#v %#v", protocol, decoded, parallel)
		}
	}
}
