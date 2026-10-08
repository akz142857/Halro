package authenticatedstate

// NativeFrameRecoveryDebt reports only this actual owner's latched mutation or
// reader-integrity failure. A nil result grants no root, role, readiness, repair,
// deletion or resource-refund authority. A serialized domain writer may use it
// after a rejected Update to distinguish a known prewrite refusal from an
// actual partial write that the owner already poisoned.
func (s *Store) NativeFrameRecoveryDebt() error {
	if s == nil {
		return ErrClosed
	}
	return s.fault()
}
