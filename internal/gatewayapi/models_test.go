package gatewayapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/akz142857/Halro/internal/gateway"
	"github.com/akz142857/Halro/internal/openaiapi"
)

func (s *fakeService) Models(_ context.Context, key string) ([]openaiapi.Model, error) {
	s.calls++
	s.key = key
	if s.err != nil {
		return nil, s.err
	}
	if s.empty {
		return []openaiapi.Model{}, nil
	}
	return []openaiapi.Model{{ID: "chat", Object: "model", OwnedBy: "halro"}}, nil
}

func (s *fakeService) Model(_ context.Context, key, alias string) (openaiapi.Model, error) {
	s.calls++
	s.key = key
	if s.err != nil {
		return openaiapi.Model{}, s.err
	}
	s.lastAlias = alias
	if alias != "chat" {
		return openaiapi.Model{}, &gateway.Error{Code: "model_not_found", Message: "model is not available to this project", HTTPStatus: http.StatusNotFound}
	}
	return openaiapi.Model{ID: alias, Object: "model", OwnedBy: "halro"}, nil
}

var _ ModelsService = (*fakeService)(nil)

func modelsRouter(t *testing.T, service Service) http.Handler {
	t.Helper()
	handler, err := New(service, 1024)
	if err != nil {
		t.Fatal(err)
	}
	router := chi.NewRouter()
	router.NotFound(handler.NotFound)
	router.Get("/v1/models", handler.ListModels)
	router.Get("/v1/models/*", handler.GetModel)
	return router
}

// The shape an OpenAI SDK's models.list() parses: a list object whose data are
// model objects. The key reaches the service as the bearer token, untouched.
func TestListModelsAnswersInTheOpenAIListShape(t *testing.T) {
	service := &fakeService{}
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer gw_test")
	response := httptest.NewRecorder()
	modelsRouter(t, service).ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if service.key != "gw_test" {
		t.Fatalf("service saw key %q", service.key)
	}
	var list openaiapi.ModelList
	if err := json.Unmarshal(response.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if list.Object != "list" || len(list.Data) != 1 || list.Data[0].ID != "chat" || list.Data[0].Object != "model" {
		t.Fatalf("list=%+v", list)
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control=%q, a per-key answer must not be cached", response.Header().Get("Cache-Control"))
	}
}

func TestGetModelPassesTheAliasThroughAndKeepsTheServiceStatus(t *testing.T) {
	router := modelsRouter(t, &fakeService{})
	request := httptest.NewRequest(http.MethodGet, "/v1/models/chat", nil)
	request.Header.Set("Authorization", "Bearer gw_test")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	var model openaiapi.Model
	if err := json.Unmarshal(response.Body.Bytes(), &model); err != nil || response.Code != http.StatusOK || model.ID != "chat" {
		t.Fatalf("status=%d body=%s err=%v", response.Code, response.Body.String(), err)
	}

	request = httptest.NewRequest(http.MethodGet, "/v1/models/other", nil)
	request.Header.Set("Authorization", "Bearer gw_test")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	var envelope openaiapi.ErrorEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || response.Code != http.StatusNotFound || envelope.Error.Code != "model_not_found" {
		t.Fatalf("status=%d body=%s err=%v", response.Code, response.Body.String(), err)
	}
}

func TestListModelsRequiresABearerToken(t *testing.T) {
	service := &fakeService{}
	response := httptest.NewRecorder()
	modelsRouter(t, service).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if response.Code != http.StatusUnauthorized || response.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("status=%d WWW-Authenticate=%q", response.Code, response.Header().Get("WWW-Authenticate"))
	}
	if service.calls != 0 {
		t.Fatalf("service was reached without a bearer token")
	}
}

// A public alias is operator-supplied free text: nothing in domain validation
// forbids a slash, and vendor-prefixed names like "openai/gpt-4o" are a common
// convention. Both spellings have to reach the service as the same alias —
// before the wildcard route, the unescaped one fell through to the router's
// NotFound as endpoint_not_implemented (an id models.list() had just
// advertised), and the escaped one arrived at the service still encoded and so
// matched nothing.
func TestAnAliasContainingASlashIsRetrievableAtEitherSpelling(t *testing.T) {
	for _, path := range []string{"/v1/models/openai/gpt-4o", "/v1/models/openai%2Fgpt-4o"} {
		service := &fakeService{}
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer gw_test")
		response := httptest.NewRecorder()
		modelsRouter(t, service).ServeHTTP(response, request)

		if service.lastAlias != "openai/gpt-4o" {
			t.Fatalf("%s: the service saw %q", path, service.lastAlias)
		}
		// The fake serves only "chat", so the answer is the endpoint's own
		// 404 rather than the router's — which is the distinction that was
		// wrong: a caller must not be able to tell "no such alias" from "no
		// such endpoint".
		var envelope openaiapi.ErrorEnvelope
		if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("%s: %v (%s)", path, err, response.Body)
		}
		if response.Code != http.StatusNotFound || envelope.Error.Code != "model_not_found" {
			t.Fatalf("%s: status=%d code=%q", path, response.Code, envelope.Error.Code)
		}
	}
}

// The alias reaches the service decoded exactly once, whichever spelling the
// caller used.
//
// net/url populates RawPath only where the escaped form differs from
// re-encoding the decoded one, and chi returns RawPath when it has one and the
// already-decoded Path when it does not. Unescaping unconditionally therefore
// decodes twice for every request whose escaping round-trips — which is not an
// exotic shape, and the two failures it produces are of different kinds: one
// alias becomes unretrievable, and another silently resolves to a second alias
// the caller never named.
func TestAnAliasIsDecodedExactlyOnce(t *testing.T) {
	tests := []struct {
		name  string
		path  string
		alias string
	}{
		{name: "percent in the alias", path: "/v1/models/rate%25", alias: "rate%"},
		{name: "the escape sequence is itself the alias", path: "/v1/models/literal%252Fname", alias: "literal%2Fname"},
		{name: "slash escaped", path: "/v1/models/openai%2Fgpt-4o", alias: "openai/gpt-4o"},
		{name: "slash literal", path: "/v1/models/openai/gpt-4o", alias: "openai/gpt-4o"},
		{name: "space escaped", path: "/v1/models/a%20b", alias: "a b"},
		{name: "plain", path: "/v1/models/chat", alias: "chat"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &fakeService{}
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			request.Header.Set("Authorization", "Bearer gw_test")
			modelsRouter(t, service).ServeHTTP(httptest.NewRecorder(), request)
			if service.lastAlias != test.alias {
				t.Fatalf("the service saw %q, want %q", service.lastAlias, test.alias)
			}
		})
	}
}

// An empty alias reaches no service call and is refused as any unavailable
// alias is, so the two are indistinguishable from outside.
func TestAnEmptyAliasIsRefusedWithoutReachingTheService(t *testing.T) {
	service := &fakeService{}
	request := httptest.NewRequest(http.MethodGet, "/v1/models/", nil)
	request.Header.Set("Authorization", "Bearer gw_test")
	response := httptest.NewRecorder()
	modelsRouter(t, service).ServeHTTP(response, request)

	var envelope openaiapi.ErrorEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("%v (%s)", err, response.Body)
	}
	if response.Code != http.StatusNotFound || envelope.Error.Code != "model_not_found" {
		t.Fatalf("status=%d code=%q", response.Code, envelope.Error.Code)
	}
	if service.calls != 0 {
		t.Fatal("an empty alias reached the service")
	}
}

// The empty list has to serialize as [] rather than null: the OpenAI SDKs type
// `data` as a required array, so a null is a client-side deserialization
// failure rather than an empty page.
func TestListModelsSerializesAnEmptyProjectAsAnEmptyArray(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer gw_test")
	response := httptest.NewRecorder()
	modelsRouter(t, &fakeService{empty: true}).ServeHTTP(response, request)

	if body := strings.TrimSpace(response.Body.String()); body != `{"object":"list","data":[]}` {
		t.Fatalf("body=%s", body)
	}
}
