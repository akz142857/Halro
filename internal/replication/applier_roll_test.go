package replication

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

// The source produces native authenticated frames and structural metadata; the
// receiver's real native sink durably installs all three generations. Derived
// owners initially reflect none of that confirmed prefix.
func rolledReplicaFixture(t *testing.T, disk bool) (*NativeProjection, *ledger.LedgerProjection, *ledger.State, *ReplicaReceiver, *OrderingJournal, func()) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	key := bytes.Repeat([]byte{0x34}, 32)
	var frames []Frame
	source, err := ledger.OpenWithOptions(filepath.Join(root, "primary", "ledger.wal"), ledger.NewStatus(), ledger.Options{
		ChainKey: key, MaxBatch: 1,
		AfterDurable: func(b ledger.DurableBatch) (uint64, error) {
			frames = append(frames, Frame{Kind: KindData, Store: StoreLedger, StoreGeneration: b.Generation, StoreSequenceFirst: b.FirstSequence, StoreSequenceLast: b.LastSequence, Payload: append([]byte(nil), b.Frames...)})
			return uint64(len(frames)), nil
		},
		WaitConfirmed: func(context.Context, uint64) error { return nil },
		BeforeRoll:    func(uint64, uint64) error { return nil },
		AfterRoll: func(s ledger.Segment) error {
			m, err := ledgerRollMetadata(s)
			if err != nil {
				return err
			}
			raw, err := m.MarshalBinary()
			if err != nil {
				return err
			}
			frames = append(frames, Frame{Kind: KindLedgerRoll, Metadata: raw})
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		e := ledger.Event{EventID: fmt.Sprintf("evt_roll_%d", i), Kind: ledger.EventReservationCreated, RequestID: fmt.Sprintf("req_roll_%d", i), AttemptID: fmt.Sprintf("att_roll_%d", i), ProjectID: "prj_1", PeriodID: "2026-10-03", PeriodTimezone: "UTC", PeriodTimezoneVersion: 1, OccurredAt: time.Unix(100, 0).UTC(), ReservationMicrosUSD: ledger.MicrosUSD(int64(i * 100))}
		if _, err := source.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
		if i < 3 {
			if _, err := source.Roll(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "replica", "ledger.wal")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	native, err := ledger.OpenWithOptions(path, ledger.NewStatus(), ledger.Options{ChainKey: key, Replica: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = native.Close() })
	receiver, journal := newTestReceiver(t, &NativeSink{ledger: native})
	t.Cleanup(func() { _ = journal.Close() })
	frames = append([]Frame{{Kind: KindLeadershipEstablished}}, frames...)
	for i, f := range frames {
		f.Index = uint64(i + 1)
		f.ClusterID = "production-a"
		f.Incarnation = "inc_01"
		f.Term = 7
		raw, err := f.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := receiver.Receive(raw); err != nil {
			t.Fatal(err)
		}
	}
	// Local representation differs from Primary; ordered plain identity does not.
	if _, err := native.Compact(1); err != nil {
		t.Fatal(err)
	}
	store, err := boltstore.Open(filepath.Join(root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	var projection *NativeProjection
	var p *ledger.LedgerProjection
	var state *ledger.State
	if disk {
		p, _, err = native.BuildProjectionFromCursor(ctx, 0, 0, filepath.Join(root, "projection.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = p.Close() })
		projection, err = NewNativeProjectionWithLedgerOwner(native, p, store)
		if err != nil {
			t.Fatal(err)
		}
	} else {
		state = ledger.NewState()
		projection, err = NewNativeProjection(native, state, store)
		if err != nil {
			t.Fatal(err)
		}
	}
	corruptActive := func() {
		t.Helper()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		n := int(binary.BigEndian.Uint32(raw[16:20]))
		payload := raw[88 : 88+n]
		payload[n/2] ^= 0x20
		crc := crc32.NewIEEE()
		_, _ = crc.Write(raw[4:20])
		_, _ = crc.Write(payload)
		binary.BigEndian.PutUint32(raw[20:24], crc.Sum32())
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return projection, p, state, receiver, journal, corruptActive
}

func TestReplicaApplierConfirmsMultipleLedgerRollsWithOneSelectedOwner(t *testing.T) {
	for _, disk := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy-State", true: "disk"}[disk], func(t *testing.T) {
			p, projection, state, receiver, journal, _ := rolledReplicaFixture(t, disk)
			applier, err := NewReplicaApplier(receiver, journal, p)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if applied, err := applier.ApplyConfirmed(ctx); err != nil || applied != 6 {
				t.Fatal(applied, err)
			}
			head, hash, err := p.ledgerOwner.ReadPosition(ctx)
			if err != nil || head.Generation != 3 || head.Sequence != 3 {
				t.Fatal(head, hash, err)
			}
			if disk {
				nativeHead, nativeHash, _ := p.ledgerLog.ChainHead()
				if head != nativeHead || hash != nativeHash || hash == ([32]byte{}) {
					t.Fatal(head, hash, nativeHead, nativeHash)
				}
			}
			key := ledger.BalanceKey{ProjectID: "prj_1", PeriodID: "2026-10-03", TimezoneVersion: 1}
			var balance ledger.Balance
			if disk {
				balance, err = projection.ReadBalance(ctx, key)
			} else {
				balance, err = state.ReadBalance(ctx, key)
			}
			if err != nil || balance.ReservedMicrosUSD != 600 {
				t.Fatal(balance, err)
			}
			if applied, err := applier.ApplyConfirmed(ctx); err != nil || applied != 6 {
				t.Fatal(applied, err)
			}
		})
	}
}

type failRollOnceProjection struct {
	*NativeProjection
	remaining int
}

func (p *failRollOnceProjection) ApplyLedgerRoll(ctx context.Context, roll LedgerRollMetadata) error {
	if p.remaining == 1 {
		p.remaining = 0
		return errors.New("injected later roll failure")
	}
	p.remaining--
	return p.NativeProjection.ApplyLedgerRoll(ctx, roll)
}

func TestReplicaApplierRetryAfterEarlierRollDoesNotRegressOrDoubleApply(t *testing.T) {
	p, disk, _, receiver, journal, _ := rolledReplicaFixture(t, true)
	selected := &failRollOnceProjection{NativeProjection: p, remaining: 2}
	applier, err := NewReplicaApplier(receiver, journal, selected)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if applied, err := applier.ApplyConfirmed(ctx); err == nil || applied != 0 {
		t.Fatal(applied, err)
	}
	head, _, err := disk.ReadPosition(ctx)
	if err != nil || head.Generation != 2 || head.Sequence != 2 {
		t.Fatal(head, err)
	}
	if _, _, applied := receiver.Progress(); applied != 0 {
		t.Fatal(applied)
	}
	if applier.ledgerDone != 4 {
		t.Fatal("successful semantic substep not retained for local retry", applier.ledgerDone)
	}
	if applied, err := applier.ApplyConfirmed(ctx); err != nil || applied != 6 {
		t.Fatal(applied, err)
	}
	b, err := disk.ReadBalance(ctx, ledger.BalanceKey{ProjectID: "prj_1", PeriodID: "2026-10-03", TimezoneVersion: 1})
	if err != nil || b.ReservedMicrosUSD != 600 {
		t.Fatal(b, err)
	}
}

func TestReplicaApplierStructuralOnlyRollAdvancesPhysicalCutWithoutSemanticEvent(t *testing.T) {
	p, disk, _, receiver, journal, _ := rolledReplicaFixture(t, true)
	// At restart the authenticated member prefix is just before the second roll.
	// This applier's source contains the later generation too, but it must not use
	// the durable head as permission to apply it.
	receiver, err := NewReplicaReceiver("production-a", "inc_01", "halro-1", "halro-0", 7, 7, 4, 0, journal, receiver.sink)
	if err != nil {
		t.Fatal(err)
	}
	applier, err := NewReplicaApplier(receiver, journal, p)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if applied, err := applier.ApplyConfirmed(ctx); err != nil || applied != 4 {
		t.Fatal(applied, err)
	}
	head, _, err := disk.ReadPosition(ctx)
	if err != nil || head.Generation != 2 || head.Sequence != 2 || head.Offset == 0 {
		t.Fatal(head, err)
	}
	if err := receiver.Confirm(CommitNotice{ClusterID: "production-a", Incarnation: "inc_01", NodeID: "halro-0", Term: 7, ConfirmedIndex: 5}); err != nil {
		t.Fatal(err)
	}
	if applied, err := applier.ApplyConfirmed(ctx); err != nil || applied != 5 {
		t.Fatal(applied, err)
	}
	after, hash, err := disk.ReadPosition(ctx)
	if err != nil || after.Generation != 3 || after.Sequence != 2 || after.Offset != 0 || hash == ([32]byte{}) {
		t.Fatal(after, hash, err)
	}
	if _, found, err := disk.ReadLease(ctx, "att_roll_3"); err != nil || found {
		t.Fatal("later native fact used during structural-only apply", found, err)
	}
}

type failSchemaOnceProjection struct {
	*NativeProjection
	failed bool
}

func (p *failSchemaOnceProjection) ValidateSchemaBoundary(ctx context.Context, from, to uint32) error {
	if !p.failed {
		p.failed = true
		return errors.New("injected schema publication failure")
	}
	return p.NativeProjection.ValidateSchemaBoundary(ctx, from, to)
}

func TestReplicaApplierCompletedLedgerRetryStillRefusesNewNativeFailure(t *testing.T) {
	p, _, _, receiver, journal, _ := rolledReplicaFixture(t, true)
	metadata, err := (SchemaBoundaryMetadata{From: 40, To: 41}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	frame := testConfirmationFrame(7, KindSchemaBoundary)
	frame.Metadata = metadata
	raw, err := frame.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := receiver.Receive(raw); err != nil {
		t.Fatal(err)
	}
	if err := receiver.Confirm(CommitNotice{ClusterID: "production-a", Incarnation: "inc_01", NodeID: "halro-0", Term: 7, ConfirmedIndex: 7}); err != nil {
		t.Fatal(err)
	}
	applier, err := NewReplicaApplier(receiver, journal, &failSchemaOnceProjection{NativeProjection: p})
	if err != nil {
		t.Fatal(err)
	}
	if applied, err := applier.ApplyConfirmed(context.Background()); err == nil || applied != 0 || applier.ledgerDone != 6 {
		t.Fatal(applied, applier.ledgerDone, err)
	}
	p.ledgerLog.Status().RequireRecovery()
	if applied, err := applier.ApplyConfirmed(context.Background()); err == nil || applied != 0 {
		t.Fatal("completed Ledger substeps bypassed new native failure", applied, err)
	}
	if _, _, applied := receiver.Progress(); applied != 0 {
		t.Fatal(applied)
	}
}

type cancelAfterRollProjection struct {
	*NativeProjection
	remaining int
	cancel    context.CancelFunc
}

func (p *cancelAfterRollProjection) ApplyLedgerRoll(ctx context.Context, roll LedgerRollMetadata) error {
	if err := p.NativeProjection.ApplyLedgerRoll(ctx, roll); err != nil {
		return err
	}
	p.remaining--
	if p.remaining == 0 {
		p.cancel()
	}
	return nil
}

func TestReplicaApplierCancelAfterCommittedRollRetriesWithoutSemanticTail(t *testing.T) {
	for _, disk := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy-State", true: "disk"}[disk], func(t *testing.T) {
			p, projection, state, receiver, journal, _ := rolledReplicaFixture(t, disk)
			receiver, err := NewReplicaReceiver("production-a", "inc_01", "halro-1", "halro-0", 7, 7, 5, 0, journal, receiver.sink)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			selected := &cancelAfterRollProjection{NativeProjection: p, remaining: 2, cancel: cancel}
			applier, err := NewReplicaApplier(receiver, journal, selected)
			if err != nil {
				t.Fatal(err)
			}
			if applied, err := applier.ApplyConfirmed(ctx); !errors.Is(err, context.Canceled) || applied != 0 || applier.ledgerDone != 5 {
				t.Fatal(applied, applier.ledgerDone, err)
			}
			head, _, err := p.ledgerOwner.ReadPosition(context.Background())
			if err != nil || head != (ledger.Watermark{Generation: 3, Sequence: 2}) {
				t.Fatal(head, err)
			}
			if applied, err := applier.ApplyConfirmed(context.Background()); err != nil || applied != 5 {
				t.Fatal(applied, err)
			}
			var balance ledger.Balance
			key := ledger.BalanceKey{ProjectID: "prj_1", PeriodID: "2026-10-03", TimezoneVersion: 1}
			if disk {
				balance, err = projection.ReadBalance(context.Background(), key)
			} else {
				balance, err = state.ReadBalance(context.Background(), key)
			}
			if err != nil || balance.ReservedMicrosUSD != 300 || p.ledgerLog.Status().Load() != ledger.AccountingHealthy {
				t.Fatal(balance, p.ledgerLog.Status().Load(), err)
			}
		})
	}
}

func TestReplicaApplierFinalBarrierRejectsExcludedSuffixAfterSuccessfulRolls(t *testing.T) {
	p, disk, _, receiver, journal, corrupt := rolledReplicaFixture(t, true)
	receiver, err := NewReplicaReceiver("production-a", "inc_01", "halro-1", "halro-0", 7, 7, 5, 0, journal, receiver.sink)
	if err != nil {
		t.Fatal(err)
	}
	applier, err := NewReplicaApplier(receiver, journal, p)
	if err != nil {
		t.Fatal(err)
	}
	corrupt()
	for i := 0; i < 2; i++ {
		if applied, err := applier.ApplyConfirmed(context.Background()); err == nil || applied != 0 || applier.ledgerDone != 5 {
			t.Fatal(applied, applier.ledgerDone, err)
		}
	}
	head, _, err := disk.ReadPosition(context.Background())
	if err != nil || head != (ledger.Watermark{Generation: 3, Sequence: 2}) || p.ledgerLog.Status().Load() != ledger.AccountingRecoveryRequired {
		t.Fatal(head, p.ledgerLog.Status().Load(), err)
	}
	if _, found, err := disk.ReadLease(context.Background(), "att_roll_3"); err != nil || found {
		t.Fatal("excluded damaged fact reduced", found, err)
	}
	if _, _, applied := receiver.Progress(); applied != 0 {
		t.Fatal(applied)
	}
}
