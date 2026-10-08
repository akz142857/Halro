package replication

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
)

func inspectionLedgerRollFixture(t *testing.T) (string, string, []byte, []byte, OrderingHeader, []OrderingRecord) {
	t.Helper()
	root := t.TempDir()
	path, nativePath := filepath.Join(root, "ordering.journal"), filepath.Join(root, "ledger", "ledger.wal")
	key, nativeKey := bytes.Repeat([]byte{0x33}, 32), bytes.Repeat([]byte{0x44}, 32)
	header := OrderingHeader{ClusterID: "usage-inspection", Incarnation: "inc_roll"}
	header.StoreCursors[0] = StoreCursor{Generation: 1}
	j, err := OpenOrderingJournal(path, key, header, 0, [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	var records []OrderingRecord
	appendFrame := func(f Frame) error {
		f.ClusterID, f.Incarnation, f.Term, f.Index = header.ClusterID, header.Incarnation, 7, uint64(len(records)+1)
		_, digest, err := f.MarshalBinaryAndDigest()
		if err != nil {
			return err
		}
		r, err := j.Append(OrderingRecord{Kind: f.Kind, Store: f.Store, StoreGeneration: f.StoreGeneration, StoreSequenceFirst: f.StoreSequenceFirst, StoreSequenceLast: f.StoreSequenceLast, Index: f.Index, Term: f.Term, Metadata: f.Metadata, FrameDigest: digest})
		if err == nil {
			records = append(records, r)
		}
		return err
	}
	if err := appendFrame(Frame{Kind: KindLeadershipEstablished}); err != nil {
		t.Fatal(err)
	}
	native, err := ledger.OpenWithOptions(nativePath, ledger.NewStatus(), ledger.Options{ChainKey: nativeKey, MaxBatch: 1,
		AfterDurable: func(b ledger.DurableBatch) (uint64, error) {
			err := appendFrame(Frame{Kind: KindData, Store: StoreLedger, StoreGeneration: b.Generation, StoreSequenceFirst: b.FirstSequence, StoreSequenceLast: b.LastSequence, Payload: b.Frames})
			return uint64(len(records)), err
		}, WaitConfirmed: func(context.Context, uint64) error { return nil }, BeforeRoll: func(uint64, uint64) error { return nil },
		AfterRoll: func(s ledger.Segment) error {
			metadata, err := ledgerRollMetadata(s)
			if err != nil {
				return err
			}
			raw, err := metadata.MarshalBinary()
			if err != nil {
				return err
			}
			return appendFrame(Frame{Kind: KindLedgerRoll, Metadata: raw})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	for i := 1; i <= 3; i++ {
		e := ledger.Event{EventID: fmt.Sprintf("evt_%d", i), Kind: ledger.EventReservationCreated, RequestID: fmt.Sprintf("req_%d", i), AttemptID: fmt.Sprintf("att_%d", i), ProjectID: "prj_1", PeriodID: "2026-10-03", PeriodTimezone: "UTC", PeriodTimezoneVersion: 1, OccurredAt: time.Unix(100, 0).UTC(), ReservationMicrosUSD: ledger.MicrosUSD(100)}
		if _, err := native.Append(context.Background(), e); err != nil {
			t.Fatal(err)
		}
		if i < 3 {
			if _, err := native.Roll(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := native.Compact(1); err != nil {
		t.Fatal(err)
	}
	return path, nativePath, key, nativeKey, header, records
}

func TestInspectOrderingLedgerBindsRealCompressedRollsAndExcludedRanges(t *testing.T) {
	path, nativePath, key, nativeKey, header, records := inspectionLedgerRollFixture(t)
	for _, through := range []uint64{0, 2, 3, 6} {
		report, err := InspectOrderingLedger(context.Background(), path, key, header.ClusterID, header.Incarnation, 6, records[5].MAC, through, nativePath, nativeKey)
		if err != nil || report.Head.Index != 6 || report.Allowed.Index != through {
			t.Fatal(through, report, err)
		}
		if through == 3 && report.Allowed.Stores[0] != (StoreCursor{Generation: 2, Sequence: 1}) {
			t.Fatal("empty-roll physical boundary was lost", report)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if report, err := InspectOrderingLedger(ctx, path, key, header.ClusterID, header.Incarnation, 6, records[5].MAC, 3, nativePath, nativeKey); !errors.Is(err, context.Canceled) || report != (OrderingInspection{}) {
		t.Fatal(report, err)
	}
}

func TestInspectOrderingLedgerRejectsAuthenticatedRollIdentityMismatch(t *testing.T) {
	path, nativePath, key, nativeKey, header, records := inspectionLedgerRollFixture(t)
	roll, err := DecodeLedgerRollMetadata(records[2].Metadata)
	if err != nil {
		t.Fatal(err)
	}
	roll.SealedAtUnix++
	records[2].Metadata, err = roll.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	f := Frame{Kind: KindLedgerRoll, Index: records[2].Index, Term: records[2].Term, ClusterID: header.ClusterID, Incarnation: header.Incarnation, Metadata: records[2].Metadata}
	_, records[2].FrameDigest, err = f.MarshalBinaryAndDigest()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := header.MarshalBinary(key)
	if err != nil {
		t.Fatal(err)
	}
	var previous [32]byte
	for i, record := range records {
		record.PreviousMAC = previous
		encoded, mac, err := record.MarshalBinaryAndMAC(key, header.ClusterID, header.Incarnation)
		if err != nil {
			t.Fatal(err)
		}
		raw = append(raw, encoded...)
		records[i].MAC, previous = mac, mac
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// Pure ordering authentication/cursors accept the valid alternative chain;
	// native identity binding must reject it even before the allowed first data.
	if _, err := InspectOrderingJournal(context.Background(), path, key, header.ClusterID, header.Incarnation, 6, records[5].MAC, 2); err != nil {
		t.Fatal("fixture is not independently authenticated", err)
	}
	if report, err := InspectOrderingLedger(context.Background(), path, key, header.ClusterID, header.Incarnation, 6, records[5].MAC, 2, nativePath, nativeKey); err == nil || report != (OrderingInspection{}) {
		t.Fatal("different authenticated native roll accepted", report, err)
	}
}
