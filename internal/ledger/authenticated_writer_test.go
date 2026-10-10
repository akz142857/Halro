package ledger

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func waitAuthenticatedApply(t *testing.T, p *AuthenticatedLedgerProjection, baseline resourcebudget.Cost) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if p.durableApply.Load() && p.budget.Snapshot().Used.RAM >= baseline.RAM+AuthenticatedLedgerSourceRAMBytes {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("accepted Apply did not retain its single slot/source workspace")
}

func TestAuthenticatedWriterWaitsForReaderAndNativeWithOneChargedSlot(t *testing.T) {
	for _, blocked := range []string{"reader", "native", "cancel-reader", "close-reader"} {
		t.Run(blocked, func(t *testing.T) {
			p, native, records := authenticatedAdvanceFixture(t, 0)
			records = appendAuthenticatedTail(t, native, records)
			cut := nativeRecordPrefix(records[3])
			baseline := p.budget.Snapshot().Used
			var unlock func()
			if blocked == "native" {
				native.mu.Lock()
				unlock = native.mu.Unlock
			} else {
				p.mu.RLock()
				unlock = p.mu.RUnlock
			}
			defer func() {
				if unlock != nil {
					unlock()
				}
			}()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				receipt, err := p.AdvanceOneFromNative(ctx, native, cut)
				if err == nil && (receipt.To.Head != cut.Head || receipt.To.Hash != cut.Hash || receipt.AppliedRecords != 1 || receipt.VerifiedRecords != 8) {
					err = errors.New("accepted Apply did not authenticate complete suffix/apply only its own fact")
				}
				if err != nil && receipt != (AuthenticatedLedgerAdvance{}) {
					err = errors.New("failed Apply returned a success receipt")
				}
				done <- err
			}()
			waitAuthenticatedApply(t, p, baseline)
			if _, err := p.AdvanceOneFromNative(context.Background(), native, cut); !errors.Is(err, authenticatedstate.ErrBusy) {
				t.Fatal("another durable update queued behind the accepted caller", err)
			}
			select {
			case err := <-done:
				t.Fatal("Apply did not join actual lock lifetime", err)
			default:
			}
			var closed chan error
			if blocked == "cancel-reader" {
				cancel()
			}
			if blocked == "close-reader" {
				closed = make(chan error, 1)
				go func() { closed <- p.Close() }()
				select {
				case <-p.closeContext.Done():
				case <-time.After(5 * time.Second):
					t.Fatal("Close did not revoke accepted update")
				}
				select {
				case err := <-closed:
					t.Fatal("Close returned before reader/Apply joined", err)
				default:
				}
			}
			unlock()
			unlock = nil
			select {
			case err := <-done:
				if blocked == "cancel-reader" && !errors.Is(err, context.Canceled) || blocked == "close-reader" && err == nil || blocked != "cancel-reader" && blocked != "close-reader" && err != nil {
					t.Fatal(blocked, err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("accepted update did not complete after actual reader/writer returned")
			}
			if closed != nil {
				if err := <-closed; err != nil {
					t.Fatal(err)
				}
			}
			want := baseline
			// A creator keeps its fixed namespace-cleanup credit until Discard.
			if used := p.budget.Snapshot().Used; used != want || p.durableApply.Load() {
				t.Fatal("accepted update leaked slot/workspace", used, want)
			}
			if blocked == "close-reader" {
				if err := p.Discard(); err != nil {
					t.Fatal(err)
				}
				if used := p.budget.Snapshot().Used; used != (resourcebudget.Cost{}) {
					t.Fatal("physical retirement retained fees", used)
				}
			}
			if blocked == "cancel-reader" {
				if _, err := p.AdvanceOneFromNative(context.Background(), native, cut); err != nil {
					t.Fatal("pre-reduction cancellation retired healthy candidate", err)
				}
			}
		})
	}
}

func TestAuthenticatedWriterRejectsSkippedDuplicateAndForgedCut(t *testing.T) {
	p, native, records := authenticatedAdvanceFixture(t, 0)
	records = appendAuthenticatedTail(t, native, records)
	cut := nativeRecordPrefix(records[3])
	wrong := cut
	wrong.Hash[0] ^= 1
	for _, bad := range []ProjectionPrefix{nativeRecordPrefix(records[4]), nativeRecordPrefix(records[2]), wrong} {
		if receipt, err := p.AdvanceOneFromNative(context.Background(), native, bad); !errors.Is(err, ErrProjectionCursor) || receipt != (AuthenticatedLedgerAdvance{}) {
			t.Fatal("invalid durable cut was applied", receipt, err)
		}
	}
	if _, err := p.AdvanceOneFromNative(context.Background(), native, cut); err != nil {
		t.Fatal("invalid caller poisoned valid next native fact", err)
	}
	if _, err := p.AdvanceOneFromNative(context.Background(), native, cut); !errors.Is(err, ErrProjectionCursor) {
		t.Fatal("duplicate semantic Apply was accepted", err)
	}
	compareAdvancedCandidate(t, p, native, cut)
}

func TestAuthenticatedWriterBindingRejectsEqualHeadOnDifferentSourceAndStaleCut(t *testing.T) {
	for _, mode := range []string{"matching", "foreign-path", "foreign-key", "stale-cut"} {
		t.Run(mode, func(t *testing.T) {
			p, native, _ := authenticatedAdvanceFixture(t, 0)
			target := native
			if mode == "foreign-path" {
				// Replay exactly the same native events into another file. Numeric
				// head/hash equality still cannot establish source ownership.
				target = openChained(t, filepath.Join(t.TempDir(), "other.wal"), nil)
				defer target.Close()
				for _, e := range append(projectionTestAttribution(), projectionTestLifecycle(1, true)[0]) {
					if _, err := target.Append(context.Background(), e); err != nil {
						t.Fatal(err)
					}
				}
				h, hash, _ := target.ChainHead()
				want, wantHash, _ := native.ChainHead()
				if h != want || hash != wantHash {
					t.Fatal("foreign fixture does not have equal native head/hash")
				}
			}
			if mode == "stale-cut" {
				appendAuthenticatedTail(t, native, nil)
			}
			lease, err := target.BeginMaintenance(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			if mode == "foreign-key" {
				target.mu.Lock()
				target.chainKey[0] ^= 1
				target.mu.Unlock()
				defer func() { target.mu.Lock(); target.chainKey[0] ^= 1; target.mu.Unlock() }()
			}
			err = p.ValidateNativeWriter(context.Background(), target, lease)
			if mode == "matching" {
				if err != nil || target.Status().Load() != AccountingHealthy {
					t.Fatal("matching exclusive native owner refused", err)
				}
			} else if err == nil || target.Status().Load() != AccountingRecoveryRequired {
				t.Fatal("mismatched native owner published", mode, err, target.Status().Load())
			}
		})
	}
}

// Writer validation only tries its locks. A reader holding the projection for
// a moment used to fail validation inside the maintenance head, which latched
// accounting recovery and failed a selected startup. It now waits that out and
// latches nothing.
func TestAuthenticatedWriterValidationWaitsOutAMomentarilyHeldLock(t *testing.T) {
	p, native, _ := authenticatedAdvanceFixture(t, 0)
	lease, err := native.BeginMaintenance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	p.mu.Lock()
	released := make(chan struct{})
	go func() {
		time.Sleep(50 * time.Millisecond)
		p.mu.Unlock()
		close(released)
	}()
	if err := p.ValidateNativeWriter(context.Background(), native, lease); err != nil {
		t.Fatal("validation refused a momentarily held lock", err)
	}
	<-released
	if native.Status().Load() != AccountingHealthy {
		t.Fatal("a held lock latched accounting recovery")
	}
}
