package budget

import (
	"context"
	"errors"
	"fmt"

	"github.com/akz142857/Halro/internal/ledger"
)

// RecoverAuthenticatedRequests freezes the two original input sets at one MAC
// root. Current authenticated settlements establish resolution, while original
// lease/request facts establish attribution. No history-sized progress map is
// retained. Writer permission/confirmation and Runtime publication are separate
// contracts; the caller retains exclusive maintenance and joins this whole job.
func RecoverAuthenticatedRequests(ctx context.Context, p *ledger.AuthenticatedLedgerProjection, writer RecoveryWriter, role RecoveryRole, limits ledger.ProjectionPageLimits) (result ProjectedRecoveryResult, resultErr error) {
	if p == nil || writer == nil || (role != RecoveryStandalone && role != RecoveryPrimary) || limits.Rows <= 0 || limits.Rows > 64 || limits.Bytes <= 0 || limits.Bytes > 8<<20 {
		return result, errors.New("invalid authenticated recovery role, writer or page limits")
	}
	resultErr = p.WithRecoveryView(ctx, func(v ledger.AuthenticatedRecoveryView) error {
		result.InputHead = v.Head().Head
		confirm := func() error {
			head, _, err := p.ReadPosition(ctx)
			if err != nil {
				return err
			}
			if err := writer.ConfirmRecoveryPrefix(ctx, head); err != nil {
				return fmt.Errorf("confirm authenticated recovery prefix through %d: %w", head.Sequence, err)
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			result.ConfirmedHead = head
			return nil
		}
		if err := confirm(); err != nil {
			return err
		}
		for cursor := ""; ; {
			page, err := v.PendingLeasePage(ctx, cursor, limits)
			if err != nil {
				return err
			}
			for _, pending := range page.Items {
				parent, found, err := v.ReadRequest(ctx, pending.Reservation.RequestID)
				if err != nil || !found {
					return errors.Join(err, errors.New("original pending attempt has no original request"))
				}
				if err := validateRecoveryParent(parent, pending.Reservation); err != nil {
					return err
				}
				attempt, settlement, eventID, err := recoverySettlement(pending)
				if err != nil {
					return err
				}
				event, err := buildSettlementEvent(eventID, attempt, settlement, pending.Reservation.OccurredAt)
				if err != nil {
					return err
				}
				event.PeriodStartMicros, event.PeriodEndMicros = pending.Reservation.PeriodStartMicros, pending.Reservation.PeriodEndMicros
				if err := appendProjectedRecovery(ctx, p, writer, event); err != nil {
					return err
				}
				result.AttemptsSettled++
			}
			if len(page.Items) > 0 {
				if err := confirm(); err != nil {
					return err
				}
			}
			if page.Done {
				break
			}
			cursor = page.Next
		}
		for cursor := ""; ; {
			page, err := v.UnfinalizedRequestPage(ctx, cursor, limits)
			if err != nil {
				return err
			}
			for _, parent := range page.Items {
				origin, err := parent.RecoveryOrigin()
				if err != nil {
					return err
				}
				if err := validateRecoveryParent(parent, origin.Event); err != nil {
					return err
				}
				for attempts := ""; ; {
					associated, err := v.RequestAttemptPage(ctx, origin.Event.RequestID, attempts, limits)
					if err != nil {
						return err
					}
					for _, input := range associated.Items {
						if err := validateRecoveryParent(parent, input.Lease.Event); err != nil {
							return err
						}
						// The frozen input remains Pending after live Apply. Require
						// actual current terminal evidence, not its old pending flag
						// or an in-memory set of previously processed attempt IDs.
						settled, found, err := p.ReadSettlement(ctx, input.Lease.Event.AttemptID)
						if err != nil || !found {
							return errors.Join(err, errors.New("cannot finalize request with unresolved authenticated attempt"))
						}
						if err := validateRecoveryParent(parent, settled.Settlement); err != nil {
							return err
						}
						if input.Settlement != nil && input.Settlement.SettlementDigest != settled.SettlementDigest {
							return errors.New("original settlement changed during authenticated recovery")
						}
					}
					if associated.Done {
						break
					}
					attempts = associated.Next
				}
				if err := appendProjectedRecovery(ctx, p, writer, recoveryFinalization(origin.Event)); err != nil {
					return err
				}
				result.RequestsFinalized++
			}
			if len(page.Items) > 0 {
				if err := confirm(); err != nil {
					return err
				}
			}
			if page.Done {
				break
			}
			cursor = page.Next
		}
		if err := v.Complete(ctx); err != nil {
			return err
		}
		// Even an empty retry must confirm the latest physical native prefix.
		return confirm()
	})
	return result, resultErr
}
