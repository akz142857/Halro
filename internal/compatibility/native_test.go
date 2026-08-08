package compatibility

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/semantic"
)

func testNativeSchema(headers []string, maxPayload, maxEvent int) NativeSchema {
	return NativeSchema{ProfileID: domain.ProfileGeminiText, SchemaRevision: 1, AllowedHeaders: headers, MaxPayloadBytes: maxPayload, MaxEventBytes: maxEvent, ValidatePayload: func(kind NativePayloadKind, payload json.RawMessage) error {
		var body struct {
			Contents []any  `json:"contents"`
			Chunk    string `json:"chunk"`
		}
		if err := json.Unmarshal(payload, &body); err != nil {
			return err
		}
		if kind == NativeRequest && body.Contents == nil {
			return errors.New("contents required")
		}
		return nil
	}, ExtractGovernance: func(kind NativePayloadKind, payload json.RawMessage) (NativeDerivedGovernance, error) {
		return NativeDerivedGovernance{EstimatedInputTokens: 2, EstimatedOutputTokens: 3, DataClassifications: []string{"synthetic"}, Requirements: semantic.Requirements{Streaming: kind == NativeEvent}}, nil
	}}
}
func testNativeIdentity() NativeIdentity {
	return NativeIdentity{ProjectID: "project_1", PrincipalID: "principal_1", CredentialRef: "cred_1", RouteID: "route_1", RequestedModel: "model"}
}

func TestNativeEnvelopeIsVersionedAllowlistedExtractedAndImmutable(t *testing.T) {
	registry, err := NewNativeSchemaRegistry(testNativeSchema([]string{"X-Provider-Version"}, 1024, 512))
	if err != nil {
		t.Fatal(err)
	}
	headers := http.Header{"X-Provider-Version": []string{"v1"}}
	payload := []byte(`{"contents":[]}`)
	envelope, err := NewNativeEnvelope(registry, domain.ProfileGeminiText, 1, headers, payload, testNativeIdentity())
	if err != nil {
		t.Fatal(err)
	}
	if envelope.Governance().EstimatedInputTokens != 2 || envelope.Governance().Requirements.Streaming {
		t.Fatalf("governance was not extracted: %#v", envelope.Governance())
	}
	headers.Set("X-Provider-Version", "mutated")
	payload[0] = '['
	copyHeaders, err := envelope.HeadersFor(domain.ProfileGeminiText, 1, NativeRequest)
	if err != nil {
		t.Fatal(err)
	}
	copyPayload, err := envelope.PayloadFor(domain.ProfileGeminiText, 1, NativeRequest)
	if err != nil {
		t.Fatal(err)
	}
	if copyHeaders.Get("X-Provider-Version") != "v1" || string(copyPayload) != `{"contents":[]}` {
		t.Fatal("native envelope retained mutable input")
	}
	copyHeaders.Set("X-Provider-Version", "copy")
	copyPayload[0] = '['
	freshHeaders, _ := envelope.HeadersFor(domain.ProfileGeminiText, 1, NativeRequest)
	freshPayload, _ := envelope.PayloadFor(domain.ProfileGeminiText, 1, NativeRequest)
	if freshHeaders.Get("X-Provider-Version") != "v1" || string(freshPayload) != `{"contents":[]}` {
		t.Fatal("native envelope exposed mutable state")
	}
	if _, err := envelope.PayloadFor(domain.ProfileOpenAIChatEmbeddings, 1, NativeRequest); err == nil {
		t.Fatal("cross-profile native payload was released")
	}
	if _, err := envelope.PayloadFor(domain.ProfileGeminiText, 1, NativeEvent); err == nil {
		t.Fatal("cross-kind native payload was released")
	}
}

func TestNativeEnvelopeRejectsUnknownSchemaSecretsAndOversizedEvents(t *testing.T) {
	registry, err := NewNativeSchemaRegistry(testNativeSchema([]string{"X-Provider-Version"}, 96, 32))
	if err != nil {
		t.Fatal(err)
	}
	identity := testNativeIdentity()
	if _, err := NewNativeEnvelope(registry, domain.ProfileGeminiText, 2, nil, []byte(`{"contents":[]}`), identity); err == nil {
		t.Fatal("unknown native schema accepted")
	}
	if _, err := NewNativeEnvelope(registry, domain.ProfileGeminiText, 1, http.Header{"Authorization": []string{"Bearer secret"}}, []byte(`{"contents":[]}`), identity); err == nil {
		t.Fatal("secret header accepted")
	}
	identity.CredentialRef = "sk-secret"
	if _, err := NewNativeEnvelope(registry, domain.ProfileGeminiText, 1, nil, []byte(`{"contents":[]}`), identity); err == nil {
		t.Fatal("raw credential reference accepted")
	}
	identity.CredentialRef = "cred_1"
	if _, err := NewNativeEventEnvelope(registry, domain.ProfileGeminiText, 1, nil, []byte(`{"chunk":"this event is intentionally larger than thirty two bytes"}`), identity); err == nil {
		t.Fatal("oversized native event accepted")
	}
	if _, err := NewNativeEnvelope(registry, domain.ProfileGeminiText, 1, nil, []byte(`{"api_key":"provider-secret","contents":[]}`), identity); err == nil {
		t.Fatal("credential field accepted")
	}
}

func TestNativeEnvelopeRejectsCredentialAliasesAndSecretLikeValues(t *testing.T) {
	registry, err := NewNativeSchemaRegistry(testNativeSchema(nil, 256, 128))
	if err != nil {
		t.Fatal(err)
	}
	for _, payload := range []string{
		`{"contents":[],"token":"provider-secret"}`,
		`{"contents":[],"credentials":{"value":"opaque"}}`,
		`{"contents":[],"private_key":"opaque"}`,
		`{"contents":[],"token":"opaque"}`,
		`{"contents":[],"note":"sk-live-credential"}`,
	} {
		if _, err := NewNativeEnvelope(registry, domain.ProfileGeminiText, 1, nil, []byte(payload), testNativeIdentity()); err == nil {
			t.Fatalf("credential-bearing payload accepted: %s", payload)
		}
	}
	if _, err := NewNativeEnvelope(registry, domain.ProfileGeminiText, 1, nil, []byte(`{"contents":[{"text":"hello"}]}`), testNativeIdentity()); err != nil {
		t.Fatalf("ordinary provider content rejected: %v", err)
	}
}

func TestNativeSchemaRequiresRegisteredProfileAndTrustedHooks(t *testing.T) {
	invalid := testNativeSchema(nil, 64, 32)
	invalid.ProfileID = "unregistered.profile.v1"
	if _, err := NewNativeSchemaRegistry(invalid); err == nil {
		t.Fatal("unregistered profile schema accepted")
	}
	invalid = testNativeSchema(nil, 64, 32)
	invalid.ValidatePayload = nil
	if _, err := NewNativeSchemaRegistry(invalid); err == nil {
		t.Fatal("schema without validator accepted")
	}
	invalid = testNativeSchema([]string{"Host"}, 64, 32)
	if _, err := NewNativeSchemaRegistry(invalid); err == nil {
		t.Fatal("routing header allowlisted")
	}
	invalid = testNativeSchema([]string{"x-api-key"}, 64, 32)
	if _, err := NewNativeSchemaRegistry(invalid); err == nil {
		t.Fatal("credential header allowlisted")
	}
}

func TestNativeEnvelopeRejectsUnsafeHeaderValuesAndSchemaMismatch(t *testing.T) {
	registry, err := NewNativeSchemaRegistry(testNativeSchema([]string{"X-Provider-Version"}, 256, 128))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewNativeEnvelope(registry, domain.ProfileGeminiText, 1, http.Header{"X-Provider-Version": []string{"Bearer secret"}}, []byte(`{"contents":[]}`), testNativeIdentity()); err == nil {
		t.Fatal("secret-like header accepted")
	}
	if _, err := NewNativeEnvelope(registry, domain.ProfileGeminiText, 1, http.Header{"X-Provider-Version": []string{"bad\r\nInjected: yes"}}, []byte(`{"contents":[]}`), testNativeIdentity()); err == nil {
		t.Fatal("control characters accepted")
	}
	if _, err := NewNativeEnvelope(registry, domain.ProfileGeminiText, 1, nil, []byte(`{"wrong":true}`), testNativeIdentity()); err == nil {
		t.Fatal("payload outside registered schema accepted")
	}
}
