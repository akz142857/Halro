package compatibility

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/semantic"
)

const (
	MaxNativePayloadBytes = 4 << 20
	MaxNativeEventBytes   = 64 << 10
	MaxNativeHeaderCount  = 16
	MaxNativeHeaderBytes  = 8 << 10
)

type GovernanceView struct {
	ProjectID             string                `json:"project_id"`
	PrincipalID           string                `json:"principal_id"`
	CredentialRef         string                `json:"credential_ref"`
	RouteID               string                `json:"route_id"`
	RequestedModel        string                `json:"requested_model"`
	EstimatedInputTokens  int64                 `json:"estimated_input_tokens"`
	EstimatedOutputTokens int64                 `json:"estimated_output_tokens"`
	DataClassifications   []string              `json:"data_classifications,omitempty"`
	Requirements          semantic.Requirements `json:"requirements"`
}

type NativeIdentity struct {
	ProjectID      string
	PrincipalID    string
	CredentialRef  string
	RouteID        string
	RequestedModel string
}
type NativeDerivedGovernance struct {
	EstimatedInputTokens  int64
	EstimatedOutputTokens int64
	DataClassifications   []string
	Requirements          semantic.Requirements
}
type NativePayloadValidator func(NativePayloadKind, json.RawMessage) error
type NativeGovernanceExtractor func(NativePayloadKind, json.RawMessage) (NativeDerivedGovernance, error)

func (view GovernanceView) Validate() error {
	if view.ProjectID == "" || view.PrincipalID == "" || view.CredentialRef == "" || view.RouteID == "" || view.RequestedModel == "" {
		return errors.New("native governance identity is incomplete")
	}
	if view.EstimatedInputTokens < 0 || view.EstimatedOutputTokens < 0 {
		return errors.New("native governance token estimate is invalid")
	}
	if view.CredentialRef != "" && (!strings.HasPrefix(view.CredentialRef, "cred_") || looksLikeSecret(view.CredentialRef)) {
		return errors.New("native governance credential must be an opaque reference")
	}
	for _, classification := range view.DataClassifications {
		if classification == "" || len(classification) > 64 {
			return errors.New("native governance data classification is invalid")
		}
	}
	return nil
}

type NativeSchema struct {
	ProfileID         domain.ProviderProfileID  `json:"profile_id"`
	SchemaRevision    uint64                    `json:"schema_revision"`
	AllowedHeaders    []string                  `json:"allowed_headers,omitempty"`
	MaxPayloadBytes   int                       `json:"max_payload_bytes"`
	MaxEventBytes     int                       `json:"max_event_bytes"`
	ValidatePayload   NativePayloadValidator    `json:"-"`
	ExtractGovernance NativeGovernanceExtractor `json:"-"`
}

func (schema NativeSchema) Validate() error {
	if schema.ProfileID == "" || schema.SchemaRevision == 0 || schema.MaxPayloadBytes <= 0 || schema.MaxPayloadBytes > MaxNativePayloadBytes || schema.MaxEventBytes <= 0 || schema.MaxEventBytes > MaxNativeEventBytes || schema.ValidatePayload == nil || schema.ExtractGovernance == nil {
		return errors.New("native schema is invalid")
	}
	if _, _, ok := domain.RegisteredProviderProfile(schema.ProfileID); !ok {
		return errors.New("native schema references an unregistered provider profile")
	}
	seen := map[string]struct{}{}
	for _, header := range schema.AllowedHeaders {
		canonical := http.CanonicalHeaderKey(header)
		if !validHeaderName(header) || canonical == "" || forbiddenNativeHeader(canonical) {
			return errors.New("native schema contains a forbidden header")
		}
		if _, exists := seen[canonical]; exists {
			return errors.New("native schema contains a duplicate header")
		}
		seen[canonical] = struct{}{}
	}
	return nil
}

type NativeSchemaRegistry struct{ schemas map[string]NativeSchema }

func NewNativeSchemaRegistry(schemas ...NativeSchema) (*NativeSchemaRegistry, error) {
	registry := &NativeSchemaRegistry{schemas: make(map[string]NativeSchema, len(schemas))}
	for _, schema := range schemas {
		if err := schema.Validate(); err != nil {
			return nil, err
		}
		schema.AllowedHeaders = canonicalHeaders(schema.AllowedHeaders)
		key := nativeSchemaKey(schema.ProfileID, schema.SchemaRevision)
		if _, exists := registry.schemas[key]; exists {
			return nil, errors.New("native schema is duplicated")
		}
		registry.schemas[key] = schema
	}
	return registry, nil
}

func (registry *NativeSchemaRegistry) Schema(profileID domain.ProviderProfileID, revision uint64) (NativeSchema, bool) {
	if registry == nil {
		return NativeSchema{}, false
	}
	schema, ok := registry.schemas[nativeSchemaKey(profileID, revision)]
	schema.AllowedHeaders = slices.Clone(schema.AllowedHeaders)
	return schema, ok
}

type NativeEnvelope struct {
	profileID      domain.ProviderProfileID
	schemaRevision uint64
	payloadKind    NativePayloadKind
	headers        http.Header
	payload        []byte
	governance     GovernanceView
}

type NativePayloadKind string

const (
	NativeRequest  NativePayloadKind = "request"
	NativeResponse NativePayloadKind = "response"
	NativeEvent    NativePayloadKind = "event"
)

func NewNativeEnvelope(registry *NativeSchemaRegistry, profileID domain.ProviderProfileID, revision uint64, headers http.Header, payload []byte, identity NativeIdentity) (*NativeEnvelope, error) {
	return newNativeEnvelope(registry, profileID, revision, NativeRequest, headers, payload, identity)
}

func NewNativeResponseEnvelope(registry *NativeSchemaRegistry, profileID domain.ProviderProfileID, revision uint64, headers http.Header, payload []byte, identity NativeIdentity) (*NativeEnvelope, error) {
	return newNativeEnvelope(registry, profileID, revision, NativeResponse, headers, payload, identity)
}

func NewNativeEventEnvelope(registry *NativeSchemaRegistry, profileID domain.ProviderProfileID, revision uint64, headers http.Header, payload []byte, identity NativeIdentity) (*NativeEnvelope, error) {
	return newNativeEnvelope(registry, profileID, revision, NativeEvent, headers, payload, identity)
}

func newNativeEnvelope(registry *NativeSchemaRegistry, profileID domain.ProviderProfileID, revision uint64, kind NativePayloadKind, headers http.Header, payload []byte, identity NativeIdentity) (*NativeEnvelope, error) {
	schema, ok := registry.Schema(profileID, revision)
	if !ok {
		return nil, errors.New("native envelope schema is not registered")
	}
	limit := schema.MaxPayloadBytes
	if kind == NativeEvent {
		limit = schema.MaxEventBytes
	}
	if kind != NativeRequest && kind != NativeResponse && kind != NativeEvent {
		return nil, errors.New("native envelope payload kind is invalid")
	}
	if len(payload) == 0 || len(payload) > limit || !json.Valid(payload) {
		return nil, errors.New("native envelope payload is invalid")
	}
	var root map[string]any
	if err := json.Unmarshal(payload, &root); err != nil || root == nil || containsCredentialField(root) {
		return nil, errors.New("native envelope payload must be a credential-free JSON object")
	}
	if err := schema.ValidatePayload(kind, bytes.Clone(payload)); err != nil {
		return nil, fmt.Errorf("validate native payload: %w", err)
	}
	derived, err := schema.ExtractGovernance(kind, bytes.Clone(payload))
	if err != nil {
		return nil, fmt.Errorf("extract native governance: %w", err)
	}
	governance := GovernanceView{ProjectID: identity.ProjectID, PrincipalID: identity.PrincipalID, CredentialRef: identity.CredentialRef, RouteID: identity.RouteID, RequestedModel: identity.RequestedModel, EstimatedInputTokens: derived.EstimatedInputTokens, EstimatedOutputTokens: derived.EstimatedOutputTokens, DataClassifications: slices.Clone(derived.DataClassifications), Requirements: derived.Requirements}
	if err := governance.Validate(); err != nil {
		return nil, err
	}
	allowed := make(map[string]struct{}, len(schema.AllowedHeaders))
	for _, header := range schema.AllowedHeaders {
		allowed[header] = struct{}{}
	}
	copyHeaders := make(http.Header, len(headers))
	if len(headers) > MaxNativeHeaderCount {
		return nil, errors.New("native envelope contains too many headers")
	}
	headerBytes := 0
	for name, values := range headers {
		canonical := http.CanonicalHeaderKey(name)
		if !validHeaderName(name) || forbiddenNativeHeader(canonical) {
			return nil, fmt.Errorf("native envelope header %q is forbidden", canonical)
		}
		if _, ok := allowed[canonical]; !ok {
			return nil, fmt.Errorf("native envelope header %q is not allowed", canonical)
		}
		if _, duplicate := copyHeaders[canonical]; duplicate {
			return nil, fmt.Errorf("native envelope header %q is duplicated", canonical)
		}
		if len(values) == 0 {
			return nil, fmt.Errorf("native envelope header %q has no value", canonical)
		}
		headerBytes += len(canonical)
		for _, value := range values {
			if value == "" || len(value) > 4096 || !validHeaderValue(value) || looksLikeSecret(value) {
				return nil, fmt.Errorf("native envelope header %q has an unsafe value", canonical)
			}
			headerBytes += len(value)
		}
		if headerBytes > MaxNativeHeaderBytes {
			return nil, errors.New("native envelope headers exceed the size limit")
		}
		copyHeaders[canonical] = slices.Clone(values)
	}
	return &NativeEnvelope{profileID: profileID, schemaRevision: revision, payloadKind: kind, headers: copyHeaders, payload: bytes.Clone(payload), governance: cloneGovernance(governance)}, nil
}

func (envelope *NativeEnvelope) ProfileID() domain.ProviderProfileID { return envelope.profileID }
func (envelope *NativeEnvelope) SchemaRevision() uint64              { return envelope.schemaRevision }
func (envelope *NativeEnvelope) PayloadKind() NativePayloadKind      { return envelope.payloadKind }
func (envelope *NativeEnvelope) HeadersFor(profileID domain.ProviderProfileID, revision uint64, kind NativePayloadKind) (http.Header, error) {
	if err := envelope.assertTarget(profileID, revision, kind); err != nil {
		return nil, err
	}
	return envelope.headers.Clone(), nil
}
func (envelope *NativeEnvelope) PayloadFor(profileID domain.ProviderProfileID, revision uint64, kind NativePayloadKind) ([]byte, error) {
	if err := envelope.assertTarget(profileID, revision, kind); err != nil {
		return nil, err
	}
	return bytes.Clone(envelope.payload), nil
}
func (envelope *NativeEnvelope) Governance() GovernanceView {
	return cloneGovernance(envelope.governance)
}
func (envelope *NativeEnvelope) AssertTarget(profileID domain.ProviderProfileID, revision uint64) error {
	if envelope == nil || envelope.profileID != profileID || envelope.schemaRevision != revision {
		return errors.New("native envelope target does not match its profile and schema")
	}
	return nil
}
func (envelope *NativeEnvelope) assertTarget(profileID domain.ProviderProfileID, revision uint64, kind NativePayloadKind) error {
	if err := envelope.AssertTarget(profileID, revision); err != nil {
		return err
	}
	if envelope.payloadKind != kind {
		return errors.New("native envelope payload kind does not match target")
	}
	return nil
}

func nativeSchemaKey(profileID domain.ProviderProfileID, revision uint64) string {
	return fmt.Sprintf("%s@%d", profileID, revision)
}
func forbiddenNativeHeader(header string) bool {
	switch strings.ToLower(header) {
	case "authorization", "proxy-authorization", "cookie", "set-cookie", "x-api-key", "api-key", "x-goog-api-key", "x-amz-security-token", "host", "content-length", "transfer-encoding", "connection", "upgrade", "te", "trailer", "forwarded", "x-forwarded-for", "x-forwarded-host", "x-forwarded-proto", "x-amz-date", "x-amz-content-sha256":
		return true
	}
	return false
}
func validHeaderName(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range []byte(value) {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(char))) {
			return false
		}
	}
	return true
}
func validHeaderValue(value string) bool {
	for _, char := range []byte(value) {
		if char == 0x7f || char < 0x20 && char != '\t' {
			return false
		}
	}
	return true
}
func canonicalHeaders(headers []string) []string {
	result := make([]string, 0, len(headers))
	for _, header := range headers {
		result = append(result, http.CanonicalHeaderKey(header))
	}
	slices.Sort(result)
	return result
}
func cloneGovernance(view GovernanceView) GovernanceView {
	view.DataClassifications = slices.Clone(view.DataClassifications)
	return view
}
func looksLikeSecret(value string) bool {
	lower := strings.ToLower(value)
	return strings.HasPrefix(value, "sk-") || strings.HasPrefix(value, "AKIA") || strings.Contains(lower, "bearer ") || strings.Contains(lower, "secret")
}

func containsCredentialField(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			normalized := strings.NewReplacer("_", "", "-", "", ".", "").Replace(strings.ToLower(key))
			switch normalized {
			case "authorization", "apikey", "accesskey", "accesskeyid", "secretaccesskey", "securitytoken", "clientsecret", "password", "credential", "credentials", "privatekey", "secretkey", "token", "accesstoken", "refreshtoken", "idtoken", "authtoken":
				return true
			}
			if containsCredentialField(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if containsCredentialField(child) {
				return true
			}
		}
	case string:
		return looksLikeCredentialValue(typed)
	}
	return false
}

func looksLikeCredentialValue(value string) bool {
	trimmed := strings.TrimSpace(value)
	lower := strings.ToLower(trimmed)
	return strings.HasPrefix(lower, "sk-") ||
		strings.HasPrefix(trimmed, "AKIA") || strings.HasPrefix(trimmed, "ASIA") ||
		strings.HasPrefix(trimmed, "AIza") || strings.HasPrefix(lower, "bearer ") ||
		strings.HasPrefix(lower, "basic ") || strings.Contains(trimmed, "-----BEGIN PRIVATE KEY-----")
}
