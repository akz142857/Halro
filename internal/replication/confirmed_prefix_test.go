package replication

import (
	"context"
	"errors"
	"testing"
	"time"
)

type confirmedPrefixTestProjection struct {
	apply func(context.Context, *ReplicaConfirmedPrefix) (ProjectionState, error)
}

func (p *confirmedPrefixTestProjection) ApplyConfirmedPrefix(ctx context.Context, scope *ReplicaConfirmedPrefix) (ProjectionState, error) {
	return p.apply(ctx, scope)
}
func (*confirmedPrefixTestProjection) ApplyLedgerThrough(context.Context, uint64, uint64) error {
	return errors.New("whole-prefix owner fell through legacy Ledger apply")
}
func (*confirmedPrefixTestProjection) ApplyMetadataThrough(context.Context, uint64, uint64) error {
	return errors.New("whole-prefix owner fell through legacy Metadata apply")
}

func TestReplicaConfirmedPrefixActualPermissionAndJoin(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "publish"
		if fail {
			name = "borrower_failure"
		}
		t.Run(name, func(t *testing.T) {
			r, j := newTestReceiver(t, &recordingFrameSink{})
			defer j.Close()
			for i := uint64(1); i <= 3; i++ {
				frame := testConfirmationFrame(i, KindLeadershipEstablished)
				if i > 1 {
					frame = testConfirmationFrame(i, KindData)
					frame.StoreSequenceFirst, frame.StoreSequenceLast = i-1, i-1
				}
				frame.ConfirmedIndex = i - 1
				encoded, err := frame.MarshalBinary()
				if err != nil {
					t.Fatal(err)
				}
				if _, err := r.Receive(encoded); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.Confirm(CommitNotice{ClusterID: r.clusterID, Incarnation: r.incarnation, NodeID: r.primaryNodeID, Term: r.term, ConfirmedIndex: 3}); err != nil {
				t.Fatal(err)
			}
			entered, release, result := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			var escaped *ReplicaConfirmedPrefix
			borrowErr := errors.New("actual borrower rejected component proof")
			p := &confirmedPrefixTestProjection{apply: func(ctx context.Context, scope *ReplicaConfirmedPrefix) (ProjectionState, error) {
				escaped = scope
				if err := scope.WithPosition(ctx, r, j, func(ctx context.Context, pos ReplicaConfirmedPosition) error {
					if pos.Applied != 0 || pos.Confirmed != 3 || pos.Durable != 3 || pos.Term != pos.PromisedTerm {
						return ErrMemberAuthority
					}
					return nil
				}); err != nil {
					return ProjectionState{}, err
				}
				if err := scope.WithPosition(ctx, &ReplicaReceiver{}, j, func(context.Context, ReplicaConfirmedPosition) error { t.Error("foreign receiver allowed"); return nil }); !errors.Is(err, ErrMemberAuthority) {
					return ProjectionState{}, errors.New("foreign receiver qualification failed")
				}
				go func() {
					_ = scope.WithPosition(ctx, r, j, func(context.Context, ReplicaConfirmedPosition) error {
						close(entered)
						<-release
						if fail {
							return borrowErr
						}
						return nil
					})
				}()
				<-entered
				return ProjectionState{Index: 3}, nil
			}}
			a, err := NewReplicaApplier(r, j, p)
			if err != nil {
				t.Fatal(err)
			}
			go func() { _, err := a.ApplyConfirmed(context.Background()); result <- err }()
			<-entered
			select {
			case err := <-result:
				t.Fatal("applier published before actual borrower joined", err)
			case <-time.After(20 * time.Millisecond):
			}
			close(release)
			err = <-result
			if fail != errors.Is(err, borrowErr) || !fail && err != nil {
				t.Fatal("apply outcome", err)
			}
			_, _, applied := r.Progress()
			if !fail && applied != 3 || fail && applied != 0 {
				t.Fatal("unjoined/errorful whole-prefix job published", applied)
			}
			if err := escaped.WithPosition(context.Background(), r, j, func(context.Context, ReplicaConfirmedPosition) error {
				t.Error("escaped confirmed capability allowed")
				return nil
			}); !errors.Is(err, ErrMemberAuthority) {
				t.Fatal("escaped permission", err)
			}
		})
	}
}
