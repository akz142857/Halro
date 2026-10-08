package authenticatedstate

import (
	"context"
	"errors"
	"os"
	"syscall"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// PrepareCurrentPhysicalRoot qualifies the actual already-selected Standalone
// envelope as the future inactive target. Its independently selected identity,
// existing publisher guard and fixed disk owner must already be qualified by
// the caller; this function grants no source or role authority. It never changes
// encoded bytes or EOF. Both current and future target remain allocated across
// the later captured exchange, rather than freeing the old inode to a foreign
// writer before rebuilding a vector.
func (p *SealCandidatePublisher) PrepareCurrentPhysicalRoot(ctx context.Context, expected CandidateSeal) (reference resourcebudget.PhysicalCandidateReference, resultErr error) {
	return p.prepareCurrentQualifiedPhysicalRoot(ctx, expected, false)
}

func (p *SealCandidatePublisher) prepareCurrentQualifiedPhysicalRoot(ctx context.Context, expected CandidateSeal, ha bool) (reference resourcebudget.PhysicalCandidateReference, resultErr error) {
	if p == nil {
		return reference, ErrSealIneligible
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	validRole := !ha && expected.Role == SealStandalone || ha && (expected.Role == SealPrimary || expected.Role == SealReplica)
	if p.closed || p.readOnly || p.fault != nil || p.activeInfo == nil || p.activeSeal != expected || !validRole {
		return reference, errors.Join(ErrSealIneligible, p.fault)
	}
	if err := errors.Join(ctx.Err(), p.checkActive()); err != nil {
		return reference, err
	}
	work, err := p.budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: 4096})
	if err != nil {
		return reference, err
	}
	file, err := os.OpenFile(p.path, os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return reference, errors.Join(err, work.Release())
	}
	defer func() {
		closeErr := ownedfile.CloseFile(file)
		resultErr = errors.Join(resultErr, closeErr)
		if errors.Is(resultErr, resourcebudget.ErrPhysicalAllocationUncertain) || errors.Is(resultErr, ownedfile.ErrRetirement) {
			p.physicalCurrentWork = work
			p.fault = errors.Join(p.fault, ErrSealFileRetirement, resultErr)
		} else {
			resultErr = errors.Join(resultErr, work.Release())
		}
	}()
	encoded, encodeErr := EncodeSealCandidate(p.activeSeal, p.key, p.hard)
	actual, err := file.Stat()
	if err != nil || encodeErr != nil || !os.SameFile(actual, p.activeInfo) || actual.Size() != int64(len(encoded)) {
		return reference, errors.Join(ownedfile.ErrReplaced, err, encodeErr)
	}
	if err := resourcebudget.AllocatePhysicalRange(file, 0, candidateSealMaxBytes); err != nil {
		return reference, err
	}
	if err := resourcebudget.CheckPhysicalRange(file, 0, candidateSealMaxBytes); err != nil {
		return reference, err
	}
	if err := resourcebudget.CheckPhysicalTail(file, actual.Size(), candidateSealMaxBytes); err != nil {
		return reference, err
	}
	reference.File, err = resourcebudget.CapturePhysicalIdentity(file)
	if err != nil {
		return reference, err
	}
	reference.Parent, _, err = physicalPathIdentity(p.dir)
	if err != nil {
		return reference, err
	}
	reference.Bytes = candidateSealMaxBytes
	return reference, p.checkActive()
}

func (p *SealCandidatePublisher) checkPhysicalCurrentRoot() (resultErr error) {
	file, err := os.Open(p.path)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, ownedfile.CloseFile(file)) }()
	encoded, encodeErr := EncodeSealCandidate(p.activeSeal, p.key, p.hard)
	actual, err := file.Stat()
	if err != nil || encodeErr != nil || !os.SameFile(actual, p.activeInfo) || actual.Size() != int64(len(encoded)) {
		return errors.Join(ownedfile.ErrReplaced, err, encodeErr)
	}
	return errors.Join(resourcebudget.CheckPhysicalRange(file, 0, candidateSealMaxBytes), resourcebudget.CheckPhysicalTail(file, actual.Size(), candidateSealMaxBytes))
}
