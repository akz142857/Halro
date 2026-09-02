package usage

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"runtime"
	"testing"
	"time"
	"unsafe"

	"github.com/akz142857/Halro/internal/ledger"
)

// The facts a retention window is built on.
//
// Trimming the aggregate is only safe because of three properties of the code
// around it, and every one of them was read out of the source rather than
// asserted anywhere: the export selects strictly above its own watermark, the
// reconciliation compares only what Parquet still holds, and the dashboard
// needs seven days. A window built on an unpinned reading is a window that
// silently starts losing history the day one of them changes.
//
// These tests exist before the window does, for the same reason the failure
// taxonomy was pinned before anything was built on it.

func retentionEvents(t *testing.T, count int, at time.Time) *Aggregate {
	t.Helper()
	aggregate := NewAggregate()
	var sequence uint64
	for index := range count {
		requestID := fmt.Sprintf("req_%06d", index)
		occurred := at.Add(time.Duration(index) * time.Second)
		for _, event := range []ledger.Event{
			{
				EventID: requestID + "_accepted", Kind: ledger.EventRequestAccepted,
				RequestID: requestID, ProjectID: "project", PeriodID: "2026-09-01",
				RequestedModel: "chat", OccurredAt: occurred,
			},
			{
				EventID: requestID + "_settled", Kind: ledger.EventAttemptSettled,
				RequestID: requestID, AttemptID: requestID + ":1", AttemptNumber: 1,
				ProjectID: "project", PeriodID: "2026-09-01", ProviderID: "provider_1",
				DeploymentID: "dep_1", ProviderModel: "gpt-4o", RequestedModel: "chat",
				OccurredAt: occurred, Outcome: "success",
				CommittedMicrosUSD: ledger.MicrosUSD(1), ProviderInputTokens: 10,
			},
			{
				EventID: requestID + "_final", Kind: ledger.EventRequestFinalized,
				RequestID: requestID, ProjectID: "project", PeriodID: "2026-09-01",
				RequestedModel: "chat", OccurredAt: occurred.Add(time.Second), Outcome: "success",
			},
		} {
			sequence++
			if err := aggregate.Apply(ledger.Record{
				Generation: 1,
				Sequence:   sequence, Offset: int64(sequence * 100), Event: event,
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	return aggregate
}

// The bound on trimming. Export takes only attempts above the manifest's
// watermark, so anything trimmed below it is already archived and anything
// trimmed above it is gone for good. The window has to stop at this line, and
// this is the line.
func TestExportTakesOnlyWhatIsAboveItsWatermark(t *testing.T) {
	root := filepath.Join(t.TempDir(), "usage")
	exporter, err := NewExporter(root)
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	aggregate := retentionEvents(t, 3, day)
	snapshot := aggregate.Snapshot()

	first, err := exporter.Export(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if first.LastSequence == 0 {
		t.Fatalf("nothing was exported: %#v", first)
	}
	var exported int64
	for _, entry := range first.Files {
		exported += entry.Records
	}
	if exported != int64(len(snapshot.Attempts)) {
		t.Fatalf("exported %d of %d attempts", exported, len(snapshot.Attempts))
	}

	// Offering the same snapshot again writes nothing: every attempt is at or
	// below the watermark. This is what makes trimming below it safe.
	repeat, err := exporter.Export(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if repeat.LastSequence != first.LastSequence || len(repeat.Files) != len(first.Files) {
		t.Fatalf("a second export of the same snapshot changed the manifest: %#v", repeat)
	}

	// And an attempt above the watermark is still pending. Trimming one of
	// these would remove it from the only place it had left to be archived
	// from — which is why the window is bounded by LastSequence and not by
	// time alone.
	for _, attempt := range snapshot.Attempts {
		if attempt.Sequence > first.LastSequence {
			t.Fatalf("attempt %d is above the watermark it was exported under", attempt.Sequence)
		}
	}
}

// Reconciliation compares the aggregate against Parquet only inside the range
// Parquet still holds. A shorter aggregate window therefore does not, by
// itself, make the two disagree about records outside it — but it does make
// them disagree inside it, which is why the aggregate's own floor has to be
// taught to Reconcile before any trimming ships.
func TestReconcileComparesOnlyWhatParquetStillHolds(t *testing.T) {
	root := filepath.Join(t.TempDir(), "usage")
	exporter, err := NewExporter(root)
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	aggregate := retentionEvents(t, 4, day)
	snapshot := aggregate.Snapshot()
	if _, err := exporter.Export(snapshot); err != nil {
		t.Fatal(err)
	}
	report, err := exporter.Reconcile(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if report.LedgerRecords != report.ParquetRecords || report.ParquetRecords == 0 {
		t.Fatalf("a freshly exported snapshot does not reconcile: %#v", report)
	}

	// The shape a trimmed aggregate will present: the same Parquet, fewer
	// attempts in hand. Reconcile does not report this as a difference — it
	// refuses outright, and `halro doctor` and `halro usage verify` refuse with
	// it. So trimming cannot ship before Reconcile is taught the aggregate's
	// own floor; this is not a cosmetic mismatch to tidy up afterwards.
	trimmed := snapshot
	trimmed.Attempts = snapshot.Attempts[len(snapshot.Attempts)-1:]
	if _, err := exporter.Reconcile(trimmed); err == nil {
		t.Fatal("a trimmed aggregate reconciled against a full Parquet; the ordering constraint this plan rests on is gone")
	}
}

// Seven days is the floor for any console window, because the dashboard reads
// hourly buckets and request summaries over exactly that span. A window below
// it would leave the overview with holes rather than with a shorter history.
func TestTheDashboardNeedsSevenDaysOfHistory(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	aggregate := NewAggregate()
	var sequence uint64
	for _, age := range []time.Duration{1 * time.Hour, 3 * 24 * time.Hour, 6 * 24 * time.Hour, 8 * 24 * time.Hour} {
		at := now.Add(-age)
		requestID := fmt.Sprintf("req_%d", int(age.Hours()))
		for _, event := range []ledger.Event{
			{
				EventID: requestID + "_accepted", Kind: ledger.EventRequestAccepted,
				RequestID: requestID, ProjectID: "project", PeriodID: "2026-09-08",
				RequestedModel: "chat", OccurredAt: at,
			},
			{
				EventID: requestID + "_final", Kind: ledger.EventRequestFinalized,
				RequestID: requestID, ProjectID: "project", PeriodID: "2026-09-08",
				RequestedModel: "chat", OccurredAt: at, Outcome: "success",
			},
		} {
			sequence++
			if err := aggregate.Apply(ledger.Record{
				Generation: 1,
				Sequence:   sequence, Offset: int64(sequence * 100), Event: event,
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	dashboard := aggregate.Dashboard(now, Period{ID: "2026-09-08", TimezoneVersion: 1})
	var counted int64
	for _, bucket := range dashboard.Hourly {
		counted += bucket.Requests
	}
	// Three of the four are inside the window; the eight-day-old one is not.
	if counted != 3 {
		t.Fatalf("the dashboard counted %d requests over its seven-day window, want 3", counted)
	}
}

// The baseline this plan is measured against, and a ceiling on the record's
// size while we are here. The checkpoint carries every attempt, so its cost is
// this number times the window — a change that quietly doubles the record
// doubles the window's cost with it.
func TestTheCheckpointCostsAKnownAmountPerAttempt(t *testing.T) {
	day := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	const attempts = 500
	aggregate := retentionEvents(t, attempts, day)
	snapshot, err := aggregate.TakeCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	// Head plus every segment: what one round writes for a window it has never
	// checkpointed before is the whole window, and that total is what the
	// sizing table is derived from. What a *later* round writes is a different
	// question, and TestCheckpointWritesOnlyTheRecordsSinceTheLastOne asks it.
	perAttempt := checkpointBytes(snapshot) / attempts
	t.Logf("checkpoint: %d bytes for %d attempts (%d bytes each)",
		checkpointBytes(snapshot), attempts, perAttempt)
	// Wide enough that ordinary field changes do not trip it, narrow enough
	// that a structural bloat does. The measured figure for a settled attempt
	// with a price snapshot is about 1150 bytes; these fixtures carry no
	// snapshot, so they sit lower.
	if perAttempt < 200 || perAttempt > 2000 {
		t.Fatalf("a checkpointed attempt is %d bytes; the window's cost is this times its length", perAttempt)
	}
	// And it is linear in the number of attempts, which is the whole reason a
	// window bounds anything.
	half := retentionEvents(t, attempts/2, day)
	halfSnapshot, err := half.TakeCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	ratio := float64(checkpointBytes(snapshot)) / float64(checkpointBytes(halfSnapshot))
	if ratio < 1.7 || ratio > 2.3 {
		t.Fatalf("checkpoint size is not linear in attempt count: %.2fx for twice the attempts", ratio)
	}
	var decoded map[string]any
	if err := json.Unmarshal(snapshot.Head, &decoded); err != nil {
		t.Fatalf("the checkpoint head is not the JSON this measurement assumes: %v", err)
	}
}

// The other half of what a window costs, and since S9 the half that binds
// first: the aggregate is held in memory, so the window's length is a resident
// footprint whether or not a checkpoint is running. The disk figure above and
// this one answer different questions and an operator sizing an instance needs
// both — the guide's table carries them side by side.
func TestTheWindowCostsAKnownAmountOfMemoryPerAttempt(t *testing.T) {
	day := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	const attempts = 20000

	// The part that does not depend on measuring anything: the two records are
	// held in slices, so their struct sizes alone fix a floor that no amount of
	// shortening identifiers gets under. This is the number the operator
	// guide's sizing table is anchored to.
	perAttemptFloor := int(unsafe.Sizeof(AttemptEvent{})) + int(unsafe.Sizeof(RequestSummary{}))
	if perAttemptFloor < 512 || perAttemptFloor > 1200 {
		t.Fatalf("an attempt and its summary occupy %d bytes of struct; the guide says about a kilobyte per attempt", perAttemptFloor)
	}

	// And the measured figure on top of it — strings, maps, the indexes. Twice,
	// because a single collection can leave objects that became unreachable
	// during the cycle still counted, which reads as a smaller aggregate than
	// there is.
	runtime.GC()
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	aggregate := retentionEvents(t, attempts, day)
	runtime.GC()
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	perAttempt := int(after.HeapAlloc-before.HeapAlloc) / attempts
	t.Logf("resident: %d bytes for %d attempts (%d bytes each, struct floor %d)",
		after.HeapAlloc-before.HeapAlloc, attempts, perAttempt, perAttemptFloor)

	// Only the ceiling is asserted. A baseline taken while another test's
	// garbage is still counted makes the delta *smaller*, never larger, so a
	// low reading is a noisy measurement rather than a leaner aggregate — the
	// floor above is what pins that direction. A high reading cannot be noise
	// in the same way: it means a per-record allocation appeared.
	if perAttempt > 3000 {
		t.Fatalf("a resident attempt is %d bytes; the window's memory is this times its length", perAttempt)
	}
	runtime.KeepAlive(aggregate)
}

// A sweep has to cost what it drops. The window is trimmed on a timer while the
// process serves traffic, and it holds the aggregate's write lock to do it, so
// a sweep whose cost followed the resident set would stall the collector and
// every console read for longer the longer the window is — worst exactly where
// the window matters most.
//
// The observable form of that promise is the backing array: dropping a prefix
// leaves dead slots behind, and they are reclaimed on a schedule that keeps the
// waste bounded rather than by copying the whole window every time.
func TestASweepDoesNotCopyTheWholeWindow(t *testing.T) {
	day := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	aggregate := retentionEvents(t, 4000, day)
	resident := len(aggregate.Snapshot().Attempts)
	if resident < 4000 {
		t.Fatalf("fixture holds %d attempts, too few to say anything", resident)
	}

	// Sweep repeatedly, the way the export tick does, each time taking a little
	// more of the window.
	swept := 0
	for hour := 1; hour <= 20; hour++ {
		result := aggregate.PruneBefore(day.Add(time.Duration(hour)*time.Hour), ^uint64(0))
		swept += result.Attempts
		live := len(aggregate.attempts)
		if capacity := cap(aggregate.attempts); live > 0 && capacity > 2*live+16 {
			t.Fatalf("after %d sweeps the backing array is %d slots for %d live records",
				hour, capacity, live)
		}
	}
	if swept == 0 {
		t.Fatal("nothing was swept, so this test proves nothing")
	}
	// And what is left is still exactly the records above the floor, in order.
	floor := aggregate.Floor()
	previous := uint64(0)
	for _, attempt := range aggregate.Snapshot().Attempts {
		if attempt.Sequence < floor {
			t.Fatalf("attempt %d survives below the floor %d", attempt.Sequence, floor)
		}
		if attempt.Sequence <= previous {
			t.Fatalf("attempts are out of order at %d after %d", attempt.Sequence, previous)
		}
		previous = attempt.Sequence
	}
}

// checkpointBytes is everything one round hands the store.
func checkpointBytes(snapshot CheckpointSnapshot) int {
	total := len(snapshot.Head)
	for _, segment := range snapshot.Segments {
		total += len(segment.Payload)
	}
	return total
}

// The window itself. Trimming keeps the two conditions that make it safe: old
// enough, and already exported.
func TestPruningKeepsWhatIsNotYetExported(t *testing.T) {
	day := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	aggregate := retentionEvents(t, 10, day)
	before, err := aggregate.QueryAttempts(AttemptQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Attempts) != 10 {
		t.Fatalf("fixture holds %d attempts", len(before.Attempts))
	}

	// Nothing exported: nothing is trimmed, however old. An aggregate that
	// grows is a problem; one that discards unarchived history is a defect.
	if result := aggregate.PruneBefore(day.Add(time.Hour), 0); result.Attempts != 0 || result.Summaries != 0 {
		t.Fatalf("pruning ran with no export watermark: %#v", result)
	}

	// Exported through the fifth-oldest request's settlement. QueryAttempts
	// answers newest-first, so the fifth oldest of ten is five from the end.
	// The sixth and everything after it stays, even though the cutoff covers
	// them.
	exportedThrough := before.Attempts[len(before.Attempts)-5].Sequence
	result := aggregate.PruneBefore(day.Add(24*time.Hour), exportedThrough)
	if result.Attempts != 5 {
		t.Fatalf("trimmed %d attempts, want 5: %#v", result.Attempts, result)
	}
	if result.Floor != exportedThrough+1 {
		t.Fatalf("floor = %d, want %d", result.Floor, exportedThrough+1)
	}
	after, err := aggregate.QueryAttempts(AttemptQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Attempts) != 5 {
		t.Fatalf("%d attempts survived, want 5", len(after.Attempts))
	}
	for _, attempt := range after.Attempts {
		if attempt.Sequence <= exportedThrough {
			t.Fatalf("attempt %d is below the export watermark and was kept", attempt.Sequence)
		}
	}
	// The indexes follow, or a request detail lookup would read the wrong row.
	detail, exists := aggregate.RequestDetail(after.Attempts[0].RequestID)
	if !exists || len(detail.Attempts) != 1 ||
		detail.Attempts[0].AttemptID != after.Attempts[0].AttemptID {
		t.Fatalf("the attempt index did not survive the prune: %#v", detail)
	}
}

// Age alone does not trim, and neither does the watermark alone.
func TestPruningKeepsWhatIsInsideTheWindow(t *testing.T) {
	day := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	aggregate := retentionEvents(t, 6, day)
	everything := aggregate.Snapshot().Watermark.Sequence
	// Everything exported, but the cutoff is before all of it.
	if result := aggregate.PruneBefore(day.Add(-time.Hour), everything); result.Attempts != 0 {
		t.Fatalf("a cutoff before the whole window trimmed %d attempts", result.Attempts)
	}
	if aggregate.Floor() != 0 {
		t.Fatalf("floor moved without anything being trimmed: %d", aggregate.Floor())
	}
}

// The floor has to survive a restart, or reconciliation forgets the window and
// refuses again on the next start.
func TestTheWindowFloorSurvivesACheckpoint(t *testing.T) {
	day := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	aggregate := retentionEvents(t, 8, day)
	everything := aggregate.Snapshot().Watermark.Sequence
	result := aggregate.PruneBefore(day.Add(24*time.Hour), everything)
	if result.Floor == 0 {
		t.Fatal("nothing was trimmed, so this test proves nothing")
	}
	restored, err := restoreOneRound(aggregate)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Floor() != result.Floor {
		t.Fatalf("floor = %d after restore, want %d", restored.Floor(), result.Floor)
	}
}

// The flip side of TestReconcileComparesOnlyWhatParquetStillHolds: an aggregate
// that says where its window starts reconciles, where one that does not is
// refused. This is the precondition trimming could not ship without.
func TestAWindowedAggregateStillReconciles(t *testing.T) {
	root := filepath.Join(t.TempDir(), "usage")
	exporter, err := NewExporter(root)
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	aggregate := retentionEvents(t, 6, day)
	manifest, err := exporter.Export(aggregate.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exporter.Reconcile(aggregate.Snapshot()); err != nil {
		t.Fatalf("a full aggregate does not reconcile: %v", err)
	}

	result := aggregate.PruneBefore(day.Add(24*time.Hour), manifest.LastSequence)
	if result.Attempts == 0 {
		t.Fatal("nothing was trimmed, so this test proves nothing")
	}
	if _, err := exporter.Reconcile(aggregate.Snapshot()); err != nil {
		t.Fatalf("a windowed aggregate was refused: %v", err)
	}
	// And a snapshot that lost records without declaring a floor is still
	// refused — the fix is that the aggregate says where its window starts,
	// not that reconciliation stopped checking.
	lying := aggregate.Snapshot()
	lying.Floor = 0
	if _, err := exporter.Reconcile(lying); err == nil {
		t.Fatal("an aggregate that hid its window reconciled anyway")
	}
}
