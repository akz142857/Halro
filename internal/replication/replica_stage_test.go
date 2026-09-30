package replication

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestReplicaStageBoundariesRecordFailureAndRecoveryWithoutErrorText(t *testing.T) {
	receiver, journal := newTestReceiver(t, &recordingFrameSink{})
	defer journal.Close()
	for _, frame := range []Frame{testConfirmationFrame(1, KindLeadershipEstablished), testConfirmationFrame(2, KindData)} {
		if frame.Kind == KindData {
			frame.Store, frame.StoreGeneration, frame.StoreSequenceFirst, frame.StoreSequenceLast = StoreLedger, 1, 1, 1
		}
		encoded, err := frame.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := receiver.Receive(encoded); err != nil {
			t.Fatal(err)
		}
	}
	projection := &recordingProjection{err: errors.New("secret.internal: projection unavailable")}
	applier, err := NewReplicaApplier(receiver, journal, projection)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applier.ApplyConfirmed(context.Background()); err == nil {
		t.Fatal("projection failure was ignored")
	}
	history := receiver.StageTransitions()
	if history.ReceiveState != "ready" || history.ApplyState != "blocked" || len(history.Events) != 1 ||
		history.Events[0].Reason != "projection_failed" || history.Events[0].TargetIndex != 2 || strings.Contains(history.Events[0].Reason, "secret") {
		t.Fatalf("failure boundary=%+v", history)
	}
	projection.err = nil
	if applied, err := applier.ApplyConfirmed(context.Background()); err != nil || applied != 2 {
		t.Fatalf("retry apply=%d err=%v", applied, err)
	}
	history = receiver.StageTransitions()
	if history.ApplyState != "ready" || len(history.Events) != 2 || history.Events[1].Reason != "apply_recovered" ||
		history.Events[1].From != "blocked" || history.Events[1].To != "ready" {
		t.Fatalf("recovery boundary=%+v", history)
	}
	if _, err := applier.ApplyConfirmed(context.Background()); err != nil || len(receiver.StageTransitions().Events) != 2 {
		t.Fatal("idempotent apply emitted another boundary")
	}
}

func TestReplicaStageReceiverPoisonAndBoundedRing(t *testing.T) {
	receiver, journal := newTestReceiver(t, &recordingFrameSink{err: errors.New("private disk path")})
	defer journal.Close()
	if _, err := receiver.Receive(encodeTestReplicaFrame(t, 1, KindLeadershipEstablished)); err == nil {
		t.Fatal("sink failure was ignored")
	}
	history := receiver.StageTransitions()
	if history.ReceiveState != "blocked" || len(history.Events) != 1 || history.Events[0].Reason != "sink_persist_failed" ||
		history.Events[0].TargetIndex != 1 {
		t.Fatalf("sink boundary=%+v", history)
	}
	if _, err := receiver.Receive(encodeTestReplicaFrame(t, 1, KindLeadershipEstablished)); err == nil || len(receiver.StageTransitions().Events) != 1 {
		t.Fatal("poison retry emitted duplicate boundary")
	}
	receiver.mu.Lock()
	for i := 0; i < replicaStageEventLimit+2; i++ {
		if i%2 == 0 {
			receiver.setStage("apply", "blocked", "projection_failed", uint64(i))
		} else {
			receiver.setStage("apply", "ready", "apply_recovered", uint64(i))
		}
	}
	receiver.mu.Unlock()
	history = receiver.StageTransitions()
	if len(history.Events) != replicaStageEventLimit || history.Dropped != 3 || history.Events[0].Sequence != 4 {
		t.Fatalf("ring retention=%+v", history)
	}
}
