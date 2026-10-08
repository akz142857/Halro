package replication

import "github.com/akz142857/Halro/internal/metadatajournal"

// MetadataFormatVersion authenticates the actual native capability before
// advertising it. Format2 is enabled only by independent source qualification;
// a foreign/partial candidate cannot be advertised as an accepted journal.
func (s *NativeSource) MetadataFormatVersion() (uint16, error) {
	if _, err := s.Cursor(StoreMetadata); err != nil {
		return 0, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed || s.retirement.Load() != nil {
		return 0, errSourceRevisionChanged
	}
	if s.options.MetadataAuthority != nil || s.metadataBaselineActive {
		return 2, nil
	}
	return metadatajournal.ReplicationFormatVersion(), nil
}
