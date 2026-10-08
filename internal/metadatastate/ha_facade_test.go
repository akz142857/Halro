package metadatastate

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/metadatajournal"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

func TestHAFacadeQueuesReadBeforeMemberPermissionAndCloseJoins(t *testing.T) {
	for _, closing := range []bool{false, true} {
		t.Run(map[bool]string{false: "confirmed-read", true: "close-joins"}[closing], func(t *testing.T) {
			ctx := context.Background()
			b, o, _ := linkedDeltaCandidate(t)
			ref, err := b.SealReference(ctx)
			if err != nil {
				t.Fatal(err)
			}
			a := metadatajournal.BoundReplicationAuthority{Baseline: b.position.Baseline, FileRevision: b.position.FileRevision, MaxFileBytes: 16 << 20}
			waiting, release := make(chan struct{}), make(chan struct{})
			if err := b.ConfigureHAPrimaryJournal(ctx, a, func(metadatajournal.DurableBatch) (metadatajournal.AppendAdmission, error) { return nil, nil }, func(metadatajournal.DurableBatch) (uint64, error) { return 9, nil }, func(ctx context.Context, _ uint64) error {
				close(waiting)
				<-release
				return ctx.Err()
			}); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int64
			var stale atomic.Bool
			permission := func(ctx context.Context, visit func(context.Context, authenticatedstate.SealPosition) error) error {
				calls.Add(1)
				b.mu.Lock()
				position := metadataSealPosition(b.position)
				b.mu.Unlock()
				if stale.Load() {
					position.Sequence++
				}
				return visit(ctx, position)
			}
			backend, err := b.HAFacade(ctx, o.Budget, b.evidence.Identity, b.evidence.InstanceID, b.evidence.KeyGeneration, ref, authenticatedstate.SealPrimary, permission)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := b.HAFacade(ctx, o.Budget, b.evidence.Identity, b.evidence.InstanceID, b.evidence.KeyGeneration, ref, authenticatedstate.SealReplica, permission); err == nil {
				t.Fatal("caller role substituted for actual Replica native mode")
			}
			writer := make(chan error, 1)
			go func() {
				_, err := backend.Update(ctx, func(tx boltstore.FacadeTransaction) error {
					return tx.Put([]string{"projects"}, []byte("ha-dao"), []byte("value"))
				})
				writer <- err
			}()
			<-waiting
			admitted := calls.Load()
			reader := make(chan error, 1)
			go func() {
				reader <- backend.View(ctx, func(tx boltstore.FacadeTransaction) error {
					value, found, err := tx.Get([]string{"projects"}, []byte("ha-dao"))
					if err != nil || !found || string(value) != "value" {
						return errors.Join(ErrSource, err)
					}
					return nil
				})
			}()
			deadline := time.After(time.Second)
			for b.businessWaiters.Load() != 2 {
				select {
				case <-deadline:
					t.Fatal("actual read never queued behind confirmation")
				case <-time.After(time.Millisecond):
				}
			}
			if calls.Load() != admitted {
				t.Fatal("queued read froze Member before actual writer ACK")
			}
			closed := make(chan error, 1)
			if closing {
				go func() { closed <- backend.Close() }()
				<-b.ctx.Done()
				if err := <-reader; err == nil {
					t.Fatal("closed queued read succeeded")
				}
				select {
				case err := <-closed:
					t.Fatal("Close escaped actual wait", err)
				case <-time.After(20 * time.Millisecond):
				}
			}
			close(release)
			if err := <-writer; closing && !errors.Is(err, context.Canceled) || !closing && err != nil {
				t.Fatal(err)
			}
			if closing {
				if err := <-closed; err != nil {
					t.Fatal(err)
				}
			} else {
				if err := <-reader; err != nil {
					t.Fatal(err)
				}
				stale.Store(true)
				invoked := false
				if err := backend.View(ctx, func(boltstore.FacadeTransaction) error { invoked = true; return nil }); err == nil || invoked {
					t.Fatal("stale or unapplied permission exposed actual rows", invoked, err)
				}
				if err := backend.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
