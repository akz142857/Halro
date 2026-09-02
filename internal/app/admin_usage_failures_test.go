package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/failurecapture"
	"github.com/akz142857/Halro/internal/ledger"
)

type failureRow struct {
	RequestID   string `json:"request_id"`
	Outcome     string `json:"outcome"`
	Attempts    int64  `json:"attempts"`
	LastFailure *struct {
		ErrorClass     string `json:"error_class"`
		ProviderStatus int    `json:"provider_status"`
		DeploymentID   string `json:"deployment_id"`
	} `json:"last_failure"`
}

type failuresEnvelope struct {
	Items      []failureRow `json:"items"`
	NextCursor string       `json:"next_cursor"`
}

// seedFailures applies one succeeded, one provider-failed and one rejected
// request straight into the runtime's aggregate. Driving real traffic through
// the gateway would exercise the accounting path this endpoint deliberately
// only reads from, and would not let the test choose a rejection.
func seedFailures(t *testing.T, runtime *Runtime) {
	t.Helper()
	now := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)
	var sequence uint64
	apply := func(event ledger.Event) {
		t.Helper()
		sequence++
		if err := runtime.usage.Apply(ledger.Record{
			Sequence: sequence, Offset: int64(sequence * 100), Event: event,
		}); err != nil {
			t.Fatal(err)
		}
	}
	request := func(requestID, outcome string, at time.Time, attempts ...ledger.Event) {
		apply(ledger.Event{
			EventID: requestID + "_accepted", Kind: ledger.EventRequestAccepted,
			RequestID: requestID, ProjectID: "project_1", PeriodID: "2026-08-21",
			RequestedModel: "chat", OccurredAt: at,
		})
		for _, attempt := range attempts {
			apply(attempt)
		}
		apply(ledger.Event{
			EventID: requestID + "_final", Kind: ledger.EventRequestFinalized,
			RequestID: requestID, ProjectID: "project_1", PeriodID: "2026-08-21",
			RequestedModel: "chat", OccurredAt: at.Add(time.Second), Outcome: outcome,
		})
	}
	settled := func(requestID, attemptID, outcome, class string, status int) ledger.Event {
		return ledger.Event{
			EventID: attemptID + "_settled", Kind: ledger.EventAttemptSettled,
			RequestID: requestID, AttemptID: attemptID, AttemptNumber: 1,
			ProjectID: "project_1", PeriodID: "2026-08-21", ProviderID: "provider_1",
			DeploymentID: "dep_1", ProviderModel: "gpt-4o", RequestedModel: "chat",
			OccurredAt: now, Outcome: outcome, ErrorClass: class, HTTPStatus: status,
			CommittedMicrosUSD: ledger.MicrosUSD(1),
		}
	}
	request("req_ok", "success", now, settled("req_ok", "att_ok", "success", "", 0))
	request("req_failed", "provider_error", now.Add(time.Minute),
		settled("req_failed", "att_failed", "provider_error", "authentication", 401))
	request("req_rejected", "rejected", now.Add(2*time.Minute))
}

func openSeededRuntime(t *testing.T) (*Runtime, *http.Cookie) {
	t.Helper()
	cfg := testConfig(t)
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
	t.Cleanup(func() { runtime.Close() })
	seedFailures(t, runtime)
	cookie, _ := loginAdminForTest(t, runtime)
	return runtime, cookie
}

func readFailures(t *testing.T, runtime *Runtime, cookie *http.Cookie, path string) failuresEnvelope {
	t.Helper()
	response := authenticatedAdminGet(t, runtime, cookie, path)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var envelope failuresEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return envelope
}

// One row per failed request, and every kind of failed request. The count has
// to match the summary card that links here, which means the rejection is in
// the list even though there is nothing provider-shaped to show for it.
func TestUsageFailuresListsEveryFailedRequestOncePerRequest(t *testing.T) {
	runtime, cookie := openSeededRuntime(t)
	envelope := readFailures(t, runtime, cookie, "/admin/api/v1/usage/failures")

	if uint64(len(envelope.Items)) != runtime.usage.Metrics().RequestsError {
		t.Fatalf("%d rows for %d failed requests", len(envelope.Items), runtime.usage.Metrics().RequestsError)
	}
	if len(envelope.Items) != 2 {
		t.Fatalf("items=%#v", envelope.Items)
	}
	rejected, failed := envelope.Items[0], envelope.Items[1]
	if rejected.Outcome != "rejected" || rejected.LastFailure != nil {
		t.Fatalf("a policy rejection was served with a provider context: %#v", rejected)
	}
	if failed.Outcome != "provider_error" || failed.LastFailure == nil ||
		failed.LastFailure.ErrorClass != "authentication" || failed.LastFailure.ProviderStatus != 401 {
		t.Fatalf("the provider failure lost its context: %#v", failed)
	}
}

// The filters are the attempt list's, so a link between the two views keeps
// meaning the same thing. A deployment filter is answered from the attempts,
// so it necessarily drops a request that never reached one.
func TestUsageFailuresFiltersAndRefusesUnknownParameters(t *testing.T) {
	runtime, cookie := openSeededRuntime(t)

	byDeployment := readFailures(t, runtime, cookie, "/admin/api/v1/usage/failures?deployment_id=dep_1")
	if len(byDeployment.Items) != 1 || byDeployment.Items[0].RequestID != "req_failed" {
		t.Fatalf("deployment filter=%#v", byDeployment.Items)
	}
	byRequest := readFailures(t, runtime, cookie, "/admin/api/v1/usage/failures?request_id=req_rejected")
	if len(byRequest.Items) != 1 || byRequest.Items[0].RequestID != "req_rejected" {
		t.Fatalf("request ID filter=%#v", byRequest.Items)
	}
	paged := readFailures(t, runtime, cookie, "/admin/api/v1/usage/failures?limit=1")
	if len(paged.Items) != 1 || paged.NextCursor == "" {
		t.Fatalf("first page=%#v", paged)
	}
	second := readFailures(t, runtime, cookie, "/admin/api/v1/usage/failures?limit=1&cursor="+paged.NextCursor)
	if len(second.Items) != 1 || second.Items[0].RequestID == paged.Items[0].RequestID {
		t.Fatalf("second page repeated the first: %#v", second.Items)
	}

	for _, path := range []string{
		"/admin/api/v1/usage/failures?status=error",
		"/admin/api/v1/usage/failures?limit=0",
		"/admin/api/v1/usage/failures?limit=101",
		"/admin/api/v1/usage/failures?cursor=not-a-cursor",
		"/admin/api/v1/usage/failures?start=yesterday",
		"/admin/api/v1/usage/failures?request_id=" + strings.Repeat("r", 129),
	} {
		response := authenticatedAdminGet(t, runtime, cookie, path)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s status=%d, want 400", path, response.Code)
		}
	}
}

// The endpoint reads accounting history, so it is behind the admin session like
// every other view of it.
func TestUsageFailuresRequiresAnAdminSession(t *testing.T) {
	runtime, _ := openSeededRuntime(t)
	request, err := http.NewRequest(http.MethodGet, "/admin/api/v1/usage/failures", nil)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", response.Code)
	}
}

// The payload endpoint is off unless the operator turned capture on, and off is
// an answer rather than an error: an operator told "not found" with no reason
// concludes the capture failed when it was never asked for.
func TestUsageFailurePayloadSaysWhenCaptureIsDisabled(t *testing.T) {
	runtime, cookie := openSeededRuntime(t)
	response := authenticatedAdminGet(t, runtime, cookie, "/admin/api/v1/usage/failures/req_failed/payload")
	if response.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", response.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != "failure_capture_disabled" {
		t.Fatalf("a disabled feature was reported as a missing record: %v", body)
	}
}

// It reads material a caller wrote, which is why it is the only admin GET that
// audits. Every other one is a view of Halro's own metadata; this one hands
// back a prompt, and that should be answerable afterwards.
func TestReadingACapturedPayloadIsAudited(t *testing.T) {
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
	if runtime.failureCapture == nil {
		t.Fatal("capture was enabled and no store was opened")
	}
	if _, err := runtime.failureCapture.Put(failurecapture.Record{
		RequestID: "req_failed", ProjectID: "project_1", Outcome: "provider_error",
		Request: json.RawMessage(`{"model":"chat"}`),
	}); err != nil {
		t.Fatal(err)
	}
	cookie, _ := loginAdminForTest(t, runtime)

	response := authenticatedAdminGet(t, runtime, cookie, "/admin/api/v1/usage/failures/req_failed/payload")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"model":"chat"`) {
		t.Fatalf("the captured request was not returned: %s", response.Body.String())
	}

	read := false
	if _, err := runtime.audit.Replay(func(entry audit.Record) error {
		if entry.Event.Action == "usage.failure_payload.read" && entry.Event.TargetID == "req_failed" {
			read = true
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !read {
		t.Fatal("a prompt was read and the audit log does not say so")
	}

	// A request with no capture is a plain miss, and a request that does not
	// exist in usage at all never reaches the store.
	for _, path := range []string{
		"/admin/api/v1/usage/failures/req_ok/payload",
		"/admin/api/v1/usage/failures/req_not_a_request/payload",
	} {
		if got := authenticatedAdminGet(t, runtime, cookie, path); got.Code != http.StatusNotFound {
			t.Fatalf("%s status=%d, want 404", path, got.Code)
		}
	}
}

// Turning capture off has to stop new writes without stopping the expiry of
// what is already there. It used to do the opposite: the store was opened only
// when enabled, so the retention sweep returned immediately and every prompt
// collected while it was on stayed on disk forever. An operator who switches it
// off after an incident is taking the security-conscious action, and it must
// not be the one that makes the data permanent.
func TestDisablingCaptureStillExpiresWhatWasAlreadyWritten(t *testing.T) {
	cfg := testConfig(t)
	cfg.Gateway.FailureCapture.Enabled = true
	cfg.Gateway.FailureCapture.Retain = config.Duration(time.Hour)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapAdmin(context.Background(), cfg, "admin", []byte("correct horse battery staple")); err != nil {
		t.Fatal(err)
	}
	enabled, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enabled.failureCapture.Put(failurecapture.Record{
		RequestID: "req_failed", ProjectID: "project_1", Outcome: "provider_error",
		Request: json.RawMessage(`{"model":"chat"}`),
	}); err != nil {
		t.Fatal(err)
	}
	enabled.Close()

	// The same data directory, with capture switched off.
	cfg.Gateway.FailureCapture.Enabled = false
	disabled, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer disabled.Close()
	if disabled.failureCapture == nil {
		t.Fatal("a directory holding captures was left with nothing to expire it")
	}
	// The gateway is still not allowed to write into it.
	if captureFor(cfg.Gateway.FailureCapture.Enabled, disabled.failureCapture) != nil {
		t.Fatal("a disabled instance handed the gateway a store to write to")
	}
	if _, found, _ := disabled.failureCapture.Get("req_failed", "project_1"); !found {
		t.Fatal("the record vanished before its window elapsed")
	}
	disabled.purgeFailureCaptures()
	if _, found, _ := disabled.failureCapture.Get("req_failed", "project_1"); !found {
		t.Fatal("a record inside its window was swept")
	}
}
