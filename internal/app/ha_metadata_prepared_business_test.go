package app

import (
	"context"
	"errors"
	"github.com/akz142857/Halro/internal/metadatastate"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/store/lock"
	"io"
	"log/slog"
	"testing"
	"time"
)

// This exercises the actual installed bound Primary writer and authenticated
// Replica sink over TLS. It does not confer seven-root Runtime readiness.
func TestHAMetadataPreparedWriteActualTLSOrderingConfirmationAndReplicaApply(t *testing.T) {
	f := prepareHAMetadataMaintenanceFixture(t)
	prepareHAMetadataMaintenanceArtifacts(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	owners := [2]*HAMetadataMaintenance{}
	guards := [2]*lock.Lock{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for i, cfg := range f.configs {
		var err error
		guards[i], err = lock.Acquire(cfg.Storage.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		owners[i], err = OpenHAMetadataMaintenance(ctx, cfg, f.keys, guards[i], functionalHAMetadataManager(t, cfg), logger)
		if err != nil {
			t.Fatal(err)
		}
	}
	runCtx, stop := context.WithCancel(ctx)
	complete := make(chan replication.MemberState, 2)
	done := make(chan error, 2)
	for _, m := range owners {
		go func(m *HAMetadataMaintenance) {
			done <- m.Run(runCtx, func(s replication.MemberState) error { complete <- s; return nil })
		}(m)
	}
	t.Cleanup(func() {
		stop()
		for range 2 {
			if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		}
		for i, m := range owners {
			if err := m.Close(); err != nil {
				t.Error(err)
			}
			if err := guards[i].Close(); err != nil {
				t.Error(err)
			}
		}
	})
	for range 2 {
		select {
		case <-complete:
		case <-ctx.Done():
			t.Fatal("boundary confirmation timed out", ctx.Err())
		}
	}
	before := owners[0].member.publisher.Snapshot()
	primary := owners[0].baseline
	record, err := primary.Update(ctx, func(tx *metadatastate.Transaction) error {
		if err := tx.Put([]string{"projects"}, []byte("actual-ha-write"), []byte("confirmed-native-value")); err != nil {
			return err
		}
		// Node-derived writes stay in this node's atomic root and out of the wire.
		return tx.Put([]string{"meta"}, []byte("shutdown_truncated_attempts_total"), []byte("42"))
	})
	if err != nil || record.Sequence != 1 {
		t.Fatal("real prepared Primary update", record, err)
	}
	state := owners[0].member.publisher.Snapshot()
	if state.DurableIndex != before.DurableIndex+1 || state.ConfirmedIndex != state.DurableIndex || state.AppliedIndex != state.ConfirmedIndex || state.Projection.MetadataSequence != 1 {
		t.Fatal("successful write bypassed actual confirmation", before, state)
	}
	ordered, err := owners[0].member.journal.Record(state.DurableIndex)
	if err != nil || ordered.Kind != replication.KindData || ordered.Store != replication.StoreMetadata || ordered.StoreSequenceFirst != 1 || ordered.StoreSequenceLast != 1 || ordered.PreviousMAC != before.OrderingHeadMAC {
		t.Fatal("actual ordering continuity", ordered, err)
	}
	for owners[1].member.publisher.Snapshot().AppliedIndex != state.ConfirmedIndex {
		select {
		case <-ctx.Done():
			t.Fatal("real Replica apply did not finish", ctx.Err(), owners[1].Snapshot())
		case <-time.After(10 * time.Millisecond):
		}
	}
	replicaState := owners[1].member.publisher.Snapshot()
	replicaRecord, err := owners[1].member.journal.Record(replicaState.AppliedIndex)
	if err != nil || replicaRecord.MAC != ordered.MAC || replicaState.Projection.MetadataSequence != 1 {
		t.Fatal(replicaState, replicaRecord, ordered, err)
	}
	for i, m := range owners {
		if err := m.baseline.View(ctx, func(tx *metadatastate.Transaction) error {
			value, found, err := tx.Get([]string{"projects"}, []byte("actual-ha-write"))
			if err != nil || !found || string(value) != "confirmed-native-value" {
				return errors.Join(err, errors.New("actual selected Metadata value differs"))
			}
			local, found, err := tx.Get([]string{"meta"}, []byte("shutdown_truncated_attempts_total"))
			if err != nil || i == 0 && (!found || string(local) != "42") || i == 1 && found && string(local) == "42" {
				return errors.Join(err, errors.New("node-derived local value crossed wire"))
			}
			return nil
		}); err != nil {
			t.Fatal("actual source/sink projected values", i, err)
		}
	}
}
