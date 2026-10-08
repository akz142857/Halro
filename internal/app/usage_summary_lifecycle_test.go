package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

func TestUsageSummaryHTTPResponseRetainsResourcesThroughClose(t *testing.T) {
	r, bootstrap, _ := openBootstrappedRuntime(t)
	defer r.Close()
	cookie, _ := loginAdminForTest(t, r)
	request := adminRequest(t, http.MethodGet, "/admin/api/v1/usage/summary?group_by=project", nil)
	request.AddCookie(cookie)
	w := &blockingExportResponse{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	defer once.Do(func() { close(w.release) })
	done := make(chan struct{})
	go func() { r.adminRouter().ServeHTTP(w, request); close(done) }()
	select {
	case <-w.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("response never started")
	}
	// Count/lifetime admission remains owned while the real Write is blocked.
	busy := authenticatedAdminGet(t, r, cookie, "/admin/api/v1/usage/summary")
	if busy.Code != http.StatusServiceUnavailable {
		t.Fatal("response allowed a concurrent Summary", busy.Code)
	}
	deadline, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	if err := r.backgroundWait.summaries.drain(deadline); !errors.Is(err, context.DeadlineExceeded) {
		cancel()
		t.Fatal("drain ignored actual response", err)
	}
	cancel()
	closed := make(chan error, 1)
	go func() { closed <- r.Close() }()
	select {
	case err := <-closed:
		t.Fatal("Close escaped Summary response", err)
	case <-time.After(20 * time.Millisecond):
	}
	if _, err := r.store.GetProject(context.Background(), bootstrap.ProjectID); err != nil {
		t.Fatal("Metadata closed before response", err)
	}
	late := authenticatedAdminGet(t, r, cookie, "/admin/api/v1/usage/summary")
	if late.Code != http.StatusServiceUnavailable {
		t.Fatal("retired Summary admitted", late.Code)
	}
	once.Do(func() { close(w.release) })
	<-done
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}
func TestUsageSummaryHTTPLabelsOnlyDisplayGroups(t *testing.T) {
	r, cookie := summaryRuntime(t, 1)
	if _, err := r.store.PutProject(context.Background(), domain.Project{ID: "project_1", Name: "Shown", Enabled: true}, 0, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.store.PutProject(context.Background(), domain.Project{ID: "unused_label_project", Name: "Unused", Enabled: true}, 0, nil); err != nil {
		t.Fatal(err)
	}
	response := authenticatedAdminGet(t, r, cookie, "/admin/api/v1/usage/summary?group_by=project")
	if response.Code != http.StatusOK {
		t.Fatal(response.Code, response.Body.String())
	}
	var body struct {
		Groups         []struct{ Key string }
		ResourceLabels map[string]string `json:"resource_labels"`
		TimeContext    map[string]any    `json:"time_context"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Groups) != 1 || len(body.ResourceLabels) != 1 || body.ResourceLabels[body.Groups[0].Key] == "" || body.ResourceLabels["unused_label_project"] != "" || len(body.TimeContext) == 0 {
		t.Fatal("label references/context", body)
	}
	response = authenticatedAdminGet(t, r, cookie, "/admin/api/v1/usage/summary")
	var ungrouped map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &ungrouped); err != nil {
		t.Fatal(err)
	}
	if string(ungrouped["resource_labels"]) != "{}" {
		t.Fatal("ungrouped enumerated resources", string(ungrouped["resource_labels"]))
	}
}
func TestUsageSummaryHTTPQueryAdmissionAndPermanentRetirement(t *testing.T) {
	r := &Runtime{}
	w := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/admin/api/v1/usage/summary?model="+strings.Repeat("a", 1<<20), nil)
	r.adminUsageSummary(w, request)
	if w.Code != http.StatusBadRequest {
		t.Fatal("query parsed before gate", w.Code)
	}
	release, err := r.backgroundWait.summaries.enter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.backgroundWait.summaries.drain(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := r.backgroundWait.summaries.enter(context.Background()); !errors.Is(err, errUsageSummaryUnavailable) {
		t.Fatal("canceled drain reopened", err)
	}
	release()
	release()
	if err := r.backgroundWait.summaries.drain(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func TestUsageSummaryHTTPResponseSizeAndEscaping(t *testing.T) {
	groups := []summaryGroup{{Key: "<&>\n\"\\\u2028\u2029\xff中文"}}
	body := usageSummaryResponse{Granularity: "day", Buckets: []summaryBucket{}, TimezoneChanges: []summaryTimezoneChange{}, Groups: &groups}
	raw, err := marshalUsageSummaryResponse(body)
	if err != nil || !json.Valid(raw) {
		t.Fatal("valid escaped response", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	groups[0].Key = strings.Repeat("<", usageSummaryResponseBytes/6+1)
	if raw, err := marshalUsageSummaryResponse(body); !errors.Is(err, errUsageSummaryResponseLimit) || raw != nil {
		t.Fatal("escaped size gate", len(raw), err)
	}
	groups[0].Key = strings.Repeat("w", 40<<10)
	if _, err := marshalUsageSummaryResponse(body); err != nil {
		t.Fatal("legal wide key refused", err)
	}
	body.Buckets = make([]summaryBucket, 401)
	if _, err := marshalUsageSummaryResponse(body); !errors.Is(err, errUsageSummaryResponseLimit) {
		t.Fatal("observed bucket ceiling", err)
	}
	body.Buckets = nil
	body.TimezoneChanges = make([]summaryTimezoneChange, 4097)
	if _, err := marshalUsageSummaryResponse(body); !errors.Is(err, errUsageSummaryResponseLimit) {
		t.Fatal("observed period ceiling", err)
	}
}
