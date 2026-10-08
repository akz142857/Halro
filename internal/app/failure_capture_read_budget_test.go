package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/failurecapture"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

type blockedCaptureResponse struct {
	*httptest.ResponseRecorder
	entered, resume chan struct{}
}

func (w *blockedCaptureResponse) Write(raw []byte) (int, error) {
	close(w.entered)
	<-w.resume
	return w.ResponseRecorder.Write(raw)
}

func TestFailureCaptureReadBudgetRefusesBeforeReadAndRetainsAuditedResponse(t *testing.T) {
	r, _, _ := openBootstrappedRuntimeShaped(t, "gpt-test", func(cfg *config.Config) { cfg.Gateway.FailureCapture.Enabled = true })
	seedFailures(t, r)
	if written, err := r.failureCapture.Put(failurecapture.Record{RequestID: "req_failed", ProjectID: "project_1", Outcome: "provider_error", Request: json.RawMessage(`{"model":"chat"}`)}); err != nil || !written {
		t.Fatal(written, err)
	}
	cookie, _ := loginAdminForTest(t, r)
	b := runtimeRAMBudgetFixture()
	b.IORAMBytes = 64 << 20 // Read and the independent 32 MiB audit owner overlap.
	manager, err := resourcebudget.New(b.Limits(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	r.resourcePressure = &resourcePressureRuntime{budget: manager}
	held, err := manager.TryAcquire(resourcebudget.IO, resourcebudget.Cost{RAM: 55 << 20})
	if err != nil {
		t.Fatal(err)
	}
	before := r.audit.Summary()
	response := authenticatedAdminGet(t, r, cookie, "/admin/api/v1/usage/failures/req_failed/payload")
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "resource_budget_unavailable") || r.audit.Summary() != before {
		t.Fatal("unpaid capture read reached disclosure or audit", response.Code, response.Body.String())
	}
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest(http.MethodGet, "/admin/api/v1/usage/failures/req_failed/payload", nil).WithContext(ctx)
	request.AddCookie(cookie)
	writer := &blockedCaptureResponse{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), resume: make(chan struct{})}
	done := make(chan struct{})
	go func() { defer close(done); r.adminRouter().ServeHTTP(writer, request) }()
	select {
	case <-writer.entered:
	case <-time.After(10 * time.Second):
		close(writer.resume)
		<-done
		t.Fatal("audited response did not reach output")
	}
	cancel()
	if s := manager.Snapshot(); s.Classes[resourcebudget.IO].RAM != r.failureCapture.ReadRAMBytes() || s.NormalClaims != 1 {
		close(writer.resume)
		<-done
		t.Fatal("blocked/cancelled output refunded retained plaintext", s)
	}
	close(writer.resume)
	<-done
	if writer.Code != http.StatusOK || !strings.Contains(writer.Body.String(), `"model":"chat"`) || manager.Snapshot().Used != (resourcebudget.Cost{}) {
		t.Fatal("response retirement leaked owner or changed disclosure", writer.Code, writer.Body.String(), manager.Snapshot())
	}
}
