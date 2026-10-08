package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/replication"
)

func TestReplicaOwnerFactoryBuildsAppliedCutExcludingConfirmedAndDurableSuffix(t *testing.T) {
	root := t.TempDir()
	key := bytes.Repeat([]byte{0x45}, 32)
	ctx := context.Background()
	var batches []ledger.DurableBatch
	primary, err := ledger.OpenWithOptions(filepath.Join(root, "primary", "ledger.wal"), ledger.NewStatus(), ledger.Options{ChainKey: key, MaxBatch: 1,
		AfterDurable: func(b ledger.DurableBatch) (uint64, error) {
			b.Frames = append([]byte(nil), b.Frames...)
			batches = append(batches, b)
			return uint64(len(batches)), nil
		}, WaitConfirmed: func(context.Context, uint64) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		event := ledger.Event{EventID: fmt.Sprintf("evt_owner_%d", i), Kind: ledger.EventReservationCreated, RequestID: fmt.Sprintf("req_owner_%d", i), AttemptID: fmt.Sprintf("att_owner_%d", i), ProjectID: "prj_1", PeriodID: "2026-10-03", PeriodTimezone: "UTC", PeriodTimezoneVersion: 1, OccurredAt: time.Unix(100, 0).UTC(), ReservationMicrosUSD: ledger.MicrosUSD(int64(i * 100))}
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
	native, err := ledger.OpenWithOptions(path, ledger.NewStatus(), ledger.Options{ChainKey: key, Replica: true})
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	journal, err := replication.OpenOrderingJournal(filepath.Join(root, "ordering.journal"), key, replication.OrderingHeader{ClusterID: "production-a", Incarnation: "inc_01"}, 0, [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	for index := uint64(1); index <= 4; index++ {
		frame := replication.Frame{ClusterID: "production-a", Incarnation: "inc_01", Index: index, Term: 7, Kind: replication.KindLeadershipEstablished}
		if index > 1 {
			b := batches[index-2]
			if err := native.AppendReplicated(b.Generation, b.FirstSequence, b.LastSequence, b.Frames); err != nil {
				t.Fatal(err)
			}
			frame.Kind, frame.Store, frame.StoreGeneration = replication.KindData, replication.StoreLedger, b.Generation
			frame.StoreSequenceFirst, frame.StoreSequenceLast, frame.Payload = b.FirstSequence, b.LastSequence, b.Frames
		}
		_, digest, err := frame.MarshalBinaryAndDigest()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := journal.Append(replication.OrderingRecord{Kind: frame.Kind, Store: frame.Store, StoreGeneration: frame.StoreGeneration, StoreSequenceFirst: frame.StoreSequenceFirst, StoreSequenceLast: frame.StoreSequenceLast, Index: frame.Index, Term: frame.Term, FrameDigest: digest}); err != nil {
			t.Fatal(err)
		}
	}
	_, _, mac := journal.Head()
	member := replication.MemberState{Version: replication.StateVersion, ClusterID: "production-a", Incarnation: "inc_01", NodeID: "halro-1", Role: replication.RoleReplica, Term: 7, PromisedTerm: 7, DurableIndex: 4, ConfirmedIndex: 3, AppliedIndex: 2, OrderingHeadMAC: mac, Projection: replication.ProjectionState{Index: 2}, Peers: []replication.StatePeer{{Name: "halro-0", Address: "127.0.0.1:9310", SPKISHA256: "sha256:" + strings.Repeat("a", 64)}}}
	publisher, err := replication.NewStatePublisher(filepath.Join(root, "state.json"), key, member)
	if err != nil {
		t.Fatal(err)
	}
	defer publisher.Close()
	r := &replicationRuntime{role: replication.RoleReplica, journal: journal, publisher: publisher}
	output := filepath.Join(root, "selected.db")
	p, cut, err := r.buildReplicaLedgerOwner(ctx, native, output)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if cut.Head.Generation != 1 || cut.Head.Sequence != 1 || cut.Head.Offset == 0 || cut.Hash == ([32]byte{}) {
		t.Fatal(cut)
	}
	for i := 1; i <= 3; i++ {
		_, found, err := p.ReadLease(ctx, fmt.Sprintf("att_owner_%d", i))
		if err != nil || found != (i == 1) {
			t.Fatal(i, found, err)
		}
	}
	b, err := p.ReadBalance(ctx, ledger.BalanceKey{ProjectID: "prj_1", PeriodID: "2026-10-03", TimezoneVersion: 1})
	if err != nil || b.ReservedMicrosUSD != 100 {
		t.Fatal(b, err)
	}
	if after := publisher.Snapshot(); after.AppliedIndex != member.AppliedIndex || after.OrderingHeadMAC != member.OrderingHeadMAC {
		t.Fatal("construction published member progress", after)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	absent := filepath.Join(root, "absent.db")
	if p, _, err := r.buildReplicaLedgerOwner(canceled, native, absent); !errors.Is(err, context.Canceled) || p != nil {
		t.Fatal(p, err)
	}
	r.role = replication.RolePrimary
	if p, _, err := r.buildReplicaLedgerOwner(ctx, native, absent); err == nil || p != nil {
		t.Fatal("Primary used Replica build authority", p, err)
	}
	if _, err := os.Stat(absent); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}
