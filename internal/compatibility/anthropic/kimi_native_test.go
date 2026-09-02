package anthropic

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/anthropicapi"

	"github.com/akz142857/Halro/internal/compatibility"
	"github.com/akz142857/Halro/internal/domain"
)

func kimiNativeBody(extra string) json.RawMessage {
	body := `{"model":"kimi-k3","max_tokens":16,"messages":[{"role":"user","content":"hi"}]`
	if extra != "" {
		body += "," + extra
	}
	return json.RawMessage(body + "}")
}

func kimiNativeEnvelope(t *testing.T, body json.RawMessage) error {
	t.Helper()
	registry, err := NewNativeSchemaRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	identity := compatibility.NativeIdentity{ProjectID: "project_1", PrincipalID: "key_1", CredentialRef: "cred_provider_1", RouteID: "route_1", RequestedModel: "public"}
	headers := NativeHeaders(anthropicapi.SupportedVersion, nil)
	_, err = compatibility.NewNativeEnvelope(registry, domain.ProfileKimiAnthropicMessages, 1, headers, body, identity)
	return err
}

func TestKimiNativeSchemaIsRegistered(t *testing.T) {
	if err := kimiNativeEnvelope(t, kimiNativeBody("")); err != nil {
		t.Fatalf("a plain Kimi native request was refused: %v", err)
	}
}

// The members Kimi's Messages schema does not carry, each refused for a reason
// of its own rather than as a copy of MiniMax's list.
func TestKimiNativeRefusesTheMembersItsSchemaDoesNotCarry(t *testing.T) {
	for name, extra := range map[string]string{
		// Answers 200 and appears in no Kimi schema, so nothing establishes what
		// it did. Forwarding a sampling constraint the upstream may be discarding
		// lets a caller believe it applied.
		"top_k": `"top_k":40`,
		// Pinned per model. The Chat face answers `invalid temperature: only 1 is
		// allowed for this model` to anything else, and native mode forwards
		// bytes, so a caller sending their usual value would be refused upstream
		// after the request was admitted.
		"temperature": `"temperature":0.7`,
		"top_p":       `"top_p":0.5`,
	} {
		if err := kimiNativeEnvelope(t, kimiNativeBody(extra)); err == nil {
			t.Errorf("%s was forwarded to Kimi, which has no member for it", name)
		}
	}
}

// stop_sequences is the difference from MiniMax, and it is a measured one: a
// request naming STOPHERE came back cut at it on 2026-09-01. MiniMax documents
// the same member as ignored and has it refused; carrying it here is the point
// of having a separate validator at all.
func TestKimiNativeCarriesStopSequences(t *testing.T) {
	if err := kimiNativeEnvelope(t, kimiNativeBody(`"stop_sequences":["STOPHERE"]`)); err != nil {
		t.Fatalf("stop_sequences was refused and Kimi honours it: %v", err)
	}
}

// cache_control rides inside content blocks, system blocks and tool
// definitions, which are forwarded as raw bytes — so it is found by inspecting
// the document rather than the struct. Each row below is a way that inspection
// can be wrong. The byte scan this replaced was wrong on the second row, which
// is the one that mattered: a marker it declared absent reached the upstream.
func TestKimiNativeFindsCacheControlWhereverItIsSpelled(t *testing.T) {
	for _, test := range []struct {
		name    string
		extra   string
		refused bool
	}{
		{
			name:    "in a content block, spelled plainly",
			extra:   `"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral"}}]`,
			refused: true,
		},
		{
			// The bypass. Go's decoder and Kimi's both read this as the same
			// member; a scan over the raw bytes reads neither, so the marker
			// reached the upstream on a request Halro had declared clean.
			name:    "escaped, which the decoder resolves to the same member",
			extra:   `"system":[{"type":"text","text":"s","cache\u005fcontrol":{"type":"ephemeral"}}]`,
			refused: true,
		},
		{
			name:    "inside a tool definition",
			extra:   `"tools":[{"name":"f","description":"d","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}]`,
			refused: true,
		},
		{
			// A value that spells a member name is not a member. The byte scan
			// this replaced matched `"cache_control"` with its quotes, so plain
			// prose like this already passed it — the false positive needed the
			// caller to quote the member name, as anyone asking about it in JSON
			// or in a code sample would. Kept because the property it pins is the
			// one that matters and does not depend on how the scan was spelled.
			name:    "named in the caller's own text, where it is a value",
			extra:   `"system":[{"type":"text","text":"does Kimi support cache_control?"}]`,
			refused: false,
		},
		{
			// The same word as a schema property name is still a key, so this one
			// is refused. Stated rather than left implicit: the rule is "any
			// member anywhere", and narrowing it to the places Anthropic puts
			// cache breakpoints would mean this layer modelling a schema Kimi
			// does not publish.
			name:    "as a property name inside a tool schema",
			extra:   `"tools":[{"name":"f","description":"d","input_schema":{"type":"object","properties":{"cache_control":{"type":"string"}}}}]`,
			refused: true,
		},
	} {
		err := kimiNativeEnvelope(t, kimiNativeBody(test.extra))
		if test.refused && err == nil {
			t.Errorf("%s: was forwarded, and Kimi has no member to act on it", test.name)
		}
		if !test.refused && err != nil {
			t.Errorf("%s: was refused, and nothing in it is a cache_control member: %v", test.name, err)
		}
	}
}

// The registry hands each profile its own validator through a switch, and a
// wrong case there is silent: Kimi would get MiniMax's rules or the base ones,
// both of which decode a plain request without complaint.
//
// TestNativeAnthropicListsAgree checks membership and cannot see this. What
// separates the three is what each refuses, so that is what is asserted: Kimi
// carries stop_sequences and refuses temperature, MiniMax is the mirror image,
// and the direct Anthropic profile takes both.
func TestEachNativeProfileGetsItsOwnValidator(t *testing.T) {
	registry, err := NewNativeSchemaRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	identity := compatibility.NativeIdentity{ProjectID: "project_1", PrincipalID: "key_1", CredentialRef: "cred_provider_1", RouteID: "route_1", RequestedModel: "public"}
	headers := NativeHeaders(anthropicapi.SupportedVersion, nil)
	body := func(model, extra string) json.RawMessage {
		return json.RawMessage(`{"model":"` + model + `","max_tokens":16,"messages":[{"role":"user","content":"hi"}],` + extra + `}`)
	}
	for _, test := range []struct {
		profile            domain.ProviderProfileID
		model              string
		refusesStop        bool
		refusesTemperature bool
	}{
		{profile: domain.ProfileKimiAnthropicMessages, model: "kimi-k3", refusesTemperature: true},
		{profile: domain.ProfileMiniMaxAnthropicMessages, model: "MiniMax-M3", refusesStop: true},
		{profile: domain.ProfileAnthropicMessages, model: "claude-sonnet-4-20250514"},
	} {
		stop := compatibilityRefused(registry, test.profile, headers, body(test.model, `"stop_sequences":["X"]`), identity)
		if stop != test.refusesStop {
			t.Errorf("%s refuses stop_sequences = %v, want %v", test.profile, stop, test.refusesStop)
		}
		temperature := compatibilityRefused(registry, test.profile, headers, body(test.model, `"temperature":0.7`), identity)
		if temperature != test.refusesTemperature {
			t.Errorf("%s refuses temperature = %v, want %v", test.profile, temperature, test.refusesTemperature)
		}
	}
}

func compatibilityRefused(
	registry *compatibility.NativeSchemaRegistry,
	profile domain.ProviderProfileID,
	headers http.Header,
	body json.RawMessage,
	identity compatibility.NativeIdentity,
) bool {
	_, err := compatibility.NewNativeEnvelope(registry, profile, 1, headers, body, identity)
	return err != nil
}

// A payload that is not JSON at all must be reported as such rather than
// silently treated as carrying no members.
func TestKimiNativeRefusesAPayloadItCannotRead(t *testing.T) {
	err := kimiNativeEnvelope(t, json.RawMessage(`{"model":"kimi-k3",`))
	if err == nil {
		t.Fatal("a truncated body was accepted")
	}
	if strings.Contains(err.Error(), "cache_control") {
		t.Fatalf("a truncated body was reported as a cache_control refusal: %v", err)
	}
}
