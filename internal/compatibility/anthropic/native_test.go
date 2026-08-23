package anthropic

import (
	"bytes"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/anthropicapi"
	"github.com/akz142857/Halro/internal/compatibility"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/semantic"
)

func TestNativeEnvelopePreservesThinkingSignature(t *testing.T) {
	registry, err := NewNativeSchemaRegistry()
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"model":"claude","max_tokens":64,"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"x","signature":"signed-opaque"},{"type":"tool_use","id":"toolu_1","name":"lookup","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}]}`)
	envelope, err := compatibility.NewNativeEnvelope(registry, domain.ProfileAnthropicMessages, 1, NativeHeaders(anthropicapi.SupportedVersion, nil), payload, compatibility.NativeIdentity{ProjectID: "project_1", PrincipalID: "key_1", CredentialRef: "cred_provider_1", RouteID: "route_1", RequestedModel: "public"})
	if err != nil {
		t.Fatal(err)
	}
	actual, err := envelope.PayloadFor(domain.ProfileAnthropicMessages, 1, compatibility.NativeRequest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, payload) {
		t.Fatal("native request payload changed")
	}
}

func TestNativeEnvelopeRejectsCredentialFields(t *testing.T) {
	registry, _ := NewNativeSchemaRegistry()
	payload := []byte(`{"model":"claude","max_tokens":1,"messages":[{"role":"user","content":"hi"}],"x-api-key":"sk-secret"}`)
	if _, err := compatibility.NewNativeEnvelope(registry, domain.ProfileAnthropicMessages, 1, NativeHeaders(anthropicapi.SupportedVersion, nil), payload, compatibility.NativeIdentity{ProjectID: "project_1", PrincipalID: "key_1", CredentialRef: "cred_provider_1", RouteID: "route_1", RequestedModel: "public"}); err == nil {
		t.Fatal("expected credential rejection")
	}
}

// The envelope proves "these bytes, under these headers". A beta token changes
// what the upstream does with the request, so an envelope that omits it proves
// the wrong thing — and the schema's allowlist entry for the header was inert
// while NativeHeaders never produced one.
func TestNativeHeadersCarryTheBetaTokensThatWillBeSent(t *testing.T) {
	headers := NativeHeaders(anthropicapi.SupportedVersion, []string{"context-management-2025-06-27", "fast-mode-2026-01-12"})
	if got := headers.Get(anthropicapi.BetaHeader); got != "context-management-2025-06-27,fast-mode-2026-01-12" {
		t.Fatalf("beta header=%q", got)
	}
	if headers.Get(anthropicapi.VersionHeader) != anthropicapi.SupportedVersion {
		t.Fatal("version header lost")
	}
	if NativeHeaders(anthropicapi.SupportedVersion, nil).Get(anthropicapi.BetaHeader) != "" {
		t.Fatal("no tokens must mean no header")
	}
}

// A PDF is decoded by the same multimodal pipeline as an image, so a target that
// cannot see one cannot read the other.
func TestNativeRequirementsCountDocumentsAsMultimodalInput(t *testing.T) {
	request, err := anthropicapi.DecodeMessageRequest(strings.NewReader(
		`{"model":"m","max_tokens":10,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"url","url":"https://example.test/a.pdf"}}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !NativeRequirements(request).Vision {
		t.Fatal("a document input did not declare multimodal input")
	}
}

// Native is the mode that carries an image as base64 inside the payload, so the
// governance estimate is where a picture priced as prose does its damage: the
// project input limit, the deployment context window, the TPM lease and the
// budget reservation are all measured against this number.
func TestNativeGovernanceEstimatesAnInlineImageAtItsCeiling(t *testing.T) {
	base64Image := strings.Repeat("A", 400_000)
	payload := []byte(`{"model":"m","max_tokens":10,"messages":[{"role":"user","content":[{"type":"text","text":"describe this"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + base64Image + `"}}]}]}`)
	governance, err := extractNativeGovernance(compatibility.NativeRequest, payload)
	if err != nil {
		t.Fatal(err)
	}
	if governance.EstimatedInputTokens > semantic.ImageInputTokenCeiling+64 {
		t.Fatalf("inline image estimated at %d tokens; the ceiling is %d",
			governance.EstimatedInputTokens, semantic.ImageInputTokenCeiling)
	}
	if governance.EstimatedInputTokens <= semantic.ImageInputTokenCeiling {
		t.Fatalf("inline image estimated at %d tokens, which does not charge the ceiling plus its prompt",
			governance.EstimatedInputTokens)
	}

	// A payload with no image is still measured the old way, byte for byte.
	textOnly := []byte(`{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`)
	plain, err := extractNativeGovernance(compatibility.NativeRequest, textOnly)
	if err != nil {
		t.Fatal(err)
	}
	if plain.EstimatedInputTokens != int64((len(textOnly)+3)/4) {
		t.Fatalf("text-only payload estimated %d tokens for %d bytes", plain.EstimatedInputTokens, len(textOnly))
	}
}

// Provider-executed tools are an egress decision, so the requirement has to
// reach routing rather than being settled in the decoder.
func TestNativeRequirementsDeclareProviderExecutedTools(t *testing.T) {
	request, err := anthropicapi.DecodeMessageRequest(strings.NewReader(
		`{"model":"m","max_tokens":10,"tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !NativeRequirements(request).ProviderExecutedTools {
		t.Fatal("a provider-executed tool did not declare the requirement")
	}
}
