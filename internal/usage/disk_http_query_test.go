package usage

import (
	"context"
	"encoding/base64"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDiskHTTPAttemptCursorPreservesSparsePagesAndFrozenPrefix(t *testing.T) {
	var events = diskFailureRequest("chain", "provider_error", 9, false, false)
	f := newDiskFailureFixture(t, events)
	q := AttemptQuery{Limit: 2, ProjectID: "project", ProviderID: "provider_a"}
	want, err := f.a.QueryAttempts(q)
	if err != nil {
		t.Fatal(err)
	}
	limits := DiskUsagePageLimits{ScanRows: 2, InputBytes: 8 << 20, OutputBytes: 8 << 20}
	page, err := f.p.QueryAttemptsHTTP(context.Background(), q, "", limits)
	if err != nil || len(page.Attempts) != 0 || page.NextCursor == "" {
		t.Fatal("sparse page lost its continuation", page, err)
	}
	firstToken := page.NextCursor
	// A later native append has the same predicate but must not enter the
	// existing token's immutable cut.
	for _, e := range diskFailureRequest("new", "provider_error", 1, false, false) {
		appendDiskDetailEvent(t, f, e)
	}
	var got = make([]AttemptEvent, 0)
	for n := 0; ; n++ {
		if n > 100 {
			t.Fatal("no query progress")
		}
		got = append(got, page.Attempts...)
		if page.NextCursor == "" {
			break
		}
		page, err = f.p.QueryAttemptsHTTP(context.Background(), q, page.NextCursor, limits)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(got, want.Attempts) {
		t.Fatal("mixed prefix or lost matching rows", got, want)
	}
	q.ProviderID = "provider_b"
	if _, err := f.p.QueryAttemptsHTTP(context.Background(), q, firstToken, limits); !errors.Is(err, ErrDiskUsageCursor) {
		t.Fatal("changed predicate accepted", err)
	}
}

func TestDiskHTTPFailureCursorPreservesLongJoinDecisionAndLatestContext(t *testing.T) {
	f := newDiskFailureFixture(t, diskFailureRequest("chain", "provider_error", 35, false, false))
	q := FailureQuery{Limit: 1, ProviderID: "provider_a"}
	want, err := f.a.QueryFailedRequests(q)
	if err != nil {
		t.Fatal(err)
	}
	limits := DiskUsagePageLimits{ScanRows: 4, InputBytes: 8 << 20, OutputBytes: 8 << 20}
	var got = make([]RequestFailure, 0)
	token, empty := "", 0
	for n := 0; ; n++ {
		if n > 100 {
			t.Fatal("long join never completed")
		}
		page, err := f.p.QueryFailedRequestsHTTP(context.Background(), q, token, limits)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Failures) == 0 && page.NextCursor != "" {
			empty++
		}
		got = append(got, page.Failures...)
		if page.NextCursor == "" {
			break
		}
		if page.NextCursor == token || len(page.NextCursor) != 196 {
			t.Fatal("unbounded token or lost progress", page.NextCursor)
		}
		token = page.NextCursor
	}
	if empty < 5 || !reflect.DeepEqual(got, want.Failures) || len(got) != 1 || got[0].LastFailure.ProviderID != "provider_b" {
		t.Fatal("pending predicate or latest context lost", empty, got, want)
	}
}

func TestDiskHTTPCursorsRejectForgeryOtherOwnerKindAndUnboundedInput(t *testing.T) {
	f := newDiskFailureFixture(t, diskFailureRequest("chain", "provider_error", 8, false, false))
	ctx, q := context.Background(), AttemptQuery{Limit: 1}
	page, err := f.p.QueryAttemptsHTTP(ctx, q, "", diskTestLimits)
	if err != nil || page.NextCursor == "" {
		t.Fatal(page, err)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(page.NextCursor)
	for i := range raw {
		changed := append([]byte(nil), raw...)
		changed[i] ^= 1
		if _, err := f.p.QueryAttemptsHTTP(ctx, q, base64.RawURLEncoding.EncodeToString(changed), diskTestLimits); !errors.Is(err, ErrDiskUsageCursor) {
			t.Fatal("forged position accepted", i, err)
		}
	}
	other := newDiskFailureFixture(t, diskFailureRequest("chain", "provider_error", 8, false, false))
	if _, err := other.p.QueryAttemptsHTTP(ctx, q, page.NextCursor, diskTestLimits); !errors.Is(err, ErrDiskUsageCursor) {
		t.Fatal("another owner accepted token", err)
	}
	if _, err := f.p.QueryFailedRequestsHTTP(ctx, FailureQuery{Limit: 1}, page.NextCursor, diskTestLimits); !errors.Is(err, ErrDiskUsageCursor) {
		t.Fatal("attempt token accepted as failure join", err)
	}
	for _, token := range []string{"garbage", EncodeCursor(10), strings.Repeat("a", 1<<20)} {
		if _, err := f.p.QueryAttemptsHTTP(ctx, q, token, diskTestLimits); !errors.Is(err, ErrDiskUsageCursor) {
			t.Fatal("bad token accepted", len(token), err)
		}
	}
	q.BeforeSequence = 1
	if _, err := f.p.QueryAttemptsHTTP(ctx, q, "", diskTestLimits); !errors.Is(err, ErrDiskUsageCursor) {
		t.Fatal("bare position bypassed signed cursor", err)
	}
	q.BeforeSequence = 0
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := f.p.QueryAttemptsHTTP(canceled, q, "", diskTestLimits); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := f.p.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.p.QueryAttemptsHTTP(ctx, q, page.NextCursor, diskTestLimits); !errors.Is(err, ErrDiskUsageClosed) {
		t.Fatal("retired cursor accepted", err)
	}
}

func diskHTTPDetailLimits() DiskDetailLimits {
	return DiskDetailLimits{Pages: 128, Attempts: 1024, InputBytes: 64 << 20, OutputBytes: 8 << 20, MaxDuration: time.Minute, Page: DiskUsagePageLimits{ScanRows: 4, InputBytes: 8 << 20, OutputBytes: 8 << 20}}
}

func TestDiskHTTPCompleteDetailPreservesNativeOrderAndTerminalSummary(t *testing.T) {
	f := newDiskFailureFixture(t, append(diskFailureRequest("chain", "provider_error", 12, false, false), diskFailureRequest("late", "provider_error", 1, true, false)...))
	for _, id := range []string{"chain", "late", "missing"} {
		want, found := f.a.RequestDetail(id)
		got, exists, err := f.p.ReadRequestDetailHTTP(context.Background(), id, diskHTTPDetailLimits())
		if err != nil || found != exists || found && !reflect.DeepEqual(got, want) {
			t.Fatal(id, got, want, exists, found, err)
		}
	}
}

func TestDiskHTTPCompleteDetailRejectsWholeJobLimitsWithoutPartialSuccess(t *testing.T) {
	f := newDiskFailureFixture(t, diskFailureRequest("chain", "provider_error", 12, false, false))
	for _, kind := range []string{"pages", "attempts", "input", "output", "page"} {
		t.Run(kind, func(t *testing.T) {
			limits := diskHTTPDetailLimits()
			switch kind {
			case "pages":
				limits.Pages = 1
			case "attempts":
				limits.Attempts = 2
			case "input":
				limits.InputBytes = 1
			case "output":
				limits.OutputBytes = 1
			case "page":
				limits.Page.ScanRows = 0
			}
			result, found, err := f.p.ReadRequestDetailHTTP(context.Background(), "chain", limits)
			if !errors.Is(err, ErrDiskUsageLimit) || found || result.Summary != (RequestSummary{}) || result.Attempts != nil || len(f.p.summaries) != 0 || len(f.p.readers) != 0 || f.p.db.Stats().OpenTxN != 0 {
				t.Fatal("partial response or retained ownership", result, found, err)
			}
		})
	}
}

func TestDiskHTTPCompleteDetailBusyAndPhysicalCloseJoin(t *testing.T) {
	f := newDiskFailureFixture(t, diskFailureRequest("chain", "provider_error", 12, false, false))
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	done := make(chan error, 1)
	go func() {
		first := true
		_, _, err := f.p.readRequestDetailHTTP(context.Background(), "chain", diskHTTPDetailLimits(), func(ctx context.Context, q DiskRequestDetailQuery, cursor DiskRequestDetailCursor, limits DiskUsagePageLimits) (DiskRequestDetailPage, error) {
			page, err := f.p.RequestDetailPage(ctx, q, cursor, limits)
			if first {
				first = false
				close(entered)
				<-release // after a real DB page closes, before the next one
			}
			return page, err
		})
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("source read did not enter")
	}
	if _, _, err := f.p.ReadRequestDetailHTTP(context.Background(), "chain", diskHTTPDetailLimits()); !errors.Is(err, ErrDiskUsageBusy) {
		t.Fatal("whole-job slot queued a second detail", err)
	}
	closed := make(chan error, 1)
	go func() { closed <- f.p.Close() }()
	deadline := time.Now().Add(5 * time.Second)
	for !f.p.retired.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !f.p.retired.Load() {
		t.Fatal("Close did not withdraw admission")
	}
	select {
	case err := <-closed:
		t.Fatal("Close returned before source exit", err)
	default:
	}
	unblock()
	if err := <-done; !errors.Is(err, ErrDiskUsageClosed) {
		t.Fatal("retired job published success", err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}
