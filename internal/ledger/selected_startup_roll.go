package ledger

import (
	"errors"
)

// verifyRolledSuffix continues the independently selected anchor across whole
// immutable generations, before the active file can be repaired or published.
// It retains one reader and frame decoder. Old descriptors were compared with
// the selected directory; new descriptors become evidence only after their
// actual frames authenticate the contiguous selected chain.
func (s *selectedNativeStartup) verifyRolledSuffix(d *segmentDirectoryIndex, directory string, generation uint64, verifier *chainVerifier) error {
	for g := s.anchor.Head.Generation; g < generation; g++ {
		if err := s.ctx.Err(); err != nil {
			return err
		}
		segment, found, err := d.Get(g)
		if err != nil || !found {
			return errors.Join(ErrSegmentMissing, err)
		}
		offset := int64(0)
		if g == s.anchor.Head.Generation {
			offset = s.anchor.Head.Offset
		}
		if offset > segment.Length {
			return ErrProjectionSource
		}
		if g == s.anchor.Head.Generation {
			first, start := uint64(1), [32]byte{}
			if g > 1 {
				previous, found, err := d.Get(g - 1)
				if err != nil || !found {
					return errors.Join(ErrSegmentMissing, err)
				}
				first = previous.LastSequence + 1
				start, err = decodeChainHash(previous.EndHash)
				if err != nil {
					return err
				}
			}
			actualStart, err := decodeChainHash(segment.StartHash)
			if err != nil || first == 0 || segment.FirstSequence != first || actualStart != start {
				return errors.Join(ErrTampered, err)
			}
		}
		if offset == 0 {
			start, err := decodeChainHash(segment.StartHash)
			if err != nil || start != verifier.hash || segment.FirstSequence != verifier.sequence+1 {
				return errors.Join(ErrTampered, err)
			}
		}
		if err := s.verifyRolledDescriptor(directory, segment); err != nil {
			return err
		}
		reader, err := openSegment(directory, segment)
		if err != nil {
			return err
		}
		verifier.offset = offset
		head, partial, scanErr := scan(reader, g, offset, verifier.sequence, func(Record) error {
			s.decoded++
			return s.ctx.Err()
		}, verifier)
		closeErr := reader.Close()
		if closeErr != nil {
			s.retainedSegment = reader
			s.retirementErr = errors.Join(s.retirementErr, ErrSegmentRetirement, closeErr)
		}
		if err := errors.Join(scanErr, closeErr); err != nil {
			return err
		}
		end, err := decodeChainHash(segment.EndHash)
		if err != nil || partial || head.Offset != segment.Length || head.Sequence != segment.LastSequence || end != verifier.hash || segmentEndEpoch(segment) != verifier.epoch {
			return errors.Join(ErrTampered, err)
		}
	}
	return errors.Join(d.manifest.check(), s.ctx.Err())
}
