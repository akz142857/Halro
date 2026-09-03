package gatewayapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/akz142857/Halro/internal/gateway"
	"github.com/akz142857/Halro/internal/openaiapi"
	chi "github.com/go-chi/chi/v5"
)

// The handler discovers deferred support with a comma-ok type assertion, exactly
// as it does for Messages, so a signature drift silently leaves
// h.deferredResponses nil and all four deferred endpoints answer 501 instead of
// failing the build. The Messages half has carried this assertion since the
// mistake was made there; this one did not, and the endpoints it guards are the
// newest in the facade.
var _ DeferredResponsesService = (*gateway.Service)(nil)

// DeferredResponsesService is the deferred half of the Responses facade: submit
// on one connection, collect on another.
type DeferredResponsesService interface {
	SubmitDeferredResponse(context.Context, string, string, openaiapi.ResponseRequest) (openaiapi.Response, error)
	DeferredResponse(context.Context, string, string) (openaiapi.Response, time.Duration, error)
	CancelDeferredResponse(context.Context, string, string) (openaiapi.Response, error)
	DeleteDeferredResponse(context.Context, string, string) (openaiapi.ResponseDeleted, error)
}

// submitDeferredResponse answers a POST /v1/responses carrying background:true.
// It returns before the upstream is called, which is the whole request: the
// caller gets an identifier and drops the connection.
func (h *Handler) submitDeferredResponse(writer http.ResponseWriter, request *http.Request, key string, decoded openaiapi.ResponseRequest) {
	if h.deferredResponses == nil {
		writeError(writer, http.StatusNotImplemented, "unsupported_feature", "deferred responses are unavailable", nil)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), h.routeTimeout)
	defer cancel()
	response, err := h.deferredResponses.SubmitDeferredResponse(
		ctx, key, request.Header.Get("Idempotency-Key"), decoded,
	)
	if err != nil {
		h.writeDeferredError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, response)
}

func (h *Handler) GetDeferredResponse(writer http.ResponseWriter, request *http.Request) {
	h.deferredAction(writer, request, func(ctx context.Context, key, id string) (any, time.Duration, error) {
		response, retryAfter, err := h.deferredResponses.DeferredResponse(ctx, key, id)
		return response, retryAfter, err
	})
}

func (h *Handler) CancelDeferredResponse(writer http.ResponseWriter, request *http.Request) {
	h.deferredAction(writer, request, func(ctx context.Context, key, id string) (any, time.Duration, error) {
		response, err := h.deferredResponses.CancelDeferredResponse(ctx, key, id)
		return response, 0, err
	})
}

func (h *Handler) DeleteDeferredResponse(writer http.ResponseWriter, request *http.Request) {
	h.deferredAction(writer, request, func(ctx context.Context, key, id string) (any, time.Duration, error) {
		deleted, err := h.deferredResponses.DeleteDeferredResponse(ctx, key, id)
		return deleted, 0, err
	})
}

func (h *Handler) deferredAction(
	writer http.ResponseWriter,
	request *http.Request,
	act func(context.Context, string, string) (any, time.Duration, error),
) {
	writer.Header().Set("Cache-Control", "no-store")
	if h.deferredResponses == nil {
		writeError(writer, http.StatusNotImplemented, "unsupported_feature", "deferred responses are unavailable", nil)
		return
	}
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
	ctx, cancel := context.WithTimeout(request.Context(), h.routeTimeout)
	defer cancel()
	result, retryAfter, err := act(ctx, key, chi.URLParam(request, "responseID"))
	if err != nil {
		h.writeDeferredError(writer, err)
		return
	}
	// The polling cadence is the server's to set. It goes in a header rather
	// than in the Response object, because that object has an SDK contract and a
	// non-standard field in it is a field somebody's client will choke on.
	if retryAfter > 0 {
		writer.Header().Set("Retry-After", strconv.Itoa(int((retryAfter+time.Second-1)/time.Second)))
	}
	writeJSON(writer, http.StatusOK, result)
}

func (h *Handler) writeDeferredError(writer http.ResponseWriter, err error) {
	var failure *gateway.Error
	if errors.As(err, &failure) {
		writeGatewayError(writer, failure)
		return
	}
	writeError(writer, http.StatusInternalServerError, "internal_error", "internal server error", nil)
}

func writeJSON(writer http.ResponseWriter, status int, body any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(body)
}
