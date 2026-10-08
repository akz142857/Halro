package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/governance"
)

func TestGovernanceExportLifecycleBoundsAdmissionAndRetriesDrain(t *testing.T) {
	var g governanceExportLifecycle
	ctx := context.Background()
	release, err := g.enter(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.enter(ctx); !errors.Is(err, errGovernanceExportBusy) {
		t.Fatalf("concurrent export=%v", err)
	}
	deadline, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel()
	if err := g.drain(deadline); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("drain released publication=%v", err)
	}
	if _, err := g.enter(ctx); !errors.Is(err, errGovernanceExportRetired) {
		t.Fatalf("retirement reopened=%v", err)
	}
	release()
	release()
	if err := g.drain(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := g.enter(ctx); !errors.Is(err, errGovernanceExportRetired) {
		t.Fatalf("late enter=%v", err)
	}
	var fresh governanceExportLifecycle
	c, stop := context.WithCancel(ctx)
	stop()
	if _, err := fresh.enter(c); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled enter=%v", err)
	}
	if err := fresh.drain(c); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled drain=%v", err)
	}
	if _, err := fresh.enter(ctx); !errors.Is(err, errGovernanceExportRetired) {
		t.Fatal("canceled withdrawal lost")
	}
}

type blockingExportResponse struct {
	*httptest.ResponseRecorder
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *blockingExportResponse) Write(data []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return w.ResponseRecorder.Write(data)
}

func TestGovernanceExportPublicLeaseKeepsResourcesThroughResponseAndClose(t *testing.T) {
	r, bootstrap, _ := openBootstrappedRuntime(t)
	defer r.Close()
	cookie, csrf := loginAdminForTest(t, r)
	request := adminRequest(t, http.MethodPost, "/admin/api/v1/governance/export", nil)
	request.AddCookie(cookie)
	request.Header.Set("X-CSRF-Token", csrf)
	w := &blockingExportResponse{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
	var release sync.Once
	defer release.Do(func() { close(w.release) })
	handled := make(chan struct{})
	go func() { r.adminRouter().ServeHTTP(w, request); close(handled) }()
	select {
	case <-w.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("export never reached final response")
	}
	deadline, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	if err := r.governance.exports.drain(deadline); !errors.Is(err, context.DeadlineExceeded) {
		cancel()
		t.Fatalf("early resource release=%v", err)
	}
	cancel()
	closed := make(chan error, 1)
	go func() { closed <- r.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close passed active publication=%v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if _, err := r.store.GetProject(context.Background(), bootstrap.ProjectID); err != nil {
		t.Fatalf("Metadata closed while export held=%v", err)
	}
	late := httptest.NewRecorder()
	lateReq := adminRequest(t, http.MethodPost, "/admin/api/v1/governance/export", nil)
	lateReq.AddCookie(cookie)
	lateReq.Header.Set("X-CSRF-Token", csrf)
	r.adminRouter().ServeHTTP(late, lateReq)
	if late.Code != http.StatusServiceUnavailable {
		t.Fatalf("retired endpoint accepted=%d %s", late.Code, late.Body.String())
	}
	release.Do(func() { close(w.release) })
	<-handled
	if w.Code != http.StatusCreated {
		t.Fatal(w.Code, w.Body.String())
	}
	var result struct {
		Directory string
		Manifest  governance.ExportManifest
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if _, err := governance.VerifyExport(result.Directory); err != nil {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}
