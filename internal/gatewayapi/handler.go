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

	"github.com/akz142857/Heimdall/internal/anthropicapi"
	"github.com/akz142857/Heimdall/internal/gateway"
	"github.com/akz142857/Heimdall/internal/id"
	"github.com/akz142857/Heimdall/internal/openaiapi"
	"github.com/akz142857/Heimdall/internal/provider"
	"github.com/akz142857/Heimdall/internal/requestmeta"
	"github.com/akz142857/Heimdall/internal/sse"
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

type MessagesService interface {
	Messages(context.Context, string, anthropicapi.MessageRequest) (anthropicapi.Message, error)
	MessagesStream(context.Context, string, anthropicapi.MessageRequest, func(anthropicapi.StreamEvent) error) error
	MessagesNative(context.Context, string, string, anthropicapi.MessageRequest) (anthropicapi.Message, error)
	MessagesNativeStream(context.Context, string, string, anthropicapi.MessageRequest, func(anthropicapi.RawStreamEvent) error) error
}

func (h *Handler) Responses(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	request, ok := h.withSourceIP(writer, request)
	if !ok {
		return
	}
	key, ok := bearerToken(request.Header.Get("Authorization"))
	if !ok {
		writer.Header().Set("WWW-Authenticate", `Bearer realm="heimdall"`)
		writeError(writer, http.StatusUnauthorized, "invalid_api_key", "missing or invalid bearer token", nil)
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, h.maxRequestBytes)
	decoded, err := openaiapi.DecodeResponseRequest(json.NewDecoder(request.Body))
	if err != nil {
		code := "invalid_request_error"
		message := "invalid request body"
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			code = "request_too_large"
			message = "request body exceeds the configured limit"
		}
		writeError(writer, http.StatusBadRequest, code, message, nil)
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
	request, ok := h.withSourceIP(writer, request)
	if !ok {
		return
	}
	key, ok := bearerToken(request.Header.Get("Authorization"))
	if !ok {
		writer.Header().Set("WWW-Authenticate", `Bearer realm="heimdall"`)
		writeError(writer, http.StatusUnauthorized, "invalid_api_key", "missing or invalid bearer token", nil)
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, h.maxRequestBytes)
	decoded, err := openaiapi.DecodeEmbeddingRequest(json.NewDecoder(request.Body))
	if err != nil {
		code := "invalid_request_error"
		message := "invalid request body"
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			code = "request_too_large"
			message = "request body exceeds the configured limit"
		}
		writeError(writer, http.StatusBadRequest, code, message, nil)
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
	service         Service
	responses       ResponsesService
	messages        MessagesService
	phase2          Phase2Service
	maxRequestBytes int64
	routeTimeout    time.Duration
	streamTimeout   time.Duration
	writeTimeout    time.Duration
	trustProxy      bool
	trustedProxies  []netip.Prefix
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
	}
	handler.responses, _ = service.(ResponsesService)
	handler.messages, _ = service.(MessagesService)
	handler.phase2, _ = service.(Phase2Service)
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
	if strings.TrimSpace(request.Header.Get(anthropicapi.BetaHeader)) != "" {
		writeAnthropicError(writer, http.StatusBadRequest, "invalid_request_error", "anthropic-beta is not supported by this profile", requestID)
		return
	}
	mode, err := anthropicapi.ParseExecutionMode(request.Header.Get(anthropicapi.RouteModeHeader))
	if err != nil {
		writeAnthropicError(writer, http.StatusBadRequest, "invalid_request_error", err.Error(), requestID)
		return
	}
	key, ok := anthropicGatewayKey(request.Header)
	if !ok {
		writer.Header().Set("WWW-Authenticate", `Bearer realm="heimdall"`)
		writeAnthropicError(writer, http.StatusUnauthorized, "authentication_error", "missing or invalid gateway key", requestID)
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, h.maxRequestBytes)
	decoded, err := anthropicapi.DecodeMessageRequest(request.Body)
	if err != nil {
		status, kind, message := http.StatusBadRequest, "invalid_request_error", "invalid request body"
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
			h.messagesNativeStream(writer, request.WithContext(ctx), key, version, decoded, requestID)
			return
		}
		ctx, cancel := context.WithTimeout(request.Context(), h.routeTimeout)
		defer cancel()
		if h.messages == nil {
			writeAnthropicError(writer, http.StatusNotImplemented, "api_error", "Messages API is unavailable", requestID)
			return
		}
		response, callErr := h.messages.MessagesNative(ctx, key, version, decoded)
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

func (h *Handler) messagesNativeStream(writer http.ResponseWriter, request *http.Request, key, version string, decoded anthropicapi.MessageRequest, requestID string) {
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
	err := h.messages.MessagesNativeStream(request.Context(), key, version, decoded, func(event anthropicapi.RawStreamEvent) error {
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
	request, ok := h.withSourceIP(writer, request)
	if !ok {
		return
	}
	key, ok := bearerToken(request.Header.Get("Authorization"))
	if !ok {
		writer.Header().Set("WWW-Authenticate", `Bearer realm="heimdall"`)
		writeError(writer, http.StatusUnauthorized, "invalid_api_key", "missing or invalid bearer token", nil)
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, h.maxRequestBytes)
	decoded, err := openaiapi.DecodeChatCompletionRequest(json.NewDecoder(request.Body))
	if err != nil {
		code := "invalid_request_error"
		message := "invalid request body"
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			code = "request_too_large"
			message = "request body exceeds the configured limit"
		}
		writeError(writer, http.StatusBadRequest, code, message, nil)
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
	if strings.TrimSpace(request.Header.Get("X-Forwarded-For")) == "" {
		return netip.Addr{}, false
	}
	parts := strings.Split(request.Header.Get("X-Forwarded-For"), ",")
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
