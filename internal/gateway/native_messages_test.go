package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/anthropicapi"
	"github.com/akz142857/Halro/internal/auth"
	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/semantic"
)

type nativeMessagesFake struct {
	payload      []byte
	providerType domain.ProviderType
}

func (fake *nativeMessagesFake) Type() string {
	if fake.providerType == "" {
		return string(domain.ProviderAnthropic)
	}
	return string(fake.providerType)
}
func (*nativeMessagesFake) Close() {}
func (*nativeMessagesFake) Chat(context.Context, provider.ChatCall) (openaiapi.ChatCompletionResponse, error) {
	return openaiapi.ChatCompletionResponse{}, errors.New("unexpected portable call")
}
func (*nativeMessagesFake) ChatStream(context.Context, provider.ChatCall, func(semantic.Event) error) (*openaiapi.Usage, error) {
	return nil, errors.New("unexpected portable stream")
}
func (*nativeMessagesFake) Embed(context.Context, provider.EmbeddingCall) (openaiapi.EmbeddingResponse, error) {
	return openaiapi.EmbeddingResponse{}, errors.New("unsupported")
}
func (fake *nativeMessagesFake) MessagesNative(_ context.Context, call provider.NativeMessageCall) (provider.NativeMessageResult, error) {
	fake.payload = append([]byte(nil), call.Payload...)
	return provider.NativeMessageResult{Payload: []byte(`{"id":"msg_provider","type":"message","role":"assistant","content":[{"type":"thinking","thinking":"hidden","signature":"opaque-sig"},{"type":"text","text":"ok"}],"model":"claude-provider","stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":4,"output_tokens":1}}`)}, nil
}
func (fake *nativeMessagesFake) MessagesNativeStream(_ context.Context, call provider.NativeMessageCall, emit func(anthropicapi.RawStreamEvent) error) (*anthropicapi.Usage, error) {
	fake.payload = append([]byte(nil), call.Payload...)
	events := []anthropicapi.RawStreamEvent{
		{Type: "message_start", Data: json.RawMessage(`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-provider","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":2,"output_tokens":0}}}`)},
		{Type: "content_block_start", Data: json.RawMessage(`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`)},
		{Type: "content_block_delta", Data: json.RawMessage(`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"opaque"}}`)},
		{Type: "content_block_stop", Data: json.RawMessage(`{"type":"content_block_stop","index":0}`)},
		{Type: "message_delta", Data: json.RawMessage(`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`)},
		{Type: "message_stop", Data: json.RawMessage(`{"type":"message_stop"}`)},
	}
	for _, event := range events {
		if err := emit(event); err != nil {
			return nil, err
		}
	}
	return &anthropicapi.Usage{InputTokens: 2, OutputTokens: 1}, nil
}

func newNativeMessagesFixture(t *testing.T) (*Service, *nativeMessagesFake, string, func()) {
	return newNativeMessagesFixtureForProfile(t, domain.ProfileAnthropicMessages)
}

func newNativeMessagesFixtureForProfile(t *testing.T, profileID domain.ProviderProfileID) (*Service, *nativeMessagesFake, string, func()) {
	t.Helper()
	project := domain.Project{ID: "project_native", Name: "Native", Enabled: true, AllowedRoutes: []string{"claude"}, DailyBudgetMicrosUSD: 1000000, MaxInputTokens: 10000, MaxOutputTokens: 1000}
	plaintext, key, err := auth.GenerateGatewayKey(project.ID, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := auth.NewSnapshot()
	if err := snapshot.Refresh(context.Background(), source{keys: []domain.GatewayKey{key}, projects: []domain.Project{project}}); err != nil {
		t.Fatal(err)
	}
	status := ledger.NewStatus()
	log, err := ledger.OpenWithOptions(filepath.Join(t.TempDir(), "native.wal"), status, ledger.Options{ChainKey: testChainKey})
	if err != nil {
		t.Fatal(err)
	}
	state := ledger.NewState()
	accounting, err := budget.New(log, state, mustResolver(t, "UTC"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := provider.BuiltinProfile(profileID)
	fake := &nativeMessagesFake{providerType: manifest.ProviderType}
	capabilities := domain.DefaultProviderCapabilitiesForProfile(manifest.ProviderType, profileID)
	bridge, err := provider.NewLegacyAdapterBridge(fake, manifest, domain.EvidenceForCapabilities(capabilities, domain.EvidenceVerified))
	if err != nil {
		t.Fatal(err)
	}
	registry := provider.NewRegistry()
	if err := registry.Register(provider.Target{ID: "route_native", ProviderID: "provider_native", PublicModel: "claude", ProviderModel: "claude-provider", AccessSurface: manifest.AccessSurface, ProfileID: profileID, Adapter: bridge, Capabilities: provider.Capabilities{Chat: true, Streaming: true, Tools: true, Reasoning: true, StreamUsage: true}, InputMicrosPerMillion: 1000, OutputMicrosPerMillion: 1000}); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(snapshot, registry, accounting)
	if err != nil {
		t.Fatal(err)
	}
	return service, fake, plaintext, func() { _ = log.Close() }
}

func TestNativeMessagesPinsBedrockMantleAnthropicProfile(t *testing.T) {
	service, fake, key, closeFixture := newNativeMessagesFixtureForProfile(t, domain.ProfileBedrockMantleAnthropicMessages)
	defer closeFixture()
	request, err := anthropicapi.DecodeMessageRequest(bytes.NewBufferString(`{"model":"claude","max_tokens":64,"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"hidden","signature":"mantle-sig"},{"type":"text","text":"safe projection"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.MessagesNative(context.Background(), key, anthropicapi.SupportedVersion, request); err != nil {
		t.Fatal(err)
	}
	if fake.Type() != string(domain.ProviderBedrock) || !bytes.Contains(fake.payload, []byte(`"signature":"mantle-sig"`)) {
		t.Fatalf("Mantle native route changed identity or signature: type=%s payload=%s", fake.Type(), fake.payload)
	}
}

func TestNativeMessagesPreservesSignedThinkingAndPinsProfile(t *testing.T) {
	service, fake, key, closeFixture := newNativeMessagesFixture(t)
	defer closeFixture()
	payload := []byte(`{"model":"claude","max_tokens":64,"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"hidden","signature":"opaque-sig"},{"type":"tool_use","id":"toolu_1","name":"lookup","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}]}`)
	request, err := anthropicapi.DecodeMessageRequest(bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	message, err := service.MessagesNative(context.Background(), key, anthropicapi.SupportedVersion, request)
	if err != nil {
		t.Fatal(err)
	}
	if message.Model != "claude" || !bytes.Contains(fake.payload, []byte(`"signature":"opaque-sig"`)) {
		t.Fatalf("message=%#v payload=%s", message, fake.payload)
	}
}

func TestNativeMessagesStreamPreservesRawSignatureEvent(t *testing.T) {
	service, _, key, closeFixture := newNativeMessagesFixture(t)
	defer closeFixture()
	request, err := anthropicapi.DecodeMessageRequest(bytes.NewBufferString(`{"model":"claude","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var signature bool
	var publicModel bool
	err = service.MessagesNativeStream(context.Background(), key, anthropicapi.SupportedVersion, request, func(event anthropicapi.RawStreamEvent) error {
		if event.Type == "message_start" && bytes.Contains(event.Data, []byte(`"model":"claude"`)) && !bytes.Contains(event.Data, []byte("claude-provider")) {
			publicModel = true
		}
		if event.Type == "content_block_delta" && bytes.Contains(event.Data, []byte("opaque")) {
			signature = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !signature || !publicModel {
		t.Fatalf("signature=%v public_model=%v", signature, publicModel)
	}
}

// mustResolver pins the accounting timezone for a test. Period boundaries are
// the subject of the assertions here, so the zone is stated rather than
// inherited from whatever the host is set to.
func mustResolver(t *testing.T, timezone string) *budget.PeriodResolver {
	t.Helper()
	resolver, err := budget.NewFixedPeriodResolver(timezone)
	if err != nil {
		t.Fatalf("resolver for %s: %v", timezone, err)
	}
	return resolver
}
