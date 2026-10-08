package usage

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
)

var ErrArchiveWindowProof = errors.New("Usage window requires a matching native archive proof for this owner and root")

// A complete native-row verifier (including an independently authenticated
// published date policy) or native-relative publisher can mint this witness. It is
// neither a persisted authenticated snapshot nor a native deletion permission.
type archiveWindowProof struct {
	owner     [16]byte
	position  ArchivePosition
	digest    [32]byte
	identity  os.FileInfo
	directory os.FileInfo
}

// RevokeWindowAuthority withdraws only process-local permissions. It never
// removes rows/files and cannot create a permission from a saved scalar.
func (e *Exporter) RevokeWindowAuthority() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.windowProof, e.windowPending = nil, nil
}

// BindVerifiedWindow transfers a fresh full-history archive comparison to the
// caller's live query owner, which may already have a trimmed window. Both
// atomic replay anchors authenticate against the independent native resolver;
// matching sequence numbers or derived hashes alone are insufficient.
func (e *Exporter) BindVerifiedWindow(ctx context.Context, reference, target *DiskProjection) (err error) {
	if reference == nil || target == nil || reference == target {
		return ErrArchiveWindowProof
	}
	if !e.mu.TryLock() {
		return ErrDiskUsageBusy
	}
	defer e.mu.Unlock()
	defer func() {
		err = errors.Join(err, ctx.Err())
		if reference.retired.Load() || target.retired.Load() || e.archiveRetired.Load() {
			err = errors.Join(err, ErrDiskUsageClosed)
		}
		if err != nil {
			e.windowProof, e.windowPending = nil, nil
		}
	}()
	proof := e.windowProof
	if proof == nil || proof.owner != reference.generation || !e.datePolicyReady || e.datePolicyPrefix == nil {
		return ErrArchiveWindowProof
	}
	if !reference.summaryMu.TryLock() {
		return ErrDiskUsageBusy
	}
	defer reference.summaryMu.Unlock()
	if !target.summaryMu.TryLock() {
		return ErrDiskUsageBusy
	}
	defer target.summaryMu.Unlock()
	stats, err := reference.ReadStats(ctx)
	if err != nil || stats.Window.AttemptAfter != 0 || stats.Window.SummaryAfter != 0 {
		return errors.Join(err, ErrArchiveWindowProof)
	}
	for _, p := range []*DiskProjection{reference, target} {
		anchor, err := p.ReadReplayAnchor(ctx)
		if err != nil || anchor.Head.Sequence < proof.position.LastSequence {
			return errors.Join(err, ErrArchiveWindowProof)
		}
		prefix, err := e.datePolicyPrefix(ctx, anchor.Head)
		if err != nil || prefix != (ledger.ProjectionPrefix{Head: anchor.Head, Hash: anchor.Hash}) {
			return errors.Join(err, ErrArchiveWindowProof)
		}
	}
	directory, err := os.Lstat(e.root)
	if err != nil {
		return err
	}
	root, err := openArchiveManifest(e.root)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.file.Close()) }()
	position, digest, err := root.walk(ctx, nil)
	if err != nil || !proof.matches(reference.generation, root, position, digest, directory) {
		return errors.Join(err, ErrArchiveWindowProof)
	}
	if err := root.check(); err != nil {
		return err
	}
	bound := *proof
	bound.owner = target.generation
	e.windowProof, e.windowPending = &bound, nil
	return nil
}

// PruneVerifiedWindowStep uses a native-relative complete/published-date/finite-publisher
// proof, never a caller-supplied sequence or a frontier from an unknown root.
// The caller owns cutoff policy and construction of the native-authenticated,
// role-allowed projection. A finite publisher may extend a matching witness;
// reopening or unknown roots never inherit it.
func (e *Exporter) PruneVerifiedWindowStep(ctx context.Context, p *DiskProjection, cutoff time.Time, limits DiskWindowLimits) (out DiskWindowProgress, err error) {
	return e.pruneVerifiedWindowStep(ctx, p, cutoff, limits, nil)
}

// PruneVerifiedWindowStepUnderMaintenance keeps the original native maintenance
// lease in the source loan. The lease does not replace the archive proof, and a
// foreign, closed or replicated lease cannot authorize a Standalone update.
func (e *Exporter) PruneVerifiedWindowStepUnderMaintenance(ctx context.Context, p *DiskProjection, lease *ledger.MaintenanceLease, cutoff time.Time, limits DiskWindowLimits) (DiskWindowProgress, error) {
	if lease == nil {
		return DiskWindowProgress{}, ledger.ErrLedgerMaintenance
	}
	return e.pruneVerifiedWindowStepWithLease(ctx, p, lease, cutoff, limits, nil)
}

func (e *Exporter) pruneVerifiedWindowStep(ctx context.Context, p *DiskProjection, cutoff time.Time, limits DiskWindowLimits, beforeMutation func(*archiveManifestInput) error) (DiskWindowProgress, error) {
	return e.pruneVerifiedWindowStepWithLease(ctx, p, nil, cutoff, limits, beforeMutation)
}

func (e *Exporter) pruneVerifiedWindowStepWithLease(ctx context.Context, p *DiskProjection, lease *ledger.MaintenanceLease, cutoff time.Time, limits DiskWindowLimits, beforeMutation func(*archiveManifestInput) error) (out DiskWindowProgress, err error) {
	if p == nil {
		return out, ErrArchiveWindowProof
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if !e.mu.TryLock() {
		return out, ErrDiskUsageBusy
	}
	defer e.mu.Unlock()
	if e.archiveRetired.Load() {
		return out, ErrDiskUsageClosed
	}
	proof := e.windowProof
	if proof == nil || proof.owner != p.generation {
		return out, ErrArchiveWindowProof
	}
	if p.retired.Load() || e.archiveRetired.Load() {
		return out, ErrDiskUsageClosed
	}
	if !p.summaryMu.TryLock() {
		return out, ErrDiskUsageBusy
	}
	defer p.summaryMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	defer func() {
		err = errors.Join(err, ctx.Err())
		if p.retired.Load() || e.archiveRetired.Load() {
			err = errors.Join(err, ErrDiskUsageClosed)
		}
		if err != nil {
			out = DiskWindowProgress{}
		}
	}()
	directory, statErr := os.Lstat(e.root)
	if statErr != nil || !directory.IsDir() || !os.SameFile(proof.directory, directory) {
		e.windowProof = nil
		return out, errors.Join(statErr, ErrArchiveWindowProof)
	}
	root, err := openArchiveManifest(e.root)
	if err != nil {
		e.windowProof = nil
		return out, err
	}
	defer func() {
		closeErr := root.file.Close()
		if closeErr != nil {
			e.windowProof = nil
		}
		err = errors.Join(err, closeErr)
		if err != nil {
			out = DiskWindowProgress{}
		}
	}()
	if !os.SameFile(proof.identity, root.info) || proof.identity.Size() != root.info.Size() || proof.identity.ModTime() != root.info.ModTime() {
		e.windowProof = nil
		return out, ErrArchiveWindowProof
	}
	position, digest, err := root.walk(ctx, nil)
	if err != nil {
		e.windowProof = nil
		return out, err
	}
	if position != proof.position || digest != proof.digest {
		e.windowProof = nil
		return out, ErrArchiveWindowProof
	}
	if beforeMutation != nil {
		if hookErr := beforeMutation(root); hookErr != nil {
			return out, hookErr
		}
	}
	out, err = p.pruneWindowStepHeldWithLease(ctx, lease, cutoff, proof.position.LastSequence, limits)
	// Root mutation after the commit withdraws success but cannot undo that
	// already committed derived step. The library mutex excludes its writers;
	// the caller also owns the data-directory exclusion against other processes.
	namedDirectory, directoryErr := os.Lstat(e.root)
	if directoryErr != nil || !namedDirectory.IsDir() || !os.SameFile(proof.directory, namedDirectory) {
		e.windowProof = nil
		err = errors.Join(err, directoryErr, ErrArchiveWindowProof)
	}
	if checkErr := root.check(); checkErr != nil {
		e.windowProof = nil
		err = errors.Join(err, checkErr)
	}
	return out, err
}

// A publication receipt records the bytes constructed by this publisher, never
// a frontier read from an unknown manifest. Recording after rename is tentative
// until every barrier and actual job cleanup returns successfully. A failed
// publication leaves at most one private receipt for an exact same-owner retry.
type archiveWindowPublication struct {
	owner     [16]byte
	directory os.FileInfo
	retention ArchiveDateRetention
	record    func(*archiveWindowProof) error
}

func (r *archiveWindowPublication) renamed(position ArchivePosition, digest [32]byte, identity os.FileInfo) error {
	if r == nil {
		return nil
	}
	// Appending cannot change the predicate or original prune baseline that
	// authorized omissions. A retained root needs a previously verified receipt;
	// merely observing its date/H does not manufacture one.
	if position.Retention != r.retention {
		return ErrArchiveWindowProof
	}
	return r.record(&archiveWindowProof{owner: r.owner, position: position, digest: digest, identity: identity, directory: r.directory})
}

func (r *archiveWindowPublication) checkDirectory(root string) error {
	if r == nil {
		return nil
	}
	directory, err := os.Lstat(root)
	if err != nil || !directory.IsDir() || !os.SameFile(r.directory, directory) {
		return errors.Join(err, ErrArchiveWindowProof)
	}
	return nil
}

func (r *archiveWindowProof) matches(owner [16]byte, root *archiveManifestInput, position ArchivePosition, digest [32]byte, directory os.FileInfo) bool {
	return r != nil && root != nil && r.owner == owner && r.position == position && r.digest == digest &&
		os.SameFile(r.directory, directory) && os.SameFile(r.identity, root.info) &&
		r.identity.Size() == root.info.Size() && r.identity.ModTime() == root.info.ModTime()
}
