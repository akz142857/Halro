package bedrock

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/akz142857/Heimdall/internal/domain"
	"github.com/akz142857/Heimdall/internal/openaiapi"
	"github.com/akz142857/Heimdall/internal/provider"
	"github.com/akz142857/Heimdall/internal/semantic"
)

const maxResponseBytes = 16 << 20

type Options struct {
	Endpoint       *url.URL
	CredentialJSON []byte
	Client         *http.Client
	Now            func() time.Time
	Authorizer     provider.Authorizer
	ProfileID      domain.ProviderProfileID
}

type Adapter struct {
	endpoint   *url.URL
	client     *http.Client
	authorizer provider.Authorizer
	profileID  domain.ProviderProfileID
}

type textBlock struct {
	Text string `json:"text"`
}

type message struct {
	Role    string      `json:"role"`
	Content []textBlock `json:"content"`
}

type inferenceConfig struct {
	MaxTokens     *int64   `json:"maxTokens,omitempty"`
	Temperature   *float64 `json:"temperature,omitempty"`
	TopP          *float64 `json:"topP,omitempty"`
	StopSequences []string `json:"stopSequences,omitempty"`
}

type converseRequest struct {
	Messages        []message       `json:"messages"`
	System          []textBlock     `json:"system,omitempty"`
	InferenceConfig inferenceConfig `json:"inferenceConfig,omitempty"`
}

type tokenUsage struct {
	InputTokens  int64 `json:"inputTokens"`
	OutputTokens int64 `json:"outputTokens"`
	TotalTokens  int64 `json:"totalTokens"`
}

type credentialDocument struct {
	AccessKeyID     string          `json:"access_key_id"`
	SecretAccessKey json.RawMessage `json:"secret_access_key"`
	SessionToken    json.RawMessage `json:"session_token,omitempty"`
	Region          string          `json:"region"`
}

type converseResponse struct {
	Output struct {
		Message struct {
			Role    string            `json:"role"`
			Content []json.RawMessage `json:"content"`
		} `json:"message"`
	} `json:"output"`
	StopReason string     `json:"stopReason"`
	Usage      tokenUsage `json:"usage"`
}

func New(options Options) (*Adapter, error) {
	if options.Endpoint == nil || options.Client == nil {
		return nil, errors.New("endpoint and client are required")
	}
	authorizer := options.Authorizer
	if authorizer == nil {
		var err error
		authorizer, err = NewAuthorizer(options.Endpoint, options.CredentialJSON, options.Now)
		if err != nil {
			return nil, err
		}
	}
	if authorizer.Scheme() != domain.CredentialAWSSigV4Explicit {
		authorizer.Close()
		return nil, errors.New("credential scheme does not match Bedrock profile")
	}
	profileID := options.ProfileID
	if profileID == "" {
		profileID = domain.ProfileBedrockConverseText
	}
	if profileID != domain.ProfileBedrockConverseText && profileID != domain.ProfileBedrockInvokeTitanEmbedV2 && profileID != domain.ProfileBedrockInvokeTitanImageV2 && profileID != domain.ProfileBedrockAsyncNovaReel && profileID != domain.ProfileBedrockAgentRerankCohere35 {
		authorizer.Close()
		return nil, errors.New("Bedrock Runtime profile is not implemented")
	}
	endpoint := *options.Endpoint
	return &Adapter{endpoint: &endpoint, client: options.Client, authorizer: authorizer, profileID: profileID}, nil
}

func NewAuthorizer(endpoint *url.URL, credentialJSON []byte, now func() time.Time) (provider.Authorizer, error) {
	if endpoint == nil {
		return nil, errors.New("endpoint is required")
	}
	if !strings.EqualFold(endpoint.Scheme, "https") || endpoint.User != nil ||
		(endpoint.Port() != "" && endpoint.Port() != "443") {
		return nil, errors.New("AWS Bedrock credential authorizer requires an HTTPS endpoint on port 443 without user info")
	}
	encoded := bytes.Clone(credentialJSON)
	defer clear(encoded)
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var document credentialDocument
	defer func() {
		clear(document.SecretAccessKey)
		clear(document.SessionToken)
	}()
	if err := decoder.Decode(&document); err != nil {
		return nil, errors.New("AWS credential must be valid JSON")
	}
	var trailing json.RawMessage
	trailingErr := decoder.Decode(&trailing)
	clear(trailing)
	if trailingErr != io.EOF {
		return nil, errors.New("AWS credential contains trailing data")
	}
	secret, err := decodeCredentialBytes(document.SecretAccessKey, false)
	if err != nil {
		return nil, errors.New("AWS secret_access_key is invalid")
	}
	defer clear(secret)
	sessionToken, err := decodeCredentialBytes(document.SessionToken, true)
	if err != nil {
		return nil, errors.New("AWS session_token is invalid")
	}
	defer clear(sessionToken)
	value := credentials{
		AccessKeyID: document.AccessKeyID, SecretAccessKey: secret,
		SessionToken: sessionToken, Region: document.Region,
	}
	signed, err := newSigner(value)
	if err != nil {
		return nil, err
	}
	if validBedrockAgentRuntimeHost(endpoint.Hostname(), signed.region) {
		signed.service = "bedrock-agent-runtime"
	} else if !validBedrockRuntimeHost(endpoint.Hostname(), signed.region) {
		signed.Close()
		return nil, errors.New("AWS endpoint host is not an approved Bedrock Runtime endpoint for the credential region")
	}
	signed.authority = endpoint.Host
	if now != nil {
		signed.now = now
	}
	return signed, nil
}

func validBedrockAgentRuntimeHost(host, region string) bool {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	region = strings.ToLower(region)
	for _, allowed := range []string{"bedrock-agent-runtime." + region + ".amazonaws.com", "bedrock-agent-runtime." + region + ".amazonaws.com.cn", "bedrock-agent-runtime." + region + ".api.aws"} {
		if host == allowed {
			return true
		}
	}
	return false
}

func decodeCredentialBytes(raw json.RawMessage, optional bool) ([]byte, error) {
	if len(raw) == 0 && optional {
		return nil, nil
	}
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return nil, errors.New("credential value must be a JSON string")
	}
	result := make([]byte, 0, len(raw)-2)
	for _, value := range raw[1 : len(raw)-1] {
		// AWS access secrets and session tokens use printable ASCII and do not
		// require JSON escapes. Rejecting escapes avoids immutable decoded string
		// copies that cannot be zeroized in Go.
		if value < 0x21 || value > 0x7e || value == '\\' || value == '"' {
			clear(result)
			return nil, errors.New("credential value contains unsupported characters")
		}
		result = append(result, value)
	}
	return result, nil
}

func validBedrockRuntimeHost(host, region string) bool {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	region = strings.ToLower(region)
	for _, allowed := range []string{
		"bedrock-runtime." + region + ".amazonaws.com",
		"bedrock-runtime-fips." + region + ".amazonaws.com",
		"bedrock-runtime." + region + ".amazonaws.com.cn",
		"bedrock-runtime-fips." + region + ".amazonaws.com.cn",
		"bedrock-runtime." + region + ".api.aws",
		"bedrock-runtime-fips." + region + ".api.aws",
	} {
		if host == allowed {
			return true
		}
	}
	privateLinkSuffix := ".bedrock-runtime." + region + ".vpce.amazonaws.com"
	return strings.HasPrefix(host, "vpce-") && strings.HasSuffix(host, privateLinkSuffix)
}

func (a *Adapter) Type() string { return "bedrock" }

func (a *Adapter) Capabilities() provider.Capabilities {
	if a.profileID == domain.ProfileBedrockInvokeTitanEmbedV2 {
		return provider.Capabilities{Embeddings: true, MaxContextTokens: titanEmbedV2MaxTokens}
	}
	if a.profileID == domain.ProfileBedrockInvokeTitanImageV2 {
		return provider.Capabilities{Images: true}
	}
	if a.profileID == domain.ProfileBedrockAgentRerankCohere35 {
		return provider.Capabilities{Rerank: true}
	}
	if a.profileID == domain.ProfileBedrockAsyncNovaReel {
		return provider.Capabilities{AsyncGenerate: true}
	}
	return provider.Capabilities{Chat: true, Streaming: true, StreamUsage: true}
}

func (a *Adapter) Close() {
	a.authorizer.Close()
	a.client.CloseIdleConnections()
}

func (a *Adapter) Probe(ctx context.Context, model string) error {
	if err := ValidateProfileModel(a.profileID, model); err != nil {
		return badRequest(err.Error(), nil)
	}
	operation := "converse"
	if a.profileID == domain.ProfileBedrockInvokeTitanEmbedV2 || a.profileID == domain.ProfileBedrockInvokeTitanImageV2 {
		operation = "invoke"
	}
	var endpoint url.URL
	var err error
	if a.profileID == domain.ProfileBedrockAsyncNovaReel || a.profileID == domain.ProfileBedrockAgentRerankCohere35 {
		endpoint = *a.endpoint
		if a.profileID == domain.ProfileBedrockAsyncNovaReel {
			endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/async-invoke"
		} else {
			endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/rerank"
		}
	} else {
		endpoint, err = a.operationURL(model, operation)
	}
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodHead, endpoint.String(), nil)
	if err != nil {
		return badRequest("create Bedrock probe", err)
	}
	if err := a.authorizer.Authorize(request, nil); err != nil {
		return badRequest("sign Bedrock probe", err)
	}
	response, err := a.client.Do(request)
	if err != nil {
		return transportError("Bedrock probe failed", err)
	}
	defer response.Body.Close()
	if response.StatusCode >= 200 && response.StatusCode < 300 ||
		response.StatusCode == http.StatusBadRequest || response.StatusCode == http.StatusMethodNotAllowed {
		return nil
	}
	return classifyHTTP(response)
}

func (a *Adapter) Chat(ctx context.Context, call provider.ChatCall) (openaiapi.ChatCompletionResponse, error) {
	if a.profileID != domain.ProfileBedrockConverseText {
		return openaiapi.ChatCompletionResponse{}, badRequest("Bedrock Invoke embeddings profile does not declare chat", nil)
	}
	payload, err := translateRequest(call.Request)
	if err != nil {
		return openaiapi.ChatCompletionResponse{}, err
	}
	var result converseResponse
	upstreamRequestID, err := a.postJSON(ctx, call.ProviderModel, "converse", call.RequestID, payload, &result)
	if err != nil {
		return openaiapi.ChatCompletionResponse{}, err
	}
	if result.Output.Message.Role != "assistant" {
		return openaiapi.ChatCompletionResponse{}, malformed("Bedrock response is missing output", nil)
	}
	text, err := responseBlocksText(result.Output.Message.Content)
	if err != nil {
		return openaiapi.ChatCompletionResponse{}, err
	}
	finish, err := mapFinishReason(result.StopReason)
	if err != nil {
		return openaiapi.ChatCompletionResponse{}, err
	}
	usage, err := openAIUsage(result.Usage)
	if err != nil {
		return openaiapi.ChatCompletionResponse{}, err
	}
	return openaiapi.ChatCompletionResponse{
		ID: bedrockResponseID(upstreamRequestID, call.RequestID), Object: "chat.completion", Created: time.Now().Unix(), Model: call.ProviderModel,
		Choices: []openaiapi.Choice{{Index: 0, Message: &openaiapi.Message{Role: "assistant", Content: openaiapi.TextContent(text)}, FinishReason: finish}},
		Usage:   usage,
	}, nil
}

func (a *Adapter) ChatStream(ctx context.Context, call provider.ChatCall, emit func(semantic.Event) error) (*openaiapi.Usage, error) {
	if a.profileID != domain.ProfileBedrockConverseText {
		return nil, badRequest("Bedrock Invoke embeddings profile does not declare streaming chat", nil)
	}
	if emit == nil {
		return nil, badRequest("stream callback is required", nil)
	}
	payload, err := translateRequest(call.Request)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, badRequest("encode Bedrock request", err)
	}
	endpoint, err := a.operationURL(call.ProviderModel, "converse-stream")
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(encoded))
	if err != nil {
		return nil, badRequest("create Bedrock stream request", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/vnd.amazon.eventstream")
	request.Header.Set("X-Request-ID", call.RequestID)
	if err := a.authorizer.Authorize(request, encoded); err != nil {
		return nil, badRequest("sign Bedrock stream request", err)
	}
	response, err := a.client.Do(request)
	if err != nil {
		return nil, transportError("Bedrock stream request failed", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, classifyHTTP(response)
	}
	if !strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "application/vnd.amazon.eventstream") {
		return nil, malformed("Bedrock did not return an AWS event stream", nil)
	}
	id := bedrockResponseID(providerRequestID(response.Header), call.RequestID)
	created := time.Now().Unix()
	var usage *openaiapi.Usage
	sawMessage := false
	sawStop := false
	state := newBedrockStreamState()
	for {
		frame, err := readStreamMessage(response.Body)
		if errors.Is(err, io.EOF) {
			if !sawMessage {
				return usage, malformed("Bedrock stream ended before messageStart", nil)
			}
			if !sawStop {
				return usage, malformed("Bedrock stream ended before messageStop", nil)
			}
			if !state.metadataSeen {
				return usage, malformed("Bedrock stream ended before usage metadata", nil)
			}
			return usage, nil
		}
		if err != nil {
			return usage, malformed("decode Bedrock event stream", err)
		}
		eventType := frame.Headers[":event-type"]
		if frame.Headers[":message-type"] == "exception" || strings.HasSuffix(strings.ToLower(eventType), "exception") {
			return usage, streamException(eventType, response.Header, frame.Payload)
		}
		if err := state.Accept(eventType, frame.Payload); err != nil {
			return usage, malformed("invalid Bedrock stream event order", err)
		}
		event, found, eventUsage, err := translateStreamEvent(eventType, frame.Payload, id, call.ProviderModel, created)
		if err != nil {
			return usage, err
		}
		if eventUsage != nil {
			usage = eventUsage
		}
		if eventType == "messageStop" {
			sawStop = true
		}
		if !found {
			continue
		}
		if event.Outputs[0].Role == semantic.RoleAssistant {
			sawMessage = true
		}
		if err := emit(event); err != nil {
			return usage, err
		}
	}
}

func (a *Adapter) Embed(ctx context.Context, call provider.EmbeddingCall) (openaiapi.EmbeddingResponse, error) {
	if a.profileID != domain.ProfileBedrockInvokeTitanEmbedV2 {
		return openaiapi.EmbeddingResponse{}, badRequest("Bedrock Converse profile does not declare embeddings", nil)
	}
	return a.embedTitanV2(ctx, call)
}

func translateRequest(request openaiapi.ChatCompletionRequest) (converseRequest, error) {
	if (request.N != nil && *request.N != 1) || request.Seed != nil || len(request.Tools) != 0 || len(request.ToolChoice) != 0 || request.ParallelToolCalls != nil || len(request.ResponseFormat) != 0 || request.ReasoningEffort != "" || request.User != "" {
		return converseRequest{}, badRequest("Bedrock Beta cannot represent one or more requested OpenAI fields", nil)
	}
	result := converseRequest{InferenceConfig: inferenceConfig{
		Temperature: request.Temperature, TopP: request.TopP,
	}}
	if request.MaxCompletionTokens != nil {
		result.InferenceConfig.MaxTokens = request.MaxCompletionTokens
	} else {
		result.InferenceConfig.MaxTokens = request.MaxTokens
	}
	if len(request.Stop) != 0 {
		if err := json.Unmarshal(request.Stop, &result.InferenceConfig.StopSequences); err != nil {
			var single string
			if json.Unmarshal(request.Stop, &single) != nil {
				return converseRequest{}, badRequest("Bedrock stop must be a string or string array", err)
			}
			result.InferenceConfig.StopSequences = []string{single}
		}
	}
	for _, source := range request.Messages {
		if source.Name != "" {
			return converseRequest{}, badRequest("Bedrock Beta does not declare messages[].name", nil)
		}
		text, ok := openaiapi.DecodeTextContent(source.Content)
		if !ok {
			return converseRequest{}, badRequest("Bedrock Beta supports text message content only", nil)
		}
		switch source.Role {
		case "system":
			result.System = append(result.System, textBlock{Text: text})
		case "developer":
			return converseRequest{}, badRequest("Bedrock Beta does not declare the developer role", nil)
		case "user":
			result.Messages = append(result.Messages, message{Role: "user", Content: []textBlock{{Text: text}}})
		case "assistant":
			if len(source.ToolCalls) != 0 {
				return converseRequest{}, badRequest("Bedrock Beta does not support tool calls", nil)
			}
			result.Messages = append(result.Messages, message{Role: "assistant", Content: []textBlock{{Text: text}}})
		default:
			return converseRequest{}, badRequest("Bedrock Beta does not support tool messages", nil)
		}
	}
	if len(result.Messages) == 0 {
		return converseRequest{}, badRequest("Bedrock request has no conversation content", nil)
	}
	return result, nil
}

func translateStreamEvent(eventType string, payload []byte, id, model string, created int64) (semantic.Event, bool, *openaiapi.Usage, error) {
	base := semantic.Event{Kind: semantic.EventDelta, ID: id, Created: created, Model: model}
	switch eventType {
	case "messageStart":
		var body struct {
			Role string `json:"role"`
		}
		if err := json.Unmarshal(payload, &body); err != nil || body.Role != "assistant" {
			return semantic.Event{}, false, nil, malformed("decode Bedrock message start", err)
		}
		base.Outputs = []semantic.OutputDelta{{Index: 0, Role: semantic.RoleAssistant}}
	case "contentBlockDelta":
		var body struct {
			Delta map[string]json.RawMessage `json:"delta"`
		}
		if err := json.Unmarshal(payload, &body); err != nil {
			return semantic.Event{}, false, nil, malformed("decode Bedrock content delta", err)
		}
		if len(body.Delta) != 1 || body.Delta["text"] == nil {
			return semantic.Event{}, false, nil, malformed("Bedrock Beta received undeclared non-text stream delta", nil)
		}
		var text string
		if err := json.Unmarshal(body.Delta["text"], &text); err != nil {
			return semantic.Event{}, false, nil, malformed("decode Bedrock text delta", err)
		}
		base.Outputs = []semantic.OutputDelta{{Index: 0, Content: []semantic.ContentDelta{{Kind: semantic.ContentText, Text: text}}}}
	case "messageStop":
		var body struct {
			StopReason *string `json:"stopReason"`
		}
		if err := json.Unmarshal(payload, &body); err != nil || body.StopReason == nil {
			return semantic.Event{}, false, nil, malformed("decode Bedrock message stop", err)
		}
		finish, err := mapFinishReason(*body.StopReason)
		if err != nil {
			return semantic.Event{}, false, nil, err
		}
		native := ""
		if finish != nil {
			native = *finish
		}
		base.Outputs = []semantic.OutputDelta{{Index: 0, NativeTermination: native, Termination: normalizeBedrockTermination(native)}}
	case "metadata":
		var body struct {
			Usage *tokenUsage `json:"usage"`
		}
		if err := json.Unmarshal(payload, &body); err != nil || body.Usage == nil {
			return semantic.Event{}, false, nil, malformed("decode Bedrock stream metadata", err)
		}
		usage, err := openAIUsage(*body.Usage)
		if err == nil && usage == nil {
			err = malformed("Bedrock stream metadata contains empty usage", nil)
		}
		return semantic.Event{}, false, usage, err
	case "contentBlockStart":
		var body struct {
			Start map[string]json.RawMessage `json:"start"`
		}
		if err := json.Unmarshal(payload, &body); err != nil {
			return semantic.Event{}, false, nil, malformed("decode Bedrock content block start", err)
		}
		if len(body.Start) != 0 {
			return semantic.Event{}, false, nil, malformed("Bedrock Beta received undeclared non-text stream content", nil)
		}
		return semantic.Event{}, false, nil, nil
	case "contentBlockStop":
		return semantic.Event{}, false, nil, nil
	default:
		return semantic.Event{}, false, nil, malformed("unsupported Bedrock stream event", nil)
	}
	if err := base.Validate(); err != nil {
		return semantic.Event{}, false, nil, malformed("Bedrock stream event is semantically invalid", err)
	}
	return base, true, nil, nil
}

func normalizeBedrockTermination(value string) string {
	switch value {
	case "stop":
		return "complete"
	case "length":
		return "max_output"
	case "content_filter":
		return "refusal"
	default:
		return "unknown"
	}
}

func (a *Adapter) postJSON(ctx context.Context, model, operation, requestID string, input, output any) (string, error) {
	encoded, err := json.Marshal(input)
	if err != nil {
		return "", badRequest("encode Bedrock request", err)
	}
	endpoint, err := a.operationURL(model, operation)
	if err != nil {
		return "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(encoded))
	if err != nil {
		return "", badRequest("create Bedrock request", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-Request-ID", requestID)
	if err := a.authorizer.Authorize(request, encoded); err != nil {
		return "", badRequest("sign Bedrock request", err)
	}
	response, err := a.client.Do(request)
	if err != nil {
		return "", transportError("Bedrock request failed", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", classifyHTTP(response)
	}
	if !strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "application/json") {
		return "", malformed("Bedrock did not return JSON", nil)
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil || len(payload) > maxResponseBytes {
		return "", malformed("read Bedrock response", err)
	}
	if err := json.Unmarshal(payload, output); err != nil {
		return "", malformed("decode Bedrock response", err)
	}
	return providerRequestID(response.Header), nil
}

func (a *Adapter) operationURL(model, operation string) (url.URL, error) {
	model = strings.TrimSpace(model)
	if model == "" || strings.ContainsAny(model, "?#\\") {
		return url.URL{}, badRequest("invalid Bedrock model id", nil)
	}
	endpoint := *a.endpoint
	basePath := strings.TrimRight(endpoint.Path, "/")
	endpoint.Path = basePath + "/model/" + model + "/" + operation
	endpoint.RawPath = strings.TrimRight((&url.URL{Path: basePath}).EscapedPath(), "/") + "/model/" + url.PathEscape(model) + "/" + operation
	return endpoint, nil
}

func blocksText(blocks []textBlock) string {
	var result strings.Builder
	for _, block := range blocks {
		result.WriteString(block.Text)
	}
	return result.String()
}

func responseBlocksText(blocks []json.RawMessage) (string, error) {
	var result strings.Builder
	for _, encoded := range blocks {
		var block map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &block); err != nil {
			return "", malformed("decode Bedrock output block", err)
		}
		if len(block) != 1 || block["text"] == nil {
			return "", malformed("Bedrock Beta received undeclared non-text output", nil)
		}
		var text string
		if err := json.Unmarshal(block["text"], &text); err != nil {
			return "", malformed("decode Bedrock output text", err)
		}
		result.WriteString(text)
	}
	return result.String(), nil
}

func openAIUsage(value tokenUsage) (*openaiapi.Usage, error) {
	if value.InputTokens < 0 || value.OutputTokens < 0 || value.TotalTokens < 0 {
		return nil, malformed("Bedrock usage contains negative tokens", nil)
	}
	if value.InputTokens == 0 && value.OutputTokens == 0 && value.TotalTokens == 0 {
		return nil, nil
	}
	return &openaiapi.Usage{PromptTokens: value.InputTokens, CompletionTokens: value.OutputTokens, TotalTokens: max(value.TotalTokens, value.InputTokens+value.OutputTokens)}, nil
}

func mapFinishReason(value string) (*string, error) {
	if value == "" {
		return nil, malformed("Bedrock response is missing stop reason", nil)
	}
	mapped := "stop"
	switch value {
	case "end_turn", "stop_sequence":
	case "max_tokens":
		mapped = "length"
	case "model_context_window_exceeded":
		mapped = "length"
	case "guardrail_intervened", "content_filtered":
		mapped = "content_filter"
	case "malformed_model_output", "malformed_tool_use":
		return nil, malformed("Bedrock model returned malformed output", nil)
	case "tool_use":
		return nil, malformed("Bedrock text profile received an undeclared tool result", nil)
	default:
		return nil, malformed("Bedrock Beta received an unknown stop reason", nil)
	}
	return &mapped, nil
}

func classifyHTTP(response *http.Response) *provider.Error {
	status := response.StatusCode
	payload, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	var body struct {
		Code string `json:"code"`
		Type string `json:"__type"`
	}
	_ = json.Unmarshal(payload, &body)
	code := safeProviderCode(body.Code)
	if code == "" {
		code = safeProviderCode(body.Type)
	}
	headerErrorType := response.Header.Get("x-amzn-errortype")
	if index := strings.IndexByte(headerErrorType, ':'); index >= 0 {
		headerErrorType = headerErrorType[:index]
	}
	if headerCode := safeProviderCode(headerErrorType); headerCode != "" {
		code = headerCode
	}
	result := &provider.Error{
		StatusCode: status, Message: fmt.Sprintf("Bedrock error (%d)", status),
		ProviderRequestID: providerRequestID(response.Header), ProviderCode: code,
		RetryAfter: retryAfter(response.Header, time.Now()),
	}
	switch strings.ToLower(code) {
	case "modeltimeoutexception":
		result.Class, result.Ambiguous = provider.ErrorTimeout, true
	case "modelerrorexception":
		result.Class, result.Ambiguous = provider.ErrorProvider5xx, true
	case "throttlingexception", "servicequotaexceededexception":
		result.Class, result.Retryable = provider.ErrorRateLimit, true
	case "accessdeniedexception", "unrecognizedclientexception":
		result.Class = provider.ErrorAuthentication
	case "validationexception":
		result.Class = provider.ErrorBadRequest
	default:
		switch {
		case status == http.StatusUnauthorized || status == http.StatusForbidden:
			result.Class = provider.ErrorAuthentication
		case status == http.StatusRequestTimeout:
			result.Class, result.Ambiguous = provider.ErrorTimeout, true
		case status == http.StatusTooManyRequests:
			result.Class, result.Retryable = provider.ErrorRateLimit, true
		case status == http.StatusFailedDependency:
			result.Class, result.Ambiguous = provider.ErrorProvider5xx, true
		case status >= 500:
			// The request reached the Bedrock data plane. A 5xx cannot prove that
			// model execution did not start, so retry/fallback must not duplicate it.
			result.Class, result.Ambiguous = provider.ErrorProvider5xx, true
		default:
			result.Class = provider.ErrorBadRequest
		}
	}
	return result
}

func providerRequestID(header http.Header) string {
	value := strings.TrimSpace(header.Get("x-amzn-requestid"))
	if value == "" {
		value = strings.TrimSpace(header.Get("x-amz-request-id"))
	}
	if len(value) > 256 {
		return ""
	}
	for _, char := range value {
		if char < 0x21 || char > 0x7e {
			return ""
		}
	}
	return value
}

func safeProviderCode(value string) string {
	value = strings.TrimSpace(value)
	if index := strings.LastIndexAny(value, "#/"); index >= 0 {
		value = value[index+1:]
	}
	if len(value) > 128 {
		return ""
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || strings.ContainsRune("._:-", char) {
			continue
		}
		return ""
	}
	return value
}

func retryAfter(header http.Header, now time.Time) time.Duration {
	const maximum = 24 * time.Hour
	value := strings.TrimSpace(header.Get("Retry-After"))
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
		if seconds >= int64(maximum/time.Second) {
			return maximum
		}
		return time.Duration(seconds) * time.Second
	}
	if deadline, err := http.ParseTime(value); err == nil && deadline.After(now) {
		return min(deadline.Sub(now), maximum)
	}
	return 0
}

func bedrockResponseID(upstream, fallback string) string {
	if upstream != "" {
		return "bedrock-" + upstream
	}
	return "bedrock-" + fallback
}

func streamException(eventType string, header http.Header, payload []byte) *provider.Error {
	requestID := providerRequestID(header)
	code := safeProviderCode(eventType)
	var details struct {
		OriginalStatusCode int `json:"originalStatusCode"`
	}
	_ = json.Unmarshal(payload, &details)
	switch strings.ToLower(eventType) {
	case "validationexception":
		return &provider.Error{Class: provider.ErrorBadRequest, Message: "Bedrock rejected the stream request", ProviderRequestID: requestID, ProviderCode: code}
	case "throttlingexception":
		return &provider.Error{Class: provider.ErrorRateLimit, Retryable: true, Message: "Bedrock stream is temporarily unavailable", ProviderRequestID: requestID, ProviderCode: code, RetryAfter: retryAfter(header, time.Now())}
	case "modeltimeoutexception":
		return &provider.Error{Class: provider.ErrorTimeout, Ambiguous: true, Message: "Bedrock model stream timed out after acceptance", ProviderRequestID: requestID, ProviderCode: code}
	case "modelstreamerrorexception":
		class := provider.ErrorProvider5xx
		if details.OriginalStatusCode == http.StatusRequestTimeout {
			class = provider.ErrorTimeout
		}
		return &provider.Error{Class: class, Ambiguous: true, Message: "Bedrock stream failed after acceptance", ProviderRequestID: requestID, ProviderCode: code}
	case "internalserverexception", "serviceunavailableexception":
		return &provider.Error{Class: provider.ErrorProvider5xx, Ambiguous: true, Message: "Bedrock stream failed after acceptance", ProviderRequestID: requestID, ProviderCode: code}
	default:
		return &provider.Error{Class: provider.ErrorMalformed, Ambiguous: true, Message: "Bedrock returned an unknown stream exception", ProviderRequestID: requestID, ProviderCode: code}
	}
}

type bedrockStreamState struct {
	messageStarted bool
	messageStopped bool
	metadataSeen   bool
	blocks         map[int]bool
}

func newBedrockStreamState() *bedrockStreamState {
	return &bedrockStreamState{blocks: make(map[int]bool)}
}

func (s *bedrockStreamState) Accept(eventType string, payload []byte) error {
	switch eventType {
	case "messageStart":
		if s.messageStarted || s.messageStopped {
			return errors.New("messageStart is duplicated or late")
		}
		s.messageStarted = true
	case "contentBlockStart", "contentBlockDelta", "contentBlockStop":
		if !s.messageStarted || s.messageStopped {
			return errors.New("content block event is outside the active message")
		}
		var body struct {
			ContentBlockIndex *int `json:"contentBlockIndex"`
		}
		if err := json.Unmarshal(payload, &body); err != nil || body.ContentBlockIndex == nil || *body.ContentBlockIndex < 0 || *body.ContentBlockIndex > 1024 {
			return errors.New("content block index is invalid")
		}
		index := *body.ContentBlockIndex
		closed, exists := s.blocks[index]
		if !exists && len(s.blocks) >= 1024 {
			return errors.New("content block count exceeds the profile limit")
		}
		if eventType == "contentBlockStop" {
			if !exists || closed {
				return errors.New("content block stop has no active block")
			}
			s.blocks[index] = true
			return nil
		}
		if closed {
			return errors.New("content block event follows contentBlockStop")
		}
		if eventType == "contentBlockStart" && exists {
			return errors.New("contentBlockStart is duplicated")
		}
		if eventType == "contentBlockDelta" && !exists {
			return errors.New("contentBlockDelta is missing contentBlockStart")
		}
		s.blocks[index] = false
	case "messageStop":
		if !s.messageStarted || s.messageStopped {
			return errors.New("messageStop is missing messageStart or duplicated")
		}
		for _, closed := range s.blocks {
			if !closed {
				return errors.New("messageStop precedes contentBlockStop")
			}
		}
		s.messageStopped = true
	case "metadata":
		if !s.messageStopped || s.metadataSeen {
			return errors.New("metadata precedes messageStop or is duplicated")
		}
		s.metadataSeen = true
	default:
		if !s.messageStarted || s.messageStopped {
			return errors.New("unknown event is outside the active message")
		}
	}
	return nil
}

func badRequest(message string, cause error) *provider.Error {
	return &provider.Error{Class: provider.ErrorBadRequest, Message: message, Cause: cause}
}

func malformed(message string, cause error) *provider.Error {
	return &provider.Error{Class: provider.ErrorMalformed, Ambiguous: true, Message: message, Cause: cause}
}

func transportError(message string, err error) *provider.Error {
	class := provider.ErrorConnect
	if errors.Is(err, context.DeadlineExceeded) {
		class = provider.ErrorTimeout
	}
	return &provider.Error{Class: class, Retryable: true, Ambiguous: !provider.Unsent(err), Message: message, Cause: err}
}
