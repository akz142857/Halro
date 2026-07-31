package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAdminRuntimeSettingsAreValidatedHotAppliedAndPersisted(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapAdmin(context.Background(), cfg, "admin", []byte("correct horse battery staple")); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	runtime, err := Open(context.Background(), cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	cookie, csrf := loginAdminForTest(t, runtime)

	get := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/admin/api/v1/settings", nil)
	request.AddCookie(cookie)
	runtime.adminRouter().ServeHTTP(get, request)
	if get.Code != http.StatusOK || get.Header().Get("ETag") != `"1"` {
		t.Fatalf("initial settings status=%d etag=%q body=%s", get.Code, get.Header().Get("ETag"), get.Body.String())
	}

	update := performAdminMutation(t, runtime, cookie, csrf, http.MethodPut,
		"/admin/api/v1/settings", `"1"`, map[string]int64{"health_probe_interval_seconds": 45})
	if update.Code != http.StatusOK || update.Header().Get("ETag") != `"2"` {
		t.Fatalf("update status=%d etag=%q body=%s", update.Code, update.Header().Get("ETag"), update.Body.String())
	}
	if runtime.healthProbeInterval() != 45*time.Second {
		t.Fatalf("setting was not hot applied: %s", runtime.healthProbeInterval())
	}

	stale := performAdminMutation(t, runtime, cookie, csrf, http.MethodPut,
		"/admin/api/v1/settings", `"1"`, map[string]int64{"health_probe_interval_seconds": 60})
	if stale.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale update status=%d body=%s", stale.Code, stale.Body.String())
	}
	invalid := performAdminMutation(t, runtime, cookie, csrf, http.MethodPut,
		"/admin/api/v1/settings", `"2"`, map[string]int64{"health_probe_interval_seconds": 1})
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid update status=%d body=%s", invalid.Code, invalid.Body.String())
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(context.Background(), cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.healthProbeInterval() != 45*time.Second {
		t.Fatalf("setting was not persisted: %s", reopened.healthProbeInterval())
	}
	stored := reopened.runtimeSettings.Load()
	encoded, err := json.Marshal(stored)
	if err != nil || stored.Revision != 2 || len(encoded) == 0 {
		t.Fatalf("unexpected stored settings: %#v err=%v", stored, err)
	}
}
