package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
)

// This verifies only the whole-handler lifecycle shared by actual selected HA
// Runtime. Cancellation must exit before any service/physical publication; the
// Linux TLS fixture independently covers those original native owners.
func TestHACheckpointRequestDrainCancellationPreservesAdmission(t *testing.T) {
	r := &Runtime{runtimeResourceOwnership: runtimeResourceOwnership{haSelectedState: &haSelectedRuntimeFoundation{}}, config: config.Config{ResourceBudget: &config.ResourceBudget{PhysicalTerminalReserve: true}}}
	entered, release, exited := make(chan struct{}), make(chan struct{}), make(chan struct{})
	handler := r.withMemoryCheckpointRequest(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	go func() {
		defer close(exited)
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/business", nil))
	}()
	<-entered
	defer func() { close(release); <-exited }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- r.CheckpointMemoryState(ctx) }()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for {
		r.liveCheckpoint.mu.Lock()
		paused := r.liveCheckpoint.paused != nil
		r.liveCheckpoint.mu.Unlock()
		if paused {
			break
		}
		select {
		case err := <-result:
			t.Fatal("checkpoint bypassed actual HA handler", err)
		case <-deadline.C:
			t.Fatal("original HA request was not joined")
		case <-time.After(time.Millisecond):
		}
	}
	denied := httptest.NewRecorder()
	r.withMemoryCheckpointRequest(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("fresh HA business admitted while original handler is live")
	})).ServeHTTP(denied, httptest.NewRequest(http.MethodGet, "/business", nil))
	if denied.Code != http.StatusServiceUnavailable || denied.Header().Get("Retry-After") == "" {
		t.Fatal("missing explicit maintenance refusal", denied.Code)
	}
	live := httptest.NewRecorder()
	r.withMemoryCheckpointRequest(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })).ServeHTTP(live, httptest.NewRequest(http.MethodGet, "/health/live", nil))
	if live.Code != http.StatusOK {
		t.Fatal("maintenance blocked liveness")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("cancelled HA request drain reached publication", err)
		}
	case <-time.After(time.Second):
		t.Fatal("original HA drain ignored cancellation")
	}
	resumed := httptest.NewRecorder()
	r.withMemoryCheckpointRequest(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) })).ServeHTTP(resumed, httptest.NewRequest(http.MethodGet, "/business", nil))
	if resumed.Code != http.StatusAccepted {
		t.Fatal("cancelled pre-publication drain did not resume admission")
	}
}
