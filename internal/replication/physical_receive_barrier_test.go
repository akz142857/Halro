package replication

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPhysicalReceiveBarrierActuallyExcludesNativeApplyAndPromise(t *testing.T) {
	r, j := newTestReceiver(t, &recordingFrameSink{})
	defer j.Close()
	a, err := NewReplicaApplier(r, j, &recordingProjection{})
	if err != nil {
		t.Fatal(err)
	}
	entered, exit := make(chan struct{}), make(chan struct{})
	joined := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		joined <- a.WithPhysicalReceiveReadBarrier(ctx, func(ctx context.Context, p ReplicaConfirmedPosition, ack Acknowledgement) error {
			if p.NodeID != ack.NodeID || p.Durable != ack.DurableIndex || p.Applied != ack.AppliedIndex {
				return errors.New("actual owner observation mismatch")
			}
			close(entered)
			<-exit
			return nil
		})
	}()
	<-entered
	encoded, err := testConfirmationFrame(1, KindLeadershipEstablished).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	native := make(chan error, 1)
	apply := make(chan error, 1)
	promise := make(chan error, 1)
	go func() { _, err := r.Receive(encoded); native <- err }()
	go func() { _, err := a.ApplyConfirmed(context.Background()); apply <- err }()
	go func() { promise <- r.Promise(8, func() error { return nil }) }()
	for _, ch := range []chan error{native, apply, promise} {
		select {
		case err := <-ch:
			t.Fatal("original producer escaped actual barrier", err)
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err = a.WithPhysicalReceiveReadBarrier(context.Background(), func(context.Context, ReplicaConfirmedPosition, Acknowledgement) error {
		t.Fatal("second reader entered")
		return nil
	}); !errors.Is(err, ErrMemberAuthority) {
		t.Fatal(err)
	}
	cancel()
	close(exit)
	if err = <-joined; !errors.Is(err, context.Canceled) {
		t.Fatal("canceled owner returned proof", err)
	}
	if err = <-promise; err != nil {
		t.Fatal(err)
	}
	if err = <-apply; err != nil {
		t.Fatal(err)
	}
	// Whichever original producer acquires r.mu first, an accepted promise may
	// correctly reject the old-term frame; exclusion never removes that rule.
	if err = <-native; err != nil && !strings.Contains(err.Error(), "durable promise") {
		t.Fatal(err)
	}
	canceled, stop := context.WithCancel(context.Background())
	stop()
	if err = a.WithPhysicalReceiveReadBarrier(canceled, func(context.Context, ReplicaConfirmedPosition, Acknowledgement) error {
		t.Fatal("canceled reader entered")
		return nil
	}); err == nil {
		t.Fatal("canceled source read admitted")
	}
}

func TestPhysicalReceiveAdmissionRefusesBeforeNativeAndOrderingAndSkipsDuplicates(t *testing.T) {
	sink := &recordingFrameSink{}
	r, j := newTestReceiver(t, sink)
	defer j.Close()
	if _, err := r.Receive(encodeTestReplicaFrame(t, 1, KindLeadershipEstablished)); err != nil {
		t.Fatal(err)
	}
	a, err := NewReplicaApplier(r, j, &recordingProjection{})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	admit := false
	if err := a.InstallPhysicalReceiveAdmission(context.Background(), func(f Frame) error {
		calls++
		if !admit {
			return ErrPhysicalCredit
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.InstallPhysicalReceiveAdmission(context.Background(), func(Frame) error { return nil }); err == nil {
		t.Fatal("policy replaced")
	}
	before, term, head := j.Head()
	writes := len(sink.frames)
	data := encodeTestReplicaFrame(t, 2, KindData)
	if _, err := r.Receive(data); !errors.Is(err, ErrPhysicalCredit) {
		t.Fatal(err)
	}
	after, afterTerm, afterHead := j.Head()
	if before != after || term != afterTerm || head != afterHead || len(sink.frames) != writes || calls != 1 {
		t.Fatal("pre-byte refusal changed native or Ordering", calls)
	}
	wrong := testConfirmationFrame(2, KindData)
	wrong.Term--
	wrong.StoreSequenceFirst = 1
	wrong.StoreSequenceLast = 1
	encoded, err := wrong.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Receive(encoded); err == nil || calls != 1 {
		t.Fatal("wrong authority entered credit policy", err, calls)
	}
	admit = true
	if _, err := r.Receive(data); err != nil {
		t.Fatal(err)
	}
	admit = false
	if _, err := r.Receive(data); err != nil {
		t.Fatal("original durable duplicate consumed future credit", err)
	}
	if calls != 2 || len(sink.frames) != writes+1 {
		t.Fatal("original duplicate invoked policy", calls, len(sink.frames))
	}
}
