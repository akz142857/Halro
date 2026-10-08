package app

import (
	"context"
	"errors"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"testing"
	"testing/synctest"
	"time"
)

func TestProjectedRecoveryQueueDeadlinePreservesNativeOwnerAndEnteredContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l, path, key := projectedWriterLog(t, nil, ledger.Options{MaxBatch: 1})
		defer l.Close()
		lease, p := projectedWriterCandidate(t, l, path, key)
		defer p.Close()
		defer lease.Close()
		r := &Runtime{ledger: l}
		r.draining.Store(true)
		w, err := r.newProjectedRecoveryWriter(context.Background(), lease, p)
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()
		w.gate <- struct{}{}
		if _, _, err := w.enter(context.Background()); !errors.Is(err, resourcebudget.ErrWorkQueueWait) {
			t.Fatal(err)
		}
		if _, err := lease.ValidateOwner(l); err != nil || w.failure != nil || l.Status().Load() != ledger.AccountingHealthy {
			t.Fatal("wait failure revoked native owner", err)
		}
		<-w.gate
		ctx, release, err := w.enter(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		time.Sleep(resourcebudget.MaxWorkQueueWait + time.Second)
		if ctx.Err() != nil || len(w.gate) != 1 {
			t.Fatal("entered recovery lost owner", ctx.Err())
		}
	})
}
