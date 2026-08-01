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
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Heimdall/internal/openaiapi"
	"github.com/akz142857/Heimdall/internal/provider"
	"github.com/akz142857/Heimdall/internal/semantic"
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
	if len(events) != 3 || events[0].Choices[0].Delta.Role != "assistant" ||
		*events[2].Choices[0].FinishReason != "length" || usage == nil || usage.TotalTokens != 5 {
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
	if validation.Class != provider.ErrorBadRequest || validation.Retryable || validation.Ambiguous {
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
	if throttled.Class != provider.ErrorRateLimit || throttled.RetryAfter != 5*time.Second {
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
