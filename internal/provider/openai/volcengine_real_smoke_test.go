package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/semantic"
)

// Seven bounded billable requests, enabled only by explicit operator opt-in.
// Load the secret into HALRO_SMOKE_API_KEY in the parent process; never source
// .env as shell code or put the key in a command-line argument.
func TestRealVolcengineSmoke(t *testing.T) {
	if os.Getenv("HALRO_REAL_PROVIDER_SMOKE") != "1" || os.Getenv("HALRO_SMOKE_PROFILE") != "volcengine" {
		t.Skip("opt-in billable Ark smoke: HALRO_REAL_PROVIDER_SMOKE=1 HALRO_SMOKE_PROFILE=volcengine")
	}
	key, model := os.Getenv("HALRO_SMOKE_API_KEY"), os.Getenv("HALRO_SMOKE_MODEL")
	if key == "" || model == "" {
		t.Fatal("HALRO_SMOKE_API_KEY and HALRO_SMOKE_MODEL are required")
	}
	endpoint, _ := url.Parse(domain.ResolveBaseURL(domain.ProfileVolcengineChat, ""))
	client := &http.Client{Timeout: 45 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if iface := os.Getenv("HALRO_SMOKE_NETWORK_INTERFACE"); iface != "" {
		if _, err := exec.LookPath("curl"); err != nil {
			t.Fatal("curl is required for the explicit interface diagnostic")
		}
		client.Transport = &volcengineSmokeCurlTransport{directory: t.TempDir(), networkInterface: iface, resolveIP: os.Getenv("HALRO_SMOKE_RESOLVE_IP")}
	}
	adapter, err := NewWithOptions(Options{Endpoint: endpoint, APIKey: []byte(key), Client: client, ProviderType: string(domain.ProviderVolcengine), ProfileID: domain.ProfileVolcengineChat, Capabilities: domain.DefaultProviderCapabilities(domain.ProviderVolcengine), DisableTargetDescribe: true})
	if err != nil {
		t.Fatal("construct Ark adapter")
	}
	defer adapter.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	targets, err := adapter.ListInvocationTargets(ctx, domain.TargetQuery{})
	if err != nil {
		t.Fatalf("catalog: %s", smokeErrorClass(err))
	}
	listed := false
	for _, target := range targets {
		listed = listed || target.TargetID == model
	}
	if !listed {
		t.Fatal("selected model was not returned by upstream; refusing billable requests")
	}
	t.Logf("catalog: models=%d selected_model_listed=true", len(targets))
	t.Run("catalog", func(t *testing.T) {
		shutdown, deprecated := 0, 0
		for _, target := range targets {
			if target.Availability == domain.AvailabilityAvailable {
				t.Fatal("platform inventory was treated as proof of account activation")
			}
			if target.Availability == domain.AvailabilityUnavailable {
				shutdown++
			}
			if target.Lifecycle == domain.TargetLifecycleDeprecated {
				deprecated++
			}
			if target.MetadataSource == domain.MetadataSourceProvider && len(target.Metadata.InputModalities) == 0 && len(target.Metadata.OutputModalities) == 0 && len(target.Metadata.SupportedOperations) == 0 && target.Metadata.MaxContextTokens == 0 && target.Metadata.MaxOutputTokens == 0 {
				t.Fatal("provider metadata label without structured fields")
			}
		}
		t.Logf("catalog lifecycle: unavailable=%d deprecated=%d", shutdown, deprecated)
	})
	limit := int64(64)
	ask := func(prompt string) openaiapi.ChatCompletionRequest {
		return openaiapi.ChatCompletionRequest{Model: model, Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent(prompt)}}, MaxCompletionTokens: &limit}
	}
	chat := func(t *testing.T, request openaiapi.ChatCompletionRequest) openaiapi.ChatCompletionResponse {
		t.Helper()
		result, err := adapter.Chat(ctx, provider.ChatCall{RequestID: "ark_real_smoke", ProviderModel: model, Request: request})
		if err != nil {
			t.Fatalf("chat: %s", smokeErrorClass(err))
		}
		if result.Usage == nil || result.Usage.PromptTokens <= 0 || result.Usage.CompletionTokens <= 0 || result.Usage.TotalTokens < result.Usage.PromptTokens+result.Usage.CompletionTokens {
			t.Fatal("invalid usage accounting")
		}
		if result.Usage.ReasoningTokens() != 0 || len(result.Choices) != 1 || result.Choices[0].Message == nil || result.Choices[0].Message.ReasoningContent != "" {
			t.Fatal("thinking remained on or response was incomplete")
		}
		t.Logf("usage: input=%d output=%d total=%d cached=%d reasoning=0", result.Usage.PromptTokens, result.Usage.CompletionTokens, result.Usage.TotalTokens, result.Usage.CachedPromptTokens())
		return result
	}
	if !t.Run("plain", func(t *testing.T) {
		result := chat(t, ask("Reply with OK, and nothing else."))
		var text string
		if json.Unmarshal(result.Choices[0].Message.Content, &text) != nil || strings.TrimSpace(text) != "OK" {
			t.Fatal("unexpected plain reply")
		}
	}) {
		t.Fatal("baseline failed; refusing additional billable requests")
	}
	t.Run("stream", func(t *testing.T) {
		var text strings.Builder
		usage, err := adapter.ChatStream(ctx, provider.ChatCall{ProviderModel: model, Request: ask("Reply with OK, and nothing else.")}, func(event semantic.Event) error {
			for _, output := range event.Outputs {
				for _, part := range output.Content {
					if part.Kind == semantic.ContentText {
						text.WriteString(part.Text)
					}
					if part.Kind == semantic.ContentReasoning {
						t.Error("thinking returned while disabled")
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("stream: %s", smokeErrorClass(err))
		}
		if usage == nil || usage.PromptTokens <= 0 || usage.CompletionTokens <= 0 || usage.ReasoningTokens() != 0 || strings.TrimSpace(text.String()) != "OK" {
			t.Fatal("stream text or usage was not preserved")
		}
		t.Logf("stream usage: input=%d output=%d total=%d reasoning=0", usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens)
	})
	t.Run("tool_round_trip", func(t *testing.T) {
		request := ask("Call echo with value ping. After the tool returns, reply with its result and nothing else.")
		request.Tools = []openaiapi.Tool{{Type: "function", Function: openaiapi.ToolFunction{Name: "echo", Parameters: json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":false}`)}}}
		request.ToolChoice = json.RawMessage(`{"type":"function","function":{"name":"echo"}}`)
		result := chat(t, request)
		message := result.Choices[0].Message
		if len(message.ToolCalls) != 1 || message.ToolCalls[0].Function.Name != "echo" {
			t.Fatal("expected echo tool call")
		}
		var arguments struct {
			Value string `json:"value"`
		}
		if json.Unmarshal([]byte(message.ToolCalls[0].Function.Arguments), &arguments) != nil || arguments.Value != "ping" {
			t.Fatal("incorrect tool arguments")
		}
		request.Messages = append(request.Messages, *message, openaiapi.Message{Role: "tool", ToolCallID: message.ToolCalls[0].ID, Content: openaiapi.TextContent("pong")})
		request.ToolChoice = json.RawMessage(`"none"`)
		final := chat(t, request)
		var text string
		if json.Unmarshal(final.Choices[0].Message.Content, &text) != nil || strings.TrimSpace(text) != "pong" {
			t.Fatal("tool result was not preserved")
		}
	})
	for _, format := range []string{"json_object", "json_schema"} {
		t.Run(format, func(t *testing.T) {
			request := ask(`Return a JSON object with exactly one member: ok, whose value is true.`)
			request.ResponseFormat = json.RawMessage(`{"type":"json_object"}`)
			if format == "json_schema" {
				request.ResponseFormat = json.RawMessage(`{"type":"json_schema","json_schema":{"name":"smoke","strict":true,"schema":{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}}}`)
			}
			result := chat(t, request)
			var text string
			var value map[string]any
			if json.Unmarshal(result.Choices[0].Message.Content, &text) != nil || json.Unmarshal([]byte(text), &value) != nil || len(value) != 1 || value["ok"] != true {
				t.Fatal("JSON output failed validation")
			}
		})
	}
	t.Run("vision", func(t *testing.T) {
		img := image.NewRGBA(image.Rect(0, 0, 32, 32))
		for y := 0; y < 32; y++ {
			for x := 0; x < 32; x++ {
				img.SetRGBA(x, y, color.RGBA{R: 255, A: 255})
			}
		}
		var encoded bytes.Buffer
		if err := png.Encode(&encoded, img); err != nil {
			t.Fatal("encode synthetic image")
		}
		content, _ := json.Marshal([]any{map[string]any{"type": "text", "text": "What is the dominant color? Reply with one English word."}, map[string]any{"type": "image_url", "image_url": map[string]string{"url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(encoded.Bytes()), "detail": "auto"}}})
		request := ask("")
		request.Messages[0].Content = content
		result := chat(t, request)
		var text string
		if json.Unmarshal(result.Choices[0].Message.Content, &text) != nil || strings.ToLower(strings.Trim(text, " .\n\r\t")) != "red" {
			t.Fatal("image color was not understood")
		}
	})
}

// Optional diagnostic transport for hosts whose TUN route cannot reach Ark.
// curl binds only this request to the chosen interface, with normal HTTPS
// hostname/certificate checks. Credentials enter curl through stdin, never argv
// or files. Response buffering means this smoke checks SSE mapping, not latency.
type volcengineSmokeCurlTransport struct{ directory, networkInterface, resolveIP string }

func (transport *volcengineSmokeCurlTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	quote := func(value string) string {
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\r", `\r`, "\n", `\n`).Replace(value) + `"`
	}
	var config strings.Builder
	line := func(name, value string) { config.WriteString(name + " = " + quote(value) + "\n") }
	line("url", request.URL.String())
	line("request", request.Method)
	line("interface", transport.networkInterface)
	line("noproxy", "*")
	line("connect-timeout", "8")
	line("max-time", "40")
	config.WriteString("silent\nshow-error\n")
	if transport.resolveIP != "" {
		line("resolve", request.URL.Hostname()+":443:"+transport.resolveIP)
	}
	for name, values := range request.Header {
		for _, value := range values {
			line("header", name+": "+value)
		}
	}
	if request.Body != nil {
		defer request.Body.Close()
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		line("data-binary", string(body))
	}
	bodyPath := filepath.Join(transport.directory, "response.body")
	line("output", bodyPath)
	line("dump-header", filepath.Join(transport.directory, "response.headers"))
	line("write-out", "%{http_code}")
	command := exec.CommandContext(request.Context(), "curl", "--config", "-")
	command.Stdin = strings.NewReader(config.String())
	status, err := command.Output()
	if err != nil {
		return nil, contextOrCurlError(request.Context(), err)
	}
	code, err := strconv.Atoi(string(status))
	if err != nil {
		return nil, err
	}
	body, err := os.ReadFile(bodyPath)
	if err != nil {
		return nil, err
	}
	rawHeaders, err := os.ReadFile(filepath.Join(transport.directory, "response.headers"))
	if err != nil {
		return nil, err
	}
	blocks := strings.Split(strings.TrimSpace(string(rawHeaders)), "\r\n\r\n")
	last := strings.SplitN(blocks[len(blocks)-1], "\r\n", 2)
	header := make(http.Header)
	if len(last) == 2 {
		fields, err := textproto.NewReader(bufio.NewReader(strings.NewReader(last[1] + "\r\n\r\n"))).ReadMIMEHeader()
		if err != nil {
			return nil, err
		}
		header = http.Header(fields)
	}
	return &http.Response{StatusCode: code, Header: header, Body: io.NopCloser(bytes.NewReader(body)), Request: request}, nil
}

func contextOrCurlError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}
