package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

func normalizedDiskSummaryResponse(t *testing.T, response *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatal(response.Code, response.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if context, ok := result["time_context"].(map[string]any); !ok || len(context) == 0 {
		t.Fatal("missing time context", result)
	}
	delete(result, "time_context") // response evaluation time differs, facts do not
	return result
}

func TestUsageSummaryPublicDiskViewMatchesLegacyWithoutMetadataRollupMerge(t *testing.T) {
	for _, saved := range []bool{false, true} {
		t.Run(fmt.Sprint("saved=", saved), func(t *testing.T) {
			r, cookie := summaryRuntime(t, 2)
			if saved {
				r.saveUsageCheckpoint()
			}
			start, end := time.Now().UTC().AddDate(0, 0, -3).Format(summaryDateLayout), time.Now().UTC().AddDate(0, 0, 3).Format(summaryDateLayout)
			queries := []string{"", "group_by=project", "group_by=provider", "group_by=deployment", "group_by=requested_model", "group_by=provider_model", "provider_id=missing", "project_id=project_1"}
			for _, granularity := range []string{"day", "month", "year"} {
				for _, sort := range []string{"cost", "calls", "tokens", "errors", "success_rate"} {
					for _, order := range []string{"asc", "desc"} {
						queries = append(queries, "granularity="+granularity+"&group_by=project&sort="+sort+"&order="+order+"&limit=1")
					}
				}
			}
			want := make([]map[string]any, len(queries))
			path := func(query string) string {
				return "/admin/api/v1/usage/summary?start=" + start + "&end=" + end + "&" + query
			}
			for i, query := range queries {
				want[i] = normalizedDiskSummaryResponse(t, authenticatedAdminGet(t, r, cookie, path(query)))
			}
			installDiskUsageQueries(t, r)
			if r.usage.Aggregate != nil {
				t.Fatal("retained resident fallback")
			}
			for i, query := range queries {
				got := normalizedDiskSummaryResponse(t, authenticatedAdminGet(t, r, cookie, path(query)))
				if !reflect.DeepEqual(got, want[i]) {
					t.Fatal("disk Summary HTTP result differs", query, got, want[i])
				}
				if _, err := os.Lstat(usageSummaryScratchPath(r.config)); !os.IsNotExist(err) {
					t.Fatal("successful query retained scratch", err)
				}
			}
			// Removing the old checkpoint AND daily rows must not change the
			// selected disk result. It has its own atomic cumulative rollup cut.
			if err := r.store.ResetUsageDerivatives(); err != nil {
				t.Fatal(err)
			}
			got := normalizedDiskSummaryResponse(t, authenticatedAdminGet(t, r, cookie, path("")))
			if !reflect.DeepEqual(got, want[0]) {
				t.Fatal("disk Summary depended on old Metadata rollup", got, want[0])
			}
		})
	}
}

func TestUsageSummaryPublicDiskScratchPreservesExistingAuthorityAndLeftovers(t *testing.T) {
	for _, kind := range []string{"configured-future", "native-hardlink", "unowned-leftover"} {
		t.Run(kind, func(t *testing.T) {
			r, cookie := summaryRuntime(t, 1)
			installDiskUsageQueries(t, r)
			path := usageSummaryScratchPath(r.config)
			dbPath := filepath.Join(path, "summary.db")
			var preserve string
			switch kind {
			case "configured-future":
				old := r.config.Storage.MasterKey.File
				r.config.Storage.MasterKey.File = dbPath
				defer func() { r.config.Storage.MasterKey.File = old }()
			case "native-hardlink":
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(r.config.LedgerPath(), dbPath); err != nil {
					t.Fatal(err)
				}
				preserve = r.config.LedgerPath()
			case "unowned-leftover":
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(dbPath, []byte("unowned previous scratch"), 0o600); err != nil {
					t.Fatal(err)
				}
				preserve = dbPath
			}
			var before []byte
			if preserve != "" {
				var err error
				before, err = os.ReadFile(preserve)
				if err != nil {
					t.Fatal(err)
				}
			}
			response := authenticatedAdminGet(t, r, cookie, "/admin/api/v1/usage/summary")
			if response.Code != http.StatusServiceUnavailable {
				t.Fatal("scratch authority was overwritten or ignored", kind, response.Code, response.Body.String())
			}
			if preserve != "" {
				after, err := os.ReadFile(preserve)
				if err != nil || !reflect.DeepEqual(after, before) {
					t.Fatal("query changed preexisting authoritative/unowned bytes", err)
				}
			} else if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Fatal("configured namespace was created", err)
			}
		})
	}
}

func TestUsageSummaryPublicDiskFailureAndInvalidQueries(t *testing.T) {
	r, cookie := summaryRuntime(t, 1)
	p := installDiskUsageQueries(t, r)
	for _, query := range []string{"group_by=project&provider_id=provider_1", "granularity=week", "limit=101", "sort=bad", "order=bad", "group_by=" + url.QueryEscape(domain.RollupDimensionTotal)} {
		response := authenticatedAdminGet(t, r, cookie, "/admin/api/v1/usage/summary?"+query)
		if response.Code != http.StatusBadRequest {
			t.Fatal("invalid query became success", query, response.Code, response.Body.String())
		}
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	response := authenticatedAdminGet(t, r, cookie, "/admin/api/v1/usage/summary")
	if response.Code != http.StatusServiceUnavailable {
		t.Fatal("closed disk view fell back to old rows/empty success", response.Code, response.Body.String())
	}
}

func TestUsageSummaryPublicDiskResponseJoinsRuntimeCloseAfterScratchCleanup(t *testing.T) {
	r, cookie := summaryRuntime(t, 1)
	installDiskUsageQueries(t, r)
	request := adminRequest(t, http.MethodGet, "/admin/api/v1/usage/summary?group_by=project", nil)
	request.AddCookie(cookie)
	w := &blockingExportResponse{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	unblock := func() { once.Do(func() { close(w.release) }) }
	t.Cleanup(unblock)
	done := make(chan struct{})
	go func() { r.adminRouter().ServeHTTP(w, request); close(done) }()
	awaitDrainSignal(t, w.entered)
	if _, err := os.Lstat(usageSummaryScratchPath(r.config)); !os.IsNotExist(err) {
		t.Fatal("HTTP response started before real scratch cleanup", err)
	}
	closed := make(chan error, 1)
	go func() { closed <- r.Close() }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		r.backgroundWait.summaries.mu.Lock()
		retired := r.backgroundWait.summaries.retired
		r.backgroundWait.summaries.mu.Unlock()
		if retired {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Close did not retire Summary")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := r.store.ReadMetadataAppliedPosition(context.Background()); err != nil {
		t.Fatal("Metadata closed during an actual disk Summary response", err)
	}
	select {
	case err := <-closed:
		t.Fatal("Close returned before real HTTP Write", err)
	default:
	}
	late := authenticatedAdminGet(t, r, cookie, "/admin/api/v1/usage/summary")
	if late.Code != http.StatusServiceUnavailable {
		t.Fatal("retired Summary admitted a new response", late.Code)
	}
	unblock()
	<-done
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}
