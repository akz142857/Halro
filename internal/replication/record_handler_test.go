package replication

import (
	"context"
	"strings"
	"testing"
)

func TestReplicaRecordHandlerPersistsAppliesAndAcknowledges(t *testing.T) {
	receiver, journal := newTestReceiver(t, &recordingFrameSink{})
	defer journal.Close()
	projection := &recordingProjection{}
	applier, err := NewReplicaApplier(receiver, journal, projection)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewReplicaRecordHandler(receiver, applier)
	if err != nil {
		t.Fatal(err)
	}
	peer := Hello{NodeID: "halro-0", Role: RolePrimary}
	var acknowledgements []Acknowledgement
	send := func(encoded []byte) error {
		ack, err := UnmarshalAcknowledgement(encoded)
		if err != nil {
			return err
		}
		acknowledgements = append(acknowledgements, ack)
		return nil
	}
	anchor := encodeTestReplicaFrame(t, 1, KindLeadershipEstablished)
	if err := handler.Handle(context.Background(), peer, StreamRecord{Kind: StreamRecordFrame, Encoded: anchor}, send); err != nil {
		t.Fatal(err)
	}
	data := testConfirmationFrame(2, KindData)
	data.ConfirmedIndex = 1
	data.Store, data.StoreGeneration = StoreLedger, 1
	data.StoreSequenceFirst, data.StoreSequenceLast = 1, 1
	encodedData, err := data.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if err := handler.Handle(context.Background(), peer, StreamRecord{Kind: StreamRecordFrame, Encoded: encodedData}, send); err != nil {
		t.Fatal(err)
	}
	notice, err := (CommitNotice{ClusterID: "production-a", Incarnation: "inc_01", NodeID: "halro-0", Term: 7, ConfirmedIndex: 2}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if err := handler.Handle(context.Background(), peer, StreamRecord{Kind: StreamRecordCommitNotice, Encoded: notice}, send); err != nil {
		t.Fatal(err)
	}
	if len(acknowledgements) != 3 {
		t.Fatalf("acknowledgements=%d", len(acknowledgements))
	}
	if acknowledgements[0].DurableIndex != 1 || acknowledgements[0].AppliedIndex != 0 ||
		acknowledgements[1].DurableIndex != 2 || acknowledgements[1].AppliedIndex != 1 ||
		acknowledgements[2].DurableIndex != 2 || acknowledgements[2].AppliedIndex != 2 {
		t.Fatalf("acknowledgements=%#v", acknowledgements)
	}
	if len(projection.ledger) != 1 || projection.ledger[0] != (StoreCursor{Generation: 1, Sequence: 1}) {
		t.Fatalf("ledger projection=%v", projection.ledger)
	}
}

func TestRecordHandlersBindWireIdentityToAuthenticatedHello(t *testing.T) {
	primary, journal := newTestPrimary(t, &recordingOutbound{})
	defer journal.Close()
	if _, err := primary.RecordDurable(Frame{Kind: KindLeadershipEstablished}); err != nil {
		t.Fatal(err)
	}
	handler, err := NewPrimaryRecordHandler(primary)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := testAcknowledgement("halro-1", 1).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	err = handler.Handle(context.Background(), Hello{NodeID: "halro-2", Role: RoleReplica}, StreamRecord{Kind: StreamRecordAcknowledgement, Encoded: encoded}, nil)
	if err == nil || !strings.Contains(err.Error(), "authenticated peer") {
		t.Fatalf("identity mismatch error=%v", err)
	}

	receiver, replicaJournal := newTestReceiver(t, &recordingFrameSink{})
	defer replicaJournal.Close()
	applier, err := NewReplicaApplier(receiver, replicaJournal, &recordingProjection{})
	if err != nil {
		t.Fatal(err)
	}
	replicaHandler, err := NewReplicaRecordHandler(receiver, applier)
	if err != nil {
		t.Fatal(err)
	}
	commit, err := (CommitNotice{ClusterID: "production-a", Incarnation: "inc_01", NodeID: "halro-0", Term: 7, ConfirmedIndex: 1}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	err = replicaHandler.Handle(context.Background(), Hello{NodeID: "halro-9", Role: RolePrimary}, StreamRecord{Kind: StreamRecordCommitNotice, Encoded: commit}, func([]byte) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "authenticated peer") {
		t.Fatalf("commit identity mismatch error=%v", err)
	}
}
