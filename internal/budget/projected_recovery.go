package budget

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/akz142857/Halro/internal/ledger"
)

type RecoveryRole string

const (
	RecoveryStandalone RecoveryRole = "standalone"
	RecoveryPrimary    RecoveryRole = "primary"
)

// RecoveryWriter belongs to an exclusive maintenance runtime with business
// admission closed. AppendApplyRecovery must authorize the current role/term,
// persist the fact and ordering evidence, then apply it to this projection in
// sequence before returning. An apply failure must poison further writes.
// ConfirmRecoveryPrefix must bind the supplied Ledger head to the full native
// ordering target (including earlier non-Ledger facts), and wait for the role's
// confirmation evidence. Standalone verifies local durability/application.
// Ordinary per-event WaitConfirmed is not an implementation of this contract.
// No callback is called with a DB reader held. Runtime wiring is not supplied
// by this candidate; it must also establish D-03 trust and resource owners.
type RecoveryWriter interface {
	AppendApplyRecovery(context.Context, ledger.Event) (ledger.Record, error)
	ConfirmRecoveryPrefix(context.Context, ledger.Watermark) error
}

type ProjectedRecoveryResult struct {
	InputHead         ledger.Watermark
	ConfirmedHead     ledger.Watermark
	AttemptsSettled   uint64
	RequestsFinalized uint64
}

// RecoverProjectedRequests resolves both independent input sets, using bounded
// pages and deterministic facts. It never admits a new request or calls a
// provider. Success proves this algorithm's writer contract, not publication,
// readiness, Usage/Governance catch-up or production HA acceptance.
func RecoverProjectedRequests(ctx context.Context, p *ledger.LedgerProjection, writer RecoveryWriter, role RecoveryRole, limits ledger.ProjectionPageLimits) (ProjectedRecoveryResult, error) {
	var result ProjectedRecoveryResult
	if p == nil || writer == nil || (role != RecoveryStandalone && role != RecoveryPrimary) || limits.Rows <= 0 || limits.Rows > 64 || limits.Bytes <= 0 || limits.Bytes > 8<<20 {
		return result, errors.New("invalid projected recovery role, writer or page limits")
	}
	scan, err := p.BeginRecoveryScan(ctx)
	if err != nil {
		return result, err
	}
	result.InputHead = scan.Head
	confirm := func() error {
		head, _, err := p.ReadPosition(ctx)
		if err != nil {
			return err
		}
		if err := writer.ConfirmRecoveryPrefix(ctx, head); err != nil {
			return fmt.Errorf("confirm recovery prefix through %d: %w", head.Sequence, err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		result.ConfirmedHead = head
		return nil
	}
	// Existing unconfirmed facts must be confirmable before new recovery facts
	// consume the bounded unconfirmed window or termination reserve.
	if err := confirm(); err != nil {
		return result, err
	}
	for cursor := scan.Pending; ; {
		page, err := p.PendingLeasePage(ctx, cursor, limits)
		if err != nil {
			return result, err
		}
		for _, pending := range page.Items {
			parent, found, err := p.ReadRequest(ctx, pending.Reservation.RequestID)
			if err != nil || !found {
				return result, errors.Join(err, errors.New("pending attempt has no accepted request"))
			}
			if err := validateRecoveryParent(parent, pending.Reservation); err != nil {
				return result, err
			}
			attempt, settlement, eventID, err := recoverySettlement(pending)
			if err != nil {
				return result, err
			}
			event, err := buildSettlementEvent(eventID, attempt, settlement, pending.Reservation.OccurredAt)
			if err != nil {
				return result, err
			}
			// Legacy leases may omit these bounds. Preserve the durable fields
			// rather than serializing Go's zero Time as a negative timestamp.
			event.PeriodStartMicros = pending.Reservation.PeriodStartMicros
			event.PeriodEndMicros = pending.Reservation.PeriodEndMicros
			if err := appendProjectedRecovery(ctx, p, writer, event); err != nil {
				return result, err
			}
			result.AttemptsSettled++
		}
		if len(page.Items) > 0 {
			if err := confirm(); err != nil {
				return result, err
			}
		}
		cursor = page.Next
		if page.Done {
			break
		}
	}
	for cursor := scan.Requests; ; {
		page, err := p.UnfinalizedRequestPage(ctx, cursor, limits)
		if err != nil {
			return result, err
		}
		for _, request := range page.Items {
			origin, err := request.RecoveryOrigin()
			if err != nil {
				return result, err
			}
			if err := validateRecoveryParent(request, origin.Event); err != nil {
				return result, err
			}
			// Enumerate associations independently of the global pending set.
			// Zero attempts is a valid accepted-before-lease crash boundary.
			for attempts := (ledger.ProjectionCursor{}); ; {
				associated, err := p.RequestAttemptPage(ctx, origin.Event.RequestID, attempts, limits)
				if err != nil {
					return result, err
				}
				for _, attempt := range associated.Items {
					if attempt.Pending || attempt.Settlement == nil {
						return result, errors.New("cannot finalize request with unresolved attempt")
					}
					if err := validateRecoveryParent(request, attempt.Lease.Event); err != nil {
						return result, err
					}
					if err := validateRecoveryParent(request, attempt.Settlement.Settlement); err != nil {
						return result, err
					}
				}
				attempts = associated.Next
				if associated.Done {
					break
				}
			}
			event := recoveryFinalization(origin.Event)
			if err := appendProjectedRecovery(ctx, p, writer, event); err != nil {
				return result, err
			}
			result.RequestsFinalized++
		}
		if len(page.Items) > 0 {
			if err := confirm(); err != nil {
				return result, err
			}
		}
		cursor = page.Next
		if page.Done {
			break
		}
	}
	// Includes finalized facts even if an earlier invocation applied them but
	// failed while waiting for confirmation. An empty input cannot bypass it.
	if err := confirm(); err != nil {
		return result, err
	}
	return result, nil
}

func validateRecoveryParent(parent ledger.ProjectedRequest, event ledger.Event) error {
	if parent.LegacyLease != nil {
		origin, err := parent.RecoveryOrigin()
		// Legacy State did not enforce Accepted's KeyID/model/period-bound
		// contract across a request. Each settlement still uses and validates its
		// own REAL lease; legacy origin never grants Run/WorkUnit attribution.
		if err != nil || origin.Event.RequestID != event.RequestID || event.WorkUnitID != "" || event.RunID != "" {
			return errors.New("legacy recovery conflicts with original un-attributed lease")
		}
		if parent.Finalized != nil && (parent.Finalized.Sequence == 0 || parent.Finalized.Event.Kind != ledger.EventRequestFinalized || parent.Finalized.Event.RequestID != event.RequestID || parent.Finalized.Event.WorkUnitID != "" || parent.Finalized.Event.RunID != "") {
			return errors.New("invalid original legacy recovery finalization")
		}
		return nil
	}
	a := parent.Accepted.Event
	if parent.Accepted.Sequence == 0 || a.Kind != ledger.EventRequestAccepted || a.RequestID != event.RequestID || a.ProjectID != event.ProjectID || a.WorkUnitID != event.WorkUnitID || a.RunID != event.RunID || a.KeyID != event.KeyID || a.RequestedModel != event.RequestedModel || a.PeriodID != event.PeriodID || a.PeriodTimezone != event.PeriodTimezone || a.PeriodTimezoneVersion != event.PeriodTimezoneVersion || a.PeriodStartMicros != event.PeriodStartMicros || a.PeriodEndMicros != event.PeriodEndMicros {
		return errors.New("recovery event conflicts with accepted request attribution")
	}
	if parent.Finalized != nil {
		f := parent.Finalized
		if f.Sequence <= parent.Accepted.Sequence || f.Event.Kind != ledger.EventRequestFinalized {
			return errors.New("invalid original recovery finalization")
		}
		copy := parent
		copy.Finalized = nil
		return validateRecoveryParent(copy, f.Event)
	}
	return nil
}

func recoveryFinalization(accepted ledger.Event) ledger.Event {
	digest := sha256.Sum256([]byte("halro:request-recovery:v1\x00" + accepted.RequestID))
	return ledger.Event{
		EventID: "evt_request_recovery_" + hex.EncodeToString(digest[:]), Kind: ledger.EventRequestFinalized,
		RequestID: accepted.RequestID, ProjectID: accepted.ProjectID, WorkUnitID: accepted.WorkUnitID, RunID: accepted.RunID,
		KeyID: accepted.KeyID, RequestedModel: accepted.RequestedModel,
		PeriodID: accepted.PeriodID, PeriodTimezone: accepted.PeriodTimezone, PeriodTimezoneVersion: accepted.PeriodTimezoneVersion,
		PeriodStartMicros: accepted.PeriodStartMicros, PeriodEndMicros: accepted.PeriodEndMicros,
		// This is the stable recovery origin, not an observed upstream finish.
		OccurredAt: accepted.OccurredAt, Outcome: "recovered_interrupted", FailurePhase: "accounting", Ambiguous: true,
	}
}

type recoveryPositionReader interface {
	ReadPosition(context.Context) (ledger.Watermark, [32]byte, error)
}

func appendProjectedRecovery(ctx context.Context, p recoveryPositionReader, writer RecoveryWriter, event ledger.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	before, _, err := p.ReadPosition(ctx)
	if err != nil {
		return err
	}
	record, err := writer.AppendApplyRecovery(ctx, event)
	if err != nil {
		return fmt.Errorf("append/apply recovery %q: %w", event.EventID, err)
	}
	// The callback cannot turn an unapplied or unrelated fact into success.
	want, err := json.Marshal(event)
	if err != nil {
		return err
	}
	got, err := json.Marshal(record.Event)
	if err != nil {
		return err
	}
	head, hash, err := p.ReadPosition(ctx)
	if err != nil {
		return err
	}
	if !bytes.Equal(want, got) || before.Sequence == ^uint64(0) || record.Sequence != before.Sequence+1 || head != (ledger.Watermark{Generation: record.Generation, Offset: record.Offset, Sequence: record.Sequence}) || hash != record.Hash {
		return errors.New("recovery writer did not apply the expected next fact")
	}
	return nil
}
