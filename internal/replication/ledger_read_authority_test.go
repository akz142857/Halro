package replication

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/akz142857/Halro/internal/resourcebudget"
)

func TestMemberLedgerReadRetainsOnlySamePrimaryDurableCut(t *testing.T) {
	for _, shape := range []string{"confirmed-only", "durable-changed", "promise", "foreign-file", "strict-common-cut"} {
		t.Run(shape, func(t *testing.T) {
			p, manager := memberReadAuthorityFixture(t)
			if err := p.PublishPrimary(PrimaryProgress{DurableIndex: 2, ConfirmedIndex: 1, OrderingHeadMAC: [32]byte{1}, Projection: ProjectionState{Index: 1, MetadataEpoch: 4, MetadataSequence: 9}}); err != nil {
				t.Fatal(err)
			}
			entered, resume, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			read := p.WithLedgerReadAuthority
			if shape == "strict-common-cut" {
				read = p.WithReadAuthority
			}
			called := 0
			go func() {
				done <- read(context.Background(), manager, func(ctx context.Context, s MemberState, authorize func(func() error) error) error {
					if s.DurableIndex != 2 || s.ConfirmedIndex != 1 {
						return ErrMemberAuthority
					}
					close(entered)
					<-resume
					return authorize(func() error { called++; return ctx.Err() })
				})
			}()
			<-entered
			var err error
			switch shape {
			case "confirmed-only", "strict-common-cut":
				err = p.PublishPrimary(PrimaryProgress{DurableIndex: 2, ConfirmedIndex: 2, OrderingHeadMAC: [32]byte{1}, Projection: ProjectionState{Index: 2, MetadataEpoch: 4, MetadataSequence: 10}})
			case "durable-changed":
				err = p.PublishPrimary(PrimaryProgress{DurableIndex: 3, ConfirmedIndex: 1, OrderingHeadMAC: [32]byte{2}, Projection: ProjectionState{Index: 1, MetadataEpoch: 4, MetadataSequence: 9}})
			case "promise":
				_, err = p.Promise(p.Snapshot().PromisedTerm + 1)
			case "foreign-file":
				err = os.WriteFile(p.path, []byte("foreign-state"), 0600)
			}
			close(resume)
			readErr := <-done
			if err != nil {
				t.Fatal(err)
			}
			if shape == "confirmed-only" {
				if readErr != nil || called != 1 {
					t.Fatal("same actual durable cut was revoked", called, readErr)
				}
			} else if readErr == nil || called != 0 {
				t.Fatal("changed authority accepted", shape, called, readErr)
			}
			if manager.Snapshot().Used != (resourcebudget.Cost{}) {
				t.Fatal("actual reader fee leaked", manager.Snapshot())
			}
			if shape == "foreign-file" && !errors.Is(readErr, ErrMemberAuthority) {
				t.Fatal(readErr)
			}
		})
	}
}
