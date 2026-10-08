package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/provider"
	bbolt "go.etcd.io/bbolt"
)

// All semantic comparisons use MAC-authenticated native records and the selected
// Ledger disk owner. Only fault tests below mutate derived records directly.
type diskFailureFixture struct {
	p      *DiskProjection
	a      *Aggregate
	native *ledger.Log
	owner  *ledger.LedgerProjection
	d      *LedgerDerivative
}

func newDiskFailureFixture(t *testing.T, events []ledger.Event) *diskFailureFixture {
	t.Helper()
	ctx, root := context.Background(), t.TempDir()
	native, err := ledger.OpenWithOptions(filepath.Join(root, "ledger.wal"), ledger.NewStatus(), ledger.Options{ChainKey: bytes.Repeat([]byte{0x71}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = native.Close() })
	for _, e := range events {
		if _, err := native.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	owner, _, err := native.BuildProjectionFromCursor(ctx, 1, uint64(len(events)), filepath.Join(root, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	d, p, err := NewDiskLedgerDerivative(owner, filepath.Join(root, "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close(); _ = p.Close() })
	a, err := NewAggregateWithFinalizationReader(owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := native.ReplayAuthenticated(ctx, ledger.ReplayAnchor{}, func(r ledger.Record) error {
		if err := d.Apply(r); err != nil {
			return err
		}
		return a.Apply(r)
	}); err != nil {
		t.Fatal(err)
	}
	return &diskFailureFixture{p, a, native, owner, d}
}

func diskFailureBase(id string) ledger.Event {
	return ledger.Event{RequestID: id, ProjectID: "project", KeyID: "key", PeriodID: "2026-10-03", PeriodTimezone: "UTC", PeriodTimezoneVersion: 1, RequestedModel: "chat", OccurredAt: time.Date(2026, 10, 3, 1, 0, 0, 0, time.UTC)}
}

func diskFailureRequest(id, outcome string, n int, late, latestSuccess bool) []ledger.Event {
	base := diskFailureBase(id)
	accepted := base
	accepted.Kind = ledger.EventRequestAccepted
	accepted.EventID = id + "_accepted"
	rows := []ledger.Event{accepted}
	final := base
	final.Kind = ledger.EventRequestFinalized
	final.EventID = id + "_final"
	final.Outcome = outcome
	final.OccurredAt = base.OccurredAt.Add(time.Minute)
	for i := 0; i < n; i++ {
		e := base
		e.AttemptID = fmt.Sprintf("%s_att_%d", id, i)
		e.AttemptNumber = i + 1
		e.ProviderID, e.DeploymentID, e.ProviderModel = "provider_b", "dep_b", "model-b"
		if i == 0 {
			e.ProviderID, e.DeploymentID, e.ProviderModel = "provider_a", "dep_a", "model-a"
			e.AttemptNumber = 99
		}
		e.OfferingID, e.ProfileID, e.AccountRegionID = domain.OfferingMiniMaxSubscriptionAccess, domain.ProfileMiniMaxGlobalSubscriptionOpenAIChat, domain.RegionGlobal
		e.ProviderPrimitive = provider.PrimitiveMiniMaxChat
		// Arrival order, not timestamps or attempt numbers, selects context.
		e.OccurredAt = base.OccurredAt.Add(time.Duration(n-i) * time.Second)
		for _, kind := range []ledger.EventKind{ledger.EventReservationCreated, ledger.EventAttemptStarted, ledger.EventAttemptSettled} {
			r := e
			r.Kind = kind
			r.EventID = fmt.Sprintf("%s_%d", e.AttemptID, kind)
			if kind == ledger.EventReservationCreated {
				r.ReservationMicrosUSD = ledger.MicrosUSD(100)
			}
			if kind == ledger.EventAttemptSettled {
				r.CommittedMicrosUSD = ledger.MicrosUSD(7)
				r.Outcome = "provider_error"
				r.ErrorClass = "timeout"
				r.HTTPStatus = 504
				r.OfferingID, r.ProfileID, r.AccountRegionID = domain.OfferingMiniMaxSubscriptionAccess, domain.ProfileMiniMaxGlobalSubscriptionOpenAIChat, domain.RegionGlobal
				r.ProviderFailureReason = provider.FailureReasonRateLimited
				r.ProviderCode = "invalid_image_url:messages[0].content[1].image_url"
				r.ProviderRequestID = "upstream-req-42"
				r.FailurePhase = "provider"
				r.Retryable = true
				r.Ambiguous = true
				r.FailureSemanticsRecorded = true
				if latestSuccess && i == n-1 {
					r.Outcome = "success"
					r.ErrorClass, r.ProviderCode, r.ProviderRequestID, r.FailurePhase = "", "", "", ""
					r.HTTPStatus = 0
					r.ProviderFailureReason = ""
					r.Retryable, r.Ambiguous, r.FailureSemanticsRecorded = false, false, false
				}
				if late {
					rows = append(rows, final)
					r.Outcome = "recovered_started_unknown_result"
					r.FailurePhase = "accounting"
					r.ErrorClass = ""
					r.HTTPStatus = 0
					r.ProviderCode, r.ProviderRequestID, r.ProviderFailureReason = "", "", ""
					r.Retryable, r.FailureSemanticsRecorded = false, false
					r.TokenEstimated, r.CostEstimated = true, true
				}
			}
			rows = append(rows, r)
		}
	}
	if !late {
		rows = append(rows, final)
	}
	return rows
}

func drainDiskFailures(t *testing.T, p *DiskProjection, q FailureQuery, cursor DiskFailureCursor, limits DiskUsagePageLimits) ([]RequestFailure, int) {
	t.Helper()
	out := make([]RequestFailure, 0)
	var head ledger.Watermark
	for calls := 1; calls <= 2000; calls++ {
		page, err := p.FailurePage(context.Background(), q, cursor, limits)
		if err != nil {
			t.Fatalf("page %d: %v (cursor %+v)", calls, err, cursor)
		}
		if page.Scanned > limits.ScanRows || page.InputBytes > limits.InputBytes || page.OutputBytes > limits.OutputBytes {
			t.Fatal("page exceeded budget", page)
		}
		if head == (ledger.Watermark{}) {
			head = page.Head
		} else if page.Head != head {
			t.Fatal("head changed")
		}
		if p.db.Stats().OpenTxN != 0 || len(p.readers) != 0 {
			t.Fatal("page retained transaction or reader slot")
		}
		out = append(out, page.Items...)
		if page.Done {
			return out, calls
		}
		if page.Next == (DiskFailureCursor{}) || page.Next == cursor {
			t.Fatal("nonterminal page made no progress")
		}
		cursor = page.Next
	}
	t.Fatal("join failed to terminate")
	return nil, 0
}

func TestDiskFailurePageMatchesAuthenticatedLegacySemantics(t *testing.T) {
	var events []ledger.Event
	for _, c := range []struct {
		id, outcome   string
		n             int
		late, success bool
	}{
		{"ok", "success", 1, false, true}, {"fallback", "success", 2, false, true}, {"failed", "provider_error", 2, false, false},
		{"rejected", "rejected", 0, false, false}, {"unrenderable", "provider_error", 2, false, true}, {"late", "interrupted", 1, true, false},
		{"empty-outcome", "", 0, false, false},
	} {
		events = append(events, diskFailureRequest(c.id, c.outcome, c.n, c.late, c.success)...)
	}
	f := newDiskFailureFixture(t, events)
	queries := []FailureQuery{{}, {RequestID: "failed"}, {RequestID: "missing"}, {ProjectID: "project"}, {ProjectID: "foreign"}, {RequestedModel: "chat"}, {ProviderID: "provider_a"}, {DeploymentID: "dep_b"}, {ProviderModel: "model-a"}, {ProviderID: "provider_a", DeploymentID: "dep_b"}, {OfferingID: domain.OfferingMiniMaxSubscriptionAccess}, {AccountRegionID: domain.RegionGlobal}, {ProviderFailureReason: provider.FailureReasonRateLimited}, {BeforeSequence: 20}, {Start: diskFailureBase("").OccurredAt.Add(time.Minute)}, {End: diskFailureBase("").OccurredAt.Add(time.Minute)}}
	for i, q := range queries {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			q.Limit = 100
			want, err := f.a.QueryFailedRequests(q)
			if err != nil {
				t.Fatal(err)
			}
			q.Limit = 2
			got, _ := drainDiskFailures(t, f.p, q, DiskFailureCursor{}, DiskUsagePageLimits{ScanRows: 4, InputBytes: 8 << 20, OutputBytes: 8 << 20})
			if !reflect.DeepEqual(got, want.Failures) {
				t.Fatalf("disk %#v != legacy %#v", got, want.Failures)
			}
		})
	}
}

func TestDiskFailureWideJoinAndOutputContinuation(t *testing.T) {
	f := newDiskFailureFixture(t, diskFailureRequest("wide", "provider_error", 180, false, false))
	q := FailureQuery{Limit: 100, ProviderID: "provider_a"}
	got, calls := drainDiskFailures(t, f.p, q, DiskFailureCursor{}, DiskUsagePageLimits{ScanRows: 4, InputBytes: 8 << 20, OutputBytes: 8 << 20})
	if calls < 50 || len(got) != 1 || got[0].LastFailure == nil || got[0].LastFailure.AttemptID != "wide_att_179" {
		t.Fatal(calls, got)
	}
	q.ProviderID = "absent"
	got, calls = drainDiskFailures(t, f.p, q, DiskFailureCursor{}, DiskUsagePageLimits{ScanRows: 4, InputBytes: 8 << 20, OutputBytes: 8 << 20})
	if calls < 50 || len(got) != 0 {
		t.Fatal("unresolved predicate became a match", calls, got)
	}
	// Byte admission must also stop before decoding the next association. A
	// resumed page can reread its summary and exactly one largest attempt.
	maxSummary, maxAttempt := 0, 0
	if err := f.p.db.View(func(tx *bbolt.Tx) error {
		for name, target := range map[string]*int{"summaries": &maxSummary, "attempts": &maxAttempt} {
			if err := tx.Bucket([]byte(name)).ForEach(func(_, v []byte) error {
				if len(v) > *target {
					*target = len(v)
				}
				return nil
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	q.ProviderID = "provider_a"
	got, calls = drainDiskFailures(t, f.p, q, DiskFailureCursor{}, DiskUsagePageLimits{ScanRows: 256, InputBytes: maxSummary + 8 + maxAttempt + 49, OutputBytes: 8 << 20})
	if calls < 170 || len(got) != 1 {
		t.Fatal("byte budget did not resume a wide join", calls, got)
	}
	var events []ledger.Event
	for i := 0; i < 4; i++ {
		events = append(events, diskFailureRequest(fmt.Sprint("out", i), "provider_error", 1, false, false)...)
	}
	f = newDiskFailureFixture(t, events)
	q = FailureQuery{Limit: 100}
	want, _ := f.a.QueryFailedRequests(q)
	encoded, _ := json.Marshal(want.Failures[0])
	limits := diskTestLimits
	limits.OutputBytes = len(encoded)
	first, err := f.p.FailurePage(context.Background(), q, DiskFailureCursor{}, limits)
	if err != nil || len(first.Items) != 1 || first.Next.pending == 0 || !first.Next.resolved {
		t.Fatal("ready row lost when output page filled", first, err)
	}
	rest, _ := drainDiskFailures(t, f.p, q, first.Next, limits)
	got = append(first.Items, rest...)
	if !reflect.DeepEqual(got, want.Failures) {
		t.Fatal("output continuation lost rows", got, want.Failures)
	}
}

func TestDiskFailureCorruptionDiscardsEarlierPageItems(t *testing.T) {
	for _, fault := range []string{"missing-attempt", "foreign-attempt", "nonempty-index", "foreign-summary", "wide-attempt"} {
		t.Run(fault, func(t *testing.T) {
			events := append(diskFailureRequest("older", "provider_error", 1, false, false), diskFailureRequest("newer", "provider_error", 1, false, false)...)
			f := newDiskFailureFixture(t, events)
			if err := f.p.db.Update(func(tx *bbolt.Tx) error {
				attempts := tx.Bucket([]byte("attempts"))
				key := diskUsageSequence(4)
				switch fault {
				case "missing-attempt":
					return attempts.Delete(key)
				case "foreign-attempt":
					var a AttemptEvent
					if err := json.Unmarshal(attempts.Get(key), &a); err != nil {
						return err
					}
					a.RequestID = "foreign"
					return diskUsagePut(tx, "attempts", key, a)
				case "nonempty-index":
					return tx.Bucket([]byte("attempt_index")).Put(diskUsageIndex(1, "older", 4), []byte("untrusted"))
				case "foreign-summary":
					var s RequestSummary
					b := tx.Bucket([]byte("summaries"))
					key = diskUsageSequence(5)
					if err := json.Unmarshal(b.Get(key), &s); err != nil {
						return err
					}
					s.RequestID = "foreign"
					return diskUsagePut(tx, "summaries", key, s)
				case "wide-attempt":
					return attempts.Put(key, bytes.Repeat([]byte{'x'}, diskUsageMaxValue+1))
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			q := FailureQuery{Limit: 100}
			if fault == "foreign-summary" {
				q.ProjectID = "project"
				q.RequestID = "older"
			}
			page, err := f.p.FailurePage(context.Background(), q, DiskFailureCursor{}, diskTestLimits)
			if !errors.Is(err, ErrDiskUsageCorrupt) || !reflect.DeepEqual(page, DiskFailurePage{}) {
				t.Fatal("corruption returned partial page", page, err)
			}
			if len(f.p.readers) != 0 || f.p.db.Stats().OpenTxN != 0 {
				t.Fatal("error retained transaction/slot")
			}
		})
	}
}

func TestDiskFailureFrozenJoinExcludesLaterSettlement(t *testing.T) {
	events := diskFailureRequest("frozen", "provider_error", 20, false, false)
	// Reserve the later successful attempt before finalization; settle only
	// after the query has frozen H and begun scanning the old associations.
	future := diskFailureRequest("frozen", "provider_error", 21, false, true)
	lease, start, settled := future[len(future)-4], future[len(future)-3], future[len(future)-2]
	events = append(events[:len(events)-1], lease, start, events[len(events)-1])
	f := newDiskFailureFixture(t, events)
	q := FailureQuery{Limit: 100, ProviderID: "provider_a"}
	limits := DiskUsagePageLimits{ScanRows: 4, InputBytes: 8 << 20, OutputBytes: 8 << 20}
	first, err := f.p.FailurePage(context.Background(), q, DiskFailureCursor{}, limits)
	if err != nil || first.Done || len(first.Items) != 0 || first.Next.pending == 0 {
		t.Fatal(first, err)
	}
	row, _, err := f.native.AppendRecordWithReceipt(context.Background(), settled)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.d.Apply(row); err != nil {
		t.Fatal(err)
	}
	if err := f.owner.Apply(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	got, _ := drainDiskFailures(t, f.p, q, first.Next, limits)
	if len(got) != 1 || got[0].LastFailure == nil || got[0].LastFailure.AttemptID != "frozen_att_19" {
		t.Fatal("old H included late settlement", got)
	}
	current, _ := drainDiskFailures(t, f.p, q, DiskFailureCursor{}, limits)
	if len(current) != 1 || current[0].LastFailure != nil {
		t.Fatal("fresh H missed latest successful settlement", current)
	}
}

func TestDiskFailureCursorErrorsReturnNoPartialRows(t *testing.T) {
	f := newDiskFailureFixture(t, diskFailureRequest("errors", "provider_error", 5, false, false))
	q := FailureQuery{Limit: 100, ProviderID: "provider_a"}
	first, err := f.p.FailurePage(context.Background(), q, DiskFailureCursor{}, DiskUsagePageLimits{ScanRows: 2, InputBytes: 8 << 20, OutputBytes: 8 << 20})
	if err != nil {
		t.Fatal(err)
	}
	assertEmpty := func(page DiskFailurePage, err error) {
		t.Helper()
		if err == nil || !reflect.DeepEqual(page, DiskFailurePage{}) {
			t.Fatal("error leaked partial rows/cursor", page, err)
		}
	}
	changed := q
	changed.ProjectID = "foreign"
	page, err := f.p.FailurePage(context.Background(), changed, first.Next, diskTestLimits)
	assertEmpty(page, err)
	other := newDiskFailureFixture(t, diskFailureRequest("other", "rejected", 0, false, false))
	page, err = other.p.FailurePage(context.Background(), q, first.Next, diskTestLimits)
	assertEmpty(page, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	page, err = f.p.FailurePage(ctx, q, first.Next, diskTestLimits)
	assertEmpty(page, err)
	limits := diskTestLimits
	limits.ScanRows = 1
	page, err = f.p.FailurePage(context.Background(), q, first.Next, limits)
	assertEmpty(page, err)
	limits = diskTestLimits
	limits.InputBytes = 1
	page, err = f.p.FailurePage(context.Background(), q, DiskFailureCursor{}, limits)
	assertEmpty(page, err)
	limits = diskTestLimits
	limits.OutputBytes = 1
	page, err = f.p.FailurePage(context.Background(), q, first.Next, limits)
	assertEmpty(page, err)
	// An indexed companion disappears midway through a multi-page association.
	var saved []byte
	key := diskUsageSequence(first.Next.attemptBefore - 3)
	if err := f.p.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte("attempts"))
		saved = append([]byte(nil), b.Get(key)...)
		return b.Delete(key)
	}); err != nil {
		t.Fatal(err)
	}
	page, err = f.p.FailurePage(context.Background(), q, first.Next, diskTestLimits)
	assertEmpty(page, err)
	if !errors.Is(err, ErrDiskUsageCorrupt) {
		t.Fatal(err)
	}
	if err := f.p.db.Update(func(tx *bbolt.Tx) error { return tx.Bucket([]byte("attempts")).Put(key, saved) }); err != nil {
		t.Fatal(err)
	}
	got, _ := drainDiskFailures(t, f.p, q, first.Next, diskTestLimits)
	if len(got) != 1 {
		t.Fatal("input cursor could not retry", got)
	}
	if err := f.p.Close(); err != nil {
		t.Fatal(err)
	}
	page, err = f.p.FailurePage(context.Background(), q, first.Next, diskTestLimits)
	assertEmpty(page, err)
}

func TestDiskFailureWidthAndBusyAdmissionPrecedeEncoding(t *testing.T) {
	p, err := CreateDiskProjection(filepath.Join(t.TempDir(), "usage.db"), diskGrowthReader())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for _, q := range []FailureQuery{{Limit: 1, ProjectID: strings.Repeat("p", 2<<20)}, {Limit: 1, RequestID: strings.Repeat("r", 600000), ProviderModel: strings.Repeat("m", 600000)}, {Limit: 1, ProviderModel: strings.Repeat("<", 200000)}} {
		if _, err := p.FailurePage(context.Background(), q, DiskFailureCursor{}, diskTestLimits); !errors.Is(err, ErrDiskUsageLimit) {
			t.Fatal(err)
		}
	}
	entered, release, done := make(chan struct{}, 2), make(chan struct{}), make(chan error, 2)
	var once sync.Once
	defer once.Do(func() { close(release) })
	for i := 0; i < 2; i++ {
		go func() {
			done <- p.view(context.Background(), func(*bbolt.Tx) error { entered <- struct{}{}; <-release; return nil })
		}()
	}
	<-entered
	<-entered
	q := FailureQuery{Limit: 1, ProviderModel: strings.Repeat("<", 100000)}
	result := testing.Benchmark(func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := p.FailurePage(context.Background(), q, DiskFailureCursor{}, diskTestLimits); !errors.Is(err, ErrDiskUsageBusy) {
				b.Fatal(err)
			}
		}
	})
	if result.AllocedBytesPerOp() > 4096 {
		t.Fatal("busy query encoded before slot", result.AllocedBytesPerOp())
	}
	once.Do(func() { close(release) })
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if len(p.readers) != 0 || p.db.Stats().OpenTxN != 0 {
		t.Fatal("slots/transactions retained")
	}
}
