package ledger

import "fmt"

// All point, recovery and pricing/backup reads use the reducer's canonical
// settlement representation. A decodable row is not proof of a valid charge.
func validateProjectedSettlement(row SettledAttempt, attemptID string) error {
	event := row.Settlement
	if event.Kind != EventAttemptSettled || event.AttemptID != attemptID || event.Validate() != nil {
		return ErrProjectionCorrupt
	}
	digest, err := eventDigest(event)
	if err != nil || row.SettlementDigest != "sha256:"+fmt.Sprintf("%x", digest) || row.CostKnown != (event.CommittedMicrosUSD != nil) {
		return ErrProjectionCorrupt
	}
	cost := int64(0)
	if event.CommittedMicrosUSD != nil {
		cost = *event.CommittedMicrosUSD
	}
	if row.CostMicrosUSD != cost {
		return ErrProjectionCorrupt
	}
	return nil
}

// The stored reservation hints must be derived from the ORIGINAL lease fact.
// Otherwise settling a smaller hint could leave a phantom reserved balance.
// Started changes independently when a real AttemptStarted fact is applied.
func validateProjectedReservation(row projectionReservation, attemptID string) error {
	event := row.Evidence.Event
	if event.Kind != EventReservationCreated || event.AttemptID != attemptID || event.Validate() != nil {
		return ErrProjectionCorrupt
	}
	expected, err := eventDigest(event)
	actual, leaseErr := eventDigest(row.Reservation.Lease)
	if err != nil || leaseErr != nil || actual != expected {
		return ErrProjectionCorrupt
	}
	amount := int64(0)
	if event.ReservationMicrosUSD != nil {
		amount = *event.ReservationMicrosUSD
	}
	key := BalanceKey{ProjectID: event.ProjectID, PeriodID: event.PeriodID, TimezoneVersion: event.PeriodTimezoneVersion}
	if row.Reservation.Amount != amount || row.Reservation.Key != key {
		return ErrProjectionCorrupt
	}
	return nil
}

// Record positions must fit both the logical sequence and the physical cut.
// Older generations may have larger offsets than the current generation.
// A roll boundary has offset zero, but a lease/settlement names a real record.
func validateProjectionRecordPosition(position, head Watermark) error {
	if position.Sequence == 0 || position.Sequence > head.Sequence || position.Generation == 0 || position.Generation > head.Generation || position.Offset <= 0 || position.Generation == head.Generation && position.Offset > head.Offset {
		return ErrProjectionCorrupt
	}
	return nil
}
