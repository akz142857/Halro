package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/akz142857/Halro/internal/compatibility"
	openaiwire "github.com/akz142857/Halro/internal/compatibility/openai"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/semantic"
	"github.com/akz142857/Halro/internal/sse"
)

const maxResponseBytes = 16 << 20

type Adapter struct {
	endpoint     *url.URL
	authorizer   provider.Authorizer
	client       *http.Client
	providerType string
	apiVersion   string
	azure        bool
	deepSeek     bool
	// miniMax turns on two things this adapter does for no other upstream: the
	// dialect body in encodeChatRequest, and the base_resp guard that catches a
	// failure MiniMax reports inside a 200.
	miniMax             bool
	capabilities        provider.Capabilities
	bedrockProjectID    string
	operationPathPrefix string
	// responses says this adapter was built for the Responses profile. It is a
	// property of the profile the connection is bound to, not something inferred
	// from a request, so a deployment cannot address a surface its operator did
	// not choose.
	responses bool
}

func New(endpoint *url.URL, apiKey []byte, client *http.Client) (*Adapter, error) {
	return NewWithOptions(Options{
		Endpoint: endpoint, APIKey: apiKey, Client: client, ProviderType: "openai",
		Capabilities: provider.Capabilities{
			Chat: true, Streaming: true, Embeddings: true, Tools: true,
			Vision: true, JSONObject: true, StructuredOutputs: true, StreamUsage: true,
		},
	})
}

type Options struct {
	Endpoint         *url.URL
	APIKey           []byte
	Client           *http.Client
	ProviderType     string
	APIVersion       string
	Azure            bool
	Capabilities     provider.Capabilities
	Authorizer       provider.Authorizer
	CredentialScheme domain.CredentialScheme
	// Responses builds an adapter for the OpenAI Responses profile: the same
	// account and credential, a different endpoint, and a request that stays
	// semantic all the way to the wire.
	Responses bool
	// BedrockProjectID is set only for Bedrock Mantle providers, and only when
	// they address a project other than the account default.
	BedrockProjectID string
	// OperationPathPrefix addresses an upstream whose inference operations do
	// not sit directly under the base URL's own path. It exists for Bedrock
	// Mantle, which serves its models from two routes on one host — the default
	// /v1 and a second /openai/v1 — with each model on exactly one of them and
	// no way to tell which from the model list. The route therefore has to be
	// fixed by the profile, and one of the two cannot be reached by the default
	// join.
	//
	// Empty for every other provider, and empty means the previous behaviour
	// exactly: OpenAI, Azure, DeepSeek and compatibility servers keep joining
	// operations onto their configured base URL untouched.
	OperationPathPrefix string
}

func NewWithOptions(options Options) (*Adapter, error) {
	endpoint, apiKey, client := options.Endpoint, options.APIKey, options.Client
	if endpoint == nil || client == nil {
		return nil, errors.New("endpoint and client are required")
	}
	if len(apiKey) == 0 && options.Authorizer == nil {
		return nil, errors.New("api key is required")
	}
	if options.ProviderType == "" {
		options.ProviderType = "openai_compatible"
	}
	if options.Azure && strings.TrimSpace(options.APIVersion) == "" {
		return nil, errors.New("azure api version is required")
	}
	expectedScheme := options.CredentialScheme
	if expectedScheme == "" {
		expectedScheme = domain.CredentialBearerStatic
		if options.Azure {
			expectedScheme = domain.CredentialAzureAPIKey
		}
	}
	authorizer := options.Authorizer
	if authorizer == nil {
		var err error
		if options.Azure {
			authorizer, err = provider.NewStaticHeaderAuthorizer(expectedScheme, "api-key", "", apiKey, "Authorization")
		} else {
			authorizer, err = provider.NewStaticHeaderAuthorizer(expectedScheme, "Authorization", "Bearer ", apiKey, "api-key", "x-api-key")
		}
		if err != nil {
			return nil, err
		}
	}
	if authorizer.Scheme() != expectedScheme {
		authorizer.Close()
		return nil, errors.New("credential scheme does not match OpenAI adapter profile")
	}
	return &Adapter{
		endpoint: endpoint, authorizer: authorizer, client: client,
		providerType: options.ProviderType, apiVersion: options.APIVersion,
		azure: options.Azure, deepSeek: options.ProviderType == string(domain.ProviderDeepSeek),
		miniMax:             options.ProviderType == string(domain.ProviderMiniMax),
		capabilities:        options.Capabilities,
		bedrockProjectID:    options.BedrockProjectID,
		operationPathPrefix: strings.Trim(options.OperationPathPrefix, "/"),
		responses:           options.Responses,
	}, nil
}

// encodeChatRequest turns the OpenAI-shaped call into the bytes this upstream
// accepts.
//
// Only DeepSeek needs a second shape. It reuses this adapter because it speaks
// the same wire format, but not the same member list: it has no n, seed,
// parallel_tool_calls, max_completion_tokens or top-level reasoning_effort, it
// reaches reasoning through `thinking`, and it names the end-user reference
// user_id. Marshalling the OpenAI struct straight out sent five members it
// ignores and one under a name it does not read.
func (a *Adapter) encodeChatRequest(request openaiapi.ChatCompletionRequest) ([]byte, error) {
	switch {
	case a.deepSeek:
		body, err := compatibility.RenderDeepSeekChatRequest(request)
		if err != nil {
			return nil, err
		}
		return json.Marshal(body)
	case a.miniMax:
		// MiniMax's member list is smaller than OpenAI's in the same direction
		// DeepSeek's is, and worse in one respect: it documents its unsupported
		// parameters as ignored rather than refused, so a member sent here comes
		// back 200 having done nothing. The one that costs money is the top-level
		// reasoning_effort, which MiniMax does not read — its switch is `thinking`
		// — so a request asking not to think would think, and bill, in silence.
		body, err := compatibility.RenderMiniMaxChatRequest(request)
		if err != nil {
			return nil, err
		}
		return json.Marshal(body)
	default:
		return json.Marshal(request)
	}
}

func (a *Adapter) Type() string {
	return a.providerType
}

func (a *Adapter) Capabilities() provider.Capabilities {
	return a.capabilities
}

func (a *Adapter) modelCatalogURL() (url.URL, error) {
	if a.azure {
		return url.URL{}, &provider.Error{Class: provider.ErrorBadRequest, Message: "azure openai does not expose a universal model catalog"}
	}
	endpoint := *a.endpoint
	basePath := strings.TrimRight(endpoint.Path, "/")
	// Discovery ignores the operation route prefix on purpose, because on the
	// one upstream that pins a prefix the catalogue is not route-scoped.
	// Measured against a real Bedrock Mantle account on 2026-08-25, us-east-2:
	//
	//	GET /v1/models                        200
	//	GET /openai/v1/models                 404
	//	GET /v1/models/openai.gpt-5.5         200
	//	GET /openai/v1/models/openai.gpt-5.5  404
	//
	// Addressing the catalogue at the operation route was meant to stop
	// discovery and the calls it discovers for from disagreeing about the URL.
	// The route they disagree about has no catalogue at all, so that reading
	// 404'd every model list and every connection test on the two /openai/v1
	// Mantle profiles — which is how it was found.
	//
	// The disagreement cannot be removed here, because it belongs to the
	// service: Mantle lists the account's models once and serves each of them
	// from exactly one route, and the list does not say which. So a profile
	// pinned to /openai/v1 enumerates models that route will refuse. What keeps
	// that from being silent is the upstream itself — a model addressed on the
	// wrong route is refused with "isn't supported on this route", never served
	// by a fallback — and a connection test passing here means the model exists
	// on the account, not that this route serves it.
	endpoint.Path = versionedPath(basePath, "models")
	return endpoint, nil
}

func (a *Adapter) InvocationTargetDiscovery() domain.InvocationTargetDiscoveryCapabilities {
	if a.azure {
		return domain.InvocationTargetDiscoveryCapabilities{
			TargetKinds: []domain.DeploymentTargetKind{domain.TargetAzureDeployment},
			CanVerify:   true, RequiresManagementIdentity: true, RequiresCanonicalModelMapping: true,
		}
	}
	kind := domain.TargetModelID
	if a.providerType == string(domain.ProviderOpenAICompatible) {
		kind = domain.TargetCustomEndpointModel
	}
	return domain.InvocationTargetDiscoveryCapabilities{
		TargetKinds: []domain.DeploymentTargetKind{kind}, CanEnumerate: true, CanDescribe: true, CanVerify: true,
	}
}

func (a *Adapter) ListInvocationTargets(ctx context.Context, query domain.TargetQuery) ([]domain.InvocationTargetDescriptor, error) {
	endpoint, err := a.modelCatalogURL()
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, &provider.Error{Class: provider.ErrorBadRequest, Message: "create model catalog request", Cause: err}
	}
	if err := a.prepareRequest(request); err != nil {
		return nil, &provider.Error{Class: provider.ErrorAuthentication, Message: "authorize model catalog request", Cause: err}
	}
	request.Header.Set("Accept", "application/json")
	response, err := a.client.Do(request)
	if err != nil {
		class := provider.TransportClass(err)
		return nil, &provider.Error{Class: class, Retryable: class != provider.ErrorCanceled, Message: "model catalog request failed", Cause: err}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, classifyHTTPError(response.StatusCode, limitedErrorMessage(response.Body))
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return nil, &provider.Error{Class: provider.ErrorMalformed, Message: "read model catalog response", Cause: err}
	}
	if len(payload) > maxResponseBytes {
		return nil, &provider.Error{Class: provider.ErrorMalformed, Message: "model catalog response is too large"}
	}
	catalog, _, err := provider.DecodeOpenAIShapedModelCatalog(payload)
	if err != nil {
		return nil, &provider.Error{Class: provider.ErrorMalformed, Message: "decode model catalog response", Cause: err}
	}
	kind := domain.TargetModelID
	canonicalModelRef := func(id string) string { return id }
	if a.providerType == string(domain.ProviderOpenAICompatible) {
		kind = domain.TargetCustomEndpointModel
		// A compatible endpoint owns the meaning of its model names. Seeing a
		// familiar identifier proves availability only; it does not prove that an
		// alias such as "gpt-5" implements OpenAI's model contract. Canonical
		// capability mapping therefore remains an explicit operator action.
		canonicalModelRef = func(string) string { return "" }
	}
	models := catalog.InvocationTargets(kind, canonicalModelRef)
	now := time.Now().UTC()
	for index := range models {
		models[index].FetchedAt = now
	}
	return models, nil
}

func (a *Adapter) DescribeInvocationTarget(ctx context.Context, target domain.InvocationTargetDescriptor) (domain.InvocationTargetDescriptor, error) {
	items, err := a.ListInvocationTargets(ctx, domain.TargetQuery{TargetKind: target.TargetKind})
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

// ProbeRequiresModel is true only for Azure, whose probe is a request to one
// deployment route; the plain OpenAI surface probes the model catalogue and
// needs no deployment to exist yet.
func (a *Adapter) ProbeRequiresModel() bool { return a.azure }

func (a *Adapter) Probe(ctx context.Context, providerModel string) error {
	var endpoint url.URL
	if a.azure {
		if !validAzureDeployment(providerModel) {
			return &provider.Error{Class: provider.ErrorBadRequest, Message: "azure connection test requires a deployment route"}
		}
		endpoint = a.operationURL(providerModel, "chat/completions")
	} else {
		var endpointErr error
		endpoint, endpointErr = a.modelCatalogURL()
		if endpointErr != nil {
			return endpointErr
		}
		if model := strings.TrimSpace(providerModel); model != "" {
			basePath := strings.TrimRight(endpoint.Path, "/")
			baseEscapedPath := strings.TrimRight(endpoint.EscapedPath(), "/")
			endpoint.Path = basePath + "/" + model
			endpoint.RawPath = baseEscapedPath + "/" + url.PathEscape(model)
		}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return &provider.Error{Class: provider.ErrorBadRequest, Message: "create provider probe", Cause: err}
	}
	if err := a.prepareRequest(request); err != nil {
		return &provider.Error{Class: provider.ErrorAuthentication, Message: "authorize provider probe", Cause: err}
	}
	request.Header.Set("Accept", "application/json")
	response, err := a.client.Do(request)
	if err != nil {
		class := provider.TransportClass(err)
		return &provider.Error{Class: class, Retryable: class != provider.ErrorCanceled, Message: "provider probe failed", Cause: err}
	}
	defer response.Body.Close()
	// Azure has no universal, non-billable data-plane discovery endpoint. A
	// deployment URL returning Method Not Allowed proves DNS/TLS/routing and
	// authentication without generating tokens.
	if a.azure && response.StatusCode == http.StatusMethodNotAllowed {
		return nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return classifyHTTPError(response.StatusCode, limitedErrorMessage(response.Body))
	}
	_, err = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return &provider.Error{Class: provider.ErrorMalformed, Message: "read provider probe response", Cause: err}
	}
	return nil
}

func (a *Adapter) Close() {
	a.authorizer.Close()
	a.client.CloseIdleConnections()
}

func (a *Adapter) Chat(ctx context.Context, call provider.ChatCall) (openaiapi.ChatCompletionResponse, error) {
	if a.responses {
		// The Responses profile addresses /v1/responses and nothing else. Chat is
		// still the shape capability detection and the connection test speak, so
		// it is served here by translating rather than by addressing the other
		// endpoint: a probe that reached /v1/chat/completions would record
		// verified evidence for a surface this profile never calls, and a key
		// scoped to only one of the two would pass detection and then fail the
		// first real request — after the budget was reserved, which is the exact
		// failure this profile exists to remove.
		return a.chatViaResponses(ctx, call)
	}
	if a.azure && !validAzureDeployment(call.ProviderModel) {
		return openaiapi.ChatCompletionResponse{}, &provider.Error{Class: provider.ErrorBadRequest, Message: "invalid azure deployment name"}
	}
	requestBody := call.Request
	if a.azure {
		requestBody.Model = ""
	} else {
		requestBody.Model = call.ProviderModel
	}
	requestBody.Stream = false
	encoded, err := a.encodeChatRequest(requestBody)
	if err != nil {
		return openaiapi.ChatCompletionResponse{}, &provider.Error{Class: provider.ErrorBadRequest, Message: "encode provider request", Cause: err}
	}
	payload, err := a.postJSON(ctx, call.ProviderModel, "chat/completions", call.RequestID, encoded)
	if err != nil {
		return openaiapi.ChatCompletionResponse{}, err
	}
	var result openaiapi.ChatCompletionResponse
	if err := json.Unmarshal(payload, &result); err != nil {
		return openaiapi.ChatCompletionResponse{}, &provider.Error{Class: provider.ErrorMalformed, Message: "decode provider response", Cause: err}
	}
	if result.ID == "" || result.Object == "" || len(result.Choices) == 0 {
		return openaiapi.ChatCompletionResponse{}, &provider.Error{Class: provider.ErrorMalformed, Message: "provider response is missing required fields"}
	}
	return result, nil
}

// chatViaResponses answers a Chat-shaped call on the Responses profile by
// translating both ways around the endpoint the profile actually addresses.
//
// It is the same round trip the Bedrock Mantle Responses adapter makes, and for
// the same reason: the callers that speak Chat here — capability detection, the
// connection test — are asking a question about this profile, and the answer is
// only true if it came from this profile's own surface.
func (a *Adapter) chatViaResponses(ctx context.Context, call provider.ChatCall) (openaiapi.ChatCompletionResponse, error) {
	canonical, err := openaiwire.DecodeGenerate(call.Request)
	if err != nil {
		return openaiapi.ChatCompletionResponse{}, &provider.Error{Class: provider.ErrorBadRequest, Message: "decode provider Chat request", Cause: err}
	}
	result, err := a.GenerateSemantic(ctx, provider.GenerateCall{
		RequestID: call.RequestID, ProviderModel: call.ProviderModel, Request: canonical,
	})
	if err != nil {
		return openaiapi.ChatCompletionResponse{}, err
	}
	chat, err := openaiwire.RenderGenerateResult(result)
	if err != nil {
		return openaiapi.ChatCompletionResponse{}, &provider.Error{Class: provider.ErrorMalformed, Ambiguous: true, Message: "render provider Chat result", Cause: err}
	}
	return chat, nil
}

// GenerateSemantic serves the Responses profile, which is the one OpenAI surface
// whose request cannot be written as a Chat Completions request without losing
// something — a provider-executed tool has no Chat member, and the answer comes
// back with citations Chat has nowhere to put.
//
// It is refused on every other OpenAI profile rather than quietly serving them
// too: the chat profile addresses /v1/chat/completions and a deployment on it
// asking for /v1/responses would be reaching an endpoint its operator never
// chose.
func (a *Adapter) GenerateSemantic(ctx context.Context, call provider.GenerateCall) (semantic.GenerateResult, error) {
	if !a.responses {
		return semantic.GenerateResult{}, &provider.Error{Class: provider.ErrorBadRequest, Message: "this OpenAI profile does not serve the Responses surface"}
	}
	request, err := openaiwire.RenderProviderResponseRequest(call.Request, call.ProviderModel)
	if err != nil {
		return semantic.GenerateResult{}, &provider.Error{Class: provider.ErrorBadRequest, Message: "render provider Responses request", Cause: err}
	}
	request.Stream = false
	encoded, err := json.Marshal(request)
	if err != nil {
		return semantic.GenerateResult{}, &provider.Error{Class: provider.ErrorBadRequest, Message: "encode provider Responses request", Cause: err}
	}
	payload, err := a.postJSON(ctx, call.ProviderModel, "responses", call.RequestID, encoded)
	if err != nil {
		return semantic.GenerateResult{}, err
	}
	var wire openaiapi.Response
	if err := json.Unmarshal(payload, &wire); err != nil {
		return semantic.GenerateResult{}, &provider.Error{Class: provider.ErrorMalformed, Message: "decode provider Responses result", Cause: err}
	}
	result, err := openaiwire.DecodeProviderResponse(wire)
	if err != nil {
		return semantic.GenerateResult{}, &provider.Error{Class: provider.ErrorMalformed, Ambiguous: true, Message: "normalize provider Responses result", Cause: err}
	}
	return result, nil
}

// postJSON is the one place an OpenAI-family unary call crosses the network: it
// applies the credential, bounds what it will read back, and classifies a
// refusal. Chat and Responses share it because the differences between them are
// in the bodies, and a second copy of the transport rules is a second place for
// a size bound or an error class to drift.
func (a *Adapter) postJSON(ctx context.Context, providerModel, operation, requestID string, encoded []byte) ([]byte, error) {
	endpoint := a.operationURL(providerModel, operation)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(encoded))
	if err != nil {
		return nil, &provider.Error{Class: provider.ErrorBadRequest, Message: "create provider request", Cause: err}
	}
	if err := a.prepareRequest(request); err != nil {
		return nil, &provider.Error{Class: provider.ErrorAuthentication, Message: "authorize provider request", Cause: err}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-Request-ID", requestID)
	response, err := a.client.Do(request)
	if err != nil {
		class := provider.TransportClass(err)
		return nil, &provider.Error{
			Class:     class,
			Retryable: class == provider.ErrorConnect || class == provider.ErrorTimeout,
			Ambiguous: !provider.Unsent(err),
			Message:   "provider request failed",
			Cause:     err,
		}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message := limitedErrorMessage(response.Body)
		return nil, classifyHTTPError(response.StatusCode, message)
	}
	limited := io.LimitReader(response.Body, maxResponseBytes+1)
	payload, err := io.ReadAll(limited)
	if err != nil {
		return nil, &provider.Error{Class: provider.ErrorMalformed, Message: "read provider response", Cause: err}
	}
	if len(payload) > maxResponseBytes {
		return nil, &provider.Error{Class: provider.ErrorMalformed, Message: "provider response exceeded size limit"}
	}
	// The guard sits here rather than at each decode site because postJSON is the
	// one place a unary call comes back: Chat and Responses both pass through it,
	// and a check placed after them would be two checks that can drift apart.
	if a.miniMax {
		if err := checkMiniMaxBaseResp(payload); err != nil {
			return nil, err
		}
	}
	return payload, nil
}

func (a *Adapter) Embed(ctx context.Context, call provider.EmbeddingCall) (openaiapi.EmbeddingResponse, error) {
	if a.azure && !validAzureDeployment(call.ProviderModel) {
		return openaiapi.EmbeddingResponse{}, &provider.Error{Class: provider.ErrorBadRequest, Message: "invalid azure deployment name"}
	}
	requestBody := call.Request
	if a.azure {
		requestBody.Model = ""
	} else {
		requestBody.Model = call.ProviderModel
	}
	encoded, err := json.Marshal(requestBody)
	if err != nil {
		return openaiapi.EmbeddingResponse{}, &provider.Error{Class: provider.ErrorBadRequest, Message: "encode provider request", Cause: err}
	}
	endpoint := a.operationURL(call.ProviderModel, "embeddings")
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(encoded))
	if err != nil {
		return openaiapi.EmbeddingResponse{}, &provider.Error{Class: provider.ErrorBadRequest, Message: "create provider request", Cause: err}
	}
	if err := a.prepareRequest(request); err != nil {
		return openaiapi.EmbeddingResponse{}, &provider.Error{Class: provider.ErrorAuthentication, Message: "authorize provider embedding request", Cause: err}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-Request-ID", call.RequestID)
	response, err := a.client.Do(request)
	if err != nil {
		class := provider.TransportClass(err)
		return openaiapi.EmbeddingResponse{}, &provider.Error{
			Class:     class,
			Retryable: class == provider.ErrorConnect || class == provider.ErrorTimeout,
			Ambiguous: !provider.Unsent(err),
			Message:   "provider request failed",
			Cause:     err,
		}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return openaiapi.EmbeddingResponse{}, classifyHTTPError(response.StatusCode, limitedErrorMessage(response.Body))
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return openaiapi.EmbeddingResponse{}, &provider.Error{Class: provider.ErrorMalformed, Message: "read provider response", Cause: err}
	}
	if len(payload) > maxResponseBytes {
		return openaiapi.EmbeddingResponse{}, &provider.Error{Class: provider.ErrorMalformed, Message: "provider response exceeded size limit"}
	}
	var result openaiapi.EmbeddingResponse
	if err := json.Unmarshal(payload, &result); err != nil {
		return openaiapi.EmbeddingResponse{}, &provider.Error{Class: provider.ErrorMalformed, Message: "decode provider response", Cause: err}
	}
	if result.Object == "" || len(result.Data) == 0 {
		return openaiapi.EmbeddingResponse{}, &provider.Error{Class: provider.ErrorMalformed, Message: "provider response is missing required fields"}
	}
	return result, nil
}

func (a *Adapter) ChatStream(
	ctx context.Context,
	call provider.ChatCall,
	emit func(semantic.Event) error,
) (*openaiapi.Usage, error) {
	if emit == nil {
		return nil, &provider.Error{Class: provider.ErrorBadRequest, Message: "stream callback is required"}
	}
	if a.responses {
		// The Responses profile binds no stream primitive, so nothing should reach
		// here. Refusing beats falling through to /v1/chat/completions, which
		// would stream from an endpoint this profile does not address.
		return nil, &provider.Error{Class: provider.ErrorBadRequest, Message: "this OpenAI profile does not serve streaming"}
	}
	if a.azure && !validAzureDeployment(call.ProviderModel) {
		return nil, &provider.Error{Class: provider.ErrorBadRequest, Message: "invalid azure deployment name"}
	}
	requestBody := call.Request
	if a.azure {
		requestBody.Model = ""
	} else {
		requestBody.Model = call.ProviderModel
	}
	requestBody.Stream = true
	if a.capabilities.StreamUsage {
		requestBody.StreamOptions = &openaiapi.StreamOptions{IncludeUsage: true}
	} else {
		requestBody.StreamOptions = nil
	}
	encoded, err := a.encodeChatRequest(requestBody)
	if err != nil {
		return nil, &provider.Error{Class: provider.ErrorBadRequest, Message: "encode provider request", Cause: err}
	}
	endpoint := a.operationURL(call.ProviderModel, "chat/completions")
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(encoded))
	if err != nil {
		return nil, &provider.Error{Class: provider.ErrorBadRequest, Message: "create provider request", Cause: err}
	}
	if err := a.prepareRequest(request); err != nil {
		return nil, &provider.Error{Class: provider.ErrorAuthentication, Message: "authorize provider stream request", Cause: err}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("X-Request-ID", call.RequestID)
	response, err := a.client.Do(request)
	if err != nil {
		class := provider.TransportClass(err)
		return nil, &provider.Error{
			Class: class, Retryable: class == provider.ErrorConnect || class == provider.ErrorTimeout,
			Ambiguous: !provider.Unsent(err), Message: "provider stream request failed", Cause: err,
		}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, classifyHTTPError(response.StatusCode, limitedErrorMessage(response.Body))
	}
	if contentType := response.Header.Get("Content-Type"); !strings.HasPrefix(strings.ToLower(contentType), "text/event-stream") {
		return nil, &provider.Error{Class: provider.ErrorMalformed, Ambiguous: true, Message: "provider did not return an SSE stream"}
	}
	decoder := sse.NewDecoder(response.Body, semantic.MaxEncodedEventBytes)
	var usage *openaiapi.Usage
	// finished records that a chunk carried a finish_reason, which is the model
	// saying it stopped. It is what tells a completed stream from a truncated one
	// on an upstream that ends by closing the connection.
	finished := false
	for {
		event, err := decoder.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				// MiniMax never sends the [DONE] sentinel. Measured 2026-08-31
				// against a real account: the stream carries the content chunks, a
				// chunk with finish_reason "stop", a final chunk with choices empty
				// and usage populated, and then closes.
				//
				// Treating that as truncation cost the caller a generation the
				// upstream had already run and billed — the attempt settled as an
				// ambiguous provider failure, so they paid and received an error.
				//
				// The exemption is narrow on purpose, in two ways. It is scoped to
				// this provider, because for every other OpenAI-family upstream a
				// stream that stops without the sentinel is what a partial response
				// looks like, and that check is what keeps a partial one from
				// settling as complete. And it still requires a finish_reason: an
				// EOF before the model said it stopped is a truncation here too.
				if a.miniMax && finished {
					return usage, nil
				}
				return usage, &provider.Error{
					Class: provider.ErrorMalformed, Ambiguous: true,
					Message: "provider stream ended before [DONE]",
				}
			}
			return usage, &provider.Error{
				Class: provider.ErrorMalformed, Ambiguous: true,
				Message: "decode provider stream", Cause: err,
			}
		}
		if string(event.Data) == "[DONE]" {
			return usage, nil
		}
		if len(event.Data) == 0 {
			continue
		}
		// A stream is the harder half of the same problem: MiniMax can send well
		// formed chunks and then report the failure in a later one, and by then
		// bytes are already with the caller. Nothing can be taken back, so the
		// stream ends here and the attempt is settled as an upstream failure on
		// whatever usage was seen — never as a success, and never refunded.
		if a.miniMax {
			if err := checkMiniMaxBaseResp(event.Data); err != nil {
				return usage, err
			}
		}
		var chunk openaiapi.ChatCompletionResponse
		if err := json.Unmarshal(event.Data, &chunk); err != nil {
			return usage, &provider.Error{
				Class: provider.ErrorMalformed, Ambiguous: true,
				Message: "decode provider stream chunk", Cause: err,
			}
		}
		semanticEvent, err := openaiwire.DecodeEvent(chunk)
		if err != nil {
			return usage, &provider.Error{
				Class: provider.ErrorMalformed, Ambiguous: true,
				Message: "provider stream chunk is semantically invalid", Cause: err,
			}
		}
		for _, choice := range chunk.Choices {
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				finished = true
			}
		}
		if chunk.Usage != nil {
			copyUsage := *chunk.Usage
			usage = &copyUsage
		}
		if err := emit(semanticEvent); err != nil {
			return usage, err
		}
	}
}

func (a *Adapter) operationURL(providerModel, operation string) url.URL {
	endpoint := *a.endpoint
	basePath := strings.TrimRight(endpoint.Path, "/")
	if a.azure {
		endpoint.Path = basePath + "/openai/deployments/" + providerModel + "/" + operation
		escapedBase := strings.TrimRight((&url.URL{Path: basePath}).EscapedPath(), "/")
		endpoint.RawPath = escapedBase + "/openai/deployments/" + url.PathEscape(providerModel) + "/" + operation
		query := endpoint.Query()
		query.Set("api-version", a.apiVersion)
		endpoint.RawQuery = query.Encode()
		return endpoint
	}
	if a.operationPathPrefix != "" {
		endpoint.Path = basePath + "/" + a.operationPathPrefix + "/" + operation
		return endpoint
	}
	endpoint.Path = versionedPath(basePath, operation)
	return endpoint
}

// versionedPath joins an operation onto the configured base URL. A base with
// no path gets the OpenAI default /v1 prefix; a base that carries a path is
// taken literally, because OpenAI-compatible endpoints version their paths
// differently — /v4, /api/paas/v4 — and the operator's base is the only place
// that knows the right prefix. The old rule inserted /v1 into any path not
// already ending in it, which made those endpoints unconfigurable: every base
// produced /…/v1/chat/completions and the upstream answered 404. A deployment
// that wants /v1 under a longer path spells it out (https://host/openai/v1).
func versionedPath(basePath, operation string) string {
	if basePath == "" {
		return "/v1/" + operation
	}
	return basePath + "/" + operation
}

// prepareRequest applies resource addressing and then the credential, in that
// order: a signing credential scheme has to see every header it covers.
//
// The project header is not the authorizer's business — addressing is not
// authentication — but both belong to every outbound request, so they share one
// call site rather than being remembered separately at five of them.
func (a *Adapter) prepareRequest(request *http.Request) error {
	provider.ApplyBedrockProject(request, provider.HeaderBedrockOpenAIProject, a.bedrockProjectID)
	return a.authorizer.Authorize(request, nil)
}

func validAzureDeployment(value string) bool {
	if value == "" || value == "." || value == ".." {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '-' || char == '_' || char == '.' {
			continue
		}
		return false
	}
	return true
}

// classifyHTTPError turns an upstream refusal into a classified provider error.
//
// It takes the whole refusal rather than only its sentence, because the sentence
// is the one part that cannot be forwarded anywhere: it is prose the upstream
// wrote about the request, so it stays inside the error and never reaches a log
// or a console cell. The code and the offending parameter are identifiers, and
// they are what an operator can act on — a bare "bad_request http=400" says a
// parameter was refused without saying which, and the operator is left to bisect
// a request they did not write.
func classifyHTTPError(status int, refusal upstreamRefusal) *provider.Error {
	result := &provider.Error{StatusCode: status, ProviderCode: refusal.Code}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		result.Class = provider.ErrorAuthentication
	case status == http.StatusRequestTimeout:
		result.Class = provider.ErrorTimeout
		result.Retryable = true
	case status == http.StatusTooManyRequests:
		result.Class = provider.ErrorRateLimit
		result.Retryable = true
	case status == http.StatusServiceUnavailable:
		// A stated refusal to take the request on. Nothing ran, so a fallback
		// deployment can serve it and the attempt owes nothing.
		result.Class = provider.ErrorProvider5xx
		result.Retryable = true
	case status >= 500:
		// The request reached the upstream and no authoritative result came
		// back. A 500 can be raised part-way through a generation, and a 502 or
		// 504 comes from the edge while the origin may still be running and
		// billing. Retrying would duplicate that generation and settling it as
		// free would hide the charge, so it is ambiguous — the same answer the
		// Bedrock and Gemini profiles already give for their own 5xx.
		result.Class = provider.ErrorProvider5xx
		result.Ambiguous = true
	default:
		result.Class = provider.ErrorBadRequest
		// A body that could not be read as an OpenAI envelope says nothing about
		// the request, whatever status carried it, so it names no refusal kind
		// rather than the weaker one.
		if !refusal.Unreadable {
			result.Refusal = provider.RefusalFromOpenAIBody(status, refusal.Code)
		}
	}
	result.Message = fmt.Sprintf("provider error (%d): %s", status, refusalSentence(refusal))
	return result
}

// refusalSentence is the part of the message after the status code. When the
// upstream wrote a sentence it is quoted verbatim; when it did not, the stand-in
// says which silence this was, because the two send an operator to different
// places. A body that is not an OpenAI-shaped envelope usually means the thing
// answering is not the provider API — a proxy, a CDN, or a path that does not
// exist upstream — while a well-formed envelope with an empty message means the
// provider itself declined to say why.
//
// The stand-in must never borrow another status code's name. Reading back
// "provider error (404): Bad Gateway" costs an operator the one fact the line
// carried, because the status says the route was not found and the sentence
// says a gateway failed, and only one of them is true.
func refusalSentence(refusal upstreamRefusal) string {
	switch {
	case refusal.Message != "":
		return refusal.Message
	case refusal.Unreadable:
		return "upstream sent no OpenAI-shaped error body"
	default:
		return "upstream sent no error message"
	}
}

// upstreamRefusal is the machine-readable part of an OpenAI-shaped error body,
// kept apart from the human sentence beside it.
type upstreamRefusal struct {
	Message string
	Code    string
	// Unreadable records that the body could not be parsed as an OpenAI error
	// envelope at all, as opposed to parsing into one that carried no sentence.
	// Only the message the upstream actually wrote belongs in Message, so that a
	// stand-in Halro authored can never be mistaken for something a provider
	// said.
	Unreadable bool
}

func limitedErrorMessage(reader io.Reader) upstreamRefusal {
	payload, err := io.ReadAll(io.LimitReader(reader, 4096))
	if err != nil {
		return upstreamRefusal{Unreadable: true}
	}
	var envelope openaiapi.ErrorEnvelope
	if json.Unmarshal(payload, &envelope) != nil {
		return upstreamRefusal{Unreadable: true}
	}
	refusal := upstreamRefusal{Message: envelope.Error.Message, Code: strings.TrimSpace(envelope.Error.Code)}
	if refusal.Code == "" {
		// Not every OpenAI-shaped upstream fills `code`; `type` is the coarser
		// identifier the same bodies always carry.
		refusal.Code = strings.TrimSpace(envelope.Error.Type)
	}
	// The refused parameter is an identifier too, and it is the one an operator
	// needs: "unsupported_parameter" without it names a category, not a field.
	// Joining keeps both inside the single identifier field the error contract
	// has, in a shape the console and log already accept.
	if param := strings.TrimSpace(pointerValue(envelope.Error.Param)); param != "" && refusal.Code != "" {
		refusal.Code += ":" + param
	}
	// Narrowed where it is born. The code reaches a log attribute, a durable
	// probe result and a console cell, and every one of those inherits whatever
	// this returns — an upstream that answers with a wall of text, or with the
	// credential it has just rejected, would otherwise be writing it into all
	// three. Bounding it here means no consumer has to remember to.
	refusal.Code = provider.SafeProviderIdentifier(refusal.Code)
	return refusal
}

func pointerValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
