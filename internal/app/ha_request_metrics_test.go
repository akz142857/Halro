package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/akz142857/Halro/internal/replication"
)

func TestHAWriteResponseMetricsCountServerOutcomesWithoutReads(t *testing.T) {
	runtime := &Runtime{replication: &replicationRuntime{role: replication.RolePrimary}}
	test := func(method, path string, status int, expected int) {
		t.Helper()
		handler := runtime.trackHAWriteResponses(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		}))
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, path, nil))
		if expected >= 0 && runtime.haWriteResponses[expected].Load() == 0 {
			t.Fatalf("%s %s status %d did not increment outcome %s", method, path, status, haWriteResponseOutcomes[expected])
		}
	}
	test(http.MethodPost, "/v1/chat/completions", http.StatusOK, 0)
	test(http.MethodPost, "/v1/messages", http.StatusUnauthorized, 1)
	test(http.MethodPost, "/v1/responses", http.StatusServiceUnavailable, 3)
	test(http.MethodPost, "/halro/v1/work-units/id/close", http.StatusGatewayTimeout, 4)
	test(http.MethodGet, "/v1/responses", http.StatusOK, -1)
	test(http.MethodPost, "/v1/messages/count_tokens", http.StatusOK, -1)
	test(http.MethodPost, "/halro/v1/work-units/id/extra/close", http.StatusOK, -1)
	var total uint64
	for index := range runtime.haWriteResponses {
		total += runtime.haWriteResponses[index].Load()
	}
	if total != 4 {
		t.Fatalf("tracked %d requests, want only four write routes", total)
	}
	runtime.replication.role = replication.RoleReplica
	test(http.MethodPost, "/v1/chat/completions", http.StatusServiceUnavailable, 2)
}

func TestHAWriteResponseMetricsPreserveStreamingAndRecordWriteFailure(t *testing.T) {
	runtime := &Runtime{replication: &replicationRuntime{role: replication.RolePrimary}}
	stream := runtime.trackHAWriteResponses(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("flushing writer lost streaming interface")
		}
		_, _ = w.Write([]byte("data: hello\n\n"))
		flusher.Flush()
	}))
	recorder := httptest.NewRecorder()
	stream.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
	if !recorder.Flushed || runtime.haWriteResponses[0].Load() != 1 {
		t.Fatal("stream flush or 2xx server result was lost")
	}
	failed := runtime.trackHAWriteResponses(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("response"))
	}))
	failed.ServeHTTP(&failingHAWriter{header: http.Header{}}, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
	if runtime.haWriteResponses[7].Load() != 1 {
		t.Fatal("write error did not supersede the implied 2xx status")
	}
	ctx, cancel := context.WithCancel(context.Background())
	canceled := runtime.trackHAWriteResponses(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		cancel()
	}))
	canceled.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(ctx))
	if runtime.haWriteResponses[6].Load() != 1 {
		t.Fatal("canceled request was counted as a clean 2xx")
	}
}

type failingHAWriter struct{ header http.Header }

func (w *failingHAWriter) Header() http.Header       { return w.header }
func (w *failingHAWriter) WriteHeader(int)           {}
func (w *failingHAWriter) Write([]byte) (int, error) { return 0, errors.New("client disconnected") }
