package ledger

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/ownedfile"
)

// InspectReplayAuthenticated visits the complete history while authenticating
// epoch-4/5 frames, including sealed and compressed generations. Unlike Open it
// never repairs the WAL or a pending roll. Legacy frames remain checksum-only.
// Callers must discard the visited state on error or a partial tail, and check
// the returned head against their trusted checkpoint before persisting it.
func InspectReplayAuthenticated(path string, key []byte, visit func(Record) error) (ChainReport, bool, error) {
	return InspectReplayAuthenticatedContext(context.Background(), path, key, visit)
}

// InspectReplayAuthenticatedContext preserves the complete read-only source
// contract and checks caller cancellation during directory/record walks.
func InspectReplayAuthenticatedContext(ctx context.Context, path string, key []byte, visit func(Record) error) (ChainReport, bool, error) {
	return inspectReplayAuthenticated(ctx, path, key, visit, nil)
}

// boundary is visited only for a fully checked file. It exposes physical roll
// positions without inventing a semantic event for an empty generation.
func inspectReplayAuthenticated(ctx context.Context, path string, key []byte, visit func(Record) error, boundary func(Watermark, [32]byte) error) (result ChainReport, isPartial bool, resultErr error) {
	return inspectReplayAuthenticatedWithClose(ctx, path, key, visit, boundary, ownedfile.CloseFile)
}

// closeActive is a private actual-descriptor lifecycle seam, never a caller
// supplied authority or a substitute for reading and authenticating the file.
func inspectReplayAuthenticatedWithClose(ctx context.Context, path string, key []byte, visit func(Record) error, boundary func(Watermark, [32]byte) error, closeActive func(*os.File) error) (result ChainReport, isPartial bool, resultErr error) {
	if err := ctx.Err(); err != nil {
		return ChainReport{}, false, err
	}
	if len(key) != 32 {
		return ChainReport{}, false, errors.New("authenticated replay requires a 32-byte ledger key")
	}
	directory := filepath.Dir(path)
	manifest, err := openInspectionManifest(directory)
	if err != nil {
		return ChainReport{}, false, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, manifest.close(), ctx.Err())
		if resultErr != nil {
			result, isPartial = ChainReport{}, false
		}
	}()
	// Validate complete directory syntax/continuity/presence before any records
	// reach visit. Retain neither the raw manifest nor its segment descriptors.
	layout, digest, err := manifest.walk(func(segment Segment) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return checkSegmentPresent(directory, segment)
	})
	if err != nil {
		return ChainReport{}, false, err
	}

	verifier := &chainVerifier{key: key}
	var report ChainReport
	consume := func(record Record) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		// scan checks epoch downgrades within a file; the verifier carries that
		// boundary across generations as well, including an empty active file.
		if !authenticatedFrameEpoch(record.Epoch) && verifier.sawFrames {
			return fmt.Errorf("%w: checksum-only frame follows an authenticated generation", ErrTampered)
		}
		if authenticatedFrameEpoch(record.Epoch) {
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
	replayLayout, replayDigest, err := manifest.walk(func(segment Segment) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		start, err := decodeChainHash(segment.StartHash)
		if err != nil {
			return err
		}
		if start != verifier.hash {
			return fmt.Errorf("%w: generation %d does not continue its predecessor", ErrTampered, segment.Generation)
		}
		reader, err := openSegment(directory, segment)
		if err != nil {
			return err
		}
		verifier.offset = 0
		head, partial, scanErr := scan(reader, segment.Generation, 0, sequence, consume, verifier)
		closeErr := reader.Close()
		if closeErr != nil {
			closeErr = errors.Join(ErrProjectionSource, closeErr)
		}
		if err := errors.Join(scanErr, closeErr); err != nil {
			return err
		}
		if partial || head.Offset != segment.Length || head.Sequence != segment.LastSequence {
			return fmt.Errorf("%w: sealed generation %d does not match its recorded length and sequence", ErrCorrupt, segment.Generation)
		}
		end, err := decodeChainHash(segment.EndHash)
		if err != nil {
			return err
		}
		if verifier.hash != end {
			return fmt.Errorf("%w: sealed generation %d does not end at its recorded chain head", ErrTampered, segment.Generation)
		}
		if verifier.epoch == frameVersionRunAttribution && segment.EndEpoch != uint8(frameVersionRunAttribution) || segment.EndEpoch != 0 && byte(segment.EndEpoch) != verifier.epoch {
			return fmt.Errorf("%w: sealed generation %d ends at epoch %d, manifest says %d", ErrCorrupt, segment.Generation, verifier.epoch, segment.EndEpoch)
		}
		if boundary != nil {
			if err := boundary(Watermark{Generation: segment.Generation, Sequence: sequence}, start); err != nil {
				return visitError{err}
			}
		}
		sequence = head.Sequence
		return nil
	})
	if err != nil {
		return ChainReport{}, false, err
	}
	if replayDigest != digest || replayLayout.active != layout.active || replayLayout.pending != layout.pending || replayLayout.sealed != layout.sealed {
		return ChainReport{}, false, fmt.Errorf("%w: manifest changed between validation and replay", ErrCorrupt)
	}
	if err := manifest.check(); err != nil {
		return ChainReport{}, false, err
	}
	report.SealedGenerations = layout.active - 1
	report.SealedAuthenticated, report.Authenticated = report.Authenticated, 0
	file, err := os.Open(path)
	if err != nil {
		return ChainReport{}, false, err
	}
	// A just-rolled active generation has the same chain head at offset zero.
	verifier.offset = 0
	activeStart, activeSequence := verifier.hash, sequence
	head, partial, scanErr := scan(file, layout.active, 0, sequence, consume, verifier)
	closeErr := ownedfile.CloseError(file, closeActive(file))
	if closeErr != nil {
		closeErr = errors.Join(ErrProjectionSource, closeErr)
	}
	if err := errors.Join(scanErr, closeErr); err != nil {
		return ChainReport{}, false, err
	}
	if err := manifest.check(); err != nil {
		return ChainReport{}, false, err
	}
	if boundary != nil && !partial {
		if err := boundary(Watermark{Generation: layout.active, Sequence: activeSequence}, activeStart); err != nil {
			return ChainReport{}, false, visitError{err}
		}
	}
	report.Head = head
	report.ChainSequence, report.ChainOffset, report.ChainHash, report.ChainVerified =
		verifier.sequence, verifier.offset, verifier.hash, verifier.sawFrames
	return report, partial, nil
}
