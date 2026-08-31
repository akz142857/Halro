package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/anthropicapi"

	"github.com/akz142857/Halro/internal/compatibility"
	"github.com/akz142857/Halro/internal/domain"
)

func minimaxNativeBody(extra string) json.RawMessage {
	body := `{"model":"MiniMax-M3","max_tokens":16,"messages":[{"role":"user","content":"hi"}]`
	if extra != "" {
		body += "," + extra
	}
	return json.RawMessage(body + "}")
}

func TestMiniMaxNativeSchemaIsRegistered(t *testing.T) {
	registry, err := NewNativeSchemaRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	identity := compatibility.NativeIdentity{ProjectID: "project_1", PrincipalID: "key_1", CredentialRef: "cred_provider_1", RouteID: "route_1", RequestedModel: "public"}
	headers := NativeHeaders(anthropicapi.SupportedVersion, nil)
	if _, err := compatibility.NewNativeEnvelope(registry, domain.ProfileMiniMaxAnthropicMessages, 1, headers, minimaxNativeBody(""), identity); err != nil {
		t.Fatalf("a plain MiniMax native request was refused: %v", err)
	}
}

// MiniMax documents top_k, stop_sequences and cache_control as accepted and
// ignored. Native mode forwards bytes unchanged, so forwarding them returns 200
// for a request that did not happen as written — a completion past the caller's
// stop boundary, billed at a cache rate that does not exist. Refusing costs one
// clear error instead.
func TestMiniMaxNativeRefusesSilentlyIgnoredMembers(t *testing.T) {
	cases := map[string]string{
		"top_k":          `"top_k":40`,
		"stop_sequences": `"stop_sequences":["END"]`,
		"cache_control":  `"system":[{"type":"text","text":"be brief","cache_control":{"type":"ephemeral"}}]`,
	}
	for name, extra := range cases {
		err := validateMiniMaxNativePayload(compatibility.NativeRequest, minimaxNativeBody(extra))
		if err == nil {
			t.Errorf("%s was forwarded; MiniMax would accept it and do nothing with it", name)
			continue
		}
		if !strings.Contains(err.Error(), "MiniMax") {
			t.Errorf("%s refusal does not say which upstream refused it: %v", name, err)
		}
	}
}

// The same three members stay legal on the direct Anthropic profile, which does
// honour them. A guard written for one upstream must not narrow another.
func TestMiniMaxNativeGuardDoesNotNarrowAnthropic(t *testing.T) {
	if err := validateNativePayload(compatibility.NativeRequest, minimaxNativeBody(`"top_k":40`)); err != nil {
		t.Fatalf("top_k was refused on the direct Anthropic profile, which honours it: %v", err)
	}
}

// MiniMax extends the Anthropic wire form with video and mid_conv_system blocks.
// Halro does not follow it there: the Messages decoder accepts Anthropic's own
// block types and refuses the rest, so both are already unreachable on the
// native path. This pins that, because it is the reason no extra check exists.
func TestMiniMaxNativeRefusesExtendedContentBlocks(t *testing.T) {
	for _, blockType := range []string{"video", "mid_conv_system"} {
		body := json.RawMessage(`{"model":"MiniMax-M3","max_tokens":16,"messages":[{"role":"user","content":[{"type":"` + blockType + `","source":{"type":"url","url":"https://example.invalid/a"}}]}]}`)
		if err := validateMiniMaxNativePayload(compatibility.NativeRequest, body); err == nil {
			t.Errorf("a %s content block reached the upstream; Halro cannot inspect what it does not model", blockType)
		}
	}
}
