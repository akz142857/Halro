package replication

import (
	"context"
	"testing"
)

type recordingProjection struct {
	ledger   []StoreCursor
	metadata []StoreCursor
	err      error
	schema   []SchemaBoundaryMetadata
}

func (p *recordingProjection) ValidateSchemaBoundary(_ context.Context, from, to uint32) error {
	p.schema = append(p.schema, SchemaBoundaryMetadata{From: from, To: to})
	return p.err
}

func (p *recordingProjection) ApplyLedgerThrough(_ context.Context, generation, sequence uint64) error {
	p.ledger = append(p.ledger, StoreCursor{Generation: generation, Sequence: sequence})
	return p.err
}

func (p *recordingProjection) ApplyMetadataThrough(_ context.Context, epoch, sequence uint64) error {
	p.metadata = append(p.metadata, StoreCursor{Generation: epoch, Sequence: sequence})
	return p.err
}

func TestReplicaApplierBatchesTheLocallyQuorumConfirmedPrefix(t *testing.T) {
	receiver, journal := newTestReceiver(t, &recordingFrameSink{})
	defer journal.Close()
	frames := []Frame{
		testConfirmationFrame(1, KindLeadershipEstablished),
		testConfirmationFrame(2, KindData),
		testConfirmationFrame(3, KindData),
		testConfirmationFrame(4, KindData),
	}
	frames[1].Store, frames[1].StoreGeneration, frames[1].StoreSequenceFirst, frames[1].StoreSequenceLast = StoreLedger, 1, 1, 1
	frames[2].Store, frames[2].StoreGeneration, frames[2].StoreSequenceFirst, frames[2].StoreSequenceLast = StoreMetadata, 5, 1, 1
	frames[3].Store, frames[3].StoreGeneration, frames[3].StoreSequenceFirst, frames[3].StoreSequenceLast = StoreAudit, 1, 1, 1
	for _, frame := range frames {
		encoded, err := frame.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := receiver.Receive(encoded); err != nil {
			t.Fatal(err)
		}
	}
	projection := &recordingProjection{}
	applier, err := NewReplicaApplier(receiver, journal, projection)
	if err != nil {
		t.Fatal(err)
	}
	if applied, err := applier.ApplyConfirmed(context.Background()); err != nil || applied != 4 {
		t.Fatalf("first apply=%d err=%v", applied, err)
	}
	if len(projection.ledger) != 1 || projection.ledger[0] != (StoreCursor{Generation: 1, Sequence: 1}) ||
		len(projection.metadata) != 1 || projection.metadata[0] != (StoreCursor{Generation: 5, Sequence: 1}) {
		t.Fatalf("first projection ledger=%v metadata=%v", projection.ledger, projection.metadata)
	}
	// A commit notice is a replay/catch-up signal. It must be idempotent after
	// this Replica's fsync already formed the second durable vote.
	if err := receiver.Confirm(CommitNotice{ClusterID: "production-a", Incarnation: "inc_01", NodeID: "halro-0", Term: 7, ConfirmedIndex: 4}); err != nil {
		t.Fatal(err)
	}
	if applied, err := applier.ApplyConfirmed(context.Background()); err != nil || applied != 4 {
		t.Fatalf("second apply=%d err=%v", applied, err)
	}
	if telemetry := applier.ApplyBatchTelemetry(); telemetry.Count != 1 {
		t.Fatalf("idempotent apply counted twice: %+v", telemetry)
	}
	_, _, applied := receiver.Progress()
	if applied != 4 {
		t.Fatalf("receiver applied=%d", applied)
	}
	if projectionProgress := receiver.ProjectionProgress(); projectionProgress != (ProjectionState{Index: 4, MetadataEpoch: 5, MetadataSequence: 1}) {
		t.Fatalf("receiver projection=%#v", projectionProgress)
	}
}

func TestReplicaApplierStopsBeforeAdvancingAppliedOnCancellation(t *testing.T) {
	receiver, journal := newTestReceiver(t, &recordingFrameSink{})
	defer journal.Close()
	encoded, err := testConfirmationFrame(1, KindLeadershipEstablished).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := receiver.Receive(encoded); err != nil {
		t.Fatal(err)
	}
	if err := receiver.Confirm(CommitNotice{ClusterID: "production-a", Incarnation: "inc_01", NodeID: "halro-0", Term: 7, ConfirmedIndex: 1}); err != nil {
		t.Fatal(err)
	}
	applier, err := NewReplicaApplier(receiver, journal, &recordingProjection{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if applied, err := applier.ApplyConfirmed(ctx); err != context.Canceled || applied != 0 {
		t.Fatalf("applied=%d err=%v, want cancellation before index advance", applied, err)
	}
	_, _, applied := receiver.Progress()
	if applied != 0 {
		t.Fatalf("receiver applied index advanced to %d", applied)
	}
}

func TestReplicaApplierValidatesConfirmedSchemaBoundaryBeforeAdvancing(t *testing.T) {
	receiver, journal := newTestReceiver(t, &recordingFrameSink{})
	defer journal.Close()
	anchor, err := testConfirmationFrame(1, KindLeadershipEstablished).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := receiver.Receive(anchor); err != nil {
		t.Fatal(err)
	}
	metadata, err := (SchemaBoundaryMetadata{From: 40, To: 41}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	frame := testConfirmationFrame(2, KindSchemaBoundary)
	frame.Metadata = metadata
	encoded, err := frame.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := receiver.Receive(encoded); err != nil {
		t.Fatal(err)
	}
	if err := receiver.Confirm(CommitNotice{ClusterID: "production-a", Incarnation: "inc_01", NodeID: "halro-0", Term: 7, ConfirmedIndex: 2}); err != nil {
		t.Fatal(err)
	}
	projection := &recordingProjection{}
	applier, err := NewReplicaApplier(receiver, journal, projection)
	if err != nil {
		t.Fatal(err)
	}
	if applied, err := applier.ApplyConfirmed(context.Background()); err != nil || applied != 2 {
		t.Fatalf("schema apply=%d err=%v", applied, err)
	}
	if len(projection.schema) != 1 || projection.schema[0] != (SchemaBoundaryMetadata{From: 40, To: 41}) {
		t.Fatalf("validated schema boundaries=%#v", projection.schema)
	}
}
