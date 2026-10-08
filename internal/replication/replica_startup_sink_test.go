package replication

import (
	"context"
	"errors"
	"testing"
)

func TestReplicaStartupReceiverRefusesReceiveBeforeActualSinkHandoff(t *testing.T) {
	_, journal := newTestReceiver(t, &recordingFrameSink{})
	defer journal.Close()
	r, err := NewReplicaStartupReceiver("production-a", "inc_01", "halro-1", "halro-0", 7, 7, 0, 0, journal, ReplicaReceiverOptions{})
	if err != nil {
		t.Fatal(err)
	}
	before, term, mac := journal.Head()
	encoded := encodeTestReplicaFrame(t, 1, KindLeadershipEstablished)
	if _, err := r.Receive(encoded); !errors.Is(err, ErrMemberAuthority) {
		t.Fatal("startup received bytes without installed actual sink", err)
	}
	after, newTerm, newMAC := journal.Head()
	if before != after || term != newTerm || mac != newMAC {
		t.Fatal("startup receive changed ordering")
	}
	if err := r.InstallStartupNativeSink(&NativeSink{}); !errors.Is(err, ErrMemberAuthority) {
		t.Fatal("caller-created empty sink accepted", err)
	}
	if _, err := r.Receive(encoded); !errors.Is(err, ErrMemberAuthority) {
		t.Fatal("failed handoff admitted receive", err)
	}
	applier, err := NewReplicaApplier(r, journal, &startupNoProgressProjection{})
	if err != nil {
		t.Fatal(err)
	}
	if through, err := applier.ApplyConfirmed(context.Background()); err != nil || through != 0 {
		t.Fatal(through, err)
	}
	if _, err := r.Receive(encoded); !errors.Is(err, ErrMemberAuthority) {
		t.Fatal("zero-cut apply manufactured sink", err)
	}
}

type startupNoProgressProjection struct{}

func (*startupNoProgressProjection) ApplyLedgerThrough(context.Context, uint64, uint64) error {
	return ErrMemberAuthority
}
func (*startupNoProgressProjection) ApplyMetadataThrough(context.Context, uint64, uint64) error {
	return ErrMemberAuthority
}
func (*startupNoProgressProjection) ApplyConfirmedPrefix(context.Context, *ReplicaConfirmedPrefix) (ProjectionState, error) {
	return ProjectionState{}, ErrMemberAuthority
}
