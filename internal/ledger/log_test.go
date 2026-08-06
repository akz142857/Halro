package ledger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Heimdall/internal/domain"
)

func TestMixedV1V2ReplayAndUnsupportedFutureEpoch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mixed.wal")
	log, err := Open(path, NewStatus())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 4, 0, 0, 0, 0, time.UTC)
	if _, err = log.Append(context.Background(), Event{EventID: "legacy", Kind: EventRequestAccepted, RequestID: "r", ProjectID: "p", PeriodID: "2026-08-04", OccurredAt: now}); err != nil {
		t.Fatal(err)
	}
	unknown := domain.NewUnknownPriceSnapshot(now)
	if _, err = log.Append(context.Background(), Event{EventID: "v2", Kind: EventAttemptSettled, RequestID: "r", AttemptID: "a", ProjectID: "p", PeriodID: "2026-08-04", OccurredAt: now,
		LeaseMode: LeaseModeUnknownAllowed, PriceSnapshot: &unknown, Outcome: "success", TokenUsageSource: TokenUsageSourceNone}); err != nil {
		t.Fatal(err)
	}
	if err = log.Close(); err != nil {
		t.Fatal(err)
	}
	var epochs []uint8
	if _, partial, err := InspectReplay(path, func(record Record) error { epochs = append(epochs, record.Epoch); return nil }); err != nil || partial || !slices.Equal(epochs, []uint8{1, 2}) {
		t.Fatalf("epochs=%v partial=%t err=%v", epochs, partial, err)
	}
	payload, _ := json.Marshal(Event{EventID: "future", Kind: EventRequestAccepted, RequestID: "r", ProjectID: "p", PeriodID: "2026-08-04", OccurredAt: now})
	future := filepath.Join(t.TempDir(), "future.wal")
	if err := os.WriteFile(future, encodeFrameVersion(frameVersionPeriod+1, 1, EventRequestAccepted, payload), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Inspect(future); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("future epoch error=%v", err)
	}
}

type faultDurability struct {
	file     *os.File
	writeErr error
	syncErr  error
	partial  bool
	writes   int
	syncs    int
}

type snapshotBlockingDurability struct {
	file    *os.File
	block   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (d *snapshotBlockingDurability) Write(payload []byte) (int, error) {
	return d.file.Write(payload)
}

func (d *snapshotBlockingDurability) Sync() error {
	if d.block.CompareAndSwap(true, false) {
		close(d.entered)
		<-d.release
	}
	return d.file.Sync()
}

func TestSnapshotIsExactDuringOneHundredConcurrentAppends(t *testing.T) {
	root := t.TempDir()
	var durability *snapshotBlockingDurability
	log, err := OpenWithOptions(filepath.Join(root, "ledger.wal"), NewStatus(), Options{
		QueueCapacity: 256, MaxBatch: 1,
		WrapDurability: func(file *os.File) DurabilityWriter {
			durability = &snapshotBlockingDurability{
				file: file, entered: make(chan struct{}), release: make(chan struct{}),
			}
			return durability
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	for index := 0; index < 5; index++ {
		if _, err := log.Append(context.Background(), validReservation(
			fmt.Sprintf("seed_%d", index), fmt.Sprintf("seed_attempt_%d", index),
		)); err != nil {
			t.Fatal(err)
		}
	}
	durability.block.Store(true)
	errorsByAppend := make(chan error, 100)
	for index := 0; index < 100; index++ {
		index := index
		go func() {
			_, err := log.Append(context.Background(), validReservation(
				fmt.Sprintf("concurrent_%03d", index), fmt.Sprintf("concurrent_attempt_%03d", index),
			))
			errorsByAppend <- err
		}()
	}
	select {
	case <-durability.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent append did not reach the durability gate")
	}
	type snapshotResult struct {
		watermark Watermark
		err       error
	}
	snapshotDone := make(chan snapshotResult, 1)
	snapshotPath := filepath.Join(root, "snapshot.wal")
	go func() {
		watermark, err := log.Snapshot(snapshotPath)
		snapshotDone <- snapshotResult{watermark: watermark, err: err}
	}()
	time.Sleep(10 * time.Millisecond)
	close(durability.release)
	result := <-snapshotDone
	if result.err != nil {
		t.Fatal(result.err)
	}
	for index := 0; index < 100; index++ {
		if err := <-errorsByAppend; err != nil {
			t.Fatal(err)
		}
	}
	if result.watermark.Sequence < 6 || result.watermark.Sequence >= 105 {
		t.Fatalf("snapshot did not split the concurrent suffix: %#v", result.watermark)
	}
	snapshot, err := Open(snapshotPath, NewStatus())
	if err != nil {
		t.Fatal(err)
	}
	replayed, replayErr := snapshot.Replay(Watermark{}, nil)
	closeErr := snapshot.Close()
	if err := errors.Join(replayErr, closeErr); err != nil {
		t.Fatal(err)
	}
	if replayed != result.watermark {
		t.Fatalf("snapshot replay=%#v returned=%#v", replayed, result.watermark)
	}
	live, err := log.Replay(Watermark{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if live.Sequence != 105 || live.Sequence <= replayed.Sequence {
		t.Fatalf("live=%#v snapshot=%#v", live, replayed)
	}
}

func (d *faultDurability) Write(payload []byte) (int, error) {
	d.writes++
	if d.writeErr == nil {
		return d.file.Write(payload)
	}
	if d.partial && len(payload) > 1 {
		written, _ := d.file.Write(payload[:len(payload)/2])
		return written, d.writeErr
	}
	return 0, d.writeErr
}

func (d *faultDurability) Sync() error {
	d.syncs++
	if d.syncErr != nil {
		return d.syncErr
	}
	return d.file.Sync()
}

func TestWriteAndSyncFailuresMakeAccountingUnavailable(t *testing.T) {
	tests := []struct {
		name     string
		writeErr error
		syncErr  error
		partial  bool
	}{
		{name: "ENOSPC before write", writeErr: syscall.ENOSPC},
		{name: "EIO after partial write", writeErr: syscall.EIO, partial: true},
		{name: "EIO during fsync", syncErr: syscall.EIO},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status := NewStatus()
			var injected *faultDurability
			path := filepath.Join(t.TempDir(), "ledger.wal")
			log, err := OpenWithOptions(path, status, Options{
				MaxBatch: 1,
				WrapDurability: func(file *os.File) DurabilityWriter {
					injected = &faultDurability{file: file, writeErr: test.writeErr, syncErr: test.syncErr, partial: test.partial}
					return injected
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := log.Append(context.Background(), validReservation("evt_1", "attempt_1")); err == nil {
				t.Fatal("faulted append unexpectedly succeeded")
			}
			if status.Load() != AccountingUnavailable || log.Stats().Errors != 1 {
				t.Fatalf("status=%v stats=%#v", status.Load(), log.Stats())
			}
			writes, syncs := injected.writes, injected.syncs
			if _, err := log.Append(context.Background(), validReservation("evt_2", "attempt_2")); err == nil {
				t.Fatal("append after durability failure unexpectedly succeeded")
			}
			if injected.writes != writes || injected.syncs != syncs {
				t.Fatalf("unhealthy ledger performed more I/O: writes=%d/%d syncs=%d/%d", writes, injected.writes, syncs, injected.syncs)
			}
			if err := log.Close(); err != nil {
				t.Fatal(err)
			}
			// A partial write is repaired at restart; a full write followed by an
			// fsync error may legitimately replay because its durability was unknown.
			reopened, err := Open(path, NewStatus())
			if err != nil {
				t.Fatalf("reopen after injected fault: %v", err)
			}
			defer reopened.Close()
		})
	}
}

func TestLedgerRoundTripAndAtomicSettlement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.wal")
	status := NewStatus()
	log, err := Open(path, status)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	reservation := Event{
		EventID:              "evt_reserve",
		Kind:                 EventReservationCreated,
		RequestID:            "req_1",
		AttemptID:            "req_1:1",
		ProjectID:            "prj_1",
		PeriodID:             "prj_1:2026-07-31:tz1",
		OccurredAt:           now,
		ReservationMicrosUSD: MicrosUSD(100),
	}
	if _, err := log.Append(context.Background(), reservation); err != nil {
		t.Fatal(err)
	}
	settlement := Event{
		EventID:              "evt_settle",
		Kind:                 EventAttemptSettled,
		RequestID:            "req_1",
		AttemptID:            "req_1:1",
		ProjectID:            "prj_1",
		PeriodID:             reservation.PeriodID,
		OccurredAt:           now.Add(time.Second),
		CommittedMicrosUSD:   MicrosUSD(80),
		ProviderInputTokens:  10,
		ProviderOutputTokens: 20,
		Outcome:              "success",
	}
	if _, err := log.Append(context.Background(), settlement); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	log, err = Open(path, status)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	state := NewState()
	if _, err := log.Replay(Watermark{}, state.Apply); err != nil {
		t.Fatal(err)
	}
	balance := state.Balance("prj_1", reservation.PeriodID, reservation.PeriodTimezoneVersion)
	if balance.ReservedMicrosUSD != 0 || balance.CommittedMicrosUSD != 80 {
		t.Fatalf("unexpected balance: %#v", balance)
	}
	if balance.InputTokens != 10 || balance.OutputTokens != 20 {
		t.Fatalf("usage was not atomically applied with settlement: %#v", balance)
	}
}

func TestPendingReservationSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.wal")
	log, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	event := validReservation("evt_1", "attempt_1")
	if _, err := log.Append(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	log.Close()

	log, err = Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	state := NewState()
	if _, err := log.Replay(Watermark{}, state.Apply); err != nil {
		t.Fatal(err)
	}
	if state.PendingReservations() != 1 {
		t.Fatalf("expected one pending reservation, got %d", state.PendingReservations())
	}
	if got := state.Balance(event.ProjectID, event.PeriodID, event.PeriodTimezoneVersion).ReservedMicrosUSD; got != *event.ReservationMicrosUSD {
		t.Fatalf("unexpected reserved amount: %d", got)
	}
}

func TestPartialTailIsTruncatedOnOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.wal")
	log, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := log.Append(context.Background(), validReservation("evt_1", "attempt_1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(context.Background(), validReservation("evt_2", "attempt_2")); err != nil {
		t.Fatal(err)
	}
	log.Close()

	if err := os.Truncate(path, first.Offset+8); err != nil {
		t.Fatal(err)
	}
	log, err = Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != first.Offset {
		t.Fatalf("partial tail was not truncated: got %d want %d", info.Size(), first.Offset)
	}
}

func TestCrashRecoveryAcrossEveryByteTruncationPoint(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "source.wal")
	log, err := OpenWithOptions(sourcePath, nil, Options{MaxBatch: 1})
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 4; index++ {
		if _, err := log.Append(context.Background(), validReservation(
			fmt.Sprintf("evt_%d", index), fmt.Sprintf("attempt_%d", index),
		)); err != nil {
			t.Fatal(err)
		}
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	recoveryDir := t.TempDir()
	path := filepath.Join(recoveryDir, "ledger.wal")
	for cut := 0; cut < len(source); cut++ {
		if err := os.WriteFile(path, source[:cut], 0o600); err != nil {
			t.Fatal(err)
		}
		recovered, err := Open(path, nil)
		if err != nil {
			t.Fatalf("cut=%d: %v", cut, err)
		}
		var count int
		watermark, err := recovered.Replay(Watermark{}, func(record Record) error {
			count++
			if record.Sequence != uint64(count) {
				return fmt.Errorf("sequence=%d want=%d", record.Sequence, count)
			}
			return nil
		})
		if err != nil {
			recovered.Close()
			t.Fatalf("cut=%d replay: %v", cut, err)
		}
		if err := recovered.Close(); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if watermark.Offset != info.Size() {
			t.Fatalf("cut=%d watermark=%d size=%d", cut, watermark.Offset, info.Size())
		}
	}
}

func TestTenThousandRandomCrashInjectionsRecoverCompleteRecordsWithoutDuplicateEventIDs(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	var source []byte
	for index := 0; index < 50; index++ {
		attemptID := fmt.Sprintf("attempt_%03d", index)
		reservation := validReservation(fmt.Sprintf("reserve_%03d", index), attemptID)
		reservation.OccurredAt = now.Add(time.Duration(index) * time.Second)
		settlement := Event{
			EventID: fmt.Sprintf("settle_%03d", index), Kind: EventAttemptSettled,
			RequestID: reservation.RequestID, AttemptID: attemptID,
			ProjectID: reservation.ProjectID, PeriodID: reservation.PeriodID,
			OccurredAt:         reservation.OccurredAt.Add(time.Millisecond),
			CommittedMicrosUSD: MicrosUSD(80), ProviderInputTokens: 7, ProviderOutputTokens: 3,
			Outcome: "success",
		}
		for _, event := range []Event{reservation, settlement} {
			payload, err := json.Marshal(event)
			if err != nil {
				t.Fatal(err)
			}
			sequence := uint64(index*2 + 1)
			if event.Kind == EventAttemptSettled {
				sequence++
			}
			source = append(source, encodeFrame(sequence, event.Kind, payload)...)
		}
	}
	random := rand.New(rand.NewSource(20260731))
	for injection := 0; injection < 10_000; injection++ {
		cut := random.Intn(len(source) + 1)
		state := NewState()
		seen := make(map[string]struct{})
		watermark, partial, err := scan(bytes.NewReader(source[:cut]), 0, 0, func(record Record) error {
			if _, duplicate := seen[record.Event.EventID]; duplicate {
				return fmt.Errorf("duplicate EventID %q", record.Event.EventID)
			}
			seen[record.Event.EventID] = struct{}{}
			return state.Apply(record)
		})
		if err != nil {
			t.Fatalf("injection=%d cut=%d: %v", injection, cut, err)
		}
		stateWatermark := state.Watermark()
		stateMatches := stateWatermark == watermark || (watermark.Sequence == 0 && stateWatermark == (Watermark{}))
		if watermark.Sequence != uint64(len(seen)) || !stateMatches || watermark.Offset > int64(cut) {
			t.Fatalf("injection=%d cut=%d watermark=%#v state=%#v records=%d", injection, cut, watermark, state.Watermark(), len(seen))
		}
		if partial != (watermark.Offset != int64(cut)) {
			t.Fatalf("injection=%d cut=%d partial=%t watermark=%#v", injection, cut, partial, watermark)
		}
	}
}

func TestChecksumCorruptionRequiresRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.wal")
	log, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(context.Background(), validReservation("evt_1", "attempt_1")); err != nil {
		t.Fatal(err)
	}
	log.Close()
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte{0xff}, frameHeaderSize+5); err != nil {
		t.Fatal(err)
	}
	file.Close()

	status := NewStatus()
	if _, err := Open(path, status); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("expected corruption error, got %v", err)
	}
	if status.Load() != AccountingRecoveryRequired {
		t.Fatalf("unexpected accounting status: %v", status.Load())
	}
}

func TestBalancedGroupCommitSharesFsyncAcrossConcurrentAppends(t *testing.T) {
	log, err := OpenWithOptions(
		filepath.Join(t.TempDir(), "ledger.wal"),
		NewStatus(),
		Options{QueueCapacity: 128, MaxBatch: 128, FlushInterval: 100 * time.Millisecond},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	const records = 64
	start := make(chan struct{})
	results := make(chan Watermark, records)
	errs := make(chan error, records)
	var wait sync.WaitGroup
	for index := 0; index < records; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			event := validReservation(
				fmt.Sprintf("evt_%d", index),
				fmt.Sprintf("attempt_%d", index),
			)
			watermark, err := log.Append(context.Background(), event)
			if err != nil {
				errs <- err
				return
			}
			results <- watermark
		}(index)
	}
	close(start)
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	close(results)
	sequences := make(map[uint64]struct{}, records)
	for watermark := range results {
		sequences[watermark.Sequence] = struct{}{}
	}
	if len(sequences) != records {
		t.Fatalf("unique sequences=%d want=%d", len(sequences), records)
	}
	stats := log.Stats()
	if stats.Records != records || stats.Batches >= records {
		t.Fatalf("group commit stats=%#v", stats)
	}
}

func TestAppendRejectsCanceledContextBeforeQueueing(t *testing.T) {
	log, err := Open(filepath.Join(t.TempDir(), "ledger.wal"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := log.Append(ctx, validReservation("evt_1", "attempt_1")); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got %v", err)
	}
	if stats := log.Stats(); stats.Records != 0 {
		t.Fatalf("canceled append reached writer: %#v", stats)
	}
}

func TestDuplicateEventIDMustMatchContent(t *testing.T) {
	state := NewState()
	event := validReservation("evt_1", "attempt_1")
	record := Record{Sequence: 1, Offset: 100, Event: event}
	if err := state.Apply(record); err != nil {
		t.Fatal(err)
	}
	if err := state.Apply(record); err != nil {
		t.Fatalf("identical duplicate must be idempotent: %v", err)
	}
	(*event.ReservationMicrosUSD)++
	if err := state.Apply(Record{Sequence: 2, Offset: 200, Event: event}); err == nil {
		t.Fatal("event id reuse with different content must fail")
	}
}

func validReservation(eventID, attemptID string) Event {
	return Event{
		EventID:              eventID,
		Kind:                 EventReservationCreated,
		RequestID:            "req_1",
		AttemptID:            attemptID,
		ProjectID:            "prj_1",
		PeriodID:             "prj_1:2026-07-31:tz1",
		OccurredAt:           time.Now().UTC(),
		ReservationMicrosUSD: MicrosUSD(100),
	}
}

func BenchmarkReplayLargeWAL(b *testing.B) {
	path := filepath.Join(b.TempDir(), "large.wal")
	var encoded bytes.Buffer
	now := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	for index := 0; index < 100_000; index++ {
		event := validReservation(fmt.Sprintf("evt_%d", index), fmt.Sprintf("attempt_%d", index))
		event.OccurredAt = now
		payload, err := json.Marshal(event)
		if err != nil {
			b.Fatal(err)
		}
		encoded.Write(encodeFrame(uint64(index+1), event.Kind, payload))
	}
	if err := os.WriteFile(path, encoded.Bytes(), 0o600); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(encoded.Len()))
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		log, err := Open(path, nil)
		if err != nil {
			b.Fatal(err)
		}
		var records int
		if _, err := log.Replay(Watermark{}, func(Record) error {
			records++
			return nil
		}); err != nil {
			b.Fatal(err)
		}
		if records != 100_000 {
			b.Fatalf("records=%d", records)
		}
		if err := log.Close(); err != nil {
			b.Fatal(err)
		}
	}
}
