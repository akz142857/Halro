package gatewayapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/akz142857/Halro/internal/anthropicapi"
	"github.com/akz142857/Halro/internal/gateway"
	"github.com/akz142857/Halro/internal/id"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/requestmeta"
	"github.com/akz142857/Halro/internal/sse"
)

const defaultMaxRequestBytes = 4 << 20

// Service is the concrete interface used by Handler. It is intentionally small
// so the HTTP contract can be tested without opening a listener.
type Service interface {
	Chat(
		ctx context.Context,
		plaintextKey string,
		request openaiapi.ChatCompletionRequest,
	) (openaiapi.ChatCompletionResponse, error)
	ChatStream(
		ctx context.Context,
		plaintextKey string,
		request openaiapi.ChatCompletionRequest,
		emit func(openaiapi.ChatCompletionResponse) error,
	) error
	Embeddings(
		ctx context.Context,
		plaintextKey string,
		request openaiapi.EmbeddingRequest,
	) (openaiapi.EmbeddingResponse, error)
}

type ResponsesService interface {
	Responses(context.Context, string, openaiapi.ResponseRequest) (openaiapi.Response, error)
	ResponsesStream(context.Context, string, openaiapi.ResponseRequest, func(openaiapi.ResponseStreamEvent) error) error
}

// The handler discovers Messages support with a comma-ok type assertion, so a
// signature drift silently leaves h.messages nil and every /v1/messages request
// answers 501 instead of failing the build. This assertion puts that failure
// back at compile time, where it belongs.
var _ MessagesService = (*gateway.Service)(nil)

type MessagesService interface {
	Messages(context.Context, string, anthropicapi.MessageRequest) (anthropicapi.Message, error)
	MessagesStream(context.Context, string, anthropicapi.MessageRequest, func(anthropicapi.StreamEvent) error) error
	MessagesNative(context.Context, string, string, []string, anthropicapi.MessageRequest) (anthropicapi.Message, error)
	MessagesNativeStream(context.Context, string, string, []string, anthropicapi.MessageRequest, func(anthropicapi.RawStreamEvent) error) error
	MessagesCountTokens(context.Context, string, string, []string, anthropicapi.MessageRequest) (anthropicapi.TokenCount, error)
}

func (h *Handler) Responses(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	request, ok := withOpenAIRequestID(writer, request)
	if !ok {
		return
	}
	request, ok = h.withSourceIP(writer, request)
	if !ok {
		return
	}
	key, ok := bearerToken(request.Header.Get("Authorization"))
	if !ok {
		writer.Header().Set("WWW-Authenticate", `Bearer realm="halro"`)
		writeError(writer, http.StatusUnauthorized, "invalid_api_key", "missing or invalid bearer token", nil)
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, h.maxRequestBytes)
	decoded, err := openaiapi.DecodeResponseRequest(json.NewDecoder(request.Body))
	if err != nil {
		code := "invalid_request_error"
		message, param := decodeProblem(err)
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			code, message, param = "request_too_large", "request body exceeds the configured limit", nil
		}
		writeError(writer, http.StatusBadRequest, code, message, param)
		return
	}
	if decoded.Stream {
		ctx, cancel := context.WithTimeout(request.Context(), h.streamTimeout)
		defer cancel()
		h.responsesStream(writer, request.WithContext(ctx), key, decoded)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), h.routeTimeout)
	defer cancel()
	if h.responses == nil {
		writeError(writer, http.StatusNotImplemented, "unsupported_feature", "Responses API is unavailable", nil)
		return
	}
	response, err := h.responses.Responses(ctx, key, decoded)
	if err != nil {
		var gatewayError *gateway.Error
		if errors.As(err, &gatewayError) {
			setRetryAfter(writer, gatewayError.RetryAfter)
			writeError(writer, gatewayError.HTTPStatus, gatewayError.Code, gatewayError.Message, nil)
			return
		}
		writeError(writer, http.StatusInternalServerError, "internal_error", "internal server error", nil)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(writer).Encode(response)
}

func (h *Handler) responsesStream(writer http.ResponseWriter, request *http.Request, key string, decoded openaiapi.ResponseRequest) {
	flusher, ok := writer.(http.Flusher)
	if !ok {
		writeError(writer, http.StatusInternalServerError, "streaming_unsupported", "response streaming is unavailable", nil)
		return
	}
	encoder := sse.NewEncoder(writer)
	started := false
	nextSequence := int64(0)
	start := func() {
		if started {
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.Header().Set("Cache-Control", "no-cache, no-store")
		writer.Header().Set("Connection", "keep-alive")
		writer.Header().Set("X-Accel-Buffering", "no")
		writer.WriteHeader(http.StatusOK)
		started = true
	}
	if h.responses == nil {
		writeError(writer, http.StatusNotImplemented, "unsupported_feature", "Responses API is unavailable", nil)
		return
	}
	err := h.responses.ResponsesStream(request.Context(), key, decoded, func(event openaiapi.ResponseStreamEvent) error {
		if event.SequenceNumber >= nextSequence {
			nextSequence = event.SequenceNumber + 1
		}
		payload, err := json.Marshal(event)
		if err != nil {
			return err
		}
		start()
		if err := http.NewResponseController(writer).SetWriteDeadline(time.Now().Add(h.writeTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return err
		}
		if err := encoder.Write(sse.Event{Event: event.Type, Data: payload}); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	})
	if !started && err != nil {
		var gatewayError *gateway.Error
		if errors.As(err, &gatewayError) {
			setRetryAfter(writer, gatewayError.RetryAfter)
			writeError(writer, gatewayError.HTTPStatus, gatewayError.Code, gatewayError.Message, nil)
		} else if !errors.Is(err, context.Canceled) {
			writeError(writer, http.StatusBadGateway, "provider_error", "provider stream failed", nil)
		}
		return
	}
	if errors.Is(err, context.Canceled) || err == nil {
		return
	}
	start()
	_ = http.NewResponseController(writer).SetWriteDeadline(time.Now().Add(h.writeTimeout))
	failure := openaiapi.ResponseStreamEvent{Type: "error", SequenceNumber: nextSequence, Code: "stream_error", Message: "stream terminated safely"}
	payload, _ := json.Marshal(failure)
	_ = encoder.Write(sse.Event{Event: "error", Data: payload})
	flusher.Flush()
}

func (h *Handler) Embeddings(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	request, ok := withOpenAIRequestID(writer, request)
	if !ok {
		return
	}
	request, ok = h.withSourceIP(writer, request)
	if !ok {
		return
	}
	key, ok := bearerToken(request.Header.Get("Authorization"))
	if !ok {
		writer.Header().Set("WWW-Authenticate", `Bearer realm="halro"`)
		writeError(writer, http.StatusUnauthorized, "invalid_api_key", "missing or invalid bearer token", nil)
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, h.maxRequestBytes)
	decoded, err := openaiapi.DecodeEmbeddingRequest(json.NewDecoder(request.Body))
	if err != nil {
		code := "invalid_request_error"
		message, param := decodeProblem(err)
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			code, message, param = "request_too_large", "request body exceeds the configured limit", nil
		}
		writeError(writer, http.StatusBadRequest, code, message, param)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), h.routeTimeout)
	defer cancel()
	response, err := h.service.Embeddings(ctx, key, decoded)
	if err != nil {
		var gatewayError *gateway.Error
		if errors.As(err, &gatewayError) {
			setRetryAfter(writer, gatewayError.RetryAfter)
			writeError(writer, gatewayError.HTTPStatus, gatewayError.Code, gatewayError.Message, nil)
			return
		}
		writeError(writer, http.StatusInternalServerError, "internal_error", "internal server error", nil)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(writer).Encode(response)
}

func (h *Handler) chatCompletionsStream(
	writer http.ResponseWriter,
	request *http.Request,
	key string,
	decoded openaiapi.ChatCompletionRequest,
) {
	flusher, ok := writer.(http.Flusher)
	if !ok {
		writeError(writer, http.StatusInternalServerError, "streaming_unsupported", "response streaming is unavailable", nil)
		return
	}
	encoder := sse.NewEncoder(writer)
	started := false
	start := func() {
		if started {
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.Header().Set("Cache-Control", "no-cache, no-store")
		writer.Header().Set("Connection", "keep-alive")
		writer.Header().Set("X-Accel-Buffering", "no")
		writer.WriteHeader(http.StatusOK)
		started = true
	}
	err := h.service.ChatStream(request.Context(), key, decoded, func(chunk openaiapi.ChatCompletionResponse) error {
		payload, err := json.Marshal(chunk)
		if err != nil {
			return err
		}
		start()
		if err := http.NewResponseController(writer).SetWriteDeadline(time.Now().Add(h.writeTimeout)); err != nil &&
			!errors.Is(err, http.ErrNotSupported) {
			return err
		}
		if err := encoder.Write(sse.Event{Data: payload}); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	})
	if !started && err != nil {
		var gatewayError *gateway.Error
		if errors.As(err, &gatewayError) {
			setRetryAfter(writer, gatewayError.RetryAfter)
			writeError(writer, gatewayError.HTTPStatus, gatewayError.Code, gatewayError.Message, nil)
		} else if !errors.Is(err, context.Canceled) {
			writeError(writer, http.StatusBadGateway, "provider_error", "provider stream failed", nil)
		}
		return
	}
	if errors.Is(err, context.Canceled) {
		return
	}
	start()
	_ = http.NewResponseController(writer).SetWriteDeadline(time.Now().Add(h.writeTimeout))
	if err != nil {
		envelope, _ := json.Marshal(openaiapi.ErrorEnvelope{Error: openaiapi.ErrorBody{
			Message: "stream terminated safely",
			Type:    "server_error",
			Code:    "stream_error",
		}})
		_ = encoder.Write(sse.Event{Event: "error", Data: envelope})
		flusher.Flush()
		return
	}
	_ = encoder.Write(sse.Event{Data: []byte("[DONE]")})
	flusher.Flush()
}

type Handler struct {
	service            Service
	responses          ResponsesService
	messages           MessagesService
	inferenceResources InferenceResourcesService
	maxRequestBytes    int64
	routeTimeout       time.Duration
	streamTimeout      time.Duration
	writeTimeout       time.Duration
	trustProxy         bool
	trustedProxies     []netip.Prefix
	authorizeKey       func(string) (int64, error)
	sourceLimit        SourceLimiter
}

// SourceLimiter bounds how many requests one source address may start per
// window. It is consulted before the guard and before any body is read, so it
// must be cheap and must never block. A false result carries how long the
// caller should wait.
type SourceLimiter interface {
	Allow(source netip.Addr, now time.Time) (bool, time.Duration)
}

func New(service Service, maxRequestBytes int64) (*Handler, error) {
	return NewWithOptions(service, Options{MaxRequestBytes: maxRequestBytes})
}

type Options struct {
	MaxRequestBytes   int64
	RouteTimeout      time.Duration
	StreamTimeout     time.Duration
	WriteTimeout      time.Duration
	TrustProxyHeaders bool
	TrustedProxyCIDRs []netip.Prefix

	// AuthorizeKey, when supplied, is consulted before a request body is read.
	// It must be cheap and must reject only what full authentication would also
	// reject — it is a guard in front of the real check, never a substitute for
	// it.
	//
	// It also reports the request-size ceiling the key's Project declares, zero
	// meaning the Project sets none. That ceiling belongs to a Project, and the
	// Project is not known until the key has authenticated, so this is the
	// earliest point at which it can bound a body — which is the only point that
	// matters, because a limit applied after the body is read has already paid
	// for the bytes it was meant to refuse.
	AuthorizeKey func(plaintextKey string) (maxRequestBytes int64, err error)

	// SourceLimiter, when supplied, bounds request starts per source address
	// ahead of authentication. Leaving it nil leaves the data plane unbounded
	// for anonymous callers.
	SourceLimiter SourceLimiter
}

func NewWithOptions(service Service, options Options) (*Handler, error) {
	if service == nil {
		return nil, errors.New("gateway service is required")
	}
	if options.MaxRequestBytes <= 0 {
		options.MaxRequestBytes = defaultMaxRequestBytes
	}
	if options.RouteTimeout <= 0 {
		options.RouteTimeout = 2 * time.Minute
	}
	if options.StreamTimeout <= 0 {
		options.StreamTimeout = 10 * time.Minute
	}
	if options.WriteTimeout <= 0 {
		options.WriteTimeout = 15 * time.Second
	}
	handler := &Handler{
		service: service, maxRequestBytes: options.MaxRequestBytes,
		routeTimeout: options.RouteTimeout, streamTimeout: options.StreamTimeout,
		writeTimeout:   options.WriteTimeout,
		trustProxy:     options.TrustProxyHeaders,
		trustedProxies: append([]netip.Prefix(nil), options.TrustedProxyCIDRs...),
		authorizeKey:   options.AuthorizeKey,
		sourceLimit:    options.SourceLimiter,
	}
	handler.responses, _ = service.(ResponsesService)
	handler.messages, _ = service.(MessagesService)
	handler.inferenceResources, _ = service.(InferenceResourcesService)
	return handler, nil
}

func (h *Handler) Messages(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	requestID, err := id.New("req")
	if err != nil {
		writeAnthropicError(writer, http.StatusInternalServerError, "api_error", "internal server error", "")
		return
	}
	writer.Header().Set("request-id", requestID)
	request, ok := h.withSourceIPAnthropic(writer, request, requestID)
	if !ok {
		return
	}
	version := strings.TrimSpace(request.Header.Get(anthropicapi.VersionHeader))
	if version == "" {
		writeAnthropicError(writer, http.StatusBadRequest, "invalid_request_error", "anthropic-version header is required", requestID)
		return
	}
	if version != anthropicapi.SupportedVersion {
		writeAnthropicError(writer, http.StatusBadRequest, "invalid_request_error", "unsupported anthropic-version", requestID)
		return
	}
	mode, err := anthropicapi.ParseExecutionMode(request.Header.Get(anthropicapi.RouteModeHeader))
	if err != nil {
		writeAnthropicError(writer, http.StatusBadRequest, "invalid_request_error", err.Error(), requestID)
		return
	}
	betas, err := anthropicapi.ParseBetaTokens(request.Header.Get(anthropicapi.BetaHeader))
	if err != nil {
		writeAnthropicError(writer, http.StatusBadRequest, "invalid_request_error", err.Error(), requestID)
		return
	}
	if len(betas) > 0 && mode != anthropicapi.ModeNative {
		// Portable mode re-authors the request through the canonical model. A
		// beta token describes wire behaviour of the request as written, so
		// forwarding one alongside a rewritten body would claim semantics Halro
		// did not send.
		writeAnthropicError(writer, http.StatusBadRequest, "invalid_request_error", "anthropic-beta requires Halro-Route-Mode: native", requestID)
		return
	}
	key, ok := anthropicGatewayKey(request.Header)
	if !ok {
		writer.Header().Set("WWW-Authenticate", `Bearer realm="halro"`)
		writeAnthropicError(writer, http.StatusUnauthorized, "authentication_error", "missing or invalid gateway key", requestID)
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, h.maxRequestBytes)
	decoded, err := anthropicapi.DecodeMessageRequest(request.Body)
	if err != nil {
		message, _ := decodeProblem(err)
		status, kind := http.StatusBadRequest, "invalid_request_error"
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) || strings.Contains(err.Error(), "exceeds limit") {
			status, kind, message = http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds the configured limit"
		}
		writeAnthropicError(writer, status, kind, message, requestID)
		return
	}
	if mode == anthropicapi.ModeNative {
		if decoded.Stream {
			ctx, cancel := context.WithTimeout(request.Context(), h.streamTimeout)
			defer cancel()
			h.messagesNativeStream(writer, request.WithContext(ctx), key, version, betas, decoded, requestID)
			return
		}
		ctx, cancel := context.WithTimeout(request.Context(), h.routeTimeout)
		defer cancel()
		if h.messages == nil {
			writeAnthropicError(writer, http.StatusNotImplemented, "api_error", "Messages API is unavailable", requestID)
			return
		}
		response, callErr := h.messages.MessagesNative(ctx, key, version, betas, decoded)
		if callErr != nil {
			h.renderAnthropicGatewayError(writer, callErr, requestID)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(writer).Encode(response)
		return
	}
	if decoded.Stream {
		ctx, cancel := context.WithTimeout(request.Context(), h.streamTimeout)
		defer cancel()
		h.messagesStream(writer, request.WithContext(ctx), key, decoded, requestID)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), h.routeTimeout)
	defer cancel()
	if h.messages == nil {
		writeAnthropicError(writer, http.StatusNotImplemented, "api_error", "Messages API is unavailable", requestID)
		return
	}
	response, err := h.messages.Messages(ctx, key, decoded)
	if err != nil {
		h.renderAnthropicGatewayError(writer, err, requestID)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(writer).Encode(response)
}

// CountTokens serves Anthropic's count_tokens.
//
// It has only one execution shape. The request body reaches the provider
// verbatim — there is nothing to translate, because nothing is generated — so
// the route-mode header has no portable alternative to select. An explicit
// portable is refused rather than silently treated as native, since it would
// claim a translation that does not happen; an absent header is fine.
func (h *Handler) CountTokens(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	requestID, err := id.New("req")
	if err != nil {
		writeAnthropicError(writer, http.StatusInternalServerError, "api_error", "internal server error", "")
		return
	}
	writer.Header().Set("request-id", requestID)
	request, ok := h.withSourceIPAnthropic(writer, request, requestID)
	if !ok {
		return
	}
	version := strings.TrimSpace(request.Header.Get(anthropicapi.VersionHeader))
	if version == "" {
		writeAnthropicError(writer, http.StatusBadRequest, "invalid_request_error", "anthropic-version header is required", requestID)
		return
	}
	if version != anthropicapi.SupportedVersion {
		writeAnthropicError(writer, http.StatusBadRequest, "invalid_request_error", "unsupported anthropic-version", requestID)
		return
	}
	if declared := strings.TrimSpace(request.Header.Get(anthropicapi.RouteModeHeader)); declared != "" {
		mode, modeErr := anthropicapi.ParseExecutionMode(declared)
		if modeErr != nil {
			writeAnthropicError(writer, http.StatusBadRequest, "invalid_request_error", modeErr.Error(), requestID)
			return
		}
		if mode != anthropicapi.ModeNative {
			writeAnthropicError(writer, http.StatusBadRequest, "invalid_request_error", "count_tokens is served natively; Halro-Route-Mode: portable does not apply", requestID)
			return
		}
	}
	betas, err := anthropicapi.ParseBetaTokens(request.Header.Get(anthropicapi.BetaHeader))
	if err != nil {
		writeAnthropicError(writer, http.StatusBadRequest, "invalid_request_error", err.Error(), requestID)
		return
	}
	key, ok := anthropicGatewayKey(request.Header)
	if !ok {
		writer.Header().Set("WWW-Authenticate", `Bearer realm="halro"`)
		writeAnthropicError(writer, http.StatusUnauthorized, "authentication_error", "missing or invalid gateway key", requestID)
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, h.maxRequestBytes)
	decoded, err := anthropicapi.DecodeMessageRequest(request.Body)
	if err != nil {
		message, _ := decodeProblem(err)
		status, kind := http.StatusBadRequest, "invalid_request_error"
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) || strings.Contains(err.Error(), "exceeds limit") {
			status, kind, message = http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds the configured limit"
		}
		writeAnthropicError(writer, status, kind, message, requestID)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), h.routeTimeout)
	defer cancel()
	if h.messages == nil {
		writeAnthropicError(writer, http.StatusNotImplemented, "api_error", "Messages API is unavailable", requestID)
		return
	}
	count, err := h.messages.MessagesCountTokens(ctx, key, version, betas, decoded)
	if err != nil {
		h.renderAnthropicGatewayError(writer, err, requestID)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(writer).Encode(count)
}

func (h *Handler) messagesNativeStream(writer http.ResponseWriter, request *http.Request, key, version string, betas []string, decoded anthropicapi.MessageRequest, requestID string) {
	flusher, ok := writer.(http.Flusher)
	if !ok {
		writeAnthropicError(writer, http.StatusInternalServerError, "api_error", "response streaming is unavailable", requestID)
		return
	}
	if h.messages == nil {
		writeAnthropicError(writer, http.StatusNotImplemented, "api_error", "Messages API is unavailable", requestID)
		return
	}
	encoder := sse.NewEncoder(writer)
	started := false
	start := func() {
		if started {
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.Header().Set("Cache-Control", "no-cache, no-store")
		writer.Header().Set("Connection", "keep-alive")
		writer.Header().Set("X-Accel-Buffering", "no")
		writer.WriteHeader(http.StatusOK)
		started = true
	}
	err := h.messages.MessagesNativeStream(request.Context(), key, version, betas, decoded, func(event anthropicapi.RawStreamEvent) error {
		start()
		if err := http.NewResponseController(writer).SetWriteDeadline(time.Now().Add(h.writeTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return err
		}
		if err := encoder.Write(sse.Event{Event: event.Type, Data: event.Data}); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	})
	if !started && err != nil {
		h.renderAnthropicGatewayError(writer, err, requestID)
		return
	}
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	start()
	failure := anthropicapi.StreamEvent{Type: "error", Error: &anthropicapi.ErrorDetail{Type: "api_error", Message: "stream terminated safely"}}
	payload, _ := json.Marshal(failure)
	_ = encoder.Write(sse.Event{Event: "error", Data: payload})
	flusher.Flush()
}

func (h *Handler) messagesStream(writer http.ResponseWriter, request *http.Request, key string, decoded anthropicapi.MessageRequest, requestID string) {
	flusher, ok := writer.(http.Flusher)
	if !ok {
		writeAnthropicError(writer, http.StatusInternalServerError, "api_error", "response streaming is unavailable", requestID)
		return
	}
	if h.messages == nil {
		writeAnthropicError(writer, http.StatusNotImplemented, "api_error", "Messages API is unavailable", requestID)
		return
	}
	encoder := sse.NewEncoder(writer)
	started := false
	start := func() {
		if started {
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.Header().Set("Cache-Control", "no-cache, no-store")
		writer.Header().Set("Connection", "keep-alive")
		writer.Header().Set("X-Accel-Buffering", "no")
		writer.WriteHeader(http.StatusOK)
		started = true
	}
	err := h.messages.MessagesStream(request.Context(), key, decoded, func(event anthropicapi.StreamEvent) error {
		payload, err := json.Marshal(event)
		if err != nil {
			return err
		}
		start()
		if err := http.NewResponseController(writer).SetWriteDeadline(time.Now().Add(h.writeTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return err
		}
		if err := encoder.Write(sse.Event{Event: event.Type, Data: payload}); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	})
	if !started && err != nil {
		h.renderAnthropicGatewayError(writer, err, requestID)
		return
	}
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	start()
	failure := anthropicapi.StreamEvent{Type: "error", Error: &anthropicapi.ErrorDetail{Type: "api_error", Message: "stream terminated safely"}}
	payload, _ := json.Marshal(failure)
	_ = encoder.Write(sse.Event{Event: "error", Data: payload})
	flusher.Flush()
}

func anthropicGatewayKey(header http.Header) (string, bool) {
	apiKey := strings.TrimSpace(header.Get("x-api-key"))
	bearer, hasBearer := bearerToken(header.Get("Authorization"))
	if apiKey != "" && hasBearer && apiKey != bearer {
		return "", false
	}
	if apiKey != "" {
		return apiKey, true
	}
	return bearer, hasBearer
}

func (h *Handler) withSourceIPAnthropic(writer http.ResponseWriter, request *http.Request, requestID string) (*http.Request, bool) {
	source, ok := h.sourceIP(request)
	if !ok {
		writeAnthropicError(writer, http.StatusBadRequest, "invalid_request_error", "invalid X-Forwarded-For header", requestID)
		return request, false
	}
	return request.WithContext(requestmeta.WithSourceIP(request.Context(), source)), true
}

func (h *Handler) renderAnthropicGatewayError(writer http.ResponseWriter, err error, requestID string) {
	var gatewayError *gateway.Error
	if !errors.As(err, &gatewayError) {
		writeAnthropicError(writer, http.StatusInternalServerError, "api_error", "internal server error", requestID)
		return
	}
	setRetryAfter(writer, gatewayError.RetryAfter)
	var providerError *provider.Error
	if errors.As(err, &providerError) && (providerError.StatusCode == 529 || providerError.ProviderCode == "overloaded_error") {
		writeAnthropicError(writer, 529, "overloaded_error", gatewayError.Message, requestID)
		return
	}
	kind := "invalid_request_error"
	switch gatewayError.HTTPStatus {
	case 401:
		kind = "authentication_error"
	case 403:
		kind = "permission_error"
	case 404:
		kind = "not_found_error"
	case 413:
		kind = "request_too_large"
	case 429:
		kind = "rate_limit_error"
	case 500, 502, 504:
		kind = "api_error"
	case 503:
		kind = "overloaded_error"
	}
	writeAnthropicError(writer, gatewayError.HTTPStatus, kind, gatewayError.Message, requestID)
}

func writeAnthropicError(writer http.ResponseWriter, status int, kind, message, requestID string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	if requestID != "" {
		writer.Header().Set("request-id", requestID)
	}
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(anthropicapi.ErrorResponse{Type: "error", Error: anthropicapi.ErrorDetail{Type: kind, Message: message}, RequestID: requestID})
}

func (h *Handler) ChatCompletions(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	request, ok := withOpenAIRequestID(writer, request)
	if !ok {
		return
	}
	request, ok = h.withSourceIP(writer, request)
	if !ok {
		return
	}
	key, ok := bearerToken(request.Header.Get("Authorization"))
	if !ok {
		writer.Header().Set("WWW-Authenticate", `Bearer realm="halro"`)
		writeError(writer, http.StatusUnauthorized, "invalid_api_key", "missing or invalid bearer token", nil)
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, h.maxRequestBytes)
	decoded, err := openaiapi.DecodeChatCompletionRequest(json.NewDecoder(request.Body))
	if err != nil {
		code := "invalid_request_error"
		message, param := decodeProblem(err)
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			code, message, param = "request_too_large", "request body exceeds the configured limit", nil
		}
		writeError(writer, http.StatusBadRequest, code, message, param)
		return
	}
	if decoded.Stream {
		ctx, cancel := context.WithTimeout(request.Context(), h.streamTimeout)
		defer cancel()
		h.chatCompletionsStream(writer, request.WithContext(ctx), key, decoded)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), h.routeTimeout)
	defer cancel()
	response, err := h.service.Chat(ctx, key, decoded)
	if err != nil {
		var gatewayError *gateway.Error
		if errors.As(err, &gatewayError) {
			setRetryAfter(writer, gatewayError.RetryAfter)
			writeError(writer, gatewayError.HTTPStatus, gatewayError.Code, gatewayError.Message, nil)
			return
		}
		writeError(writer, http.StatusInternalServerError, "internal_error", "internal server error", nil)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(writer).Encode(response)
}

func setRetryAfter(writer http.ResponseWriter, duration time.Duration) {
	if duration <= 0 {
		return
	}
	seconds := int64((duration + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	writer.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
}

func withOpenAIRequestID(writer http.ResponseWriter, request *http.Request) (*http.Request, bool) {
	requestID, err := id.New("req")
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "internal_error", "unable to create request ID", nil)
		return request, false
	}
	writer.Header().Set("X-Request-ID", requestID)
	return request.WithContext(requestmeta.WithRequestID(request.Context(), requestID)), true
}

func (h *Handler) withSourceIP(writer http.ResponseWriter, request *http.Request) (*http.Request, bool) {
	source, ok := h.sourceIP(request)
	if !ok {
		writeError(writer, http.StatusBadRequest, "invalid_forwarded_for", "invalid X-Forwarded-For header", nil)
		return request, false
	}
	return request.WithContext(requestmeta.WithSourceIP(request.Context(), source)), true
}

func (h *Handler) sourceIP(request *http.Request) (netip.Addr, bool) {
	remote, err := netip.ParseAddrPort(request.RemoteAddr)
	if err != nil {
		return netip.Addr{}, false
	}
	current := remote.Addr().Unmap()
	if !h.trustProxy || !prefixContains(h.trustedProxies, current) {
		return current, true
	}
	// Repeated field lines are one list in order (RFC 9110 §5.3), and a proxy that
	// appends rather than rewrites produces exactly that. Reading only the first
	// line let a client send its own X-Forwarded-For and have the proxy's
	// truthful, appended line ignored: the forged hop then looked like the
	// rightmost untrusted address, which is the one this function trusts.
	forwarded := strings.Join(request.Header.Values("X-Forwarded-For"), ",")
	if strings.TrimSpace(forwarded) == "" {
		return netip.Addr{}, false
	}
	parts := strings.Split(forwarded, ",")
	for index := len(parts) - 1; index >= 0; index-- {
		candidate, err := netip.ParseAddr(strings.TrimSpace(parts[index]))
		if err != nil {
			return netip.Addr{}, false
		}
		candidate = candidate.Unmap()
		if !prefixContains(h.trustedProxies, candidate) {
			return candidate, true
		}
	}
	return current, true
}

func prefixContains(prefixes []netip.Prefix, address netip.Addr) bool {
	for _, prefix := range prefixes {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

func bearerToken(value string) (string, bool) {
	scheme, token, found := strings.Cut(strings.TrimSpace(value), " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return "", false
	}
	return token, true
}

// GuardOpenAI refuses a request whose key cannot authenticate before anything
// expensive happens to its body, answering in the OpenAI envelope.
//
// Authentication ran inside the service, which the handler reaches only after
// reading up to MaxRequestBytes and building a decoded request from it. An
// unauthenticated caller therefore decided how much parsing the gateway did,
// and the limiter that would have bounded it is keyed by project — that is, it
// does not exist until authentication has already succeeded. A forged bearer
// token and a large deeply nested body was the whole attack.
// WithWriteDeadline bounds how long a client may take to read a response it asked
// for. The streaming paths arm a deadline before every event, so a stalled reader
// costs one write; every other response — a completion, an embedding, an error
// envelope — had no write deadline at all, and a client that opened a connection
// and never read held the serving goroutine and its connection for as long as it
// liked. http.Server.WriteTimeout is not the instrument for this: it starts
// counting at the request header and would sever every SSE stream on the gateway.
func (h *Handler) WithWriteDeadline(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		next.ServeHTTP(&deadlineWriter{ResponseWriter: writer, timeout: h.writeTimeout}, request)
	})
}

// deadlineWriter arms the deadline when the response starts rather than when the
// request arrives. A provider is allowed to take routeTimeout to answer, which is
// far longer than the write budget; arming early would abort responses that only
// took a slow upstream, not a slow client. Streaming handlers re-arm per event and
// so simply overwrite what this set.
type deadlineWriter struct {
	http.ResponseWriter
	timeout time.Duration
	armed   bool
}

// Unwrap lets http.NewResponseController reach the real writer, which is how the
// streaming paths keep extending the deadline through this wrapper.
func (w *deadlineWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *deadlineWriter) Flush() { _ = http.NewResponseController(w.ResponseWriter).Flush() }

func (w *deadlineWriter) WriteHeader(status int) {
	w.arm()
	w.ResponseWriter.WriteHeader(status)
}

func (w *deadlineWriter) Write(payload []byte) (int, error) {
	w.arm()
	return w.ResponseWriter.Write(payload)
}

func (w *deadlineWriter) arm() {
	if w.armed {
		return
	}
	w.armed = true
	// A writer without deadline support (httptest, a test double) is not an error:
	// the deadline is a defence, and its absence must not fail the response.
	_ = http.NewResponseController(w.ResponseWriter).SetWriteDeadline(time.Now().Add(w.timeout))
}

// LimitOpenAI sheds requests from a source that has outrun its per-minute
// budget. It runs ahead of GuardOpenAI, so the work an anonymous caller can
// demand — the authentication attempt included — is itself bounded.
func (h *Handler) LimitOpenAI(next http.Handler) http.Handler {
	return h.limitBySource(next, func(writer http.ResponseWriter, _ *http.Request, retryAfter time.Duration) {
		setRetryAfter(writer, retryAfter)
		writeError(writer, http.StatusTooManyRequests, "rate_limit_exceeded",
			"too many requests from this source address", nil)
	})
}

// LimitAnthropic is LimitOpenAI in the envelope an Anthropic SDK can parse.
func (h *Handler) LimitAnthropic(next http.Handler) http.Handler {
	return h.limitBySource(next, func(writer http.ResponseWriter, request *http.Request, retryAfter time.Duration) {
		setRetryAfter(writer, retryAfter)
		writeAnthropicError(writer, http.StatusTooManyRequests, "rate_limit_error",
			"too many requests from this source address", request.Header.Get("X-Request-Id"))
	})
}

func (h *Handler) limitBySource(next http.Handler, deny func(http.ResponseWriter, *http.Request, time.Duration)) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if h.sourceLimit == nil {
			next.ServeHTTP(writer, request)
			return
		}
		// An address this cannot resolve is charged to the zero Addr rather
		// than waved through. The endpoint behind still answers such a request
		// with 400 invalid_forwarded_for — but it has to be reachable at a
		// bounded rate, or a malformed header is a way around the bound.
		source, _ := h.sourceIP(request)
		allowed, retryAfter := h.sourceLimit.Allow(source, time.Now())
		if !allowed {
			writer.Header().Set("Cache-Control", "no-store")
			deny(writer, request, retryAfter)
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func (h *Handler) GuardOpenAI(next http.Handler) http.Handler {
	return h.guardKey(next, func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("WWW-Authenticate", `Bearer realm="halro"`)
		writeError(writer, http.StatusUnauthorized, "invalid_api_key", "invalid API key", nil)
	})
}

// GuardAnthropic is GuardOpenAI for the endpoint that answers in Anthropic's
// envelope. A caller's SDK has to be able to parse the refusal.
func (h *Handler) GuardAnthropic(next http.Handler) http.Handler {
	return h.guardKey(next, func(writer http.ResponseWriter, request *http.Request) {
		writeAnthropicError(writer, http.StatusUnauthorized, "authentication_error",
			"invalid API key", request.Header.Get("X-Request-Id"))
	})
}

func (h *Handler) guardKey(next http.Handler, deny http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if h.authorizeKey == nil {
			next.ServeHTTP(writer, request)
			return
		}
		// Only a key that is present and definitely unknown is stopped here. A
		// missing one falls through so the endpoint keeps ownership of that
		// response, and anything this guard cannot settle is left to the
		// authoritative check behind it.
		key, present := anthropicGatewayKey(request.Header)
		if !present {
			next.ServeHTTP(writer, request)
			return
		}
		if projectBytes, err := h.authorizeKey(key); err == nil {
			// The endpoint behind applies the instance ceiling to the same body.
			// Wrapping here rather than replacing it there keeps both in force:
			// whichever is smaller stops the read first, and either way the
			// endpoint reports the *http.MaxBytesError as request_too_large.
			if projectBytes > 0 && projectBytes < h.maxRequestBytes {
				request.Body = http.MaxBytesReader(writer, request.Body, projectBytes)
			}
			next.ServeHTTP(writer, request)
			return
		}
		writer.Header().Set("Cache-Control", "no-store")
		deny(writer, request)
	})
}

// unimplementedHints name the endpoints callers most often probe for, so the
// answer says what to reach for instead of only what is missing. An SDK calling
// models.list() is the common case: it is the first thing many clients do, and
// a bare 404 tells the operator nothing about why their model list is empty.
var unimplementedHints = map[string]string{
	"/v1/models": "Halro does not implement /v1/models. " +
		"Applications address models by the public alias configured on their Project.",
}

// NotFound answers an unrouted path with the envelope every other error uses.
// The router's default is plain text, which an SDK's error type cannot parse,
// so a caller probing an endpoint Halro does not serve gets an opaque
// transport failure rather than the reason for it.
func (h *Handler) NotFound(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	message := "this endpoint is not implemented by Halro; " +
		"see docs/compatibility for the supported surface"
	if hint, ok := unimplementedHints[strings.TrimSuffix(request.URL.Path, "/")]; ok {
		message = hint
	}
	writeError(writer, http.StatusNotFound, "endpoint_not_implemented", message, nil)
}

// MethodNotAllowed answers a known path reached with the wrong verb, for the
// same reason NotFound exists.
func (h *Handler) MethodNotAllowed(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	writeError(writer, http.StatusMethodNotAllowed, "method_not_allowed",
		"this endpoint does not accept "+request.Method, nil)
}

// maxDecodeProblemBytes bounds what a rejection echoes back. One decoder
// problem quotes the content block type the caller sent, and a caller should
// not be able to size the response by sending a long one.
const maxDecodeProblemBytes = 512

// decodeProblem turns a decoder rejection into something the caller can act on.
//
// The decoders already say exactly what is wrong and where — "messages[3].role
// is invalid", "tools[2].strict=true cannot be represented losslessly" — but
// every one of those was being collapsed into "invalid request body" at the
// HTTP boundary. That reduced the compatibility promise, that unsupported
// fields are rejected rather than silently dropped, to a rejection the caller
// cannot act on without bisecting their payload field by field.
//
// The text is built from constants and indices, so passing it through leaks
// nothing the caller did not send.
func decodeProblem(err error) (string, *string) {
	message := strings.TrimSpace(err.Error())
	if message == "" {
		return "invalid request body", nil
	}
	if len(message) > maxDecodeProblemBytes {
		message = message[:maxDecodeProblemBytes] + "…"
	}
	if param := decodeParam(message); param != "" {
		return message, &param
	}
	return message, nil
}

// decodeParam recovers the field path a problem points at, so SDKs that surface
// error.param land the caller on the offending field. Problems are written as
// "<path> <what is wrong>", sometimes with the value attached as "<path>=<v>".
//
// Naming the wrong field is worse than naming none, because it sends the caller
// looking somewhere the problem is not. So a bare leading word only counts when
// something marks it as a reference rather than the opening of a sentence:
// either an attached value, or the dots and brackets of a path. Without one of
// those, "invalid character 'x' ..." would announce a field called "invalid".
func decodeParam(message string) string {
	head, _, _ := strings.Cut(message, " ")
	if head == "" {
		return ""
	}
	if field, _, valued := strings.Cut(head, "="); valued {
		if isFieldPath(field) {
			return field
		}
		return ""
	}
	if !strings.ContainsAny(head, ".[") || !isFieldPath(head) {
		return ""
	}
	return head
}

func isFieldPath(candidate string) bool {
	if candidate[0] < 'a' || candidate[0] > 'z' {
		return false
	}
	for _, symbol := range candidate {
		switch {
		case symbol >= 'a' && symbol <= 'z', symbol >= 'A' && symbol <= 'Z',
			symbol >= '0' && symbol <= '9',
			symbol == '_', symbol == '.', symbol == '[', symbol == ']':
		default:
			return false
		}
	}
	return true
}

func writeError(writer http.ResponseWriter, status int, code, message string, param *string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(openaiapi.ErrorEnvelope{
		Error: openaiapi.ErrorBody{
			Message: message,
			Type:    errorType(status),
			Param:   param,
			Code:    code,
		},
	})
}

func errorType(status int) string {
	switch {
	case status == http.StatusUnauthorized:
		return "authentication_error"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status >= 500:
		return "server_error"
	default:
		return "invalid_request_error"
	}
}
