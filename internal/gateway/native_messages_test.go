package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
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
	// streamEvents overrides the default event script. Tests that care about
	// incremental text set it; the default deliberately keeps the
	// signature-only script the earlier suite used, so its coverage is unchanged.
	streamEvents []anthropicapi.RawStreamEvent
	tokenCount   []byte
	countCalls   int
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
	events := fake.streamEvents
	if events == nil {
		events = []anthropicapi.RawStreamEvent{
			{Type: "message_start", Data: json.RawMessage(`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-provider","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":2,"output_tokens":0}}}`)},
			{Type: "content_block_start", Data: json.RawMessage(`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`)},
			{Type: "content_block_delta", Data: json.RawMessage(`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"opaque"}}`)},
			{Type: "content_block_stop", Data: json.RawMessage(`{"type":"content_block_stop","index":0}`)},
			{Type: "message_delta", Data: json.RawMessage(`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`)},
			{Type: "message_stop", Data: json.RawMessage(`{"type":"message_stop"}`)},
		}
	}
	for _, event := range events {
		if err := emit(event); err != nil {
			return nil, err
		}
	}
	return &anthropicapi.Usage{InputTokens: 2, OutputTokens: 1}, nil
}

func (fake *nativeMessagesFake) CountTokensNative(_ context.Context, call provider.NativeMessageCall) (provider.NativeMessageResult, error) {
	fake.payload = append([]byte(nil), call.Payload...)
	fake.countCalls++
	payload := fake.tokenCount
	if payload == nil {
		payload = []byte(`{"input_tokens":42}`)
	}
	return provider.NativeMessageResult{Payload: payload}, nil
}

func newNativeMessagesFixture(t *testing.T) (*Service, *nativeMessagesFake, string, func()) {
	return newNativeMessagesFixtureForProfile(t, domain.ProfileAnthropicMessages)
}

func newNativeMessagesFixtureWithCapabilities(t *testing.T, adjust func(*provider.Capabilities)) (*Service, *nativeMessagesFake, string, func()) {
	return newNativeMessagesFixtureFull(t, domain.ProfileAnthropicMessages, adjust)
}

func newNativeMessagesFixtureForProfile(t *testing.T, profileID domain.ProviderProfileID, allowedBetas ...string) (*Service, *nativeMessagesFake, string, func()) {
	return newNativeMessagesFixtureFull(t, profileID, nil, allowedBetas...)
}

func newNativeMessagesFixtureFull(t *testing.T, profileID domain.ProviderProfileID, adjust func(*provider.Capabilities), allowedBetas ...string) (*Service, *nativeMessagesFake, string, func()) {
	t.Helper()
	project := domain.Project{ID: "project_native", Name: "Native", Enabled: true, AllowedModels: []string{"claude"}, DailyBudgetMicrosUSD: 1000000, MaxInputTokens: 10000, MaxOutputTokens: 1000}
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
	targetCapabilities := provider.Capabilities{Chat: true, Streaming: true, Tools: true, Reasoning: true, StreamUsage: true}
	if adjust != nil {
		adjust(&targetCapabilities)
	}
	if err := registry.Register(provider.Target{ID: "route_native", DeploymentID: "dep_route_native", ProviderID: "provider_native", PublicModel: "claude", ProviderModel: "claude-provider", AccessSurface: manifest.AccessSurface, ProfileID: profileID, Adapter: bridge, Capabilities: targetCapabilities, AllowedAnthropicBetas: allowedBetas, InputMicrosPerMillion: 1000, OutputMicrosPerMillion: 1000}); err != nil {
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
	if _, err := service.MessagesNative(context.Background(), key, anthropicapi.SupportedVersion, nil, request); err != nil {
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
	message, err := service.MessagesNative(context.Background(), key, anthropicapi.SupportedVersion, nil, request)
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
	err = service.MessagesNativeStream(context.Background(), key, anthropicapi.SupportedVersion, nil, request, func(event anthropicapi.RawStreamEvent) error {
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

// A secret in metadata is precisely the case the portable projection could not
// see: it assigned projection.Metadata = nil before inspecting, so the field
// reached the provider unread. This is the regression test for that hole, and
// it is the reason the accepted-field set can now grow safely.
func TestNativeMessagesInspectsFieldsOutsideThePortableProjection(t *testing.T) {
	// A Gateway Key is the sharpest case available. The envelope's own
	// credential guard (containsCredentialField) keys off sk-/AKIA/AIza/bearer
	// prefixes and does not know the gw_ shape, so this value clears that guard
	// and only the redaction walk can stop it — and metadata is exactly what
	// the old projection discarded before walking.
	gatewayKey := "gw_" + strings.Repeat("A", 44)
	for _, testCase := range []struct {
		name string
		body string
	}{
		{"metadata", `{"model":"claude","max_tokens":64,"metadata":{"user_id":"` + gatewayKey + `"},"messages":[{"role":"user","content":"hi"}]}`},
		{"thinking config", `{"model":"claude","max_tokens":64,"thinking":{"type":"enabled","note":"` + gatewayKey + `"},"messages":[{"role":"user","content":"hi"}]}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			service, fake, key, closeFixture := newNativeMessagesFixture(t)
			defer closeFixture()
			request, err := anthropicapi.DecodeMessageRequest(bytes.NewBufferString(testCase.body))
			if err != nil {
				t.Fatal(err)
			}
			_, err = service.MessagesNative(context.Background(), key, anthropicapi.SupportedVersion, nil, request)
			// Assert the specific code, not merely that something failed: an
			// unrelated error would otherwise let this test pass against the
			// very projection it exists to rule out.
			var gatewayErr *Error
			if !errors.As(err, &gatewayErr) || gatewayErr.Code != "sensitive_data_detected" {
				t.Fatalf("want sensitive_data_detected, got %v (payload=%s)", err, fake.payload)
			}
		})
	}
}

// The widened walk must not start rejecting ordinary requests: a metadata block
// with nothing secret in it is the control for the test above.
func TestNativeMessagesAcceptsBenignMetadata(t *testing.T) {
	service, fake, key, closeFixture := newNativeMessagesFixture(t)
	defer closeFixture()
	request, err := anthropicapi.DecodeMessageRequest(bytes.NewBufferString(`{"model":"claude","max_tokens":64,"metadata":{"user_id":"tenant-42"},"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.MessagesNative(context.Background(), key, anthropicapi.SupportedVersion, nil, request); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(fake.payload, []byte(`"user_id":"tenant-42"`)) {
		t.Fatalf("metadata did not survive to the provider: payload=%s", fake.payload)
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

// A beta token is a request for behaviour Halro has not modelled, so it travels
// only when the connection has been configured to accept that exact token.
func TestNativeMessagesGatesAnthropicBetasOnTheConnectionAllowlist(t *testing.T) {
	const accepted = "context-management-2025-06-27"
	body := `{"model":"claude","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`

	t.Run("token absent from the allowlist is refused", func(t *testing.T) {
		service, fake, key, closeFixture := newNativeMessagesFixture(t)
		defer closeFixture()
		request, err := anthropicapi.DecodeMessageRequest(bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		_, err = service.MessagesNative(context.Background(), key, anthropicapi.SupportedVersion, []string{accepted}, request)
		var gatewayErr *Error
		if !errors.As(err, &gatewayErr) || gatewayErr.Code != "unsupported_feature" {
			t.Fatalf("want unsupported_feature, got %v", err)
		}
		if fake.payload != nil {
			t.Fatalf("provider was called despite the refusal: %s", fake.payload)
		}
	})

	t.Run("token on the allowlist reaches the provider", func(t *testing.T) {
		service, fake, key, closeFixture := newNativeMessagesFixtureForProfile(t, domain.ProfileAnthropicMessages, accepted)
		defer closeFixture()
		request, err := anthropicapi.DecodeMessageRequest(bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.MessagesNative(context.Background(), key, anthropicapi.SupportedVersion, []string{accepted}, request); err != nil {
			t.Fatal(err)
		}
		if fake.payload == nil {
			t.Fatal("provider was not called")
		}
	})
}

// textStreamEvents is a native stream that actually carries text, which the
// repository's only native streaming test never did.
func textStreamEvents(index int, deltas ...string) []anthropicapi.RawStreamEvent {
	events := []anthropicapi.RawStreamEvent{
		{Type: "message_start", Data: json.RawMessage(`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-provider","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":2,"output_tokens":0}}}`)},
		{Type: "content_block_start", Data: json.RawMessage(`{"type":"content_block_start","index":` + strconv.Itoa(index) + `,"content_block":{"type":"text","text":""}}`)},
	}
	for _, delta := range deltas {
		encoded, _ := json.Marshal(delta)
		events = append(events, anthropicapi.RawStreamEvent{Type: "content_block_delta", Data: json.RawMessage(`{"type":"content_block_delta","index":` + strconv.Itoa(index) + `,"delta":{"type":"text_delta","text":` + string(encoded) + `}}`)})
	}
	return append(events,
		anthropicapi.RawStreamEvent{Type: "content_block_stop", Data: json.RawMessage(`{"type":"content_block_stop","index":` + strconv.Itoa(index) + `}`)},
		anthropicapi.RawStreamEvent{Type: "message_delta", Data: json.RawMessage(`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`)},
		anthropicapi.RawStreamEvent{Type: "message_stop", Data: json.RawMessage(`{"type":"message_stop"}`)},
	)
}

// Native streaming refused every delta that carried text. The gate compared the
// stream redactor's output against the fragment that produced it, and that
// redactor withholds a trailing suffix by design, so the two could never be
// equal. Only signature_delta — which carries no text — got through, and that is
// all the old test used, so the mode looked healthy while being unusable.
func TestNativeMessagesStreamDeliversTextDeltas(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		deltas []string
	}{
		{"short", []string{"hi"}},
		{"multiple fragments", []string{"the quick ", "brown fox ", "jumps"}},
		{"longer than the withheld window", []string{strings.Repeat("lorem ipsum ", 400)}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			service, fake, key, closeFixture := newNativeMessagesFixture(t)
			defer closeFixture()
			fake.streamEvents = textStreamEvents(0, testCase.deltas...)
			request, err := anthropicapi.DecodeMessageRequest(bytes.NewBufferString(`{"model":"claude","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
			if err != nil {
				t.Fatal(err)
			}
			var seen []anthropicapi.RawStreamEvent
			err = service.MessagesNativeStream(context.Background(), key, anthropicapi.SupportedVersion, nil, request, func(event anthropicapi.RawStreamEvent) error {
				seen = append(seen, anthropicapi.RawStreamEvent{Type: event.Type, Data: append(json.RawMessage(nil), event.Data...)})
				return nil
			})
			if err != nil {
				t.Fatalf("stream failed: %v", err)
			}
			if len(seen) != len(fake.streamEvents) {
				t.Fatalf("delivered %d of %d events: %#v", len(seen), len(fake.streamEvents), seen)
			}
			// Byte identity, not merely arrival: native mode's contract is that the
			// provider's own event bytes reach the client unaltered.
			for index, event := range fake.streamEvents {
				if event.Type == "message_start" {
					continue // the public model alias is substituted here by design
				}
				if !bytes.Equal(seen[index].Data, event.Data) {
					t.Fatalf("event %d changed:\n got %s\nwant %s", index, seen[index].Data, event.Data)
				}
			}
		})
	}
}

// The withheld window exists so a secret split across two chunks cannot be
// delivered half-inspected. Holding each event until its text is confirmed is
// what makes that window load-bearing rather than merely present: neither half
// of the key may be emitted.
func TestNativeMessagesStreamWithholdsASecretSplitAcrossDeltas(t *testing.T) {
	gatewayKey := "gw_" + strings.Repeat("A", 44)
	service, fake, key, closeFixture := newNativeMessagesFixture(t)
	defer closeFixture()
	fake.streamEvents = textStreamEvents(0, "here it is: "+gatewayKey[:20], gatewayKey[20:]+" done")
	request, err := anthropicapi.DecodeMessageRequest(bytes.NewBufferString(`{"model":"claude","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var delivered []byte
	err = service.MessagesNativeStream(context.Background(), key, anthropicapi.SupportedVersion, nil, request, func(event anthropicapi.RawStreamEvent) error {
		delivered = append(delivered, event.Data...)
		return nil
	})
	if err == nil {
		t.Fatal("want the split secret to fail the stream")
	}
	// The events before the block's text are unaffected, so their arrival is what
	// distinguishes "held back the secret" from "never started the stream".
	if !bytes.Contains(delivered, []byte("message_start")) {
		t.Fatalf("the stream never started, so nothing was actually withheld: %s", delivered)
	}
	if bytes.Contains(delivered, []byte(gatewayKey[:20])) || bytes.Contains(delivered, []byte(gatewayKey[20:])) {
		t.Fatalf("a fragment of the key was delivered: %s", delivered)
	}
}

// count_tokens is not billed by Anthropic and is settled at zero here, but it is
// a real provider call and has to leave the same trail every other one does.
func TestMessagesCountTokensCallsTheProviderAndSettlesAtZero(t *testing.T) {
	service, fake, key, closeFixture := newNativeMessagesFixture(t)
	defer closeFixture()
	request, err := anthropicapi.DecodeMessageRequest(bytes.NewBufferString(`{"model":"claude","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	count, err := service.MessagesCountTokens(context.Background(), key, anthropicapi.SupportedVersion, nil, request)
	if err != nil {
		t.Fatal(err)
	}
	if count.InputTokens != 42 || fake.countCalls != 1 {
		t.Fatalf("count=%#v calls=%d", count, fake.countCalls)
	}
	// Only the upstream model identifier is substituted; nothing else is touched,
	// and in particular no stream flag is invented for an endpoint without one.
	if !bytes.Contains(fake.payload, []byte(`"model":"claude"`)) || bytes.Contains(fake.payload, []byte(`"stream"`)) {
		t.Fatalf("payload was re-authored: %s", fake.payload)
	}
}

// The Mantle profile shares the Messages wire format but its count_tokens
// surface is not established, and guessing would send a prompt at an endpoint
// nobody verified.
func TestMessagesCountTokensRefusesBedrockMantle(t *testing.T) {
	service, fake, key, closeFixture := newNativeMessagesFixtureForProfile(t, domain.ProfileBedrockMantleAnthropicMessages)
	defer closeFixture()
	request, err := anthropicapi.DecodeMessageRequest(bytes.NewBufferString(`{"model":"claude","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.MessagesCountTokens(context.Background(), key, anthropicapi.SupportedVersion, nil, request)
	var gatewayErr *Error
	if !errors.As(err, &gatewayErr) || gatewayErr.Code != "unsupported_feature" {
		t.Fatalf("want unsupported_feature, got %v", err)
	}
	if fake.countCalls != 0 {
		t.Fatal("the provider was called despite the refusal")
	}
}

// Requirements derived from a native request were dead code: the native path
// never filtered on them, so a structured-output request could reach a target
// whose ceiling has no JSON mode.
func TestNativeMessagesFiltersOnRequirementsDerivedFromTheRequest(t *testing.T) {
	service, fake, key, closeFixture := newNativeMessagesFixture(t)
	defer closeFixture()
	// The fixture registers a target without JSONMode.
	request, err := anthropicapi.DecodeMessageRequest(bytes.NewBufferString(`{"model":"claude","max_tokens":64,"output_config":{"format":{"type":"json_schema","name":"invoice","schema":{"type":"object"}}},"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.MessagesNative(context.Background(), key, anthropicapi.SupportedVersion, nil, request)
	var gatewayErr *Error
	if !errors.As(err, &gatewayErr) || gatewayErr.Code != "unsupported_feature" {
		t.Fatalf("want unsupported_feature, got %v", err)
	}
	if fake.payload != nil {
		t.Fatalf("provider was called despite the missing capability: %s", fake.payload)
	}
}

// A provider-executed tool means the upstream makes its own network calls,
// outside SafeTransport's host allowlist. It is admitted by the connection's
// capability, not by whether the decoder recognises the tool type.
func TestNativeMessagesGatesProviderExecutedToolsOnTheConnectionCapability(t *testing.T) {
	body := `{"model":"claude","max_tokens":64,"tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[{"role":"user","content":"hi"}]}`
	t.Run("refused when the connection does not declare it", func(t *testing.T) {
		service, fake, key, closeFixture := newNativeMessagesFixture(t)
		defer closeFixture()
		request, err := anthropicapi.DecodeMessageRequest(bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		_, err = service.MessagesNative(context.Background(), key, anthropicapi.SupportedVersion, nil, request)
		var gatewayErr *Error
		if !errors.As(err, &gatewayErr) || gatewayErr.Code != "unsupported_feature" {
			t.Fatalf("want unsupported_feature, got %v", err)
		}
		if fake.payload != nil {
			t.Fatalf("provider was called despite the missing capability: %s", fake.payload)
		}
	})
	t.Run("reaches the provider when it does", func(t *testing.T) {
		service, fake, key, closeFixture := newNativeMessagesFixtureWithCapabilities(t, func(capabilities *provider.Capabilities) {
			capabilities.ProviderExecutedTools = true
		})
		defer closeFixture()
		request, err := anthropicapi.DecodeMessageRequest(bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.MessagesNative(context.Background(), key, anthropicapi.SupportedVersion, nil, request); err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(fake.payload, []byte(`"web_search_20250305"`)) {
			t.Fatalf("tool did not reach the provider: %s", fake.payload)
		}
	})
}
