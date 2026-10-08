package ledger

import "reflect"

func validateProjectedLease(record Record, attemptID string, head Watermark) error {
	if record.Event.Kind != EventReservationCreated || record.Event.AttemptID != attemptID || record.Event.Validate() != nil || validateProjectionRecordPosition(Watermark{record.Generation, record.Offset, record.Sequence}, head) != nil {
		return ErrProjectionCorrupt
	}
	return nil
}

// Validate every original fact against one read/transaction cut. Legacy
// journals may start with a lease or finalization and accept the request later;
// their individual attempt periods must not be replaced by modern attribution.
func validateProjectedRequest(row ProjectedRequest, requestID string, head Watermark) error {
	accepted := row.Accepted
	if accepted.Sequence == 0 {
		if !reflect.ValueOf(accepted).IsZero() {
			return ErrProjectionCorrupt
		}
	} else if accepted.Event.Kind != EventRequestAccepted || accepted.Event.RequestID != requestID || accepted.Event.Validate() != nil || validateProjectionRecordPosition(Watermark{accepted.Generation, accepted.Offset, accepted.Sequence}, head) != nil {
		return ErrProjectionCorrupt
	}
	if row.LegacyLease != nil {
		lease := *row.LegacyLease
		if validateProjectedLease(lease, lease.Event.AttemptID, head) != nil || lease.Event.RequestID != requestID || lease.Event.WorkUnitID != "" || lease.Event.RunID != "" {
			return ErrProjectionCorrupt
		}
	}
	if accepted.Sequence == 0 && row.LegacyLease == nil && row.Finalized == nil {
		return ErrProjectionCorrupt
	}
	legacy := row.Legacy()
	if legacy && (accepted.Event.WorkUnitID != "" || accepted.Event.RunID != "") {
		return ErrProjectionCorrupt
	}
	if row.Finalized != nil {
		final := *row.Finalized
		if final.Event.Kind != EventRequestFinalized || final.Event.RequestID != requestID || final.Event.Validate() != nil || validateProjectionRecordPosition(Watermark{final.Generation, final.Offset, final.Sequence}, head) != nil {
			return ErrProjectionCorrupt
		}
		if legacy {
			if final.Event.WorkUnitID != "" || final.Event.RunID != "" {
				return ErrProjectionCorrupt
			}
		} else if final.Sequence <= accepted.Sequence || final.Generation < accepted.Generation || final.Generation == accepted.Generation && final.Offset <= accepted.Offset || final.Event.ProjectID != accepted.Event.ProjectID || final.Event.PeriodID != accepted.Event.PeriodID || final.Event.PeriodTimezoneVersion != accepted.Event.PeriodTimezoneVersion || final.Event.WorkUnitID != accepted.Event.WorkUnitID || final.Event.RunID != accepted.Event.RunID {
			return ErrProjectionCorrupt
		}
	}
	return nil
}
