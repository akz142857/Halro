package authenticatedstate

// physicalHeadroomExhaustedLocked is only a bounded capacity signal. Exact
// root/pack/control and independent common selection checks still precede
// retirement and copying. No logical fee or unused tail increases the ceiling.
func (s *Store) physicalHeadroomExhaustedLocked() bool {
	p := s.physical
	if p == nil || p.maxObservedTerminalCount <= 0 || p.maxObservedTerminalCount > maxPhysicalTerminalSlots || p.debt != 0 || p.windows != 0 || p.failure != nil || s.appendLimit <= 0 || s.ceiling <= 0 || s.physicalExtent > uint64(s.ceiling) || p.allocatedEnd < int64(s.physicalExtent) || p.allocatedEnd > s.ceiling {
		return false
	}
	remaining := s.ceiling - int64(s.physicalExtent)
	// Division avoids multiplication overflow. One ordinary update must stay
	// above every original terminal obligation.
	return remaining/s.appendLimit < p.maxObservedTerminalCount+1
}
