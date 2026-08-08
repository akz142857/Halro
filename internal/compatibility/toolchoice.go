package compatibility

import (
	"errors"
	"slices"

	"github.com/akz142857/Halro/internal/semantic"
)

type ToolChoiceProtocol string

const (
	ToolProtocolOpenAI    ToolChoiceProtocol = "openai"
	ToolProtocolAnthropic ToolChoiceProtocol = "anthropic"
	ToolProtocolGemini    ToolChoiceProtocol = "gemini"
)

type ToolChoiceWire struct {
	Protocol        ToolChoiceProtocol
	Mode            string
	NamedTool       string
	AllowedTools    []string
	ParallelAllowed bool
	StrictValidated bool
}

// DecodeToolChoice records protocol distinctions before producing the small
// portable semantic set. It never treats Gemini VALIDATED as generic AUTO.
func DecodeToolChoice(wire ToolChoiceWire) (semantic.ToolChoice, *bool, error) {
	parallel := wire.ParallelAllowed
	choice := semantic.ToolChoice{}
	switch wire.Protocol {
	case ToolProtocolOpenAI:
		switch wire.Mode {
		case "auto", "none", "required":
			choice.Mode = semantic.ToolChoiceMode(wire.Mode)
		case "named":
			choice.Mode, choice.Name = semantic.ToolChoiceNamed, wire.NamedTool
		default:
			return choice, nil, errors.New("unsupported OpenAI tool choice")
		}
	case ToolProtocolAnthropic:
		switch wire.Mode {
		case "auto", "none":
			choice.Mode = semantic.ToolChoiceMode(wire.Mode)
		case "any":
			choice.Mode = semantic.ToolChoiceRequired
		case "tool":
			choice.Mode, choice.Name = semantic.ToolChoiceNamed, wire.NamedTool
		default:
			return choice, nil, errors.New("unsupported Anthropic tool choice")
		}
	case ToolProtocolGemini:
		if wire.StrictValidated || wire.Mode == "VALIDATED" {
			return choice, nil, errors.New("Gemini VALIDATED has no portable equivalent")
		}
		switch wire.Mode {
		case "AUTO":
			choice.Mode = semantic.ToolChoiceAuto
		case "NONE":
			choice.Mode = semantic.ToolChoiceNone
		case "ANY":
			if len(wire.AllowedTools) == 0 {
				choice.Mode = semantic.ToolChoiceRequired
			} else if len(wire.AllowedTools) == 1 {
				choice.Mode, choice.Name = semantic.ToolChoiceNamed, wire.AllowedTools[0]
			} else {
				return choice, nil, errors.New("Gemini allowed function subset is not portable")
			}
		default:
			return choice, nil, errors.New("unsupported Gemini function calling mode")
		}
	default:
		return choice, nil, errors.New("unknown tool choice protocol")
	}
	if choice.Mode == semantic.ToolChoiceNamed && choice.Name == "" {
		return choice, nil, errors.New("named tool choice requires a name")
	}
	return choice, &parallel, nil
}

func RenderToolChoice(choice semantic.ToolChoice, parallel bool, protocol ToolChoiceProtocol) (ToolChoiceWire, error) {
	wire := ToolChoiceWire{Protocol: protocol, ParallelAllowed: parallel}
	switch protocol {
	case ToolProtocolOpenAI:
		wire.Mode = string(choice.Mode)
		if choice.Mode == semantic.ToolChoiceNamed {
			wire.Mode, wire.NamedTool = "named", choice.Name
		}
	case ToolProtocolAnthropic:
		switch choice.Mode {
		case semantic.ToolChoiceAuto, semantic.ToolChoiceNone:
			wire.Mode = string(choice.Mode)
		case semantic.ToolChoiceRequired:
			wire.Mode = "any"
		case semantic.ToolChoiceNamed:
			wire.Mode, wire.NamedTool = "tool", choice.Name
		default:
			return ToolChoiceWire{}, errors.New("unsupported portable tool choice")
		}
	case ToolProtocolGemini:
		switch choice.Mode {
		case semantic.ToolChoiceAuto:
			wire.Mode = "AUTO"
		case semantic.ToolChoiceNone:
			wire.Mode = "NONE"
		case semantic.ToolChoiceRequired:
			wire.Mode = "ANY"
		case semantic.ToolChoiceNamed:
			wire.Mode, wire.AllowedTools = "ANY", []string{choice.Name}
		default:
			return ToolChoiceWire{}, errors.New("unsupported portable tool choice")
		}
	default:
		return ToolChoiceWire{}, errors.New("unknown tool choice protocol")
	}
	return wire, nil
}

func EqualAllowedTools(left, right []string) bool {
	left, right = slices.Clone(left), slices.Clone(right)
	slices.Sort(left)
	slices.Sort(right)
	return slices.Equal(left, right)
}
