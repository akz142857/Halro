package replication

import "errors"

// ValidateActivatedMetadataBaselineOrdering authenticates the signed old MAC
// prefix and its actual journal N+1 boundary after installation. The existing
// append validator still requires exact head N before accepting a new boundary.
// This verifier neither appends nor repairs, and requires activation already
// established by the original journal-aware source protocol.
func (s *NativeSource) ValidateActivatedMetadataBaselineOrdering(journal *OrderingJournal) error {
	if s == nil || journal == nil {
		return ErrMemberAuthority
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	plan := s.options.MetadataBaseline
	if s.closed || s.retirement.Load() != nil || plan == nil || !s.metadataBaselineActive {
		return ErrMemberAuthority
	}
	if err := plan.Authority.validateOldCut(journal); err != nil {
		return err
	}
	head, _, _ := journal.Head()
	if head < plan.Authority.claims.Index+1 {
		return ErrMemberAuthority
	}
	record, err := journal.Record(plan.Authority.claims.Index + 1)
	if err != nil {
		return err
	}
	if err := plan.Authority.validateRecord(record); err != nil {
		return err
	}
	if err := s.validateMetadataBaselineLocked(record); err != nil {
		return errors.Join(ErrMemberAuthority, err)
	}
	return nil
}
