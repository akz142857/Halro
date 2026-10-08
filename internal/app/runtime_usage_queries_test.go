package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/usage"
)

// Select a disk-only query view after the old worker generation has joined.
// This exercises real public handlers against authenticated native history;
// it does not claim the default startup/maintenance migration is complete.
func installDiskUsageQueries(t *testing.T, r *Runtime) *usage.DiskProjection {
	t.Helper()
	ctx := context.Background()
	r.stopBackgroundWork()
	if err := r.usageCollector.Retire(ctx); err != nil {
		t.Fatal(err)
	}
	if r.usage.owner != nil {
		if err := r.usage.closeOwner(); err != nil {
			t.Fatal(err)
		}
		exporter, err := usage.NewExporterWithOptions(r.config.UsagePath(), usage.Options{Format: r.config.Usage.ExportFormat})
		if err != nil {
			t.Fatal(err)
		}
		r.usageExporter = exporter
	}
	lease, err := r.ledger.BeginMaintenance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	view, collector, err := prepareStandaloneUsage(ctx, r.config, r.ledger, r.accountingState.projection, lease, r.resourcePressure)
	if err = errors.Join(err, lease.Close()); err != nil {
		t.Fatal(err)
	}
	r.usage, r.usageCollector = view, collector
	p := view.disk

	return p
}

func TestUsagePublicQueriesSelectDiskWithoutAggregateFallback(t *testing.T) {
	r, cookie := openSeededRuntime(t)
	ctx := context.Background()
	if err := r.usageCollector.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	wantAttempts, _ := r.usage.queryAttempts(ctx, usage.AttemptQuery{Limit: 100}, "")
	wantFailures, _ := r.usage.queryFailures(ctx, usage.FailureQuery{Limit: 100}, "")
	wantDetail, _, detailErr := r.usage.requestDetail(ctx, "req_failed")
	if detailErr != nil {
		t.Fatal(detailErr)
	}
	installDiskUsageQueries(t, r)
	if r.usage.Aggregate != nil {
		t.Fatal("disk query retained a resident fallback")
	}
	var attempts struct {
		Items []usage.AttemptEvent `json:"items"`
		Next  string               `json:"next_cursor"`
	}
	response := authenticatedAdminGet(t, r, cookie, "/admin/api/v1/usage?limit=100")
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &attempts) != nil || !reflect.DeepEqual(attempts.Items, wantAttempts.Attempts) {
		t.Fatal("selected attempt view differs", response.Code, response.Body.String(), wantAttempts)
	}
	var failures struct{ Items []usage.RequestFailure }
	response = authenticatedAdminGet(t, r, cookie, "/admin/api/v1/usage/failures?limit=100")
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &failures) != nil || !reflect.DeepEqual(failures.Items, wantFailures.Failures) {
		t.Fatal("selected failed-request view differs", response.Code, response.Body.String(), wantFailures)
	}
	response = authenticatedAdminGet(t, r, cookie, "/admin/api/v1/usage/requests/req_failed")
	var detail usage.RequestDetail
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &detail) != nil || !reflect.DeepEqual(detail, wantDetail) {
		t.Fatal("selected complete detail differs", response.Code, response.Body.String(), wantDetail)
	}
	response = authenticatedAdminGet(t, r, cookie, "/admin/api/v1/usage/requests/missing")
	if response.Code != http.StatusNotFound {
		t.Fatal("missing detail was manufactured", response.Code, response.Body.String())
	}
}

func TestUsagePublicDiskCursorAndErrors(t *testing.T) {
	r, cookie := openSeededRuntime(t)
	p := installDiskUsageQueries(t, r)
	path := "/admin/api/v1/usage?limit=1"
	first := authenticatedAdminGet(t, r, cookie, path)
	var page struct {
		Items []usage.AttemptEvent `json:"items"`
		Next  string               `json:"next_cursor"`
	}
	if first.Code != http.StatusOK || json.Unmarshal(first.Body.Bytes(), &page) != nil || len(page.Items) != 1 || page.Next == "" {
		t.Fatal(first.Code, first.Body.String())
	}
	response := authenticatedAdminGet(t, r, cookie, path+"&cursor="+url.QueryEscape(page.Next))
	var next struct{ Items []usage.AttemptEvent }
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &next) != nil || len(next.Items) != 1 || next.Items[0].Sequence >= page.Items[0].Sequence {
		t.Fatal("HTTP cursor lost strict progress", response.Code, response.Body.String())
	}
	for _, path := range []string{
		"/admin/api/v1/usage?limit=1000",
		"/admin/api/v1/usage?account_region_id=bad",
		"/admin/api/v1/usage?cursor=garbage",
		"/admin/api/v1/usage?limit=2&cursor=" + url.QueryEscape(page.Next),
		"/admin/api/v1/usage/failures?cursor=" + url.QueryEscape(page.Next),
	} {
		response := authenticatedAdminGet(t, r, cookie, path)
		if response.Code != http.StatusBadRequest {
			t.Fatal("invalid query/token was accepted", path, response.Code, response.Body.String())
		}
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/admin/api/v1/usage", "/admin/api/v1/usage/failures", "/admin/api/v1/usage/requests/req_failed"} {
		response := authenticatedAdminGet(t, r, cookie, path)
		if response.Code != http.StatusServiceUnavailable {
			t.Fatal("failed selected owner became empty/legacy success", path, response.Code, response.Body.String())
		}
	}
}

func TestUsagePublicResponseJoinsDirectCloseAndWithdrawsLateReads(t *testing.T) {
	r, cookie := openSeededRuntime(t)
	installDiskUsageQueries(t, r)
	request := adminRequest(t, http.MethodGet, "/admin/api/v1/usage", nil)
	request.AddCookie(cookie)
	w := &blockingExportResponse{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	unblock := func() { once.Do(func() { close(w.release) }) }
	t.Cleanup(unblock)
	done := make(chan struct{})
	go func() { r.adminRouter().ServeHTTP(w, request); close(done) }()
	awaitDrainSignal(t, w.entered)
	busy := authenticatedAdminGet(t, r, cookie, "/admin/api/v1/usage/failures")
	if busy.Code != http.StatusServiceUnavailable {
		t.Fatal("busy query response queued", busy.Code)
	}
	closed := make(chan error, 1)
	go func() { closed <- r.Close() }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		r.usage.reads.mu.Lock()
		retired := r.usage.reads.retired
		r.usage.reads.mu.Unlock()
		if retired {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Close did not withdraw query admission")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := r.store.ReadMetadataAppliedPosition(context.Background()); err != nil {
		t.Fatal("Metadata closed while an actual response remained owned", err)
	}
	select {
	case err := <-closed:
		t.Fatal("Close passed a blocked response", err)
	default:
	}
	late := authenticatedAdminGet(t, r, cookie, "/admin/api/v1/usage/requests/req_failed")
	if late.Code != http.StatusServiceUnavailable {
		t.Fatal("late response admitted", late.Code)
	}
	unblock()
	<-done
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}
