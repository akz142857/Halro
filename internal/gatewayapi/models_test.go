package gatewayapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	return []openaiapi.Model{{ID: "chat", Object: "model", OwnedBy: "halro"}}, nil
}

func (s *fakeService) Model(_ context.Context, key, alias string) (openaiapi.Model, error) {
	s.calls++
	s.key = key
	if s.err != nil {
		return openaiapi.Model{}, s.err
	}
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
	router.Get("/v1/models", handler.ListModels)
	router.Get("/v1/models/{modelID}", handler.GetModel)
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
