package bedrock

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/semantic"
)

const testCredential = `{"access_key_id":"AKIDEXAMPLE12345678","secret_access_key":"test-secret-access-key-value","session_token":"session-token","region":"us-east-1"}`

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return fn(request) }

func TestConverseSignsAndTranslatesTextWithoutLeakingSecret(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.EscapedPath() != "/model/anthropic.claude-test-v1:0/converse" {
			t.Fatalf("path=%s escaped=%s", request.URL.Path, request.URL.EscapedPath())
		}
		authorization := request.Header.Get("Authorization")
		if !strings.HasPrefix(authorization, "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE12345678/20260731/us-east-1/bedrock/aws4_request") ||
			strings.Contains(authorization, "test-secret") || request.Header.Get("x-amz-security-token") != "session-token" ||
			request.Header.Get("x-amz-content-sha256") == "" {
			t.Fatalf("invalid signed headers: %#v", request.Header)
		}
		var payload converseRequest
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if len(payload.System) != 1 || payload.System[0].Text != "be concise" || len(payload.Messages) != 1 || payload.Messages[0].Role != "user" {
			t.Fatalf("payload=%#v", payload)
		}
		return jsonResponse(http.StatusOK, `{"output":{"message":{"role":"assistant","content":[{"text":"hello "},{"text":"world"}]}},"stopReason":"end_turn","usage":{"inputTokens":4,"outputTokens":2,"totalTokens":6}}`), nil
	})}
	adapter := newTestAdapter(t, client)
	defer adapter.Close()
	response, err := adapter.Chat(context.Background(), provider.ChatCall{
		RequestID: "req_1", ProviderModel: "anthropic.claude-test-v1:0",
		Request: openaiapi.ChatCompletionRequest{Messages: []openaiapi.Message{
			{Role: "system", Content: openaiapi.TextContent("be concise")},
			{Role: "user", Content: openaiapi.TextContent("hi")},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	text, _ := openaiapi.DecodeTextContent(response.Choices[0].Message.Content)
	if text != "hello world" || response.Usage == nil || response.Usage.TotalTokens != 6 || *response.Choices[0].FinishReason != "stop" {
		t.Fatalf("response=%#v text=%q", response, text)
	}
}

func TestTranslateRequestRejectsFieldsBedrockWouldDrop(t *testing.T) {
	candidates := 2
	request := openaiapi.ChatCompletionRequest{
		Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("hi")}},
		N:        &candidates,
	}
	if _, err := translateRequest(request); err == nil {
		t.Fatal("multiple candidates were silently dropped")
	}
}

func TestConverseStreamNormalizesAWSFramesAndUsage(t *testing.T) {
	var stream bytes.Buffer
	writeEventFrame(t, &stream, "messageStart", `{"role":"assistant"}`)
	writeEventFrame(t, &stream, "contentBlockStart", `{"contentBlockIndex":0,"start":{}}`)
	writeEventFrame(t, &stream, "contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"hello"}}`)
	writeEventFrame(t, &stream, "contentBlockStop", `{"contentBlockIndex":0}`)
	writeEventFrame(t, &stream, "messageStop", `{"stopReason":"max_tokens"}`)
	writeEventFrame(t, &stream, "metadata", `{"usage":{"inputTokens":3,"outputTokens":2,"totalTokens":5}}`)
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.EscapedPath() != "/model/anthropic.claude-test-v1:0/converse-stream" {
			t.Fatalf("path=%s", request.URL.EscapedPath())
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/vnd.amazon.eventstream"}}, Body: io.NopCloser(bytes.NewReader(stream.Bytes()))}, nil
	})}
	adapter := newTestAdapter(t, client)
	defer adapter.Close()
	var events []semantic.Event
	usage, err := adapter.ChatStream(context.Background(), provider.ChatCall{
		RequestID: "req_stream", ProviderModel: "anthropic.claude-test-v1:0",
		Request: openaiapi.ChatCompletionRequest{Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("hi")}}},
	}, func(event semantic.Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[0].Outputs[0].Role != semantic.RoleAssistant ||
		events[2].Outputs[0].NativeTermination != "length" || usage == nil || usage.TotalTokens != 5 {
		t.Fatalf("events=%#v usage=%#v", events, usage)
	}
}

func TestCredentialRegionErrorsAndProviderErrorsAreSafe(t *testing.T) {
	endpoint, _ := url.Parse("https://bedrock-runtime.us-west-2.amazonaws.com")
	if _, err := New(Options{Endpoint: endpoint, CredentialJSON: []byte(testCredential), Client: &http.Client{}}); err == nil {
		t.Fatal("region-mismatched endpoint was accepted")
	}
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		response := jsonResponse(http.StatusTooManyRequests, `{"message":"test-secret-access-key-value","code":"ThrottlingException"}`)
		response.Header.Set("x-amzn-requestid", "aws-request-123")
		response.Header.Set("Retry-After", "7")
		return response, nil
	})}
	adapter := newTestAdapter(t, client)
	defer adapter.Close()
	_, err := adapter.Chat(context.Background(), provider.ChatCall{
		RequestID: "req_error", ProviderModel: "model",
		Request: openaiapi.ChatCompletionRequest{Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("hi")}}},
	})
	if err == nil || strings.Contains(err.Error(), "test-secret") {
		t.Fatalf("unsafe error=%v", err)
	}
	var classified *provider.Error
	if !errorsAs(err, &classified) || classified.Class != provider.ErrorRateLimit ||
		classified.ProviderRequestID != "aws-request-123" || classified.ProviderCode != "ThrottlingException" ||
		classified.RetryAfter != 7*time.Second {
		t.Fatalf("classification=%#v err=%v", classified, err)
	}
}

func TestBedrockAuthorizerRejectsUnapprovedAndCrossAudienceHosts(t *testing.T) {
	malicious, _ := url.Parse("https://attacker.us-east-1.example.com")
	if _, err := NewAuthorizer(malicious, []byte(testCredential), nil); err == nil {
		t.Fatal("non-AWS host containing the region was accepted")
	}
	insecure, _ := url.Parse("http://bedrock-runtime.us-east-1.amazonaws.com")
	if _, err := NewAuthorizer(insecure, []byte(testCredential), nil); err == nil {
		t.Fatal("plaintext Bedrock endpoint was accepted")
	}
	endpoint, _ := url.Parse("https://bedrock-runtime.us-east-1.amazonaws.com")
	authorizer, err := NewAuthorizer(endpoint, []byte(testCredential), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer authorizer.Close()
	request, _ := http.NewRequest(http.MethodPost, "https://bedrock-runtime.us-west-2.amazonaws.com/model/x/converse", nil)
	if err := authorizer.Authorize(request, nil); err == nil {
		t.Fatal("authorizer signed a request for another audience")
	}
}

func TestBedrockHTTP5xxIsAmbiguousAndNotRetryable(t *testing.T) {
	response := jsonResponse(http.StatusInternalServerError, `{"__type":"InternalServerException"}`)
	classified := classifyHTTP(response)
	if classified.Class != provider.ErrorProvider5xx || !classified.Ambiguous || classified.Retryable {
		t.Fatalf("classification=%#v", classified)
	}
}

func TestBedrockExecutionErrorsUseHeaderCodeAndRemainAmbiguous(t *testing.T) {
	for _, test := range []struct {
		status int
		code   string
		class  provider.ErrorClass
	}{
		{http.StatusRequestTimeout, "ModelTimeoutException:http", provider.ErrorTimeout},
		{http.StatusFailedDependency, "ModelErrorException", provider.ErrorProvider5xx},
	} {
		response := jsonResponse(test.status, `{"message":"safe"}`)
		response.Header.Set("x-amzn-errortype", test.code)
		classified := classifyHTTP(response)
		if classified.Class != test.class || !classified.Ambiguous || classified.Retryable || classified.ProviderCode == "" {
			t.Fatalf("status=%d classification=%#v", test.status, classified)
		}
	}
}

func TestRetryAfterSaturatesWithoutDurationOverflow(t *testing.T) {
	header := http.Header{"Retry-After": []string{"9223372036854775807"}}
	if delay := retryAfter(header, time.Now()); delay != 24*time.Hour {
		t.Fatalf("delay=%v", delay)
	}
}

func TestBedrockStopReasonsPreserveLengthAndRejectUnknownValues(t *testing.T) {
	if finish, err := mapFinishReason("model_context_window_exceeded"); err != nil || finish == nil || *finish != "length" {
		t.Fatalf("finish=%v err=%v", finish, err)
	}
	if _, err := mapFinishReason("future_unknown_reason"); err == nil {
		t.Fatal("unknown stop reason was silently accepted")
	} else {
		var classified *provider.Error
		if !errors.As(err, &classified) || classified.Class != provider.ErrorMalformed || !classified.Ambiguous {
			t.Fatalf("unknown stop classification=%#v", classified)
		}
	}
}

func TestEventStreamRejectsChecksumCorruption(t *testing.T) {
	var stream bytes.Buffer
	writeEventFrame(t, &stream, "messageStart", `{}`)
	encoded := stream.Bytes()
	encoded[len(encoded)-1] ^= 0xff
	if _, err := readStreamMessage(bytes.NewReader(encoded)); err == nil {
		t.Fatal("corrupted AWS event stream frame was accepted")
	}
}

func TestConverseStreamRejectsTruncatedAndNonTextStreams(t *testing.T) {
	tests := []struct {
		name  string
		frame func(*testing.T, *bytes.Buffer)
	}{
		{name: "missing message stop", frame: func(t *testing.T, stream *bytes.Buffer) {
			writeEventFrame(t, stream, "messageStart", `{"role":"assistant"}`)
		}},
		{name: "missing usage metadata", frame: func(t *testing.T, stream *bytes.Buffer) {
			writeEventFrame(t, stream, "messageStart", `{"role":"assistant"}`)
			writeEventFrame(t, stream, "messageStop", `{"stopReason":"end_turn"}`)
		}},
		{name: "empty usage metadata", frame: func(t *testing.T, stream *bytes.Buffer) {
			writeEventFrame(t, stream, "messageStart", `{"role":"assistant"}`)
			writeEventFrame(t, stream, "messageStop", `{"stopReason":"end_turn"}`)
			writeEventFrame(t, stream, "metadata", `{}`)
		}},
		{name: "zero usage metadata", frame: func(t *testing.T, stream *bytes.Buffer) {
			writeEventFrame(t, stream, "messageStart", `{"role":"assistant"}`)
			writeEventFrame(t, stream, "messageStop", `{"stopReason":"end_turn"}`)
			writeEventFrame(t, stream, "metadata", `{"usage":{}}`)
		}},
		{name: "missing content block index", frame: func(t *testing.T, stream *bytes.Buffer) {
			writeEventFrame(t, stream, "messageStart", `{"role":"assistant"}`)
			writeEventFrame(t, stream, "contentBlockStart", `{"start":{}}`)
		}},
		{name: "missing stop reason", frame: func(t *testing.T, stream *bytes.Buffer) {
			writeEventFrame(t, stream, "messageStart", `{"role":"assistant"}`)
			writeEventFrame(t, stream, "messageStop", `{}`)
		}},
		{name: "tool content block", frame: func(t *testing.T, stream *bytes.Buffer) {
			writeEventFrame(t, stream, "messageStart", `{"role":"assistant"}`)
			writeEventFrame(t, stream, "contentBlockStart", `{"contentBlockIndex":0,"start":{"toolUse":{"toolUseId":"1","name":"unsafe"}}}`)
		}},
		{name: "tool delta", frame: func(t *testing.T, stream *bytes.Buffer) {
			writeEventFrame(t, stream, "messageStart", `{"role":"assistant"}`)
			writeEventFrame(t, stream, "contentBlockDelta", `{"contentBlockIndex":0,"delta":{"toolUse":{"input":"{}"}}}`)
		}},
		{name: "unsupported stop reason", frame: func(t *testing.T, stream *bytes.Buffer) {
			writeEventFrame(t, stream, "messageStart", `{"role":"assistant"}`)
			writeEventFrame(t, stream, "messageStop", `{"stopReason":"tool_use"}`)
		}},
		{name: "delta before message start", frame: func(t *testing.T, stream *bytes.Buffer) {
			writeEventFrame(t, stream, "contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"late"}}`)
		}},
		{name: "message stop before content block stop", frame: func(t *testing.T, stream *bytes.Buffer) {
			writeEventFrame(t, stream, "messageStart", `{"role":"assistant"}`)
			writeEventFrame(t, stream, "contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"partial"}}`)
			writeEventFrame(t, stream, "messageStop", `{"stopReason":"end_turn"}`)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stream bytes.Buffer
			test.frame(t, &stream)
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/vnd.amazon.eventstream"}}, Body: io.NopCloser(bytes.NewReader(stream.Bytes()))}, nil
			})}
			adapter := newTestAdapter(t, client)
			defer adapter.Close()
			_, err := adapter.ChatStream(context.Background(), provider.ChatCall{
				RequestID: "req_reject", ProviderModel: "model",
				Request: openaiapi.ChatCompletionRequest{Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("hi")}}},
			}, func(semantic.Event) error { return nil })
			if err == nil {
				t.Fatal("invalid Bedrock stream was accepted")
			}
		})
	}
}

func TestBedrockStreamExceptionClassification(t *testing.T) {
	validation := streamException("validationException", http.Header{}, nil)
	// The refusal kind is asserted here rather than only on the HTTP path: both
	// streaming probes carry a dependency, so losing it on this path would
	// silently return them to "inconclusive" for every refused stream.
	if validation.Class != provider.ErrorBadRequest || validation.Retryable || validation.Ambiguous ||
		validation.Refusal != provider.RefusalInvalid {
		t.Fatalf("validation=%#v", validation)
	}
	model := streamException("modelStreamErrorException", http.Header{}, nil)
	if model.Class != provider.ErrorProvider5xx || !model.Ambiguous || model.Retryable {
		t.Fatalf("model stream=%#v", model)
	}
	timeout := streamException("modelTimeoutException", http.Header{}, nil)
	if timeout.Class != provider.ErrorTimeout || !timeout.Ambiguous || timeout.Retryable {
		t.Fatalf("model timeout=%#v", timeout)
	}
	header := http.Header{"Retry-After": []string{"5"}}
	throttled := streamException("throttlingException", header, nil)
	// Not a refusal of the request body, so it names no kind.
	if throttled.Class != provider.ErrorRateLimit || throttled.RetryAfter != 5*time.Second ||
		throttled.Refusal != provider.RefusalNone {
		t.Fatalf("throttled=%#v", throttled)
	}
	unknown := streamException("futureException", http.Header{}, nil)
	if unknown.Class != provider.ErrorMalformed || !unknown.Ambiguous {
		t.Fatalf("unknown=%#v", unknown)
	}
}

func TestConverseRejectsNonTextOutputAndInvalidUsage(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "tool output", body: `{"output":{"message":{"role":"assistant","content":[{"toolUse":{"toolUseId":"1","name":"unsafe","input":{}}}]}},"stopReason":"tool_use"}`},
		{name: "negative usage", body: `{"output":{"message":{"role":"assistant","content":[{"text":"ok"}]}},"stopReason":"end_turn","usage":{"inputTokens":-1}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return jsonResponse(http.StatusOK, test.body), nil
			})}
			adapter := newTestAdapter(t, client)
			defer adapter.Close()
			_, err := adapter.Chat(context.Background(), provider.ChatCall{
				RequestID: "req_reject", ProviderModel: "model",
				Request: openaiapi.ChatCompletionRequest{Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("hi")}}},
			})
			if err == nil {
				t.Fatal("unsupported Bedrock response was accepted")
			}
			var classified *provider.Error
			if !errors.As(err, &classified) || classified.Class != provider.ErrorMalformed || !classified.Ambiguous {
				t.Fatalf("classification=%#v err=%v", classified, err)
			}
		})
	}
}

func TestBedrockTextProfileRejectsUndeclaredDeveloperRoleBeforeProviderIO(t *testing.T) {
	called := false
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return jsonResponse(http.StatusOK, `{}`), nil
	})}
	adapter := newTestAdapter(t, client)
	defer adapter.Close()
	_, err := adapter.Chat(context.Background(), provider.ChatCall{
		RequestID: "req_developer", ProviderModel: "model",
		Request: openaiapi.ChatCompletionRequest{Messages: []openaiapi.Message{
			{Role: "developer", Content: openaiapi.TextContent("do not accept")},
			{Role: "user", Content: openaiapi.TextContent("hi")},
		}},
	})
	if err == nil || called {
		t.Fatalf("developer role err=%v provider_called=%t", err, called)
	}
}

func TestBedrockTextProfileRejectsMessageNameBeforeProviderIO(t *testing.T) {
	called := false
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return jsonResponse(http.StatusOK, `{}`), nil
	})}
	adapter := newTestAdapter(t, client)
	defer adapter.Close()
	_, err := adapter.Chat(context.Background(), provider.ChatCall{
		RequestID: "req_name", ProviderModel: "model",
		Request: openaiapi.ChatCompletionRequest{Messages: []openaiapi.Message{{Role: "user", Name: "customer", Content: openaiapi.TextContent("hi")}}},
	})
	if err == nil || called {
		t.Fatalf("message name err=%v provider_called=%t", err, called)
	}
}

func TestTitanEmbedV2SignsExactInvokeSchemaAndMapsUsage(t *testing.T) {
	vector := make([]float64, 256)
	for index := range vector {
		vector[index] = float64(index) / 256
	}
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || request.URL.EscapedPath() != "/model/amazon.titan-embed-text-v2:0/invoke" {
			t.Fatalf("method=%s path=%s", request.Method, request.URL.EscapedPath())
		}
		if !strings.Contains(request.Header.Get("Authorization"), "/us-east-1/bedrock/aws4_request") ||
			request.Header.Get("Content-Type") != "application/json" || request.Header.Get("Accept") != "application/json" {
			t.Fatalf("headers=%#v", request.Header)
		}
		var payload map[string]json.RawMessage
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if len(payload) != 4 || string(payload["inputText"]) != `"hello"` || string(payload["dimensions"]) != "256" ||
			string(payload["normalize"]) != "true" || string(payload["embeddingTypes"]) != `["float"]` {
			t.Fatalf("payload=%s", mustJSON(t, payload))
		}
		body := mustJSON(t, map[string]any{
			"embedding": vector, "inputTextTokenCount": 3,
			"embeddingsByType": map[string]any{"float": vector},
		})
		return jsonResponse(http.StatusOK, body), nil
	})}
	adapter := newTitanTestAdapter(t, client)
	defer adapter.Close()
	dimensions := int64(256)
	response, err := adapter.Embed(context.Background(), provider.EmbeddingCall{
		RequestID: "req_embed", ProviderModel: titanEmbedV2ModelID,
		Request: openaiapi.EmbeddingRequest{Input: json.RawMessage(`"hello"`), Dimensions: &dimensions, EncodingFormat: "float"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got []float64
	if len(response.Data) != 1 || json.Unmarshal(response.Data[0].Embedding, &got) != nil || !slices.Equal(got, vector) ||
		response.Usage == nil || response.Usage.PromptTokens != 3 || response.Usage.CompletionTokens != 0 || response.Usage.TotalTokens != 3 {
		t.Fatalf("response=%#v vector_length=%d", response, len(got))
	}
}

func TestTitanEmbedV2RejectsUnsupportedRequestsBeforeProviderIO(t *testing.T) {
	called := false
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return jsonResponse(http.StatusOK, `{}`), nil
	})}
	adapter := newTitanTestAdapter(t, client)
	defer adapter.Close()
	invalidDimensions := int64(128)
	for _, request := range []openaiapi.EmbeddingRequest{
		{Input: json.RawMessage(`["one","two"]`)},
		{Input: json.RawMessage(`"hello"`), EncodingFormat: "base64"},
		{Input: json.RawMessage(`"hello"`), Dimensions: &invalidDimensions},
		{Input: json.RawMessage(`"hello"`), User: "tenant"},
	} {
		if _, err := adapter.Embed(context.Background(), provider.EmbeddingCall{ProviderModel: titanEmbedV2ModelID, Request: request}); err == nil {
			t.Fatalf("unsupported request was accepted: %#v", request)
		}
	}
	if _, err := adapter.Embed(context.Background(), provider.EmbeddingCall{ProviderModel: "cohere.embed-v4:0", Request: openaiapi.EmbeddingRequest{Input: json.RawMessage(`"hello"`)}}); err == nil {
		t.Fatal("wrong model family was accepted")
	}
	if called {
		t.Fatal("provider was called for an unsupported request")
	}
}

func TestTitanEmbedV2RejectsMalformedNativeResponse(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"embedding":[0.1],"inputTextTokenCount":1,"embeddingsByType":{"float":[0.1]}}`), nil
	})}
	adapter := newTitanTestAdapter(t, client)
	defer adapter.Close()
	_, err := adapter.Embed(context.Background(), provider.EmbeddingCall{ProviderModel: titanEmbedV2ModelID, Request: openaiapi.EmbeddingRequest{Input: json.RawMessage(`"hello"`)}})
	var classified *provider.Error
	if !errors.As(err, &classified) || classified.Class != provider.ErrorMalformed || !classified.Ambiguous {
		t.Fatalf("classification=%#v err=%v", classified, err)
	}
}

func newTestAdapter(t *testing.T, client *http.Client) *Adapter {
	t.Helper()
	endpoint, _ := url.Parse("https://bedrock-runtime.us-east-1.amazonaws.com")
	adapter, err := New(Options{
		Endpoint: endpoint, CredentialJSON: []byte(testCredential), Client: client,
		Now: func() time.Time { return time.Date(2026, 7, 31, 12, 34, 56, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func newTitanTestAdapter(t *testing.T, client *http.Client) *Adapter {
	t.Helper()
	endpoint, _ := url.Parse("https://bedrock-runtime.us-east-1.amazonaws.com")
	adapter, err := New(Options{
		Endpoint: endpoint, CredentialJSON: []byte(testCredential), Client: client,
		ProfileID: domain.ProfileBedrockInvokeTitanEmbedV2,
		Now:       func() time.Time { return time.Date(2026, 7, 31, 12, 34, 56, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}

func writeEventFrame(t *testing.T, output *bytes.Buffer, eventType, payload string) {
	t.Helper()
	headers := encodeStringHeader(":message-type", "event")
	headers = append(headers, encodeStringHeader(":event-type", eventType)...)
	total := 16 + len(headers) + len(payload)
	frame := make([]byte, total)
	binary.BigEndian.PutUint32(frame[0:4], uint32(total))
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(headers)))
	binary.BigEndian.PutUint32(frame[8:12], crc32.ChecksumIEEE(frame[:8]))
	copy(frame[12:], headers)
	copy(frame[12+len(headers):], payload)
	binary.BigEndian.PutUint32(frame[total-4:], crc32.ChecksumIEEE(frame[:total-4]))
	_, _ = output.Write(frame)
}

func encodeStringHeader(name, value string) []byte {
	result := []byte{byte(len(name))}
	result = append(result, name...)
	result = append(result, 7, byte(len(value)>>8), byte(len(value)))
	return append(result, value...)
}

func errorsAs(err error, target any) bool { return errors.As(err, target) }

// The connection test asks the operation a question it must refuse.
//
// It used to ask with HEAD, on the theory that a POST-only route answers 400 or
// 405. A real account answered a bare 404 — no `x-amzn-errortype`, so not one of
// Bedrock's modelled errors but the frontend refusing a method its route does
// not have — which made the probe unpassable and left a working connection
// unable to be enabled, because enabling is gated on its own test.
func TestProbeAsksTheOperationWithABodyItMustReject(t *testing.T) {
	var seen struct {
		method string
		path   string
		body   string
		signed bool
	}
	respond := func(status int, errorType string) *http.Client {
		return &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(request.Body)
			seen.method, seen.path, seen.body = request.Method, request.URL.EscapedPath(), string(body)
			seen.signed = strings.HasPrefix(request.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") &&
				request.Header.Get("x-amz-content-sha256") != ""
			header := http.Header{"Content-Type": []string{"application/json"}}
			if errorType != "" {
				header.Set("x-amzn-errortype", errorType)
			}
			return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader("{}"))}, nil
		})}
	}

	// The validation refusal is the evidence: the host resolved, the signature
	// verified, the model exists and the credential may call the operation.
	adapter := newTestAdapter(t, respond(http.StatusBadRequest, "ValidationException"))
	if err := adapter.Probe(context.Background(), "anthropic.claude-test-v1:0"); err != nil {
		t.Fatalf("a validation refusal was read as an unreachable connection: %v", err)
	}
	if seen.method != http.MethodPost {
		t.Fatalf("probe method=%q, which is not the method the operation routes", seen.method)
	}
	if seen.path != "/model/anthropic.claude-test-v1:0/converse" {
		t.Fatalf("probe path=%q", seen.path)
	}
	// Nothing that could be inferred on, and therefore nothing that can be
	// metered: an empty object has no messages for Converse to answer.
	if seen.body != "{}" {
		t.Fatalf("probe body=%q — a probe must carry nothing the upstream could bill for", seen.body)
	}
	if !seen.signed {
		t.Fatal("the probe body was not covered by the signature")
	}

	// A model this account cannot invoke still has to fail, or the test that
	// gates enabling a deployment would pass for a model that never answers.
	adapter = newTestAdapter(t, respond(http.StatusNotFound, "ResourceNotFoundException"))
	err := adapter.Probe(context.Background(), "anthropic.claude-test-v1:0")
	var missing *provider.Error
	if !errors.As(err, &missing) || missing.StatusCode != http.StatusNotFound {
		t.Fatalf("a missing model was accepted: %v", err)
	}

	adapter = newTestAdapter(t, respond(http.StatusForbidden, "AccessDeniedException"))
	err = adapter.Probe(context.Background(), "anthropic.claude-test-v1:0")
	var denied *provider.Error
	if !errors.As(err, &denied) || denied.Class != provider.ErrorAuthentication {
		t.Fatalf("a refused credential was not classified as authentication: %v", err)
	}
}

// The two profiles that answer for a connection with nothing deployed on it.
//
// ProbeRequiresModel reports false for Nova Reel and Cohere rerank because
// their probes address the operation's own collection endpoint and never name a
// model — which is what lets the Admin connection test run before a deployment
// exists. Both are pinned profiles, so the model check at the top of Probe read
// the absent id as a wrong one and answered "Nova Reel profile requires model
// amazon.nova-reel-v1:0": the exemption existed and the probe refused anyway,
// for exactly the value the caller was told it did not have to supply.
func TestProbeWithoutAModelIsRefusedOnlyByProfilesThatAddressOne(t *testing.T) {
	for _, test := range []struct {
		profile domain.ProviderProfileID
		path    string
	}{
		{domain.ProfileBedrockAsyncNovaReel, "/async-invoke"},
		{domain.ProfileBedrockAgentRerankCohere35, "/rerank"},
	} {
		t.Run(string(test.profile), func(t *testing.T) {
			seen := ""
			adapter := newProfileTestAdapter(t, &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				seen = request.URL.EscapedPath()
				return &http.Response{
					StatusCode: http.StatusBadRequest, Header: http.Header{},
					Body: io.NopCloser(strings.NewReader("{}")),
				}, nil
			})}, test.profile)
			if adapter.ProbeRequiresModel() {
				t.Fatal("this profile claims to need a model, so the exemption below does not apply to it")
			}
			if err := adapter.Probe(context.Background(), ""); err != nil {
				t.Fatalf("a probe that names no model was refused for not naming one: %v", err)
			}
			if seen != test.path {
				t.Fatalf("probe path=%q want %q", seen, test.path)
			}

			// The pin still holds on a model that was supplied: an exemption
			// from naming one is not permission to name the wrong one.
			if err := adapter.Probe(context.Background(), "anthropic.claude-test-v1:0"); err == nil {
				t.Fatal("a model outside the profile's pin was accepted")
			}
		})
	}

	// A profile whose probe does address a model path still refuses an absent
	// one — the Admin API stops that case before it gets here, and the adapter
	// must not become the thing that lets it through.
	titan := newProfileTestAdapter(t, refuseEveryRequest(t), domain.ProfileBedrockInvokeTitanEmbedV2)
	if !titan.ProbeRequiresModel() {
		t.Fatal("the Titan embedding profile probes a model path and must say so")
	}
	if err := titan.Probe(context.Background(), ""); err == nil {
		t.Fatal("a model-addressed probe accepted an empty model")
	}
}

// Bedrock names the exception, never the field inside the request that caused
// it: an unsupported inference parameter and a malformed message list are both
// ValidationException. The refusal is therefore reported as unattributed, which
// is what keeps a capability probe from reading a nonexistent model as a
// missing capability.
func TestBedrockBadRequestRefusalIsUnattributed(t *testing.T) {
	classified := classifyHTTP(jsonResponse(http.StatusBadRequest, `{"__type":"ValidationException","message":"toolConfig is not supported"}`))
	if classified.Class != provider.ErrorBadRequest || classified.Refusal != provider.RefusalInvalid {
		t.Fatalf("classification=%#v", classified)
	}
	// Only a refusal of the request body carries a kind. A throttle is not the
	// upstream saying anything about what was in the request.
	throttled := classifyHTTP(jsonResponse(http.StatusTooManyRequests, `{"__type":"ThrottlingException"}`))
	if throttled.Refusal != provider.RefusalNone {
		t.Fatalf("throttle carried a refusal kind: %#v", throttled)
	}
}

// End to end, because each half is convincing alone and the pair is the claim:
// the adapter maps ValidationException onto the normalized kind and the detector
// turns that into a capability answer. Before contract v3 every refused probe on
// this family recorded "inconclusive", whatever Bedrock had said.
func TestBedrockRefusalBecomesAnUnsupportedVerdictOnlyForADependentProbe(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusBadRequest, `{"__type":"ValidationException","message":"streaming is not supported for this model"}`), nil
	})}
	adapter := newTestAdapter(t, client)
	defer adapter.Close()
	manifest, ok := provider.BuiltinProfile(domain.ProfileBedrockConverseText)
	if !ok {
		t.Fatal("profile missing")
	}
	capabilities := domain.DefaultProviderCapabilitiesForProfile(domain.ProviderBedrock, domain.ProfileBedrockConverseText)
	bridge, err := provider.NewLegacyAdapterBridge(adapter, manifest, domain.EvidenceForCapabilities(capabilities, domain.EvidenceDeclared))
	if err != nil {
		t.Fatal(err)
	}
	target := provider.ModelCapabilityDetectionTarget{ProviderModel: "model", BindingID: "binding", ProfileID: manifest.ID, RiskTier: "safe_automatic"}

	dependent := bridge.DetectCapability(context.Background(), target,
		provider.CapabilityProbe{Capability: "streaming", Kind: "minimal_stream", DependsOn: []string{"chat"}, MaxOutputTokens: 16})
	if dependent.Status != domain.ProbeUnsupported {
		t.Fatalf("dependent probe recorded %s, want unsupported: %#v", dependent.Status, dependent)
	}
	// A model that does not exist is refused with the same exception, so a probe
	// with nothing behind it establishes nothing.
	root := bridge.DetectCapability(context.Background(), target,
		provider.CapabilityProbe{Capability: "chat", Kind: "minimal_chat", MaxOutputTokens: 16})
	if root.Status != domain.ProbeInconclusive {
		t.Fatalf("baseline probe recorded %s, want inconclusive: %#v", root.Status, root)
	}
}
