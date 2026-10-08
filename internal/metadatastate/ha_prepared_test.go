package metadatastate

import (
	"bytes"
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"errors"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"testing"
	"time"
)

func TestHAPreparedBaselineCommitsRootBeforeActualConfirmationAndCloseJoins(t *testing.T) {
	for _, closing := range []bool{false, true} {
		t.Run(map[bool]string{false: "confirmed", true: "close-joins"}[closing], func(t *testing.T) {
			ctx := context.Background()
			b, o, path := linkedDeltaCandidate(t)
			old, err := b.store.RootIdentity(ctx)
			if err != nil {
				t.Fatal(err)
			}
			nativeKey, err := hkdf.Key(sha256.New, bytes.Repeat([]byte{51}, 32), old.Scope[:], "halro:metadata:bound-journal:hmac:v1", 32)
			if err != nil {
				t.Fatal(err)
			}
			defer clear(nativeKey)
			a := metadatajournal.BoundReplicationAuthority{Baseline: b.position.Baseline, FileRevision: b.position.FileRevision, MaxFileBytes: 16 << 20}
			waiting, release := make(chan struct{}), make(chan struct{})
			err = b.ConfigureHAPrimaryJournal(ctx, a, func(metadatajournal.DurableBatch) (metadatajournal.AppendAdmission, error) { return nil, nil }, func(metadatajournal.DurableBatch) (uint64, error) {
				if _, seq, err := metadatajournal.BoundReplicationCursor(ctx, path, nativeKey, a); err != nil || seq != 1 {
					t.Fatal(seq, err)
				}
				current, err := b.store.RootIdentity(ctx)
				if !errors.Is(err, authenticatedstate.ErrBusy) {
					return 0, errors.New("candidate publication lost exclusion before actual ordering")
				}
				_ = current
				return 9, nil
			}, func(waitCtx context.Context, index uint64) error {
				actual, err := b.store.RootIdentity(ctx)
				if err != nil || index != 9 || actual.Revision != old.Revision+1 || actual.Cut == old.Cut || b.position.Sequence != 1 {
					t.Fatal("confirmation before actual root publication", actual, index, err)
				}
				close(waiting)
				if closing {
					<-waitCtx.Done()
				}
				<-release
				return waitCtx.Err()
			})
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				_, err := b.AppendFrame(ctx, []metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte("confirmed-write"), Value: []byte("v")}})
				done <- err
			}()
			<-waiting
			if b.ready.Load() {
				t.Fatal("ready escaped actual wait")
			}
			closed := make(chan error, 1)
			if closing {
				go func() { closed <- b.Close() }()
				<-b.ctx.Done()
				select {
				case err := <-closed:
					t.Fatal("close escaped actual confirmation", err)
				case <-time.After(25 * time.Millisecond):
				}
				if o.Budget.Snapshot().Classes[resourcebudget.IO].RAM < DeltaRAMBytes {
					t.Fatal("work loan returned before actual confirmation")
				}
			}
			close(release)
			if err := <-done; closing && !errors.Is(err, context.Canceled) || !closing && err != nil {
				t.Fatal(err)
			}
			if closing {
				if err := <-closed; err != nil {
					t.Fatal(err)
				}
			} else {
				if !b.ready.Load() {
					t.Fatal("confirmed owner unavailable")
				}
				ref, err := b.SealReference(ctx)
				if err != nil || ref.Position.Sequence != 1 {
					t.Fatal(ref, err)
				}
			}
			if closing && b.ready.Load() {
				t.Fatal("closed owner became ready")
			}
			if _, err := b.journal.AppendPreparedContext(ctx, []metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte("bad"), Value: []byte("v")}}, func(metadatajournal.Record, func() error) error {
				t.Fatal("Standalone prepared admission escaped")
				return nil
			}); err == nil {
				t.Fatal("Standalone append accepted")
			}

		})
	}
}
