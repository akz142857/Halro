package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/akz142857/Halro/internal/ledger"
	bbolt "go.etcd.io/bbolt"
)

func appendDiskDetailEvent(t *testing.T, f *diskFailureFixture, e ledger.Event) ledger.Record {
	t.Helper()
	ctx := context.Background()
	r, _, err := f.native.AppendRecordWithReceipt(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.d.Apply(r); err != nil {
		t.Fatal(err)
	}
	if err := f.owner.Apply(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := f.a.Apply(r); err != nil {
		t.Fatal(err)
	}
	return r
}

func drainDiskDetail(t *testing.T, p *DiskProjection, q DiskRequestDetailQuery, cursor DiskRequestDetailCursor, limits DiskUsagePageLimits) (DiskRequestDetailPage, []AttemptEvent, int) {
	t.Helper()
	var first DiskRequestDetailPage
	out := make([]AttemptEvent, 0)
	for calls := 1; calls <= 1000; calls++ {
		page, err := p.RequestDetailPage(context.Background(), q, cursor, limits)
		if err != nil {
			t.Fatal("page", calls, err, cursor)
		}
		if page.Scanned > limits.ScanRows || page.InputBytes > limits.InputBytes || page.OutputBytes > limits.OutputBytes {
			t.Fatal("page budget exceeded", page)
		}
		if calls == 1 {
			first = page
		} else if page.Head != first.Head || page.SummarySequence != first.SummarySequence || page.Summary != first.Summary || page.Found != first.Found {
			t.Fatal("mixed detail cuts", first, page)
		}
		if len(p.readers) != 0 || p.db.Stats().OpenTxN != 0 {
			t.Fatal("reader/transaction retained")
		}
		out = append(out, page.Attempts...)
		if page.Done {
			return first, out, calls
		}
		if page.Next == (DiskRequestDetailCursor{}) || page.Next == cursor {
			t.Fatal("no-progress detail cursor")
		}
		cursor = page.Next
	}
	t.Fatal("detail scan failed to terminate")
	return first, nil, 0
}

func TestDiskRequestDetailMatchesNativeLegacyArrivalOrder(t *testing.T) {
	var events []ledger.Event
	for _, c := range []struct {
		id   string
		n    int
		late bool
	}{{"chain", 12, false}, {"rejected", 0, false}, {"late", 1, true}} {
		events = append(events, diskFailureRequest(c.id, "provider_error", c.n, c.late, false)...)
	}
	f := newDiskFailureFixture(t, events)
	for _, id := range []string{"chain", "rejected", "late", "missing"} {
		t.Run(id, func(t *testing.T) {
			want, found := f.a.RequestDetail(id)
			page, attempts, _ := drainDiskDetail(t, f.p, DiskRequestDetailQuery{RequestID: id, Limit: 3}, DiskRequestDetailCursor{}, DiskUsagePageLimits{ScanRows: 4, InputBytes: 8 << 20, OutputBytes: 8 << 20})
			if page.Found != found || page.Summary != want.Summary || (found && len(attempts) != len(want.Attempts)) {
				t.Fatal(page, want, found)
			}
			if len(attempts) > 0 && !reflect.DeepEqual(attempts, want.Attempts) {
				t.Fatal("arrival order/attempt facts changed", attempts, want.Attempts)
			}
			if id == "late" && (page.Summary.Attempts != 0 || len(attempts) != 1) {
				t.Fatal("late settlement rewrote terminal summary", page, attempts)
			}
		})
	}
}

func TestDiskRequestDetailPinsActiveVersionAcrossFinalization(t *testing.T) {
	events := diskFailureRequest("active", "provider_error", 3, false, false)
	f := newDiskFailureFixture(t, events[:len(events)-1])
	q := DiskRequestDetailQuery{RequestID: "active", Limit: 1}
	first, err := f.p.RequestDetailPage(context.Background(), q, DiskRequestDetailCursor{}, diskTestLimits)
	if err != nil || first.Done || !first.Found || first.Summary.Sequence != 0 || first.Summary.Attempts != 3 || len(first.Attempts) != 1 {
		t.Fatal(first, err)
	}
	want, _ := f.a.RequestDetail("active")
	future := diskFailureRequest("active", "provider_error", 4, false, false)
	for _, e := range future[len(future)-4:] {
		appendDiskDetailEvent(t, f, e)
	}
	q.Limit = 2 // Row count is tunable; identity and cut remain fixed.
	page, rest, _ := drainDiskDetail(t, f.p, q, first.Next, diskTestLimits)
	if page.Head != first.Head || page.SummarySequence != first.SummarySequence || page.Summary != want.Summary || !reflect.DeepEqual(append(first.Attempts, rest...), want.Attempts) {
		t.Fatal("active cursor observed finalization/later settlement", page, want)
	}
	current, all, _ := drainDiskDetail(t, f.p, q, DiskRequestDetailCursor{}, diskTestLimits)
	newWant, _ := f.a.RequestDetail("active")
	if current.Summary != newWant.Summary || current.Summary.Sequence == 0 || current.Summary.Attempts != 4 || !reflect.DeepEqual(all, newWant.Attempts) {
		t.Fatal("fresh query missed terminal state", current, newWant)
	}
}

func diskDetailVersionDigest(t *testing.T, p *DiskProjection) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := p.db.View(func(tx *bbolt.Tx) error {
		for _, name := range []string{"request_versions", "request_version_head"} {
			if err := tx.Bucket([]byte(name)).ForEach(func(k, v []byte) error { out.Write(k); out.Write(v); return nil }); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestDiskRequestVersionsIgnoreDuplicatesLateAndRollbackAtomically(t *testing.T) {
	events := diskFailureRequest("late-version", "interrupted", 1, true, false)
	f := newDiskFailureFixture(t, events[:4])
	before := diskDetailVersionDigest(t, f.p)
	appendDiskDetailEvent(t, f, events[4])
	appendDiskDetailEvent(t, f, events[4]) // Physical duplicate, original ID retained.
	if !bytes.Equal(before, diskDetailVersionDigest(t, f.p)) {
		t.Fatal("late/duplicate changed terminal version")
	}
	page, attempts, _ := drainDiskDetail(t, f.p, DiskRequestDetailQuery{RequestID: "late-version", Limit: 100}, DiskRequestDetailCursor{}, diskTestLimits)
	if page.SummarySequence != 4 || page.Summary.Sequence != 4 || page.Summary.Attempts != 0 || len(attempts) != 1 || page.Head.Sequence != 6 {
		t.Fatal(page, attempts)
	}
	// Unchanged reservation/start frames retain only the accepted version.
	count := 0
	if err := f.p.db.View(func(tx *bbolt.Tx) error { count = tx.Bucket([]byte("request_versions")).Stats().KeyN; return nil }); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatal("unchanged frames produced versions", count)
	}
	// Force version publication failure after active/attempt/summary writes.
	rows := finalizationFixture(false, false)
	reader := fixtureFinalReader(rows)
	p, err := CreateDiskProjection(filepath.Join(t.TempDir(), "usage.db"), reader)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for _, r := range rows[:4] {
		if err := p.Apply(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket([]byte("request_versions")).Put(diskRequestVersionKey("req", 5), []byte("foreign existing version"))
	}); err != nil {
		t.Fatal(err)
	}
	digest := diskUsageDigest(t, p)
	if err := p.Apply(rows[4]); !errors.Is(err, ErrDiskUsageCorrupt) {
		t.Fatal("immutable version overwrite accepted", err)
	}
	if diskUsageDigest(t, p) != digest {
		t.Fatal("failed version publication changed another bucket/head")
	}
}

func TestDiskRequestDetailByteBudgetsAndWholePageErrors(t *testing.T) {
	f := newDiskFailureFixture(t, diskFailureRequest("bounds", "provider_error", 4, false, false))
	q := DiskRequestDetailQuery{RequestID: "bounds", Limit: 100}
	want, _ := f.a.RequestDetail("bounds")
	summaryRaw, _ := json.Marshal(want.Summary)
	maxAttempt := 0
	for _, a := range want.Attempts {
		raw, _ := json.Marshal(a)
		if len(raw) > maxAttempt {
			maxAttempt = len(raw)
		}
	}
	limits := diskTestLimits
	limits.OutputBytes = len(summaryRaw) + maxAttempt
	_, attempts, calls := drainDiskDetail(t, f.p, q, DiskRequestDetailCursor{}, limits)
	if calls < 4 || !reflect.DeepEqual(attempts, want.Attempts) {
		t.Fatal("output byte continuation lost attempts", calls, attempts)
	}
	var rawSize int
	if err := f.p.db.View(func(tx *bbolt.Tx) error {
		rawSize = len(tx.Bucket([]byte("request_versions")).Get(diskRequestVersionKey("bounds", 14)))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	limits = diskTestLimits
	limits.InputBytes = rawSize + 80 + maxAttempt + 49
	_, attempts, calls = drainDiskDetail(t, f.p, q, DiskRequestDetailCursor{}, limits)
	if calls < 4 || !reflect.DeepEqual(attempts, want.Attempts) {
		t.Fatal("input budget did not resume", calls, attempts)
	}
	assertEmpty := func(page DiskRequestDetailPage, err error) {
		t.Helper()
		if err == nil || !reflect.DeepEqual(page, DiskRequestDetailPage{}) {
			t.Fatal("error leaked partial detail", page, err)
		}
	}
	for _, limits := range []DiskUsagePageLimits{{ScanRows: 1, InputBytes: 8 << 20, OutputBytes: 8 << 20}, {ScanRows: 16, InputBytes: 1, OutputBytes: 8 << 20}, {ScanRows: 16, InputBytes: 8 << 20, OutputBytes: len(summaryRaw) + maxAttempt - 1}} {
		page, err := f.p.RequestDetailPage(context.Background(), q, DiskRequestDetailCursor{}, limits)
		assertEmpty(page, err)
	}
	first, err := f.p.RequestDetailPage(context.Background(), DiskRequestDetailQuery{RequestID: "bounds", Limit: 1}, DiskRequestDetailCursor{}, diskTestLimits)
	if err != nil {
		t.Fatal(err)
	}
	changed := q
	changed.RequestID = "foreign"
	page, err := f.p.RequestDetailPage(context.Background(), changed, first.Next, diskTestLimits)
	assertEmpty(page, err)
	other := newDiskFailureFixture(t, diskFailureRequest("bounds", "provider_error", 1, false, false))
	page, err = other.p.RequestDetailPage(context.Background(), q, first.Next, diskTestLimits)
	assertEmpty(page, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	page, err = f.p.RequestDetailPage(ctx, q, first.Next, diskTestLimits)
	assertEmpty(page, err)
	// Remove the second companion after the first page has been copied. The
	// complete later call fails; retry of the original cursor survives repair.
	key := diskUsageSequence(want.Attempts[1].Sequence)
	var saved []byte
	if err := f.p.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte("attempts"))
		saved = append([]byte(nil), b.Get(key)...)
		return b.Delete(key)
	}); err != nil {
		t.Fatal(err)
	}
	page, err = f.p.RequestDetailPage(context.Background(), q, DiskRequestDetailCursor{}, diskTestLimits)
	assertEmpty(page, err)
	if !errors.Is(err, ErrDiskUsageCorrupt) {
		t.Fatal(err)
	}
	if err := f.p.db.Update(func(tx *bbolt.Tx) error { return tx.Bucket([]byte("attempts")).Put(key, saved) }); err != nil {
		t.Fatal(err)
	}
	_, rest, _ := drainDiskDetail(t, f.p, q, first.Next, diskTestLimits)
	if !reflect.DeepEqual(rest, want.Attempts[1:]) {
		t.Fatal("cursor retry failed", rest)
	}
	if err := f.p.Close(); err != nil {
		t.Fatal(err)
	}
	page, err = f.p.RequestDetailPage(context.Background(), q, first.Next, diskTestLimits)
	assertEmpty(page, err)
}

func TestDiskRequestDetailVersionCompanionsFailClosed(t *testing.T) {
	for _, fault := range []string{"missing-version", "missing-head", "foreign-id", "version-sequence", "head-future", "wide-version", "selected-deleted"} {
		t.Run(fault, func(t *testing.T) {
			f := newDiskFailureFixture(t, diskFailureRequest("corrupt-detail", "provider_error", 2, false, false))
			q := DiskRequestDetailQuery{RequestID: "corrupt-detail", Limit: 1}
			first, err := f.p.RequestDetailPage(context.Background(), q, DiskRequestDetailCursor{}, diskTestLimits)
			if err != nil {
				t.Fatal(err)
			}
			key := diskRequestVersionKey(q.RequestID, first.SummarySequence)
			if err := f.p.db.Update(func(tx *bbolt.Tx) error {
				versions := tx.Bucket([]byte("request_versions"))
				heads := tx.Bucket([]byte("request_version_head"))
				switch fault {
				case "missing-version", "selected-deleted":
					return versions.Delete(key)
				case "missing-head":
					return heads.Delete(diskUsageID(q.RequestID))
				case "head-future":
					return heads.Put(diskUsageID(q.RequestID), diskUsageSequence(first.Head.Sequence+1))
				case "wide-version":
					return versions.Put(key, bytes.Repeat([]byte{'x'}, diskUsageMaxValue+1))
				default:
					var v diskRequestVersion
					if err := json.Unmarshal(versions.Get(key), &v); err != nil {
						return err
					}
					if fault == "foreign-id" {
						v.Summary.RequestID = "foreign"
					} else {
						v.Sequence++
					}
					return diskUsagePut(tx, "request_versions", key, v)
				}
			}); err != nil {
				t.Fatal(err)
			}
			cursor := DiskRequestDetailCursor{}
			if fault == "selected-deleted" {
				cursor = first.Next
			}
			page, err := f.p.RequestDetailPage(context.Background(), q, cursor, diskTestLimits)
			if !errors.Is(err, ErrDiskUsageCorrupt) || !reflect.DeepEqual(page, DiskRequestDetailPage{}) {
				t.Fatal("version corruption returned current/older/fake-empty summary", fault, page, err)
			}
		})
	}
}

func TestDiskRequestDetailWidthAndSlotsBoundPreparation(t *testing.T) {
	p, err := CreateDiskProjection(filepath.Join(t.TempDir(), "usage.db"), diskGrowthReader())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for _, id := range []string{"", strings.Repeat("p", 2<<20), strings.Repeat("<", 200000)} {
		if _, err := p.RequestDetailPage(context.Background(), DiskRequestDetailQuery{RequestID: id, Limit: 1}, DiskRequestDetailCursor{}, diskTestLimits); !errors.Is(err, ErrDiskUsageLimit) {
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
	q := DiskRequestDetailQuery{RequestID: strings.Repeat("<", 100000), Limit: 1}
	result := testing.Benchmark(func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := p.RequestDetailPage(context.Background(), q, DiskRequestDetailCursor{}, diskTestLimits); !errors.Is(err, ErrDiskUsageBusy) {
				b.Fatal(err)
			}
		}
	})
	if result.AllocedBytesPerOp() > 4096 {
		t.Fatal("busy detail encoded before admission", result.AllocedBytesPerOp())
	}
	once.Do(func() { close(release) })
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if len(p.readers) != 0 || p.db.Stats().OpenTxN != 0 {
		t.Fatal("reader/tx retained")
	}
	page, err := p.RequestDetailPage(context.Background(), DiskRequestDetailQuery{RequestID: "missing", Limit: 1}, DiskRequestDetailCursor{}, diskTestLimits)
	if err != nil || page.Found || !page.Done {
		t.Fatal(page, err)
	}
}

func TestDiskRequestDetailWideNativeValuesAndOwnedResults(t *testing.T) {
	events := diskFailureRequest("wide-detail", "provider_error", 2, false, false)
	for i := range events {
		events[i].RequestedModel = strings.Repeat("m", 40<<10)
		if events[i].AttemptID != "" {
			events[i].ProviderModel = strings.Repeat("p", 40<<10)
		}
	}
	f := newDiskFailureFixture(t, events)
	q := DiskRequestDetailQuery{RequestID: "wide-detail", Limit: 100}
	want, _ := f.a.RequestDetail(q.RequestID)
	summaryBytes, _ := json.Marshal(want.Summary)
	maxAttempt := 0
	for _, a := range want.Attempts {
		raw, _ := json.Marshal(a)
		if len(raw) > maxAttempt {
			maxAttempt = len(raw)
		}
	}
	limits := diskTestLimits
	limits.OutputBytes = len(summaryBytes) + maxAttempt
	page, attempts, calls := drainDiskDetail(t, f.p, q, DiskRequestDetailCursor{}, limits)
	if calls < 2 || page.Summary != want.Summary || !reflect.DeepEqual(attempts, want.Attempts) {
		t.Fatal("legal wide native detail was truncated", calls, page, attempts)
	}
	*attempts[0].CostMicrosUSD = 999
	attempts[0].ProviderModel = "changed returned alias"
	page.Summary.RequestedModel = "changed returned summary"
	fresh, again, _ := drainDiskDetail(t, f.p, q, DiskRequestDetailCursor{}, limits)
	if fresh.Summary != want.Summary || !reflect.DeepEqual(again, want.Attempts) {
		t.Fatal("caller mutation changed stored detail", fresh, again)
	}
}
