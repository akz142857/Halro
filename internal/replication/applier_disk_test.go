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
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

// Real native frames enter through the Replica sink/ordering receiver. Rebuild
// represents persisted applied=2/confirmed=2 with a newer durable index=3;
// retransmission authenticates/establishes its confirmation before applying.
func diskReplicaFixture(t *testing.T) (*NativeProjection, *ledger.LedgerProjection, *ReplicaReceiver, *OrderingJournal, []byte, func(bool)) {
	t.Helper()
	ctx := context.Background()
	key := bytes.Repeat([]byte{0x32}, 32)
	root := t.TempDir()
	var native [][]byte
	primary, err := ledger.OpenWithOptions(filepath.Join(root, "primary", "ledger.wal"), ledger.NewStatus(), ledger.Options{ChainKey: key, MaxBatch: 1, AfterDurable: func(b ledger.DurableBatch) (uint64, error) {
		native = append(native, append([]byte(nil), b.Frames...))
		return uint64(len(native)), nil
	}, WaitConfirmed: func(context.Context, uint64) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		event := ledger.Event{EventID: fmt.Sprintf("evt_%d", i), Kind: ledger.EventReservationCreated, RequestID: fmt.Sprintf("req_%d", i), AttemptID: fmt.Sprintf("att_%d", i), ProjectID: "prj_1", PeriodID: "2026-10-03", PeriodTimezone: "UTC", PeriodTimezoneVersion: 1, OccurredAt: time.Unix(100, 0).UTC(), ReservationMicrosUSD: ledger.MicrosUSD(int64(100 * i))}
		if _, err := primary.Append(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	if err := primary.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "replica", "ledger.wal")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	log, err := ledger.OpenWithOptions(path, ledger.NewStatus(), ledger.Options{ChainKey: key, Replica: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	sink := &NativeSink{ledger: log}
	initial, journal := newTestReceiver(t, sink)
	t.Cleanup(func() { _ = journal.Close() })
	var latest []byte
	for i := uint64(1); i <= 3; i++ {
		f := testConfirmationFrame(i, KindLeadershipEstablished)
		if i > 1 {
			f.Kind = KindData
			f.Store = StoreLedger
			f.StoreGeneration = 1
			f.StoreSequenceFirst = i - 1
			f.StoreSequenceLast = i - 1
			f.Payload = native[i-2]
		}
		encoded, err := f.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := initial.Receive(encoded); err != nil {
			t.Fatal(err)
		}
		latest = encoded
	}
	// In production the prefix comes from allowed ordering/member state. This
	// fixture verifies that relationship, independently of the newer native head.
	cut, err := journal.StoreCursorsThrough(2)
	if err != nil {
		t.Fatal(err)
	}
	allowed := cut[StoreLedger-StoreLedger]
	disk, prefix, err := log.BuildProjectionFromCursor(ctx, allowed.Generation, allowed.Sequence, filepath.Join(root, "projection.db"))
	if err != nil {
		t.Fatal(err)
	}
	if prefix.Head.Generation != 1 || prefix.Head.Sequence != 1 || prefix.Head.Offset == 0 || prefix.Hash == ([32]byte{}) {
		t.Fatal(prefix)
	}
	t.Cleanup(func() { _ = disk.Close() })
	store, err := boltstore.Open(filepath.Join(root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	projection, err := NewNativeProjectionWithLedgerOwner(log, disk, store)
	if err != nil {
		t.Fatal(err)
	}
	receiver, err := NewReplicaReceiver("production-a", "inc_01", "halro-1", "halro-0", 7, 7, 2, 2, journal, sink)
	if err != nil {
		t.Fatal(err)
	}
	corruptTail := func(compressed bool) {
		t.Helper()
		if err := log.AppendReplicated(1, 3, 3, native[2]); err != nil {
			t.Fatal(err)
		}
		changed := path
		if compressed {
			oldPrimary, err := ledger.OpenWithOptions(filepath.Join(root, "primary", "ledger.wal"), ledger.NewStatus(), ledger.Options{ChainKey: key})
			if err != nil {
				t.Fatal(err)
			}
			rolled, err := oldPrimary.Roll()
			if err != nil {
				t.Fatal(err)
			}
			if err := oldPrimary.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := log.ApplyReplicatedRoll(rolled.Sealed); err != nil {
				t.Fatal(err)
			}
			segment, err := log.Compact(1)
			if err != nil {
				t.Fatal(err)
			}
			changed = filepath.Join(filepath.Dir(path), segment.File)
		}
		raw, err := os.ReadFile(changed)
		if err != nil {
			t.Fatal(err)
		}
		if compressed {
			raw[len(raw)-8] ^= 1
		} else {
			frame := raw[len(native[0])+len(native[1]):]
			n := int(binary.BigEndian.Uint32(frame[16:20]))
			payload := frame[88 : 88+n]
			payload[n/2] ^= 0x20
			crc := crc32.NewIEEE()
			_, _ = crc.Write(frame[4:20])
			_, _ = crc.Write(payload)
			binary.BigEndian.PutUint32(frame[20:24], crc.Sum32())
		}
		if err := os.WriteFile(changed, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return projection, disk, receiver, journal, latest, corruptTail
}

func TestReplicaApplierDiskOwnerRebuildsAppliedPrefixAndAdvancesOnlyAfterConfirmation(t *testing.T) {
	p, disk, receiver, journal, latest, _ := diskReplicaFixture(t)
	applier, err := NewReplicaApplier(receiver, journal, p)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	key := ledger.BalanceKey{ProjectID: "prj_1", PeriodID: "2026-10-03", TimezoneVersion: 1}
	assertBalance := func(want int64) {
		t.Helper()
		b, err := disk.ReadBalance(ctx, key)
		if err != nil || b.ReservedMicrosUSD != want {
			t.Fatal(b, err)
		}
	}
	assertBalance(100)
	if _, found, err := disk.ReadLease(ctx, "att_2"); err != nil || found {
		t.Fatal("durable suffix entered rebuilt owner", found, err)
	}
	if applied, err := applier.ApplyConfirmed(ctx); err != nil || applied != 2 {
		t.Fatal(applied, err)
	}
	assertBalance(100)
	if _, err := receiver.Receive(latest); err != nil {
		t.Fatal(err)
	}
	if applied, err := applier.ApplyConfirmed(ctx); err != nil || applied != 3 {
		t.Fatal(applied, err)
	}
	assertBalance(300)
	nativeHead, nativeHash, _ := p.ledgerLog.ChainHead()
	appliedHead, appliedHash, err := disk.ReadPosition(ctx)
	if err != nil || appliedHead != nativeHead || appliedHash != nativeHash || appliedHash == ([32]byte{}) {
		t.Fatal(appliedHead, appliedHash, nativeHead, nativeHash, err)
	}
	for range 2 {
		if applied, err := applier.ApplyConfirmed(ctx); err != nil || applied != 3 {
			t.Fatal(applied, err)
		}
	}
	assertBalance(300)
	if err := p.ApplyLedgerThrough(ctx, 2, 2); err == nil {
		t.Fatal("same-sequence generation mismatch accepted")
	}
	if err := p.ApplyLedgerThrough(ctx, 1, 1); err == nil {
		t.Fatal("prefix regression accepted")
	}
	if err := disk.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.ApplyLedgerThrough(ctx, 1, 2); !errors.Is(err, ledger.ErrProjectionUnavailable) {
		t.Fatal("closed disk owner accepted/fell back", err)
	}
}

type failingDiskOwner struct {
	ReplicaLedgerOwner
	fail bool
}

func (o *failingDiskOwner) Apply(ctx context.Context, r ledger.Record) error {
	if o.fail {
		o.fail = false
		return errors.New("injected local projection transaction failure")
	}
	return o.ReplicaLedgerOwner.Apply(ctx, r)
}

func TestReplicaApplierDiskOwnerFailureKeepsAppliedIndexAndRetriesIdempotentDerivative(t *testing.T) {
	p, disk, receiver, journal, latest, _ := diskReplicaFixture(t)
	ctx := context.Background()
	p.ledgerOwner = &failingDiskOwner{ReplicaLedgerOwner: disk, fail: true}
	calls, distinct := 0, map[uint64]bool{}
	p.ledgerDerived = []func(ledger.Record) error{func(r ledger.Record) error { calls++; distinct[r.Sequence] = true; return nil }}
	applier, err := NewReplicaApplier(receiver, journal, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := receiver.Receive(latest); err != nil {
		t.Fatal(err)
	}
	if applied, err := applier.ApplyConfirmed(ctx); err == nil || applied != 2 {
		t.Fatal(applied, err)
	}
	if _, _, applied := receiver.Progress(); applied != 2 {
		t.Fatal(applied)
	}
	head, _, err := disk.ReadPosition(ctx)
	if err != nil || head.Sequence != 1 {
		t.Fatal(head, err)
	}
	if applied, err := applier.ApplyConfirmed(ctx); err != nil || applied != 3 {
		t.Fatal(applied, err)
	}
	if calls != 2 || len(distinct) != 1 {
		t.Fatal(calls, distinct)
	}
	b, err := disk.ReadBalance(ctx, ledger.BalanceKey{ProjectID: "prj_1", PeriodID: "2026-10-03", TimezoneVersion: 1})
	if err != nil || b.ReservedMicrosUSD != 300 {
		t.Fatal(b, err)
	}
}

func TestNativeProjectionSerializesDiskApplyAndPropagatesCanceledPositionRead(t *testing.T) {
	p, disk, _, _, _, _ := diskReplicaFixture(t)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.ApplyLedgerThrough(canceled, 1, 2); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := p.ApplyLedgerThrough(context.Background(), 1, 2); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	b, err := disk.ReadBalance(context.Background(), ledger.BalanceKey{ProjectID: "prj_1", PeriodID: "2026-10-03", TimezoneVersion: 1})
	if err != nil || b.ReservedMicrosUSD != 300 {
		t.Fatal(b, err)
	}
}

func TestReplicaApplierDiskOwnerNativeSuffixFailureCannotBeBypassedBySamePositionRetry(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		t.Run(map[bool]string{false: "active-HMAC", true: "sealed-gzip-trailer"}[compressed], func(t *testing.T) {
			p, disk, receiver, journal, latest, corrupt := diskReplicaFixture(t)
			applier, err := NewReplicaApplier(receiver, journal, p)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := receiver.Receive(latest); err != nil {
				t.Fatal(err)
			}
			corrupt(compressed)
			if applied, err := applier.ApplyConfirmed(context.Background()); err == nil || applied != 2 {
				t.Fatal(applied, err)
			}
			head, hash, err := disk.ReadPosition(context.Background())
			if err != nil || head.Sequence != 2 || hash == ([32]byte{}) {
				t.Fatal("target record should apply before excluded suffix fails", head, hash, err)
			}
			if p.ledgerLog.Status().Load() != ledger.AccountingRecoveryRequired {
				t.Fatal(p.ledgerLog.Status().Load())
			}
			for range 2 {
				if applied, err := applier.ApplyConfirmed(context.Background()); err == nil || applied != 2 {
					t.Fatal("native failure bypassed", applied, err)
				}
				if _, _, applied := receiver.Progress(); applied != 2 {
					t.Fatal(applied)
				}
			}
		})
	}
}

type cancelAfterDiskApply struct {
	ReplicaLedgerOwner
	cancel context.CancelFunc
}

func (o *cancelAfterDiskApply) Apply(ctx context.Context, r ledger.Record) error {
	if err := o.ReplicaLedgerOwner.Apply(ctx, r); err != nil {
		return err
	}
	o.cancel()
	return nil
}

func TestReplicaApplierDiskOwnerCanceledAfterApplyStillValidatesBeforeSamePositionRetry(t *testing.T) {
	p, disk, receiver, journal, latest, corrupt := diskReplicaFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.ledgerOwner = &cancelAfterDiskApply{ReplicaLedgerOwner: disk, cancel: cancel}
	applier, err := NewReplicaApplier(receiver, journal, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := receiver.Receive(latest); err != nil {
		t.Fatal(err)
	}
	if applied, err := applier.ApplyConfirmed(ctx); !errors.Is(err, context.Canceled) || applied != 2 {
		t.Fatal(applied, err)
	}
	head, _, err := disk.ReadPosition(context.Background())
	if err != nil || head.Sequence != 2 {
		t.Fatal(head, err)
	}
	if p.ledgerLog.Status().Load() != ledger.AccountingHealthy {
		t.Fatal(p.ledgerLog.Status().Load())
	}
	corrupt(false)
	if applied, err := applier.ApplyConfirmed(context.Background()); !errors.Is(err, ledger.ErrTampered) || applied != 2 {
		t.Fatal("same-position validation skipped after caller cancellation", applied, err)
	}
	if _, _, applied := receiver.Progress(); applied != 2 {
		t.Fatal(applied)
	}
}
