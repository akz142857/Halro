package gatewayapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/go-chi/chi/v5"
)

// fakeDeferred is the deferred half of the facade only. It is a separate fake
// from fakeService on purpose: the handler discovers this interface with a
// comma-ok assertion, so a fake that satisfies it by accident tells us nothing.
type fakeDeferred struct {
	fakeService
	submitted   openaiapi.ResponseRequest
	key         string
	id          string
	idempotency string
	retryAfter  time.Duration
	err         error
	deleted     bool
	cancelled   bool
}

var _ DeferredResponsesService = (*fakeDeferred)(nil)

func (f *fakeDeferred) SubmitDeferredResponse(
	_ context.Context, key, idempotencyKey string, request openaiapi.ResponseRequest,
) (openaiapi.Response, error) {
	f.key, f.idempotency, f.submitted = key, idempotencyKey, request
	if f.err != nil {
		return openaiapi.Response{}, f.err
	}
	return openaiapi.Response{ID: "resp_1", Object: "response", Status: "queued", Background: true, Model: request.Model}, nil
}

func (f *fakeDeferred) DeferredResponse(_ context.Context, key, id string) (openaiapi.Response, time.Duration, error) {
	f.key, f.id = key, id
	if f.err != nil {
		return openaiapi.Response{}, 0, f.err
	}
	return openaiapi.Response{ID: id, Object: "response", Status: "queued", Background: true}, f.retryAfter, nil
}

func (f *fakeDeferred) CancelDeferredResponse(_ context.Context, key, id string) (openaiapi.Response, error) {
	f.key, f.id, f.cancelled = key, id, true
	if f.err != nil {
		return openaiapi.Response{}, f.err
	}
	return openaiapi.Response{ID: id, Object: "response", Status: "cancelled", Background: true}, nil
}

func (f *fakeDeferred) DeleteDeferredResponse(_ context.Context, key, id string) (openaiapi.ResponseDeleted, error) {
	f.key, f.id, f.deleted = key, id, true
	if f.err != nil {
		return openaiapi.ResponseDeleted{}, f.err
	}
	return openaiapi.ResponseDeleted{ID: id, Object: "response.deleted", Deleted: true}, nil
}

// deferredRequest builds a request the way chi delivers one, with the URL
// parameter the handler reads populated.
func deferredRequest(t *testing.T, method, target, id string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(method, target, nil)
	request.Header.Set("Authorization", "Bearer gw_test")
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("responseID", id)
	return request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, routeContext))
}

// The four deferred endpoints had no HTTP-layer test at all: everything below
// the handler was covered and the handler itself was not, so a wiring mistake
// would have reached an operator rather than the suite.
func TestDeferredSubmissionIsForwardedWithItsIdempotencyKey(t *testing.T) {
	service := &fakeDeferred{}
	handler, err := New(service, 2048)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"chat","input":"hello","background":true}`))
	request.Header.Set("Authorization", "Bearer gw_test")
	request.Header.Set("Idempotency-Key", "idem-1")
	response := httptest.NewRecorder()
	handler.Responses(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if service.key != "gw_test" || service.idempotency != "idem-1" || !service.submitted.Background {
		t.Fatalf("submission was not forwarded: %#v", service)
	}
	var body openaiapi.Response
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.ID != "resp_1" || body.Status != "queued" || !body.Background {
		t.Fatalf("submission response = %#v", body)
	}
}

func TestDeferredRetrievalCarriesRetryAfterInAHeaderNotTheObject(t *testing.T) {
	service := &fakeDeferred{retryAfter: 3 * time.Second}
	handler, err := New(service, 2048)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.GetDeferredResponse(response, deferredRequest(t, http.MethodGet, "/v1/responses/resp_1", "resp_1"))

	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if service.id != "resp_1" {
		t.Fatalf("the identifier was not read from the path: %q", service.id)
	}
	if got := response.Header().Get("Retry-After"); got != "3" {
		t.Fatalf("Retry-After=%q, want 3", got)
	}
	// The Response object has an SDK contract; the polling cadence is not part
	// of it and must not appear inside the body.
	if strings.Contains(response.Body.String(), "retry_after") {
		t.Fatalf("the polling cadence leaked into the response object: %s", response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("a deferred answer was cacheable")
	}
}

func TestDeferredCancelAndDeleteReachTheirOwnOperations(t *testing.T) {
	service := &fakeDeferred{}
	handler, err := New(service, 2048)
	if err != nil {
		t.Fatal(err)
	}
	cancel := httptest.NewRecorder()
	handler.CancelDeferredResponse(cancel, deferredRequest(t, http.MethodPost, "/v1/responses/resp_1/cancel", "resp_1"))
	if cancel.Code != http.StatusOK || !service.cancelled {
		t.Fatalf("cancel status=%d cancelled=%v body=%s", cancel.Code, service.cancelled, cancel.Body.String())
	}

	remove := httptest.NewRecorder()
	handler.DeleteDeferredResponse(remove, deferredRequest(t, http.MethodDelete, "/v1/responses/resp_1", "resp_1"))
	if remove.Code != http.StatusOK || !service.deleted {
		t.Fatalf("delete status=%d deleted=%v body=%s", remove.Code, service.deleted, remove.Body.String())
	}
	var deleted openaiapi.ResponseDeleted
	if err := json.Unmarshal(remove.Body.Bytes(), &deleted); err != nil {
		t.Fatal(err)
	}
	if deleted.Object != "response.deleted" || !deleted.Deleted {
		t.Fatalf("delete response = %#v", deleted)
	}
}

func TestDeferredEndpointsRefuseAnAnonymousCaller(t *testing.T) {
	handler, err := New(&fakeDeferred{}, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for _, probe := range []struct {
		name string
		call func(http.ResponseWriter, *http.Request)
	}{
		{"get", handler.GetDeferredResponse},
		{"cancel", handler.CancelDeferredResponse},
		{"delete", handler.DeleteDeferredResponse},
	} {
		request := deferredRequest(t, http.MethodGet, "/v1/responses/resp_1", "resp_1")
		request.Header.Del("Authorization")
		response := httptest.NewRecorder()
		probe.call(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("%s: status=%d, want 401", probe.name, response.Code)
		}
		if response.Header().Get("WWW-Authenticate") == "" {
			t.Fatalf("%s: a 401 carried no challenge", probe.name)
		}
	}
}

// A service that does not implement the deferred half answers 501 rather than
// panicking on a nil interface — and the compile-time assertion in
// deferred_response.go is what keeps the production service from ever being
// that service by accident.
func TestDeferredEndpointsReportUnavailableWhenTheServiceLacksThem(t *testing.T) {
	handler, err := New(&fakeService{}, 2048)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.GetDeferredResponse(response, deferredRequest(t, http.MethodGet, "/v1/responses/resp_1", "resp_1"))
	if response.Code != http.StatusNotImplemented {
		t.Fatalf("status=%d, want 501", response.Code)
	}
}
