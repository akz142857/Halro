package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/requestmeta"
	"github.com/akz142857/Halro/internal/safelog"
	"github.com/akz142857/Halro/internal/semantic"
)

// finalFailureRecords returns the "request failed" records a run produced.
func finalFailureRecords(t *testing.T, logs *bytes.Buffer) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("log line is not JSON: %s", line)
		}
		if record["msg"] == "request failed" {
			if record["level"] != "ERROR" {
				t.Fatalf("a terminal failure was written at %v, not ERROR", record["level"])
			}
			records = append(records, record)
		}
	}
	return records
}

func captureLogs(t *testing.T, f *fixture) *bytes.Buffer {
	t.Helper()
	logs := &bytes.Buffer{}
	f.service.logger = safelog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return logs
}

// The record an operator holding a 502 needs: which request, what class of
// failure, what the upstream said about it, and where it ran — none of which
// the caller's fixed sentence carries, and none of which existed anywhere at
// request level before.
func TestATerminalFailureIsReportedOnceWithItsClassAndTarget(t *testing.T) {
	f := newFixture(t, 1_000_000)
	defer f.close()
	logs := captureLogs(t, &f)
	f.adapter.err = &provider.Error{
		Class: provider.ErrorAuthentication, Retryable: false, StatusCode: 401,
		ProviderCode: "invalid_api_key", ProviderRequestID: "upstream-req-77",
		Message: "provider error (401): key sk-canary-not-a-real-key was revoked",
	}

	ctx := requestmeta.WithRequestID(context.Background(), "req_terminal")
	if _, err := f.service.Chat(ctx, f.plaintext, chatRequest()); err == nil {
		t.Fatal("the provider failure did not reach the caller")
	}

	records := finalFailureRecords(t, logs)
	if len(records) != 1 {
		t.Fatalf("got %d terminal records, want exactly 1", len(records))
	}
	record := records[0]
	for field, want := range map[string]any{
		"request_id":          "req_terminal",
		"outcome":             "provider_error",
		"phase":               "provider",
		"error_class":         "authentication",
		"provider_status":     float64(401),
		"provider_code":       "invalid_api_key",
		"provider_request_id": "upstream-req-77",
		"public_model":        "chat",
		"deployment_id":       "dep_target_1",
		"accounting_recorded": true,
	} {
		if record[field] != want {
			t.Fatalf("%s = %v, want %v (record: %v)", field, record[field], want, record)
		}
	}
	// The chain, so a single failure is not read as an exhausted one.
	if record["attempts"] != float64(1) || record["fallbacks"] != float64(0) {
		t.Fatalf("chain = %v attempts / %v fallbacks", record["attempts"], record["fallbacks"])
	}
	if _, present := record["latency_millis"]; !present {
		t.Fatalf("no latency on the record: %v", record)
	}
	// The upstream's sentence is a response body wherever it is written.
	if strings.Contains(logs.String(), "sk-canary-not-a-real-key") ||
		strings.Contains(logs.String(), "was revoked") {
		t.Fatalf("an upstream response body reached the log: %s", logs.String())
	}
}

// The distinction the whole design rests on. A request that fell back and
// answered has a failed attempt in its history and is not a failed request; an
// ERROR here would put it in the error file and in every alert built on one.
func TestAFallbackThatSucceedsWritesNoTerminalFailure(t *testing.T) {
	f := newFixture(t, 1_000_000)
	defer f.close()
	logs := captureLogs(t, &f)
	f.adapter.err = &provider.Error{
		Class: provider.ErrorProvider5xx, Retryable: true, StatusCode: 503, Message: "unavailable",
	}
	registerFallback(t, &f, &fakeAdapter{response: f.adapter.response})

	if _, err := f.service.Chat(context.Background(), f.plaintext, chatRequest()); err != nil {
		t.Fatalf("the fallback did not answer: %v", err)
	}
	if records := finalFailureRecords(t, logs); len(records) != 0 {
		t.Fatalf("a successful request was reported as failed: %v", records)
	}
	if !strings.Contains(logs.String(), "provider attempt failed") {
		t.Fatal("the attempt failure was not reported at all")
	}
}

// Every terminal state that is a policy working as configured. Each can be
// produced by a client in a retry loop at that client's own rate, so writing
// them would fill a bounded error file in minutes and push the incident's first
// real error out of it. They are still failed requests: the ledger records
// them, the console lists them, the summary counts them.
func TestPolicyRefusalsWriteNoTerminalFailure(t *testing.T) {
	if writesFailureError("rejected") || writesFailureError("token_guard_rejected") ||
		writesFailureError("unsupported_feature") || writesFailureError("policy_rejected") {
		t.Fatal("a policy refusal was classified as an incident")
	}
	if !writesFailureError("provider_error") || !writesFailureError("accounting_error") {
		t.Fatal("a failure nothing outside Halro can drive was classified as routine")
	}
	if writesFailureError("success") {
		t.Fatal("a successful request was classified as a failure")
	}

	// And through the real path, for the outcome an operator can produce most
	// cheaply: one micro-USD of budget admits a request and can never reserve
	// an attempt against it.
	f := newFixture(t, 1)
	defer f.close()
	logs := captureLogs(t, &f)
	for range 20 {
		if _, err := f.service.Chat(context.Background(), f.plaintext, chatRequest()); err == nil {
			t.Fatal("the exhausted budget admitted a request")
		}
	}
	if records := finalFailureRecords(t, logs); len(records) != 0 {
		t.Fatalf("%d budget refusals were written as incidents", len(records))
	}
}

// A failure with no upstream to blame still has to be reported, and reported
// without inventing an upstream: the answer was produced, and Halro could not
// put it on the caller's wire.
func TestAnUnrenderableAnswerIsReportedAsARenderFailure(t *testing.T) {
	f := newFixture(t, 1_000_000)
	defer f.close()
	logs := captureLogs(t, &f)

	_, err := f.service.generate(
		context.Background(), f.plaintext, "chat", chatCanonical(t),
		func(semantic.GenerateResult) error { return errors.New("wire form cannot carry this content kind") },
	)
	if err == nil {
		t.Fatal("a render that failed answered the caller successfully")
	}
	records := finalFailureRecords(t, logs)
	if len(records) != 1 {
		t.Fatalf("got %d terminal records, want 1", len(records))
	}
	if records[0]["phase"] != "response_render" {
		t.Fatalf("phase = %v, want response_render", records[0]["phase"])
	}
	// The target is still named — it is the only context this failure has —
	// but nothing claims the upstream refused anything.
	if records[0]["deployment_id"] != "dep_target_1" {
		t.Fatalf("the render failure lost its target: %v", records[0])
	}
	if _, present := records[0]["provider_status"]; present {
		t.Fatalf("a render failure was given an upstream status: %v", records[0])
	}
}

// error_class has to be a member of provider.ErrorClass on every record, or the
// console's dictionary and every alert rule keyed on it have a value they
// cannot resolve. The cancellation branch used to write a literal
// `client_disconnected_or_timed_out`, which was outside the enum and reached
// the console as raw English.
func TestEveryLoggedErrorClassIsAMemberOfTheEnum(t *testing.T) {
	known := map[string]struct{}{}
	for _, class := range []provider.ErrorClass{
		provider.ErrorAuthentication, provider.ErrorRateLimit, provider.ErrorTimeout,
		provider.ErrorProvider5xx, provider.ErrorBadRequest, provider.ErrorConnect,
		provider.ErrorMalformed, provider.ErrorCanceled, provider.ErrorUnknown,
	} {
		known[string(class)] = struct{}{}
	}

	for _, testCase := range []struct {
		name  string
		err   error
		class string
		// A caller who hung up gets no terminal record — see
		// TestACallerHangingUpWritesNoTerminalFailure — so this case is
		// asserted on the attempt record alone.
		terminal bool
	}{
		{"deadline passed", context.DeadlineExceeded, "timeout", true},
		{"nothing classified it", errors.New("adapter ignored the error contract"), "unknown", true},
		{"caller cancelled", context.Canceled, "canceled", false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			f := newFixture(t, 1_000_000)
			defer f.close()
			logs := captureLogs(t, &f)
			f.adapter.err = testCase.err

			if _, err := f.service.Chat(context.Background(), f.plaintext, chatRequest()); err == nil {
				t.Fatal("the failure did not reach the caller")
			}
			records := finalFailureRecords(t, logs)
			if testCase.terminal {
				if len(records) != 1 {
					t.Fatalf("got %d terminal records, want 1", len(records))
				}
				class, _ := records[0]["error_class"].(string)
				if _, member := known[class]; !member {
					t.Fatalf("error_class %q is not a member of provider.ErrorClass", class)
				}
				if class != testCase.class {
					t.Fatalf("error_class = %q, want %q", class, testCase.class)
				}
			} else if len(records) != 0 {
				t.Fatalf("a caller who hung up produced %d terminal records", len(records))
			}
			// The attempt record classifies it either way, and with a value the
			// console can translate: two records about one failure that
			// classify it differently cannot both be right.
			if !strings.Contains(logs.String(), `"error_class":"`+testCase.class+`"`) {
				t.Fatalf("the attempt record did not classify the failure: %s", logs.String())
			}
			if _, member := known[testCase.class]; !member {
				t.Fatalf("error_class %q is not a member of provider.ErrorClass", testCase.class)
			}
		})
	}
}

// The unclassified case is the only entry to these records with no contract
// behind it, so it has to assume the worst about what it holds.
func TestAnUnclassifiedTerminalFailureIsReportedByTypeNotByText(t *testing.T) {
	f := newFixture(t, 1_000_000)
	defer f.close()
	logs := captureLogs(t, &f)
	f.adapter.err = errors.New(
		"upstream refused: not authorized to call this project with " + unclassifiedKeyCanary)

	if _, err := f.service.Chat(context.Background(), f.plaintext, chatRequest()); err == nil {
		t.Fatal("the failure did not reach the caller")
	}
	records := finalFailureRecords(t, logs)
	if len(records) != 1 || records[0]["error_type"] != "*errors.errorString" {
		t.Fatalf("the terminal record did not name the type that produced the failure: %v", records)
	}
	if strings.Contains(logs.String(), unclassifiedKeyCanary) ||
		strings.Contains(logs.String(), "not authorized to call this project") {
		t.Fatalf("an unclassified error's text reached the log: %s", logs.String())
	}
}

// deferredFaultDurability lets the first `healthy` writes through and fails
// every one after. It is how a request gets admitted into the ledger and then
// cannot be accounted for — the accounting_error terminal state, which no
// amount of ordinary traffic reaches.
type deferredFaultDurability struct {
	file    *os.File
	healthy int
	written int
}

func (d *deferredFaultDurability) Write(payload []byte) (int, error) {
	d.written++
	if d.written > d.healthy {
		return 0, syscall.ENOSPC
	}
	return d.file.Write(payload)
}

func (d *deferredFaultDurability) Sync() error { return d.file.Sync() }

// The other terminal state that earns an ERROR, and the one that most needs it:
// when the ledger cannot take the record, the console has no row to show and
// this log line is the only account of the request that exists anywhere. The
// record says so on its face rather than leaving an operator to infer it from a
// row that is not there.
func TestAnAccountingFailureIsReportedAndSaysTheLedgerDidNotTakeIt(t *testing.T) {
	f := newFixtureWithLedgerOptions(t, 1_000_000, ledger.Options{
		MaxBatch: 1,
		WrapDurability: func(file *os.File) ledger.DurabilityWriter {
			// One healthy write: the RequestAccepted frame. Everything after
			// it — the attempt reservation, and then the finalize — fails.
			return &deferredFaultDurability{file: file, healthy: 1}
		},
	})
	defer f.close()
	logs := captureLogs(t, &f)

	if _, err := f.service.Chat(context.Background(), f.plaintext, chatRequest()); err == nil {
		t.Fatal("a request was served while accounting was unavailable")
	}
	if f.adapter.calls != 0 {
		t.Fatalf("the provider was called %d times with accounting down", f.adapter.calls)
	}
	records := finalFailureRecords(t, logs)
	if len(records) != 1 {
		t.Fatalf("got %d terminal records, want 1", len(records))
	}
	if records[0]["outcome"] != "accounting_error" || records[0]["phase"] != "accounting" {
		t.Fatalf("record = %v", records[0])
	}
	if records[0]["accounting_recorded"] != false {
		t.Fatalf("the record claimed the ledger took it: %v", records[0])
	}
}

// The identifiers an operator takes to the upstream do not survive a restart in
// the process log — that file rotates, and it is not the record. They now reach
// the ledger with the attempt, so the console can still name the request the
// upstream saw days later.
//
// This also pins the ledger and the log agreeing about the class. They used to
// classify separately, and had already drifted: the settlement wrote `canceled`
// where the log wrote a literal `client_disconnected_or_timed_out`, so one
// attempt gave two answers depending on which record was read.
func TestTheUpstreamsIdentifiersReachTheLedgerWithTheAttempt(t *testing.T) {
	f := newFixture(t, 1_000_000)
	defer f.close()
	f.adapter.err = &provider.Error{
		Class: provider.ErrorBadRequest, Retryable: false, StatusCode: 400,
		ProviderCode:      "invalid_image_url:messages[0].content[1].image_url",
		ProviderRequestID: "upstream-req-42",
		Message:           "provider error (400): Error while downloading https://example.test/photo.png",
	}
	if _, err := f.service.Chat(context.Background(), f.plaintext, chatRequest()); err == nil {
		t.Fatal("the provider failure did not reach the caller")
	}

	var settled ledger.Event
	if _, err := f.log.Replay(ledger.Watermark{}, func(record ledger.Record) error {
		if record.Event.Kind == ledger.EventAttemptSettled {
			settled = record.Event
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if settled.ProviderCode != "invalid_image_url:messages[0].content[1].image_url" ||
		settled.ProviderRequestID != "upstream-req-42" ||
		settled.FailurePhase != "provider" || settled.ErrorClass != "bad_request" {
		t.Fatalf("the ledger did not keep the upstream's identifiers: %+v", settled)
	}
	// The sentence beside them is still a response body, wherever it is written.
	encoded, err := json.Marshal(settled)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "Error while downloading") {
		t.Fatalf("an upstream response body became durable state: %s", encoded)
	}
}

// A cancellation classifies the same way in both records now, and lands in the
// client phase rather than being attributed to the upstream.
func TestACancellationIsClassifiedTheSameWayInTheLedgerAndTheLog(t *testing.T) {
	f := newFixture(t, 1_000_000)
	defer f.close()
	logs := captureLogs(t, &f)
	f.adapter.err = context.Canceled

	if _, err := f.service.Chat(context.Background(), f.plaintext, chatRequest()); err == nil {
		t.Fatal("the failure did not reach the caller")
	}
	var settled ledger.Event
	if _, err := f.log.Replay(ledger.Watermark{}, func(record ledger.Record) error {
		if record.Event.Kind == ledger.EventAttemptSettled {
			settled = record.Event
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if settled.ErrorClass != string(provider.ErrorCanceled) || settled.FailurePhase != "client" {
		t.Fatalf("ledger classification = %q / %q", settled.ErrorClass, settled.FailurePhase)
	}
	if records := finalFailureRecords(t, logs); len(records) != 0 {
		t.Fatalf("a caller who hung up produced %d terminal records", len(records))
	}
	// The attempt record still carries the ledger's classification, so the two
	// accounts of the same attempt agree.
	if !strings.Contains(logs.String(), `"error_class":"`+settled.ErrorClass+`"`) {
		t.Fatalf("the attempt log disagreed with the ledger: %s", logs.String())
	}
}

// The defect a review found, and the reason terminalDescriptor exists: nothing
// cleared the last attempt failure when a later attempt answered, so a request
// that fell back, succeeded, and then could not have its answer rendered was
// reported with the *first* target's status, provider code and upstream request
// ID. An operator took that identifier to the upstream and was asking about a
// call that had worked.
func TestARenderFailureAfterAFallbackDoesNotReportTheEarlierAttempt(t *testing.T) {
	f := newFixture(t, 1_000_000)
	defer f.close()
	logs := captureLogs(t, &f)
	f.adapter.err = &provider.Error{
		Class: provider.ErrorProvider5xx, Retryable: true, StatusCode: 503,
		ProviderCode: "srv_overload", ProviderRequestID: "upstream-req-first",
		Message: "provider error (503): overloaded",
	}
	registerFallback(t, &f, &fakeAdapter{response: f.adapter.response})

	_, err := f.service.generate(
		context.Background(), f.plaintext, "chat", chatCanonical(t),
		func(semantic.GenerateResult) error { return errors.New("wire form cannot carry this content kind") },
	)
	if err == nil {
		t.Fatal("a render that failed answered the caller successfully")
	}
	records := finalFailureRecords(t, logs)
	if len(records) != 1 {
		t.Fatalf("got %d terminal records, want 1", len(records))
	}
	record := records[0]
	if record["phase"] != "response_render" {
		t.Fatalf("phase = %v, want response_render", record["phase"])
	}
	// The target that actually served the answer, not the one that failed.
	if record["deployment_id"] != "dep_target_2" {
		t.Fatalf("the record names the wrong target: %v", record["deployment_id"])
	}
	for _, field := range []string{"provider_status", "provider_code", "provider_request_id"} {
		if value, present := record[field]; present {
			t.Fatalf("%s = %v on a request whose upstream succeeded", field, value)
		}
	}
	// The attempt that did fail still has its own record; this is about which
	// one explains the request.
	if !strings.Contains(logs.String(), "upstream-req-first") {
		t.Fatal("the failed attempt's own record lost the upstream identifier")
	}
}

// The same defect with a worse consequence: the record whose whole purpose is
// to say "the ledger could not take this" was written as a provider 5xx,
// sending an operator upstream over a disk that could not be written.
func TestAnAccountingFailureIsNotReportedAsAnEarlierProviderFailure(t *testing.T) {
	f := newFixtureWithLedgerOptions(t, 1_000_000, ledger.Options{
		MaxBatch: 1,
		WrapDurability: func(file *os.File) ledger.DurabilityWriter {
			// Enough writes for the request to be accepted and one attempt to
			// fail and settle; the next attempt's reservation cannot be taken.
			return &deferredFaultDurability{file: file, healthy: 4}
		},
	})
	defer f.close()
	logs := captureLogs(t, &f)
	f.adapter.err = &provider.Error{
		Class: provider.ErrorProvider5xx, Retryable: true, StatusCode: 503,
		ProviderCode: "srv_overload", ProviderRequestID: "upstream-req-first",
		Message: "provider error (503): overloaded",
	}

	if _, err := f.service.Chat(context.Background(), f.plaintext, chatRequest()); err == nil {
		t.Fatal("a request was served while accounting was unavailable")
	}
	for _, record := range finalFailureRecords(t, logs) {
		if record["outcome"] != "accounting_error" {
			continue
		}
		if record["phase"] != "accounting" {
			t.Fatalf("phase = %v, want accounting", record["phase"])
		}
		if record["error_class"] == "provider_5xx" || record["provider_request_id"] != nil {
			t.Fatalf("an accounting failure was reported as an upstream one: %v", record)
		}
		return
	}
	t.Fatalf("no accounting_error record was written: %s", logs.String())
}

// A caller hanging up is driven entirely from outside Halro, and one frontend
// deploy cancels every request in flight at once. Writing those would fill a
// bounded error file in seconds and push the incident's first real error out of
// it — the failure mode the four policy outcomes are excluded to prevent.
func TestACallerHangingUpWritesNoTerminalFailure(t *testing.T) {
	f := newFixture(t, 1_000_000)
	defer f.close()
	logs := captureLogs(t, &f)
	capture := withCapture(t, &f)
	f.adapter.err = context.Canceled

	for range 10 {
		if _, err := f.service.Chat(context.Background(), f.plaintext, chatRequest()); err == nil {
			t.Fatal("a cancelled request answered successfully")
		}
	}
	if records := finalFailureRecords(t, logs); len(records) != 0 {
		t.Fatalf("%d client disconnects were written as incidents", len(records))
	}
	// Nor is a prompt kept for each of them.
	if len(capture.records) != 0 {
		t.Fatalf("%d cancelled requests were captured", len(capture.records))
	}
	// A deadline that expired is not this: nobody hung up, something was slow.
	f.adapter.err = context.DeadlineExceeded
	if _, err := f.service.Chat(context.Background(), f.plaintext, chatRequest()); err == nil {
		t.Fatal("a timed-out request answered successfully")
	}
	if records := finalFailureRecords(t, logs); len(records) != 1 {
		t.Fatalf("a timeout produced %d terminal records, want 1", len(records))
	}
}

// A failure this side raised about an answer the upstream gave is not an
// upstream failure. Recording HTTP 200 for it made the console render a red dot
// beside "HTTP 200", and a blank phase made a record written today look like
// one written before the field existed.
func TestALocalRefusalIsNotSettledAsAnUpstreamSuccess(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		outcome  string
		expected string
	}{
		{"redaction refused the answer", "policy_rejected", phaseResponseRender},
		{"the target could not serve the shape", "unsupported_feature", phasePreProvider},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			settlement := budget.Settlement{Outcome: testCase.outcome}
			if testCase.outcome == "policy_rejected" {
				enrichSettlement(&settlement, nil, time.Now(), time.Now())
			} else {
				// abort's settlement, which never reaches enrichSettlement.
				settlement.FailurePhase = phasePreProvider
			}
			if settlement.HTTPStatus != 0 {
				t.Fatalf("http_status = %d on a failure the upstream did not cause", settlement.HTTPStatus)
			}
			if settlement.FailurePhase != testCase.expected {
				t.Fatalf("phase = %q, want %q", settlement.FailurePhase, testCase.expected)
			}
			if settlement.ErrorClass != "" {
				t.Fatalf("error_class = %q; no upstream class applies here", settlement.ErrorClass)
			}
		})
	}
	// The successful case still records 200, or the assertion above is
	// satisfied by a build that records nothing.
	success := budget.Settlement{Outcome: "success"}
	enrichSettlement(&success, nil, time.Now(), time.Now())
	if success.HTTPStatus != 200 || success.FailurePhase != "" {
		t.Fatalf("a successful attempt = %#v", success)
	}
}
