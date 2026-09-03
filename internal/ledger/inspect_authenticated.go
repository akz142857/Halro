package ledger

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// InspectReplayAuthenticated visits the complete history while authenticating
// epoch-4 frames, including sealed and compressed generations. Unlike Open it
// never repairs the WAL or a pending roll. Legacy frames remain checksum-only.
// Callers must discard the visited state on error or a partial tail, and check
// the returned head against their trusted checkpoint before persisting it.
func InspectReplayAuthenticated(path string, key []byte, visit func(Record) error) (ChainReport, bool, error) {
	if len(key) != 32 {
		return ChainReport{}, false, errors.New("authenticated replay requires a 32-byte ledger key")
	}
	directory := filepath.Dir(path)
	segments, _, err := resolveSegments(directory)
	if err != nil {
		return ChainReport{}, false, err
	}
	if err := checkSegmentsPresent(directory, segments); err != nil {
		return ChainReport{}, false, err
	}
	verifier := &chainVerifier{key: key}
	var report ChainReport
	consume := func(record Record) error {
		// scan checks epoch downgrades within a file; the verifier carries that
		// boundary across generations as well, including an empty active file.
		if record.Epoch != frameVersionLedgerIntegrity && verifier.sawFrames {
			return fmt.Errorf("%w: checksum-only frame follows an authenticated generation", ErrTampered)
		}
		if record.Epoch == frameVersionLedgerIntegrity {
			report.Authenticated++
		} else {
			report.ChecksumOnly++
		}
		if visit != nil {
			return visit(record)
		}
		return nil
	}
	var sequence uint64
	for _, segment := range segments {
		start, err := decodeChainHash(segment.StartHash)
		if err != nil {
			return ChainReport{}, false, err
		}
		if start != verifier.hash {
			return ChainReport{}, false, fmt.Errorf("%w: generation %d does not continue its predecessor", ErrTampered, segment.Generation)
		}
		reader, err := openSegment(directory, segment)
		if err != nil {
			return ChainReport{}, false, err
		}
		verifier.offset = 0
		head, partial, scanErr := scan(reader, segment.Generation, 0, sequence, consume, verifier)
		if err := errors.Join(scanErr, reader.Close()); err != nil {
			return ChainReport{}, false, err
		}
		if partial || head.Offset != segment.Length || head.Sequence != segment.LastSequence {
			return ChainReport{}, false, fmt.Errorf("%w: sealed generation %d does not match its recorded length and sequence", ErrCorrupt, segment.Generation)
		}
		end, err := decodeChainHash(segment.EndHash)
		if err != nil {
			return ChainReport{}, false, err
		}
		if verifier.hash != end {
			return ChainReport{}, false, fmt.Errorf("%w: sealed generation %d does not end at its recorded chain head", ErrTampered, segment.Generation)
		}
		sequence = head.Sequence
	}
	report.SealedGenerations = uint64(len(segments))
	report.SealedAuthenticated, report.Authenticated = report.Authenticated, 0
	file, err := os.Open(path)
	if err != nil {
		return ChainReport{}, false, err
	}
	// A just-rolled active generation has the same chain head at offset zero.
	verifier.offset = 0
	head, partial, scanErr := scan(file, activeGeneration(segments), 0, sequence, consume, verifier)
	if err := errors.Join(scanErr, file.Close()); err != nil {
		return ChainReport{}, false, err
	}
	report.Head = head
	report.ChainSequence, report.ChainOffset, report.ChainHash, report.ChainVerified =
		verifier.sequence, verifier.offset, verifier.hash, verifier.sawFrames
	return report, partial, nil
}
