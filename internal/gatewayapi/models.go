package gatewayapi

import (
	"context"
	"net/http"
	"net/url"
	"strings"

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
		alias, ok := pathAlias(request)
		if !ok {
			// Refused the way an alias the caller may not name is, because the
			// caller cannot tell the two apart and must not be able to.
			return nil, &gateway.Error{Code: "model_not_found", Message: "model is not available to this project", HTTPStatus: http.StatusNotFound}
		}
		return h.models.Model(ctx, key, alias)
	})
}

// pathAlias reads the alias out of the request path, decoding it exactly once.
//
// A public alias is operator-supplied free text — nothing in domain validation
// constrains its character set, and vendor-prefixed names like "openai/gpt-4o"
// are a common convention — so it is neither a single path segment nor
// necessarily URL-safe. Hence the wildcard route, for the same reason the admin
// invocation-target route is one: a {modelID} parameter answered 404
// endpoint_not_implemented for the unescaped spelling an SDK actually sends.
//
// Once is the whole difficulty. net/url populates RawPath only where the
// escaped form differs from re-encoding the decoded one, and chi hands back
// RawPath when it has one and the already-decoded Path when it does not. So
// unescaping unconditionally decodes twice for every request whose escaping
// round-trips, and that is not a rare shape: "/v1/models/rate%25" arrives with
// an empty RawPath, chi yields "rate%", and a second unescape fails on the
// trailing percent — an alias the list had just advertised, unretrievable. The
// worse half is silent: "/v1/models/literal%252Fname" yields "literal%2Fname",
// which a second unescape turns into "literal/name", answering for a different
// alias the caller never asked for and may separately hold.
func pathAlias(request *http.Request) (string, bool) {
	alias := chi.URLParam(request, "*")
	if request.URL.RawPath != "" {
		decoded, err := url.PathUnescape(alias)
		if err != nil {
			return "", false
		}
		alias = decoded
	}
	if strings.TrimSpace(alias) == "" {
		return "", false
	}
	return alias, true
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
		writeServiceError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}
