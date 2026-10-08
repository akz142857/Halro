package statecut

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/metadatastate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/replication"
)

// HAKeyRotationSource is an observation from one actual maintenance job. The
// independently selected common seal remains separate from current observed
// roots; neither copied value is a reusable role or source-opening capability.
type HAKeyRotationSource struct {
	Member         replication.MemberState
	Selected       authenticatedstate.CandidateSeal
	Observed       authenticatedstate.CandidateSeal
	BaselineDigest [32]byte
	Cursors        [4]replication.StoreCursor
	Heads          [4][32]byte
}

type HAKeyRotationSourceFailure struct {
	Owner Publication
	Cause error
}

func (e *HAKeyRotationSourceFailure) Error() string { return e.Cause.Error() }
func (e *HAKeyRotationSourceFailure) Unwrap() error { return e.Cause }

// CaptureHAKeyRotationSource uses the original exclusive native lease, actual
// Member/Ordering freezes, complete source qualification and seven real root
// owners. It does not publish anything, create a key candidate or enable DAO.
func CaptureHAKeyRotationSource(ctx context.Context, o HAOwners) (source HAKeyRotationSource, resultErr error) {
	if o.PromotionRecovery != nil || o.PeerPromotionRecovery != nil || o.Components.NativeLedger == nil {
		return source, replication.ErrMemberAuthority
	}
	var retirement Publication
	resultErr = o.Components.NativeLedger.WithHASelectedSealAuthority(ctx, func(ctx context.Context, a authenticatedstate.SealEligibility, _ ledger.HASelectedLedgerPosition) error {
		return withBorrowedHASelectedCut(ctx, o, func(ctx context.Context, cut HASelectedCut) error {
			m := cut.ActualMember
			if cut.Authority != a || m.Term != m.PromisedTerm || m.DurableIndex != m.ConfirmedIndex || m.ConfirmedIndex != m.AppliedIndex || cut.AllowedIndex != m.AppliedIndex || m.Projection.Index != m.AppliedIndex {
				return authenticatedstate.ErrSealIneligible
			}
			// The actual frozen Member/Ordering/native job excludes concurrent
			// mutation while finite authenticated transactions inspect every pin.
			// No copied root or caller-provided "no debt" flag replaces this scan.
			if err := verifyHAKeyRotationPins(ctx, o.Components.Metadata); err != nil {
				return err
			}
			authority, err := selectedHAMaintenanceAuthority(ctx, o, cut)
			if err != nil {
				return err
			}
			retirement, err = publishHAComponentsFrozen(ctx, o.Components, cut.Seal, authority, func(ctx context.Context, candidate authenticatedstate.CandidateSeal, _ func(context.Context) error) error {
				source = HAKeyRotationSource{Member: m, Selected: cut.Seal, Observed: candidate, BaselineDigest: o.Baseline.Descriptor().AuthorityDigest, Cursors: cut.Allowed, Heads: cut.Heads}
				return ctx.Err()
			})
			return err
		})
	})
	if resultErr != nil {
		source = HAKeyRotationSource{}
		if retirement.CleanupError() != nil || errors.Is(resultErr, ownedfile.ErrRetirement) {
			resultErr = &HAKeyRotationSourceFailure{Owner: retirement, Cause: resultErr}
		}
	}
	return
}

func verifyHAKeyRotationPins(ctx context.Context, owner *metadatastate.Baseline) error {
	var after []byte
	for {
		done := false
		err := owner.View(ctx, func(tx *metadatastate.Transaction) error {
			for range 32 {
				entry, found, err := tx.Seek([]string{"deployment_price_pin_intents"}, after, false, len(after) != 0)
				if err != nil {
					return err
				}
				if !found {
					done = true
					return nil
				}
				if entry.Bucket || len(entry.Key) > 32768 || len(entry.Key) == 0 || len(after) != 0 && bytes.Compare(entry.Key, after) <= 0 {
					return authenticatedstate.ErrCorrupt
				}
				var pin domain.PricePinIntent
				if err := json.Unmarshal(entry.Value, &pin); err != nil {
					return err
				}
				if pin.AttemptID != string(entry.Key) || pin.Validate() != nil {
					return authenticatedstate.ErrCorrupt
				}
				if pin.State == domain.PricePinPrepared {
					return errors.New("HA group key rotation requires Prepared price pins to recover under the original key")
				}
				after = bytes.Clone(entry.Key)
			}
			return nil
		})
		if err != nil || done {
			return err
		}
	}
}
