package bedrockmantle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	openaiwire "github.com/akz142857/Halro/internal/compatibility/openai"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/semantic"
	"github.com/akz142857/Halro/internal/sse"
)

const maxResponseBytes = 16 << 20

type ResponsesOptions struct {
	Endpoint     *url.URL
	Authorizer   provider.Authorizer
	Client       *http.Client
	Capabilities provider.Capabilities
	// BedrockProjectID is empty when this provider addresses the account's
	// default Bedrock project, which is the only reachable one until an
	// operator names another.
	BedrockProjectID string
	// OperationPathPrefix names the route this profile addresses. Mantle serves
	// Responses from both /v1/responses and /openai/v1/responses, on disjoint
	// model sets, so the profile fixes which one. Empty means the default /v1.
	OperationPathPrefix string
}

type ResponsesAdapter struct {
	endpoint            *url.URL
	authorizer          provider.Authorizer
	client              *http.Client
	capabilities        provider.Capabilities
	bedrockProjectID    string
	operationPathPrefix string
}

func ValidateEndpoint(endpoint *url.URL) error {
	if endpoint == nil || endpoint.Scheme != "https" || endpoint.User != nil || (endpoint.Port() != "" && endpoint.Port() != "443") || (endpoint.Path != "" && endpoint.Path != "/") || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return errors.New("Bedrock Mantle endpoint must be an HTTPS origin without path, query, fragment, user info, or a non-default port")
	}
	host := strings.ToLower(endpoint.Hostname())
	if !strings.HasPrefix(host, "bedrock-mantle.") || !strings.HasSuffix(host, ".api.aws") {
		return errors.New("Bedrock Mantle endpoint host is not approved")
	}
	region := strings.TrimSuffix(strings.TrimPrefix(host, "bedrock-mantle."), ".api.aws")
	if region == "" || strings.Contains(region, ".") {
		return errors.New("Bedrock Mantle endpoint region is invalid")
	}
	for _, char := range region {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '-' {
			continue
		}
		return errors.New("Bedrock Mantle endpoint region is invalid")
	}
	return nil
}

func NewResponses(options ResponsesOptions) (*ResponsesAdapter, error) {
	if options.Endpoint == nil || options.Authorizer == nil || options.Client == nil {
		return nil, errors.New("endpoint, authorizer, and client are required")
	}
	if options.Authorizer.Scheme() != domain.CredentialBedrockAPIKey {
		return nil, errors.New("credential scheme does not match Bedrock Mantle Responses profile")
	}
	endpoint := *options.Endpoint
	return &ResponsesAdapter{
		endpoint: &endpoint, authorizer: options.Authorizer, client: options.Client,
		capabilities: options.Capabilities, bedrockProjectID: options.BedrockProjectID,
		operationPathPrefix: strings.Trim(options.OperationPathPrefix, "/"),
	}, nil
}

func (*ResponsesAdapter) Type() string                                { return string(domain.ProviderBedrock) }
func (adapter *ResponsesAdapter) Capabilities() provider.Capabilities { return adapter.capabilities }
func (adapter *ResponsesAdapter) Close() {
	adapter.authorizer.Close()
	adapter.client.CloseIdleConnections()
}

func (adapter *ResponsesAdapter) Probe(ctx context.Context, _ string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, adapter.catalogURL(), nil)
	if err != nil {
		return badRequest("create Mantle probe", err)
	}
	request.Header.Set("Accept", "application/json")
	provider.ApplyBedrockProject(request, provider.HeaderBedrockOpenAIProject, adapter.bedrockProjectID)
	if err := adapter.authorizer.Authorize(request, nil); err != nil {
		return &provider.Error{Class: provider.ErrorAuthentication, Message: "authorize Mantle probe", Cause: err}
	}
	response, err := adapter.client.Do(request)
	if err != nil {
		return transportError("Mantle probe failed", err, false)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return decodeHTTPError(response, false)
	}
	_, err = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBytes+1))
	return err
}

// maxCatalogEntries bounds a model list Halro did not write. The entries feed a
// cache and a console list, and an upstream that answers with more than this is
// not describing an account an operator is choosing a model from.
const maxCatalogEntries = 2000

// maxCatalogResponseBytes is far below the operation ceiling on purpose: this is
// a list of identifiers, not a generation.
const maxCatalogResponseBytes = 1 << 20

func (adapter *ResponsesAdapter) InvocationTargetDiscovery() domain.InvocationTargetDiscoveryCapabilities {
	return domain.InvocationTargetDiscoveryCapabilities{
		TargetKinds:  []domain.DeploymentTargetKind{domain.TargetModelID},
		CanEnumerate: true, CanDescribe: true, CanVerify: true,
	}
}

// ListInvocationTargets enumerates the models this Mantle route serves.
//
// What it deliberately does not do is claim anything about them. The
// OpenAI-shaped /v1/models answer carries an identifier and an owner and
// nothing else — no modalities, no context window — so MapCapabilityClaims is
// not implemented here. A claim derived from an identifier is a guess wearing
// declared evidence, which is worse than the absence it would be covering up:
// the absence is visible and a wrong claim is not.
func (adapter *ResponsesAdapter) ListInvocationTargets(ctx context.Context, _ domain.TargetQuery) ([]domain.InvocationTargetDescriptor, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, adapter.catalogURL(), nil)
	if err != nil {
		return nil, badRequest("create Mantle model catalog request", err)
	}
	request.Header.Set("Accept", "application/json")
	provider.ApplyBedrockProject(request, provider.HeaderBedrockOpenAIProject, adapter.bedrockProjectID)
	if err := adapter.authorizer.Authorize(request, nil); err != nil {
		return nil, &provider.Error{Class: provider.ErrorAuthentication, Message: "authorize Mantle model catalog request", Cause: err}
	}
	response, err := adapter.client.Do(request)
	if err != nil {
		return nil, transportError("Mantle model catalog request failed", err, false)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, decodeHTTPError(response, false)
	}
	payload, err := readLimited(response.Body, maxCatalogResponseBytes)
	if err != nil {
		return nil, malformed("read Mantle model catalog response", err, false)
	}
	var catalog struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &catalog); err != nil {
		return nil, malformed("decode Mantle model catalog response", err, false)
	}
	if len(catalog.Data) > maxCatalogEntries {
		return nil, malformed("Mantle model catalog exceeded the entry limit", nil, false)
	}
	now := time.Now().UTC()
	targets := make([]domain.InvocationTargetDescriptor, 0, len(catalog.Data))
	for _, model := range catalog.Data {
		id := strings.TrimSpace(model.ID)
		if id == "" || len(id) > 512 {
			continue
		}
		targets = append(targets, domain.InvocationTargetDescriptor{
			TargetID: id, TargetKind: domain.TargetModelID, DisplayName: id,
			OwnedBy: strings.TrimSpace(model.OwnedBy), CanonicalModelRef: id,
			Lifecycle: domain.TargetLifecycleUnknown, MetadataSource: domain.MetadataSourceNone,
			Availability: domain.AvailabilityAvailable, FetchedAt: now,
		})
	}
	return targets, nil
}

func (adapter *ResponsesAdapter) DescribeInvocationTarget(ctx context.Context, target domain.InvocationTargetDescriptor) (domain.InvocationTargetDescriptor, error) {
	items, err := adapter.ListInvocationTargets(ctx, domain.TargetQuery{TargetKind: target.TargetKind})
	if err != nil {
		return domain.InvocationTargetDescriptor{}, err
	}
	for _, item := range items {
		if item.TargetID == strings.TrimSpace(target.TargetID) {
			return item, nil
		}
	}
	return domain.InvocationTargetDescriptor{}, &provider.Error{Class: provider.ErrorBadRequest, Message: "invocation target was not found"}
}

func (adapter *ResponsesAdapter) Chat(ctx context.Context, call provider.ChatCall) (openaiapi.ChatCompletionResponse, error) {
	canonical, err := openaiwire.DecodeGenerate(call.Request)
	if err != nil {
		return openaiapi.ChatCompletionResponse{}, badRequest("decode Mantle Responses request", err)
	}
	request, err := openaiwire.RenderProviderResponseRequest(canonical, call.ProviderModel)
	if err != nil {
		return openaiapi.ChatCompletionResponse{}, badRequest("render Mantle Responses request", err)
	}
	request.Stream = false
	payload, err := json.Marshal(request)
	if err != nil {
		return openaiapi.ChatCompletionResponse{}, badRequest("encode Mantle Responses request", err)
	}
	response, err := adapter.do(ctx, call.RequestID, payload, false)
	if err != nil {
		return openaiapi.ChatCompletionResponse{}, err
	}
	defer response.Body.Close()
	body, err := readLimited(response.Body, maxResponseBytes)
	if err != nil {
		return openaiapi.ChatCompletionResponse{}, malformed("read Mantle Responses result", err, true)
	}
	var wire openaiapi.Response
	if err := json.Unmarshal(body, &wire); err != nil {
		return openaiapi.ChatCompletionResponse{}, malformed("decode Mantle Responses result", err, true)
	}
	result, err := openaiwire.DecodeProviderResponse(wire)
	if err != nil {
		return openaiapi.ChatCompletionResponse{}, malformed("normalize Mantle Responses result", err, true)
	}
	result.ProviderRequestID = providerRequestID(response.Header)
	chat, err := openaiwire.RenderGenerateResult(result)
	if err != nil {
		return openaiapi.ChatCompletionResponse{}, malformed("render Mantle Chat result", err, true)
	}
	return chat, nil
}

func (adapter *ResponsesAdapter) ChatStream(ctx context.Context, call provider.ChatCall, emit func(semantic.Event) error) (*openaiapi.Usage, error) {
	if emit == nil {
		return nil, badRequest("stream callback is required", nil)
	}
	canonical, err := openaiwire.DecodeGenerate(call.Request)
	if err != nil {
		return nil, badRequest("decode Mantle Responses stream request", err)
	}
	request, err := openaiwire.RenderProviderResponseRequest(canonical, call.ProviderModel)
	if err != nil {
		return nil, badRequest("render Mantle Responses stream request", err)
	}
	request.Stream = true
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, badRequest("encode Mantle Responses stream request", err)
	}
	response, err := adapter.do(ctx, call.RequestID, payload, true)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if !strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		return nil, malformed("Mantle Responses did not return SSE", nil, true)
	}
	state := responseStreamState{emit: emit}
	decoder := sse.NewDecoder(response.Body, semantic.MaxEncodedEventBytes)
	for {
		event, nextErr := decoder.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return state.usage, malformed("decode Mantle Responses stream", nextErr, state.started)
		}
		if len(event.Data) == 0 || string(event.Data) == "[DONE]" {
			continue
		}
		var wire openaiapi.ResponseStreamEvent
		if err := json.Unmarshal(event.Data, &wire); err != nil {
			return state.usage, malformed("decode Mantle Responses event", err, state.started)
		}
		if wire.Type == "" {
			wire.Type = event.Event
		}
		if event.Event != "" && wire.Type != event.Event {
			return state.usage, malformed("Mantle Responses event name and type differ", nil, state.started)
		}
		if err := state.accept(wire); err != nil {
			return state.usage, err
		}
	}
	if !state.terminal {
		return state.usage, malformed("Mantle Responses stream ended before terminal event", nil, state.started)
	}
	return state.usage, nil
}

func (*ResponsesAdapter) Embed(context.Context, provider.EmbeddingCall) (openaiapi.EmbeddingResponse, error) {
	return openaiapi.EmbeddingResponse{}, badRequest("Bedrock Mantle Responses profile does not support embeddings", nil)
}

func (adapter *ResponsesAdapter) do(ctx context.Context, requestID string, payload []byte, stream bool) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, adapter.operationURL("responses"), bytes.NewReader(payload))
	if err != nil {
		return nil, badRequest("create Mantle Responses request", err)
	}
	// Protocol headers and resource addressing first, credential last, so a
	// signing credential scheme would see everything it has to cover.
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	if stream {
		request.Header.Set("Accept", "text/event-stream")
	}
	request.Header.Set("X-Request-ID", requestID)
	provider.ApplyBedrockProject(request, provider.HeaderBedrockOpenAIProject, adapter.bedrockProjectID)
	if err := adapter.authorizer.Authorize(request, nil); err != nil {
		return nil, &provider.Error{Class: provider.ErrorAuthentication, Message: "authorize Mantle Responses request", Cause: err}
	}
	response, err := adapter.client.Do(request)
	if err != nil {
		return nil, transportError("Mantle Responses request failed", err, true)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		defer response.Body.Close()
		return nil, decodeHTTPError(response, true)
	}
	return response, nil
}

// catalogURL addresses the account's model catalogue, which is not scoped to
// the route this profile pins. Measured against a real Mantle account on
// 2026-08-25, us-east-2: GET /v1/models answers 200 and GET /openai/v1/models
// answers 404, as does /openai/v1/models/{id} for a model that route serves.
// Discovery and the connection test therefore address /v1 whatever route the
// profile fixes for inference; see the same note on the OpenAI adapter's
// modelCatalogURL for what that costs — an /openai/v1 profile lists models it
// will not serve, and the upstream refuses them at call time rather than
// falling back.
func (adapter *ResponsesAdapter) catalogURL() string {
	endpoint := *adapter.endpoint
	base := strings.TrimRight(endpoint.Path, "/")
	if strings.HasSuffix(base, "/v1") {
		endpoint.Path = base + "/models"
	} else {
		endpoint.Path = base + "/v1/models"
	}
	return endpoint.String()
}

func (adapter *ResponsesAdapter) operationURL(operation string) string {
	endpoint := *adapter.endpoint
	base := strings.TrimRight(endpoint.Path, "/")
	if adapter.operationPathPrefix != "" {
		endpoint.Path = base + "/" + adapter.operationPathPrefix + "/" + operation
	} else if strings.HasSuffix(base, "/v1") {
		endpoint.Path = base + "/" + operation
	} else {
		endpoint.Path = base + "/v1/" + operation
	}
	return endpoint.String()
}

type responseStreamState struct {
	emit              func(semantic.Event) error
	id, model         string
	created           int64
	started, terminal bool
	usage             *openaiapi.Usage
}

func (state *responseStreamState) accept(event openaiapi.ResponseStreamEvent) error {
	if state.terminal {
		return malformed("Mantle Responses event followed terminal event", nil, true)
	}
	switch event.Type {
	case "response.created":
		if state.started || event.Response == nil || event.Response.ID == "" || event.Response.Model == "" {
			return malformed("invalid Mantle response.created event", nil, state.started)
		}
		state.started, state.id, state.model, state.created = true, event.Response.ID, event.Response.Model, event.Response.CreatedAt
	case "response.in_progress", "response.output_item.added", "response.content_part.added", "response.output_text.done", "response.content_part.done", "response.output_item.done":
		if !state.started {
			return malformed("Mantle Responses lifecycle event arrived before response.created", nil, false)
		}
	case "response.output_text.delta":
		if !state.started {
			return malformed("Mantle Responses delta arrived before response.created", nil, false)
		}
		return state.emit(semantic.Event{Kind: semantic.EventDelta, ID: state.id, Created: state.created, Model: state.model, Outputs: []semantic.OutputDelta{{Index: 0, Role: semantic.RoleAssistant, Content: []semantic.ContentDelta{{Kind: semantic.ContentText, Text: event.Delta}}}}, Translation: semantic.TranslationNone, MappingRevision: openaiwire.MappingRevision})
	case "response.completed", "response.incomplete":
		if !state.started || event.Response == nil || event.Response.ID != state.id || event.Response.Model != state.model {
			return malformed("invalid Mantle Responses terminal event", nil, state.started)
		}
		termination := "complete"
		if event.Type == "response.incomplete" {
			termination = "max_output"
		}
		if err := state.emit(semantic.Event{Kind: semantic.EventDelta, ID: state.id, Created: state.created, Model: state.model, Outputs: []semantic.OutputDelta{{Index: 0, Termination: termination, NativeTermination: event.Type}}, Translation: semantic.TranslationNone, MappingRevision: openaiwire.MappingRevision}); err != nil {
			return err
		}
		if usage := event.Response.Usage; usage != nil {
			// input_tokens already counts the cached span on the Responses shape,
			// so the cached figure is a subset rather than an addition.
			state.usage = &openaiapi.Usage{PromptTokens: usage.InputTokens, CompletionTokens: usage.OutputTokens, TotalTokens: usage.TotalTokens}
			state.usage.SetCachedPromptTokens(usage.InputTokensDetails.CachedTokens)
			state.usage.SetReasoningTokens(usage.OutputTokensDetails.ReasoningTokens)
			semanticUsage := semantic.Usage{
				InputTokens:       usage.InputTokens,
				CachedInputTokens: usage.InputTokensDetails.CachedTokens,
				OutputTokens:      usage.OutputTokens,
				ReasoningTokens:   usage.OutputTokensDetails.ReasoningTokens,
				AudioTokens:       usage.InputTokensDetails.AudioTokens + usage.OutputTokensDetails.AudioTokens,
				TotalTokens:       max(usage.TotalTokens, usage.InputTokens+usage.OutputTokens),
				Source:            semantic.UsageProviderReported,
			}
			if err := state.emit(semantic.Event{Kind: semantic.EventUsage, ID: state.id, Created: state.created, Model: state.model, Usage: &semanticUsage, Translation: semantic.TranslationNone, MappingRevision: openaiwire.MappingRevision}); err != nil {
				return err
			}
		}
		state.terminal = true
	case "error":
		return &provider.Error{Class: provider.ErrorProvider5xx, Ambiguous: state.started, ProviderCode: event.Code, Message: "Mantle Responses stream returned an error event"}
	default:
		return malformed("unsupported Mantle Responses stream event", nil, state.started)
	}
	return nil
}

func decodeHTTPError(response *http.Response, ambiguous bool) error {
	payload, _ := readLimited(response.Body, 1<<20)
	var envelope openaiapi.ErrorEnvelope
	_ = json.Unmarshal(payload, &envelope)
	class, retryable := provider.ErrorBadRequest, false
	switch response.StatusCode {
	case 401, 403:
		class = provider.ErrorAuthentication
	case 408, 504:
		class, retryable = provider.ErrorTimeout, true
	case 429:
		class, retryable = provider.ErrorRateLimit, true
	case 500, 502, 503:
		class, retryable = provider.ErrorProvider5xx, true
	}
	message := envelope.Error.Message
	if message == "" {
		message = http.StatusText(response.StatusCode)
	}
	code := refusalCode(envelope)
	refusal := provider.RefusalFromOpenAIBody(response.StatusCode, code)
	return &provider.Error{Class: class, StatusCode: response.StatusCode, Retryable: retryable, Ambiguous: ambiguous && response.StatusCode >= 500, Message: message, ProviderCode: code, Refusal: refusal, ProviderRequestID: providerRequestID(response.Header), RetryAfter: retryAfter(response.Header)}
}

// refusalCode is the machine-readable half of an OpenAI-shaped refusal, which is
// the shape Mantle answers in. It is kept apart from the sentence beside it for
// the reason the attempt log exists: the sentence is a provider response body
// and never leaves the error, while the code — joined to the parameter it names
// — is the one part an operator can act on. Reading only the sentence left every
// Mantle refusal logged as a bare status, so a rejected field said a request was
// refused without saying for what, on a request Halro assembled rather than the
// operator.
func refusalCode(envelope openaiapi.ErrorEnvelope) string {
	code := strings.TrimSpace(envelope.Error.Code)
	if code == "" {
		// Not every OpenAI-shaped upstream fills `code`; `type` is the coarser
		// identifier the same bodies always carry.
		code = strings.TrimSpace(envelope.Error.Type)
	}
	if envelope.Error.Param == nil || code == "" {
		return provider.SafeProviderIdentifier(code)
	}
	if param := strings.TrimSpace(*envelope.Error.Param); param != "" {
		return provider.SafeProviderIdentifier(code + ":" + param)
	}
	return provider.SafeProviderIdentifier(code)
}

func providerRequestID(header http.Header) string {
	for _, name := range []string{"x-amzn-requestid", "x-amzn-request-id", "x-request-id", "request-id"} {
		if value := strings.TrimSpace(header.Get(name)); value != "" {
			return value
		}
	}
	return ""
}

func retryAfter(header http.Header) time.Duration {
	value := strings.TrimSpace(header.Get("retry-after"))
	seconds, err := strconv.ParseInt(value, 10, 64)
	if err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	return 0
}

func readLimited(reader io.Reader, limit int64) ([]byte, error) {
	payload, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(payload)) > limit {
		return nil, errors.New("response exceeds size limit")
	}
	return payload, nil
}

func badRequest(message string, cause error) error {
	return &provider.Error{Class: provider.ErrorBadRequest, Message: message, Cause: cause}
}
func malformed(message string, cause error, ambiguous bool) error {
	return &provider.Error{Class: provider.ErrorMalformed, Message: message, Cause: cause, Ambiguous: ambiguous}
}
func transportError(message string, err error, ambiguous bool) error {
	class := provider.TransportClass(err)
	// A caller claiming ambiguity is still bounded by whether the request could
	// have reached Mantle at all; a failed dial ran nothing.
	return &provider.Error{Class: class, Retryable: class != provider.ErrorCanceled, Ambiguous: ambiguous && !provider.Unsent(err), Message: message, Cause: err}
}
