package anthropic

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

	"github.com/akz142857/Halro/internal/anthropicapi"
	anthropicwire "github.com/akz142857/Halro/internal/compatibility/anthropic"
	openaiwire "github.com/akz142857/Halro/internal/compatibility/openai"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/semantic"
	"github.com/akz142857/Halro/internal/sse"
)

const maxResponseBytes = 16 << 20

type Options struct {
	Endpoint         *url.URL
	Authorizer       provider.Authorizer
	Client           *http.Client
	Capabilities     provider.Capabilities
	ProviderType     string
	CredentialScheme domain.CredentialScheme
	// ProfileID is the provider profile this adapter was built for. Batch input
	// is checked against it line by line, because a batch is routed once for
	// many requests and nothing else will look at them.
	ProfileID    domain.ProviderProfileID
	MessagesPath string
	// BedrockProjectID is empty for Anthropic's own API, which has no such
	// concept, and for a Bedrock Mantle provider that addresses the account's
	// default project.
	BedrockProjectID string
}

type Adapter struct {
	endpoint         *url.URL
	authorizer       provider.Authorizer
	client           *http.Client
	capabilities     provider.Capabilities
	providerType     string
	messagesPath     string
	bedrockProjectID string
	profileID        domain.ProviderProfileID
}

func New(options Options) (*Adapter, error) {
	if options.Endpoint == nil || options.Authorizer == nil || options.Client == nil {
		return nil, errors.New("endpoint, authorizer, and client are required")
	}
	expectedScheme := options.CredentialScheme
	if expectedScheme == "" {
		expectedScheme = domain.CredentialAnthropicAPIKey
	}
	if options.Authorizer.Scheme() != expectedScheme {
		return nil, errors.New("credential scheme does not match Anthropic profile")
	}
	providerType := options.ProviderType
	if providerType == "" {
		providerType = string(domain.ProviderAnthropic)
	}
	profileID := options.ProfileID
	if profileID == "" {
		profileID = domain.ProfileAnthropicMessages
	}
	return &Adapter{
		endpoint: options.Endpoint, authorizer: options.Authorizer, client: options.Client,
		capabilities: options.Capabilities, providerType: providerType, messagesPath: options.MessagesPath,
		bedrockProjectID: options.BedrockProjectID, profileID: profileID,
	}, nil
}

func (adapter *Adapter) Type() string                        { return adapter.providerType }
func (adapter *Adapter) Capabilities() provider.Capabilities { return adapter.capabilities }
func (adapter *Adapter) Close()                              { adapter.authorizer.Close(); adapter.client.CloseIdleConnections() }

func (adapter *Adapter) InvocationTargetDiscovery() domain.InvocationTargetDiscoveryCapabilities {
	canEnumerate := adapter.messagesPath == "" && adapter.providerType == string(domain.ProviderAnthropic)
	return domain.InvocationTargetDiscoveryCapabilities{
		TargetKinds:  []domain.DeploymentTargetKind{domain.TargetModelID},
		CanEnumerate: canEnumerate, CanDescribe: canEnumerate, CanVerify: true,
	}
}

type anthropicModelDescriptor struct {
	ID           string                     `json:"id"`
	DisplayName  string                     `json:"display_name"`
	Capabilities map[string]json.RawMessage `json:"capabilities"`
	// MaxInputTokens is the context window; the output ceiling arrives as
	// `max_tokens`, named after the request parameter it bounds rather than
	// after the window it sits opposite.
	MaxInputTokens  int64 `json:"max_input_tokens"`
	MaxOutputTokens int64 `json:"max_tokens"`
}

// modelCatalogURL addresses GET /v1/models, which both the catalog enumeration
// and the credential-only connection test read.
func (adapter *Adapter) modelCatalogURL(limit int, afterID string) url.URL {
	endpoint := *adapter.endpoint
	endpoint.Path = strings.TrimRight(endpoint.Path, "/")
	if !strings.HasSuffix(endpoint.Path, "/v1") {
		endpoint.Path += "/v1"
	}
	endpoint.Path += "/models"
	values := endpoint.Query()
	values.Set("limit", strconv.Itoa(limit))
	if afterID != "" {
		values.Set("after_id", afterID)
	}
	endpoint.RawQuery = values.Encode()
	return endpoint
}

// newModelCatalogRequest is the authorized GET both catalog readers share.
func (adapter *Adapter) newModelCatalogRequest(ctx context.Context, endpoint url.URL) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, badRequest("create Anthropic model catalog request", err)
	}
	if err := adapter.authorizer.Authorize(request, nil); err != nil {
		return nil, &provider.Error{Class: provider.ErrorAuthentication, Message: "authorize Anthropic model catalog request", Cause: err}
	}
	request.Header.Set("anthropic-version", anthropicapi.SupportedVersion)
	request.Header.Set("accept", "application/json")
	return request, nil
}

func (adapter *Adapter) ListInvocationTargets(ctx context.Context, query domain.TargetQuery) ([]domain.InvocationTargetDescriptor, error) {
	if !adapter.InvocationTargetDiscovery().CanEnumerate {
		return nil, &provider.Error{Class: provider.ErrorBadRequest, Message: "this Anthropic-compatible profile has no model catalog"}
	}
	var targets []domain.InvocationTargetDescriptor
	afterID := ""
	for page := 0; page < 20; page++ {
		request, err := adapter.newModelCatalogRequest(ctx, adapter.modelCatalogURL(1000, afterID))
		if err != nil {
			return nil, err
		}
		response, err := adapter.client.Do(request)
		if err != nil {
			return nil, transportError(err)
		}
		var catalog struct {
			Data    []anthropicModelDescriptor `json:"data"`
			HasMore bool                       `json:"has_more"`
			LastID  string                     `json:"last_id"`
		}
		func() {
			defer response.Body.Close()
			if response.StatusCode < 200 || response.StatusCode >= 300 {
				err = decodeHTTPError(response)
				return
			}
			payload, readErr := readLimited(response.Body, maxResponseBytes)
			if readErr != nil {
				err = malformed("read Anthropic model catalog response", readErr)
				return
			}
			if decodeErr := json.Unmarshal(payload, &catalog); decodeErr != nil {
				err = malformed("decode Anthropic model catalog response", decodeErr)
			}
		}()
		if err != nil {
			return nil, err
		}
		now := time.Now().UTC()
		for _, item := range catalog.Data {
			id := strings.TrimSpace(item.ID)
			if id == "" || len(id) > 512 {
				continue
			}
			targets = append(targets, domain.InvocationTargetDescriptor{
				TargetID: id, TargetKind: domain.TargetModelID, DisplayName: firstNonEmpty(strings.TrimSpace(item.DisplayName), id),
				CanonicalModelRef: id, Lifecycle: domain.TargetLifecycleActive, Availability: domain.AvailabilityAvailable,
				MetadataSource: domain.MetadataSourceProvider, FetchedAt: now,
				Metadata: domain.NormalizedModelMetadata{
					SupportedOperations: allowlistedAnthropicCapabilities(item.Capabilities),
					MaxContextTokens:    item.MaxInputTokens, MaxOutputTokens: item.MaxOutputTokens,
				},
			})
		}
		if !catalog.HasMore {
			return targets, nil
		}
		afterID = strings.TrimSpace(catalog.LastID)
		if afterID == "" {
			return nil, malformed("Anthropic model catalog omitted pagination cursor", nil)
		}
	}
	return nil, malformed("Anthropic model catalog exceeded page limit", nil)
}

func (adapter *Adapter) DescribeInvocationTarget(ctx context.Context, target domain.InvocationTargetDescriptor) (domain.InvocationTargetDescriptor, error) {
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

// MapCapabilityClaims turns the Models API's capability report into Halro's
// vocabulary. Two things about that report shape this:
//
// Chat and streaming have no flag, because the Models API describes the models
// of the Messages API — a target it enumerates is a Messages target by
// construction, and this profile only enumerates when it is talking to
// Anthropic's own surface. They are claimed from that fact rather than from an
// absent field, and they have to be: dependencyClosure drops vision, json_mode
// and reasoning from any target that does not also claim chat.
//
// Tool use has no flag either, and unlike chat it is not implied by the
// endpoint, so nothing is claimed for it here — a capability the upstream never
// asserted is left to the model catalog rather than assumed.
func (adapter *Adapter) MapCapabilityClaims(target domain.InvocationTargetDescriptor, scope domain.InvocationTargetScopeKey, observedAt time.Time) []domain.CapabilityClaim {
	mapping := map[string]string{
		"image_input": "vision", "thinking": "reasoning", "structured_outputs": "json_mode", "batch": "batches",
	}
	claim := func(capabilityID, evidenceKey string) domain.CapabilityClaim {
		return domain.CapabilityClaim{
			CapabilityID: capabilityID, Status: domain.ClaimSupported, Evidence: domain.EvidenceDeclared,
			Source: domain.ClaimSourceProviderMetadata, Scope: scope, ObservedAt: observedAt,
			Revision: provider.CapabilityClaimRevision(string(domain.ClaimSourceProviderMetadata), target.TargetID, evidenceKey),
		}
	}
	claims := []domain.CapabilityClaim{claim("chat", "messages"), claim("streaming", "messages")}
	for _, capability := range target.Metadata.SupportedOperations {
		if capabilityID, ok := mapping[capability]; ok {
			claims = append(claims, claim(capabilityID, capability))
		}
	}
	return claims
}

// allowlistedAnthropicCapabilities keeps the capability keys the Models API
// documents, in the provider's own vocabulary. Not all of them map onto a Halro
// capability — pdf_input, citations, code_execution, context_management and
// effort have no counterpart — but they are the upstream's answer about this
// model and belong in its metadata.
func allowlistedAnthropicCapabilities(input map[string]json.RawMessage) []string {
	keys := []string{
		"batch", "citations", "code_execution", "context_management", "effort",
		"image_input", "pdf_input", "structured_outputs", "thinking",
	}
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		if capabilityFlag(input[key]) {
			result = append(result, key)
		}
	}
	return result
}

// capabilityFlag reads the `{"supported": bool}` object every capability member
// carries — including the ones that nest further members beside it, such as
// thinking.types and effort.high.
func capabilityFlag(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var wrapped struct {
		Supported bool `json:"supported"`
	}
	return json.Unmarshal(raw, &wrapped) == nil && wrapped.Supported
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// Probe answers whether this credential reaches this upstream. A provider whose
// binding has an enabled Deployment is tested through the smallest real Messages
// request; one tested before any Deployment exists has no model to name, and
// reads the Models API instead — the same fallback the OpenAI and Gemini
// adapters use. Sending Messages with an empty model would be refused by Halro's
// own request validation, before a single byte reached the network, and the
// console would report a local refusal as an upstream one.
func (adapter *Adapter) Probe(ctx context.Context, model string) error {
	if strings.TrimSpace(model) == "" {
		return adapter.probeModelCatalog(ctx)
	}
	request := anthropicapi.MessageRequest{Model: model, MaxTokens: 1, Messages: []anthropicapi.MessageParam{{Role: "user", Content: anthropicapi.ContentBlocks{{Type: "text", Text: "ping"}}}}}
	payload, _ := json.Marshal(request)
	_, err := adapter.MessagesNative(ctx, provider.NativeMessageCall{RequestID: "probe", ProviderModel: model, Version: anthropicapi.SupportedVersion, Payload: payload})
	return err
}

// probeModelCatalog reads one page of one model. It answers the same questions a
// Messages probe answers about reachability, TLS, and the credential, and asks
// the upstream for as little as the endpoint allows.
func (adapter *Adapter) probeModelCatalog(ctx context.Context) error {
	if !adapter.InvocationTargetDiscovery().CanEnumerate {
		return &provider.Error{Class: provider.ErrorBadRequest, Message: "this profile has no model catalog to test against; bind an enabled deployment and test that"}
	}
	request, err := adapter.newModelCatalogRequest(ctx, adapter.modelCatalogURL(1, ""))
	if err != nil {
		return err
	}
	response, err := adapter.client.Do(request)
	if err != nil {
		return transportError(err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return decodeHTTPError(response)
	}
	payload, err := readLimited(response.Body, maxResponseBytes)
	if err != nil {
		return malformed("read Anthropic model catalog response", err)
	}
	// A 200 carrying something other than a model list means the endpoint is
	// answering, but not as the Models API — a proxy login page reached over
	// HTTPS should not read as a healthy provider.
	var catalog struct {
		Data []anthropicModelDescriptor `json:"data"`
	}
	if err := json.Unmarshal(payload, &catalog); err != nil {
		return malformed("decode Anthropic model catalog response", err)
	}
	// An empty list is a valid answer — an account can be entitled to nothing —
	// but an absent one means this was not the Models API's reply.
	if catalog.Data == nil {
		return malformed("Anthropic model catalog response omitted its model list", nil)
	}
	return nil
}

func (adapter *Adapter) Chat(ctx context.Context, call provider.ChatCall) (openaiapi.ChatCompletionResponse, error) {
	canonical, err := openaiwire.DecodeGenerate(call.Request)
	if err != nil {
		return openaiapi.ChatCompletionResponse{}, badRequest("decode portable request", err)
	}
	request, err := anthropicwire.RenderPortableRequest(canonical, call.ProviderModel)
	if err != nil {
		return openaiapi.ChatCompletionResponse{}, badRequest("render Anthropic request", err)
	}
	request.Stream = false
	payload, err := json.Marshal(request)
	if err != nil {
		return openaiapi.ChatCompletionResponse{}, badRequest("encode Anthropic request", err)
	}
	result, err := adapter.MessagesNative(ctx, provider.NativeMessageCall{RequestID: call.RequestID, ProviderModel: call.ProviderModel, Version: anthropicapi.SupportedVersion, Payload: payload})
	if err != nil {
		return openaiapi.ChatCompletionResponse{}, err
	}
	message, err := anthropicapi.DecodeMessage(result.Payload)
	if err != nil {
		return openaiapi.ChatCompletionResponse{}, malformed("decode Anthropic response", err)
	}
	semanticResult, err := anthropicwire.DecodeResult(message)
	if err != nil {
		return openaiapi.ChatCompletionResponse{}, malformed("normalize Anthropic response", err)
	}
	semanticResult.ProviderRequestID = result.ProviderRequestID
	return openaiwire.RenderGenerateResult(semanticResult)
}

func (adapter *Adapter) ChatStream(ctx context.Context, call provider.ChatCall, emit func(semantic.Event) error) (*openaiapi.Usage, error) {
	canonical, err := openaiwire.DecodeGenerate(call.Request)
	if err != nil {
		return nil, badRequest("decode portable stream request", err)
	}
	request, err := anthropicwire.RenderPortableRequest(canonical, call.ProviderModel)
	if err != nil {
		return nil, badRequest("render Anthropic stream request", err)
	}
	request.Stream = true
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, badRequest("encode Anthropic stream request", err)
	}
	bridge := newPortableStreamBridge(emit)
	usage, err := adapter.MessagesNativeStream(ctx, provider.NativeMessageCall{RequestID: call.RequestID, ProviderModel: call.ProviderModel, Version: anthropicapi.SupportedVersion, Payload: payload}, bridge.Accept)
	if err == nil {
		err = bridge.Finalize()
	}
	if usage == nil {
		usage = bridge.usage
	}
	if usage == nil {
		return nil, err
	}
	return portableUsage(*usage), err
}

// portableUsage translates Anthropic's cache-exclusive token reporting into the
// cache-inclusive subset convention the rest of the pipeline prices against.
func portableUsage(usage anthropicapi.Usage) *openaiapi.Usage {
	prompt := usage.PromptTokens()
	portable := &openaiapi.Usage{
		PromptTokens:     prompt,
		CompletionTokens: usage.OutputTokens,
		TotalTokens:      prompt + usage.OutputTokens,
		CacheWriteTokens: usage.CacheCreationInputTokens,
	}
	portable.SetCachedPromptTokens(usage.CacheReadInputTokens)
	portable.SetReasoningTokens(usage.ThinkingTokens)
	return portable
}

// semanticUsage applies the same translation as portableUsage for the streaming
// bridge, which emits semantic events directly instead of an OpenAI-shaped body.
func semanticUsage(usage anthropicapi.Usage) *semantic.Usage {
	prompt := usage.PromptTokens()
	return &semantic.Usage{
		InputTokens:           prompt,
		CachedInputTokens:     usage.CacheReadInputTokens,
		CacheWriteInputTokens: usage.CacheCreationInputTokens,
		OutputTokens:          usage.OutputTokens,
		ReasoningTokens:       usage.ThinkingTokens,
		TotalTokens:           prompt + usage.OutputTokens,
		Source:                semantic.UsageProviderReported,
	}
}

func (adapter *Adapter) Embed(context.Context, provider.EmbeddingCall) (openaiapi.EmbeddingResponse, error) {
	return openaiapi.EmbeddingResponse{}, &provider.Error{Class: provider.ErrorBadRequest, Message: "Anthropic profile does not support embeddings"}
}

func (adapter *Adapter) MessagesNative(ctx context.Context, call provider.NativeMessageCall) (provider.NativeMessageResult, error) {
	payload, err := preparePayload(call, false)
	if err != nil {
		return provider.NativeMessageResult{}, badRequest("prepare Anthropic request", err)
	}
	request, err := adapter.request(ctx, call, payload, false)
	if err != nil {
		return provider.NativeMessageResult{}, err
	}
	response, err := adapter.client.Do(request)
	if err != nil {
		return provider.NativeMessageResult{}, transportError(err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return provider.NativeMessageResult{}, decodeHTTPError(response)
	}
	body, err := readLimited(response.Body, maxResponseBytes)
	if err != nil {
		return provider.NativeMessageResult{}, malformed("read Anthropic response", err)
	}
	if _, err := anthropicapi.DecodeMessage(body); err != nil {
		return provider.NativeMessageResult{}, malformed("validate Anthropic response", err)
	}
	return provider.NativeMessageResult{Payload: body, ProviderRequestID: upstreamRequestID(response.Header), RetryAfter: parseRetryAfter(response.Header)}, nil
}

func (adapter *Adapter) MessagesNativeStream(ctx context.Context, call provider.NativeMessageCall, emit func(anthropicapi.RawStreamEvent) error) (*anthropicapi.Usage, error) {
	if emit == nil {
		return nil, badRequest("Anthropic stream callback is required", nil)
	}
	payload, err := preparePayload(call, true)
	if err != nil {
		return nil, badRequest("prepare Anthropic stream request", err)
	}
	request, err := adapter.request(ctx, call, payload, true)
	if err != nil {
		return nil, err
	}
	response, err := adapter.client.Do(request)
	if err != nil {
		return nil, transportError(err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, decodeHTTPError(response)
	}
	validator := anthropicapi.NewStreamValidator()
	decoder := sse.NewDecoder(response.Body, semantic.MaxEncodedEventBytes)
	var usage *anthropicapi.Usage
	emitted := false
	for {
		event, nextErr := decoder.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return usage, &provider.Error{Class: provider.ErrorMalformed, Ambiguous: emitted, Message: "read Anthropic stream", Cause: nextErr}
		}
		raw := anthropicapi.RawStreamEvent{Type: event.Event, Data: append(json.RawMessage(nil), event.Data...)}
		if err := validator.Accept(raw); err != nil {
			return usage, &provider.Error{Class: provider.ErrorMalformed, Ambiguous: emitted, Message: "validate Anthropic stream event " + raw.Type, Cause: err}
		}
		usage = updateUsage(usage, raw)
		if err := emit(raw); err != nil {
			return usage, err
		}
		emitted = true
		if raw.Type == "error" {
			return usage, &provider.Error{Class: provider.ErrorProvider5xx, Retryable: false, Ambiguous: true, ProviderCode: "stream_error_event", Message: "Anthropic stream returned an error event"}
		}
	}
	if err := validator.Finalize(); err != nil {
		return usage, &provider.Error{Class: provider.ErrorMalformed, Ambiguous: emitted, Message: "Anthropic stream ended early", Cause: err}
	}
	return usage, nil
}

// CountTokensNative asks the upstream how many prompt tokens a Messages request
// would consume. Anthropic does not bill it, but it is still a real provider
// call on the operator's credential, so it runs through the same authorization,
// transport and accounting path as a generation — only its settlement is zero.
func (adapter *Adapter) CountTokensNative(ctx context.Context, call provider.NativeMessageCall) (provider.NativeMessageResult, error) {
	payload, err := prepareCountTokensPayload(call)
	if err != nil {
		return provider.NativeMessageResult{}, badRequest("prepare Anthropic count_tokens request", err)
	}
	request, err := adapter.requestTo(ctx, call, payload, false, "count_tokens")
	if err != nil {
		return provider.NativeMessageResult{}, err
	}
	response, err := adapter.client.Do(request)
	if err != nil {
		return provider.NativeMessageResult{}, transportError(err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return provider.NativeMessageResult{}, decodeHTTPError(response)
	}
	body, err := readLimited(response.Body, maxResponseBytes)
	if err != nil {
		return provider.NativeMessageResult{}, malformed("read Anthropic count_tokens response", err)
	}
	if _, err := anthropicapi.DecodeTokenCount(body); err != nil {
		return provider.NativeMessageResult{}, malformed("validate Anthropic count_tokens response", err)
	}
	return provider.NativeMessageResult{Payload: body, ProviderRequestID: upstreamRequestID(response.Header), RetryAfter: parseRetryAfter(response.Header)}, nil
}

func (adapter *Adapter) request(ctx context.Context, call provider.NativeMessageCall, payload []byte, stream bool) (*http.Request, error) {
	return adapter.requestTo(ctx, call, payload, stream, "")
}

func (adapter *Adapter) requestTo(ctx context.Context, call provider.NativeMessageCall, payload []byte, stream bool, suffix string) (*http.Request, error) {
	endpoint := *adapter.endpoint
	endpoint.Path = strings.TrimRight(endpoint.Path, "/")
	if adapter.messagesPath != "" {
		endpoint.Path += "/" + strings.Trim(adapter.messagesPath, "/")
	} else {
		if !strings.HasSuffix(endpoint.Path, "/v1") {
			endpoint.Path += "/v1"
		}
		endpoint.Path += "/messages"
	}
	if suffix != "" {
		endpoint.Path += "/" + suffix
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return nil, badRequest("create Anthropic request", err)
	}
	// Protocol headers first, credential last: a signing credential scheme has
	// to see every header it covers, and the project header is chosen here
	// rather than by the authorizer because addressing is not authentication.
	request.Header.Set("anthropic-version", call.Version)
	if len(call.Betas) > 0 {
		// Tokens are validated against the connection's allowlist upstream of
		// here, and the stored charset excludes commas and whitespace, so joining
		// cannot smuggle an unaccepted token into the header.
		request.Header.Set(anthropicapi.BetaHeader, strings.Join(call.Betas, ","))
	}
	request.Header.Set("content-type", "application/json")
	if stream {
		request.Header.Set("accept", "text/event-stream")
	} else {
		request.Header.Set("accept", "application/json")
	}
	request.Header.Set("x-request-id", call.RequestID)
	provider.ApplyBedrockProject(request, provider.HeaderBedrockAnthropicWorkspace, adapter.bedrockProjectID)
	if err := adapter.authorizer.Authorize(request, nil); err != nil {
		return nil, &provider.Error{Class: provider.ErrorAuthentication, Message: "authorize Anthropic request", Cause: err}
	}
	return request, nil
}

// preparePayload rewrites only the two fields Halro owns — the upstream model
// identifier and the stream flag — directly on the caller's bytes. Decoding into
// MessageRequest and re-marshalling would silently drop every field the struct
// does not model, which is the difference between a native mode that pins a wire
// profile and one that quietly re-authors the request on the way out. Values are
// carried as RawMessage so only the top-level key order changes; nothing nested
// is re-rendered.
func preparePayload(call provider.NativeMessageCall, stream bool) ([]byte, error) {
	if _, err := anthropicapi.DecodeMessageRequest(bytes.NewReader(call.Payload)); err != nil {
		return nil, err
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(call.Payload, &root); err != nil {
		return nil, err
	}
	model, err := json.Marshal(call.ProviderModel)
	if err != nil {
		return nil, err
	}
	streamFlag, err := json.Marshal(stream)
	if err != nil {
		return nil, err
	}
	root["model"], root["stream"] = model, streamFlag
	return json.Marshal(root)
}

// prepareCountTokensPayload rewrites the upstream model and nothing else.
// count_tokens has no stream flag to own, and every other member is the
// caller's — including max_tokens, which Anthropic decides about rather than
// Halro silently stripping.
func prepareCountTokensPayload(call provider.NativeMessageCall) ([]byte, error) {
	if _, err := anthropicapi.DecodeMessageRequest(bytes.NewReader(call.Payload)); err != nil {
		return nil, err
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(call.Payload, &root); err != nil {
		return nil, err
	}
	model, err := json.Marshal(call.ProviderModel)
	if err != nil {
		return nil, err
	}
	root["model"] = model
	return json.Marshal(root)
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

func decodeHTTPError(response *http.Response) error {
	payload, _ := readLimited(response.Body, 1<<20)
	var envelope anthropicapi.ErrorResponse
	_ = json.Unmarshal(payload, &envelope)
	class, retryable := provider.ErrorBadRequest, false
	switch response.StatusCode {
	case 401, 403:
		class = provider.ErrorAuthentication
	case 429:
		class, retryable = provider.ErrorRateLimit, true
	case 500, 502, 503, 504, 529:
		class, retryable = provider.ErrorProvider5xx, true
	}
	message := envelope.Error.Message
	if message == "" {
		message = http.StatusText(response.StatusCode)
	}
	return &provider.Error{Class: class, StatusCode: response.StatusCode, Retryable: retryable, Message: message, ProviderRequestID: upstreamRequestID(response.Header), ProviderCode: envelope.Error.Type, RetryAfter: parseRetryAfter(response.Header)}
}

func upstreamRequestID(header http.Header) string {
	for _, name := range []string{"request-id", "x-amzn-requestid", "x-amzn-request-id", "x-request-id"} {
		if value := strings.TrimSpace(header.Get(name)); value != "" {
			return value
		}
	}
	return ""
}

func parseRetryAfter(header http.Header) time.Duration {
	value := strings.TrimSpace(header.Get("retry-after"))
	if value == "" {
		return 0
	}
	seconds, err := strconv.ParseInt(value, 10, 64)
	if err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		return max(0, time.Until(at))
	}
	return 0
}

func transportError(err error) error {
	class := provider.TransportClass(err)
	return &provider.Error{Class: class, Retryable: class != provider.ErrorCanceled, Ambiguous: !provider.Unsent(err), Message: "Anthropic request failed", Cause: err}
}
func badRequest(message string, cause error) error {
	return &provider.Error{Class: provider.ErrorBadRequest, Message: message, Cause: cause}
}
func malformed(message string, cause error) error {
	return &provider.Error{Class: provider.ErrorMalformed, Ambiguous: true, Message: message, Cause: cause}
}

func updateUsage(current *anthropicapi.Usage, event anthropicapi.RawStreamEvent) *anthropicapi.Usage {
	if current == nil {
		current = &anthropicapi.Usage{}
	}
	if event.Type == "message_start" {
		var value struct {
			Message struct {
				Usage anthropicapi.Usage `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(event.Data, &value) == nil {
			*current = value.Message.Usage
		}
	}
	if event.Type == "message_delta" {
		var value struct {
			Usage anthropicapi.Usage `json:"usage"`
		}
		if json.Unmarshal(event.Data, &value) == nil {
			if value.Usage.InputTokens != 0 {
				current.InputTokens = value.Usage.InputTokens
			}
			// message_start carries the cache tiers today and the full-struct copy
			// above captures them, but a delta that restates them must win rather
			// than be dropped — losing either tier here silently under-prices the
			// whole stream.
			if value.Usage.CacheReadInputTokens != 0 {
				current.CacheReadInputTokens = value.Usage.CacheReadInputTokens
			}
			if value.Usage.CacheCreationInputTokens != 0 {
				current.CacheCreationInputTokens = value.Usage.CacheCreationInputTokens
			}
			current.OutputTokens = value.Usage.OutputTokens
			current.ThinkingTokens = value.Usage.ThinkingTokens
		}
	}
	return current
}

type portableStreamBridge struct {
	emit       func(semantic.Event) error
	id, model  string
	created    int64
	blocks     map[int]portableBlock
	usage      *anthropicapi.Usage
	terminated bool
}

type portableBlock struct {
	kind, callID, name string
	toolIndex          int
}

func newPortableStreamBridge(emit func(semantic.Event) error) *portableStreamBridge {
	return &portableStreamBridge{emit: emit, blocks: map[int]portableBlock{}}
}

func (bridge *portableStreamBridge) Accept(event anthropicapi.RawStreamEvent) error {
	bridge.usage = updateUsage(bridge.usage, event)
	switch event.Type {
	case "message_start":
		var value struct {
			Message anthropicapi.Message `json:"message"`
		}
		if err := json.Unmarshal(event.Data, &value); err != nil {
			return err
		}
		bridge.id, bridge.model = value.Message.ID, value.Message.Model
	case "content_block_start":
		var value struct {
			Index        int                       `json:"index"`
			ContentBlock anthropicapi.ContentBlock `json:"content_block"`
		}
		if err := json.Unmarshal(event.Data, &value); err != nil {
			return err
		}
		if value.ContentBlock.Type == "thinking" || value.ContentBlock.Type == "redacted_thinking" {
			return errors.New("signed thinking requires native mode")
		}
		block := portableBlock{kind: value.ContentBlock.Type, callID: value.ContentBlock.ID, name: value.ContentBlock.Name, toolIndex: value.Index}
		bridge.blocks[value.Index] = block
		if block.kind == "tool_use" {
			index := block.toolIndex
			return bridge.emitEvent(semantic.OutputDelta{Index: 0, Role: semantic.RoleAssistant, Content: []semantic.ContentDelta{{Kind: semantic.ContentToolCall, ToolIndex: &index, CallID: block.callID, Name: block.name}}})
		}
	case "content_block_delta":
		var value struct {
			Index int `json:"index"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if err := json.Unmarshal(event.Data, &value); err != nil {
			return err
		}
		block := bridge.blocks[value.Index]
		if value.Delta.Type == "text_delta" {
			return bridge.emitEvent(semantic.OutputDelta{Index: 0, Role: semantic.RoleAssistant, Content: []semantic.ContentDelta{{Kind: semantic.ContentText, Text: value.Delta.Text}}})
		}
		if value.Delta.Type == "input_json_delta" {
			index := block.toolIndex
			return bridge.emitEvent(semantic.OutputDelta{Index: 0, Content: []semantic.ContentDelta{{Kind: semantic.ContentToolCall, ToolIndex: &index, ArgumentsFragment: value.Delta.PartialJSON}}})
		}
	case "message_delta":
		var value struct {
			Delta struct {
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
		}
		if err := json.Unmarshal(event.Data, &value); err != nil {
			return err
		}
		bridge.terminated = true
		if err := bridge.emitEvent(semantic.OutputDelta{Index: 0, Termination: anthropicwire.DecodeStopReason(&value.Delta.StopReason), NativeTermination: value.Delta.StopReason}); err != nil {
			return err
		}
		if bridge.usage != nil {
			return bridge.emit(semantic.Event{Kind: semantic.EventUsage, ID: bridge.id, Model: bridge.model, Usage: semanticUsage(*bridge.usage), Translation: semantic.TranslationNone, MappingRevision: anthropicwire.MappingRevision})
		}
	}
	return nil
}

func (bridge *portableStreamBridge) emitEvent(output semantic.OutputDelta) error {
	return bridge.emit(semantic.Event{Kind: semantic.EventDelta, ID: bridge.id, Model: bridge.model, Outputs: []semantic.OutputDelta{output}, Translation: semantic.TranslationNone, MappingRevision: anthropicwire.MappingRevision})
}
func (bridge *portableStreamBridge) Finalize() error {
	if bridge.id == "" || !bridge.terminated {
		return errors.New("Anthropic portable stream did not terminate")
	}
	return nil
}
