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

	"github.com/akz142857/Heimdall/internal/gateway"
	"github.com/akz142857/Heimdall/internal/openaiapi"
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
	return handler, nil
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
