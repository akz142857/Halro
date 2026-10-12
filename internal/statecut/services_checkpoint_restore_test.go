package statecut

import (
	"context"
	"errors"
	"testing"

	"github.com/akz142857/Halro/internal/ledger"
)

// Board §40.4/§40.6: a live checkpoint that fails after Usage was bound to its
// maintenance lease restores Usage before returning. When that restore fails
// too (E1 met it under pressure at real ENOSPC), the live Usage owner is left
// on a closed lease and only a restart rebinds it, so accounting must latch
// rather than keep reporting healthy. A successful restore must not latch.
func TestSelectedLiveCheckpointLatchesWhenUsageRestoreFails(t *testing.T) {
	for _, restoreFails := range []bool{false, true} {
		name := "restored"
		if restoreFails {
			name = "restore-failed"
		}
		t.Run(name, func(t *testing.T) {
			f := newCommonFixture(t)
			o := prepareCommonSelectedReopen(t, f)
			ctx := context.Background()
			r, err := ReopenSelectedStandalone(ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { retireSelectedFixture(t, f, r) })
			if _, err := r.StartSelectedServices(ctx, SelectedServicesOptions{Authority: o.Authority, Timezone: "UTC", QueueCapacity: 4}); err != nil {
				t.Fatal(err)
			}
			advanceErr, restoreErr := errors.New("injected directory advance failure"), errors.New("injected Usage restore failure")
			r.checkpointPhase = func(at string) error {
				switch {
				case at == "directory-advance":
					return advanceErr
				case at == "usage-restore" && restoreFails:
					return restoreErr
				}
				return nil
			}
			caches := persistStartupCachesAt(t, r.Owners, o.Authority.Instance, f.dir, "restore")
			prepare := func(context.Context) (StartupCacheCandidates, error) { return caches, nil }
			_, err = r.CheckpointLiveSelectedServices(ctx, prepare)
			r.checkpointPhase = nil
			if !errors.Is(err, advanceErr) || errors.Is(err, restoreErr) != restoreFails {
				t.Fatal("checkpoint did not report the injected failures", err)
			}
			status := r.Owners.NativeLedger.Status().Load()
			if !restoreFails {
				if status != ledger.AccountingHealthy {
					t.Fatal("restored Usage latched accounting", status, err)
				}
				if _, err := r.CheckpointLiveSelectedServices(ctx, prepare); err != nil {
					t.Fatal("checkpoint after a restored failure", err)
				}
				return
			}
			if status != ledger.AccountingRecoveryRequired {
				t.Fatal("failed Usage restore left accounting unlatched", status, err)
			}
		})
	}
}
