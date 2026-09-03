package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/failurecapture"
)

// The audit entry is the whole reason this endpoint is allowed to return what a
// caller wrote: it is the only admin GET that hands back a prompt, and the only
// one that records having done so. When the record cannot be written the read
// has to be refused, not completed quietly — an unaudited read of a prompt is
// exactly what the audit trail exists to make impossible, and "fail closed for
// corrupt or unavailable state" is a stated invariant of this repository.
func TestAPayloadReadIsRefusedWhenItCannotBeAudited(t *testing.T) {
	cfg := testConfig(t)
	cfg.Gateway.FailureCapture.Enabled = true
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapAdmin(context.Background(), cfg, "admin", []byte("correct horse battery staple")); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	seedFailures(t, runtime)
	if _, err := runtime.failureCapture.Put(failurecapture.Record{
		RequestID: "req_failed", ProjectID: "project_1", Outcome: "provider_error",
		Request: json.RawMessage(`{"model":"chat","messages":[{"role":"user","content":"SECRET PROMPT"}]}`),
	}); err != nil {
		t.Fatal(err)
	}
	cookie, _ := loginAdminForTest(t, runtime)

	// Take the audit log away underneath the handler. Whatever the cause —
	// a full disk, a permissions change, a corrupt tail — the endpoint sees the
	// same thing: it cannot record that this prompt was read.
	if err := runtime.audit.Close(); err != nil {
		t.Fatal(err)
	}

	response := authenticatedAdminGet(t, runtime, cookie, "/admin/api/v1/usage/failures/req_failed/payload")
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503; body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "SECRET PROMPT") {
		t.Fatalf("the prompt was returned on an unauditable read: %s", response.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != "audit_unavailable" {
		t.Fatalf("refusal code = %q, want audit_unavailable", body["code"])
	}
}
