package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

// Exercise the actual selected application owner supplied by the migration /
// restart fixture. A checkpoint must join whole requests, retain cancelability,
// and then resume the SAME business managers with the newly published roots.
func assertSelectedRuntimeLiveCheckpoint(t *testing.T, ctx context.Context, r *Runtime) uint64 {
	t.Helper()
	before := r.selectedState.generation.Seal.Revision
	accounting, metadata := r.accounting, r.store
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
		<-done
	}()
	handler := r.withMemoryCheckpointRequest(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	go func() {
		defer close(done)
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/business", nil))
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	checkpointCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- r.CheckpointMemoryState(checkpointCtx) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		r.liveCheckpoint.mu.Lock()
		paused := r.liveCheckpoint.paused != nil
		r.liveCheckpoint.mu.Unlock()
		if paused {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("actual checkpoint did not pause request admission")
		}
		select {
		case err := <-result:
			t.Fatal("checkpoint returned before request drain", err)
		case <-time.After(time.Millisecond):
		}
	}
	denied := httptest.NewRecorder()
	r.withMemoryCheckpointRequest(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("business entered during checkpoint") })).ServeHTTP(denied, httptest.NewRequest(http.MethodGet, "/business", nil))
	if denied.Code != http.StatusServiceUnavailable || denied.Header().Get("Retry-After") == "" {
		t.Fatal("new work was not explicitly shed", denied.Code)
	}
	health := httptest.NewRecorder()
	r.withMemoryCheckpointRequest(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })).ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/health/live", nil))
	if health.Code != http.StatusOK {
		t.Fatal("maintenance blocked liveness")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("canceled checkpoint", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if r.selectedState.generation.Seal.Revision != before {
		t.Fatal("canceled request drain published a common seal")
	}
	close(release)
	<-done
	for cycle := 0; cycle < 2; cycle++ {
		if err := r.CheckpointMemoryState(ctx); err != nil {
			t.Fatal("actual online common publication", err)
		}
		if r.accounting != accounting || r.store != metadata || r.draining.Load() {
			t.Fatal("online checkpoint retired/replaced the business managers")
		}
		if _, err := r.store.PutProject(ctx, domain.Project{ID: [2]string{"prj_live_checkpoint_0", "prj_live_checkpoint_1"}[cycle], Name: "Live checkpoint remains writable", Enabled: true}, 0, nil); err != nil {
			t.Fatal("actual DAO failed after online checkpoint", err)
		}
	}
	return r.selectedState.generation.Seal.Revision - before
}
