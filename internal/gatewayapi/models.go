package gatewayapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/akz142857/Halro/internal/gateway"
	"github.com/akz142857/Halro/internal/openaiapi"
)

// ModelsService answers the OpenAI Models API from the gateway's own
// configuration. Pinned to the production service the way Messages is, so a
// signature drift fails the build rather than leaving models.list() answering
// 501 with nothing noticing.
type ModelsService interface {
	Models(context.Context, string) ([]openaiapi.Model, error)
	Model(context.Context, string, string) (openaiapi.Model, error)
}

var _ ModelsService = (*gateway.Service)(nil)

func (h *Handler) ListModels(writer http.ResponseWriter, request *http.Request) {
	h.modelsAction(writer, request, func(ctx context.Context, key string) (any, error) {
		models, err := h.models.Models(ctx, key)
		if err != nil {
			return nil, err
		}
		return openaiapi.ModelList{Object: "list", Data: models}, nil
	})
}

func (h *Handler) GetModel(writer http.ResponseWriter, request *http.Request) {
	h.modelsAction(writer, request, func(ctx context.Context, key string) (any, error) {
		return h.models.Model(ctx, key, chi.URLParam(request, "modelID"))
	})
}

// modelsAction is deferredAction's shape for a read that reaches no upstream:
// the same request-ID and source-IP handling, the same bearer requirement, and
// no timeout of its own because there is nothing to wait for.
func (h *Handler) modelsAction(writer http.ResponseWriter, request *http.Request, act func(context.Context, string) (any, error)) {
	writer.Header().Set("Cache-Control", "no-store")
	if h.models == nil {
		writeError(writer, http.StatusNotImplemented, "unsupported_feature", "model listing is unavailable", nil)
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
	result, err := act(request.Context(), key)
	if err != nil {
		var failure *gateway.Error
		if errors.As(err, &failure) {
			writeGatewayError(writer, failure)
			return
		}
		writeError(writer, http.StatusInternalServerError, "internal_error", "internal server error", nil)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}
