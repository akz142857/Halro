package anthropic

import (
	"bytes"
	"testing"

	"github.com/akz142857/Heimdall/internal/anthropicapi"
	"github.com/akz142857/Heimdall/internal/compatibility"
	"github.com/akz142857/Heimdall/internal/domain"
)

func TestNativeEnvelopePreservesThinkingSignature(t *testing.T) {
	registry, err := NewNativeSchemaRegistry()
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"model":"claude","max_tokens":64,"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"x","signature":"signed-opaque"},{"type":"tool_use","id":"toolu_1","name":"lookup","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}]}`)
	envelope, err := compatibility.NewNativeEnvelope(registry, domain.ProfileAnthropicMessages, 1, NativeHeaders(anthropicapi.SupportedVersion), payload, compatibility.NativeIdentity{ProjectID: "project_1", PrincipalID: "key_1", CredentialRef: "cred_provider_1", RouteID: "route_1", RequestedModel: "public"})
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
	if _, err := compatibility.NewNativeEnvelope(registry, domain.ProfileAnthropicMessages, 1, NativeHeaders(anthropicapi.SupportedVersion), payload, compatibility.NativeIdentity{ProjectID: "project_1", PrincipalID: "key_1", CredentialRef: "cred_provider_1", RouteID: "route_1", RequestedModel: "public"}); err == nil {
		t.Fatal("expected credential rejection")
	}
}
