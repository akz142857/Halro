package ledger

import (
	"errors"
	"fmt"
	"path/filepath"
)

// A signed plaintext descriptor can qualify its canonical compressed physical
// representation. No logical fact is taken from the replacement manifest:
// generation, sequence, epoch, chain bounds, plaintext length and SHA all stay
// exactly equal to the selected descriptor. The actual stored and decoded SHA
// are checked before any active-tail repair. Recompression and decompression
// of an already selected representation need a separate publication protocol.
func (s *selectedNativeStartup) verifySelectedCompression(saved, actual Segment) error {
	if saved.Compressed || !actual.Compressed || saved.File != fmt.Sprintf("ledger-%d.wal", saved.Generation) || actual.File != fmt.Sprintf("ledger-%d.seg.gz", saved.Generation) || actual.StoredLength <= 0 || actual.StoredChecksum == "" {
		return ErrProjectionSource
	}
	logical := actual
	logical.File, logical.Compressed = saved.File, saved.Compressed
	logical.StoredLength, logical.StoredChecksum = saved.StoredLength, saved.StoredChecksum
	if logical != saved {
		return ErrProjectionSource
	}
	if err := s.checkSelection(); err != nil {
		return err
	}
	return errors.Join(s.verifyRolledDescriptor(filepath.Dir(s.nativePath), actual), s.checkSelection())
}
