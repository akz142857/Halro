package authenticatedstate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"golang.org/x/sys/unix"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"

	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

const sealCandidateDiskCeiling = 2 * candidateSealMaxBytes // active plus one replacement.

var (
	ErrSealPublisher      = errors.New("authenticated seal publisher requires reopen")
	ErrSealFileRetirement = errors.New("authenticated seal actual file retirement is uncertain")
	ErrSealSequence       = errors.New("authenticated seal generation or revision is not a valid successor")
	sealPublishers        sync.Map // live publisher handles only; entries are removed on Close.
)

type SealPublicationReceipt struct {
	Generation uint64
	Revision   uint64
	Digest     [32]byte
}

// SealCandidatePublisher atomically replaces one bounded seal while requiring
// the caller's existing exclusive data-directory lock. It serializes durable
// publication only; callers must freeze and validate every source represented
// by the seal before calling Publish.
type SealCandidatePublisher struct {
	readOnly            bool
	mu                  sync.Mutex
	dir                 string
	path                string
	dirInfo             os.FileInfo
	activeInfo          os.FileInfo
	activeSeal          CandidateSeal
	activeBytes         [32]byte
	guard               *lock.Lock
	budget              *resourcebudget.Manager
	control             resourcebudget.Permit
	key                 []byte
	hard                SealHardLimits
	closed              bool
	transferred         bool
	retireSync          bool
	fault               error
	phase               func(string) error
	physicalUnqualified bool
	physicalCycle       *physicalSealCycle
	physicalTarget      *resourcebudget.PhysicalCandidate
	physicalTargetWork  resourcebudget.Permit
	physicalCurrentWork resourcebudget.Permit
	createPending       func(string) (*os.File, *ownedfile.Owner, error)
}

// ValidateOwner binds a multi-component publisher to the caller's actual
// process-wide directory exclusion and shared resource owner.
func (p *SealCandidatePublisher) ValidateOwner(guard *lock.Lock, budget *resourcebudget.Manager) error {
	if p == nil || guard == nil || budget == nil {
		return ErrSealIneligible
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.fault != nil || p.guard != guard || p.budget != budget {
		return errors.Join(ErrSealIneligible, p.fault)
	}
	return p.checkDirectory()
}

// OpenSealCandidatePublisher takes a persistent logical disk owner for the
// active seal and its one bounded replacement file. Pending cleanup accepts
// only an authenticated successor or the same-inode first-publication link.
func OpenSealCandidatePublisher(dir string, guard *lock.Lock, key []byte, hard SealHardLimits, budget *resourcebudget.Manager, reservation resourcebudget.Permit) (*SealCandidatePublisher, error) {
	return openSealCandidatePublisher(dir, guard, key, hard, budget, reservation, nil)
}

// OpenSealCandidateReader qualifies the same selected envelope and actual guard,
// while refusing pending cleanup, Publish and Retire. Close preserves all files.
func OpenSealCandidateReader(dir string, guard *lock.Lock, key []byte, hard SealHardLimits, budget *resourcebudget.Manager, reservation resourcebudget.Permit) (*SealCandidatePublisher, error) {
	return openSealCandidatePublisherPolicy(dir, guard, key, hard, budget, reservation, nil, true)
}

func openSealCandidatePublisher(dir string, guard *lock.Lock, key []byte, hard SealHardLimits, budget *resourcebudget.Manager, reservation resourcebudget.Permit, phase func(string) error) (*SealCandidatePublisher, error) {
	return openSealCandidatePublisherPolicy(dir, guard, key, hard, budget, reservation, phase, false)
}

func openSealCandidatePublisherPolicy(dir string, guard *lock.Lock, key []byte, hard SealHardLimits, budget *resourcebudget.Manager, reservation resourcebudget.Permit, phase func(string) error, readOnly bool) (*SealCandidatePublisher, error) {
	return openSealCandidatePublisherPrepared(dir, guard, key, hard, budget, reservation, phase, readOnly, nil, nil, false)
}

// OpenSealCandidatePublisherPrepared defers no arbitrary pending cleanup. The
// caller supplies the independently selected envelope and a receipt-qualified
// original target, whose exact existing pending name is checked before use.
func OpenSealCandidatePublisherPrepared(dir string, guard *lock.Lock, key []byte, hard SealHardLimits, budget *resourcebudget.Manager, reservation resourcebudget.Permit, selected CandidateSeal, target *resourcebudget.PhysicalCandidate) (*SealCandidatePublisher, error) {
	if target == nil {
		return nil, ErrSealIneligible
	}
	return openSealCandidatePublisherPrepared(dir, guard, key, hard, budget, reservation, nil, false, &selected, target, false)
}

func openSealCandidatePublisherPrepared(dir string, guard *lock.Lock, key []byte, hard SealHardLimits, budget *resourcebudget.Manager, reservation resourcebudget.Permit, phase func(string) error, readOnly bool, selected *CandidateSeal, target *resourcebudget.PhysicalCandidate, deferPending bool) (result *SealCandidatePublisher, resultErr error) {
	if guard == nil || !guard.CoversDataDirectory(dir) || len(key) != 32 || budget == nil || len(hard.RequiredRoots) == 0 {
		return nil, ErrLimit
	}
	dir = filepath.Clean(dir)
	dirInfo, err := os.Lstat(dir)
	if err != nil || !dirInfo.IsDir() {
		return nil, errors.Join(ErrCorrupt, err)
	}
	control, err := reservation.RetainCovering(budget, resourcebudget.Cost{Disk: sealCandidateDiskCeiling})
	if err != nil {
		return nil, err
	}
	p := &SealCandidatePublisher{dir: dir, path: filepath.Join(dir, "active.seal"), dirInfo: dirInfo, guard: guard, budget: budget, control: control, key: bytes.Clone(key), hard: cloneSealHardLimits(hard), phase: phase, readOnly: readOnly}
	if _, loaded := sealPublishers.LoadOrStore(dir, p); loaded {
		clear(p.key)
		return nil, errors.Join(ErrBusy, p.control.Release())
	}
	failed := true
	defer func() {
		if failed {
			sealPublishers.CompareAndDelete(dir, p)
			clear(p.key)
			if !errors.Is(resultErr, ErrSealFileRetirement) {
				resultErr = errors.Join(resultErr, p.control.Release())
			}
		}
	}()
	if err := p.checkDirectory(); err != nil {
		return nil, err
	}
	readPermit, err := budget.TryAcquire(resourcebudget.IO, resourcebudget.Cost{RAM: candidateSealMaxBytes})
	if err != nil {
		return nil, err
	}
	active, activeInfo, activeDigest, err := readSealFile(p.path, p.key, p.hard)
	defer func() {
		if !errors.Is(resultErr, ErrSealFileRetirement) {
			resultErr = errors.Join(resultErr, readPermit.Release())
		}
	}()
	if err == nil {
		p.activeSeal, p.activeInfo, p.activeBytes = active, activeInfo, activeDigest
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if target == nil && !deferPending {
		for i := 0; i < 2; i++ {
			if _, err := os.Lstat(physicalSealControlPath(dir, i)); err == nil {
				return nil, ErrRecovery
			} else if !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
		}
	}
	pendingPath := filepath.Join(dir, "active.seal.pending")
	pendingInfo, pendingErr := os.Lstat(pendingPath)
	if target != nil {
		if selected == nil || p.activeInfo == nil || p.activeSeal != *selected {
			return nil, ErrSealIneligible
		}
		if err := target.ValidatePrepared(pendingPath, candidateSealMaxBytes, budget); err != nil {
			return nil, err
		}
		p.physicalTarget = target
	}
	if deferPending {
		if selected == nil || p.activeInfo == nil || p.activeSeal != *selected {
			return nil, ErrSealIneligible
		}
		p.physicalUnqualified = true
	}
	if pendingErr == nil && target == nil && !deferPending {
		if readOnly {
			return nil, ErrRecovery
		}
		if p.activeInfo == nil || !os.SameFile(p.activeInfo, pendingInfo) {
			// A synced, but unpublished, authenticated successor is a recoverable
			// crash artifact. The active name remains the sole commit point. Never
			// promote pending; discard it only after proving the complete envelope
			// and successor relationship. Partial, foreign and stale files survive.
			pending, verifiedInfo, _, err := readSealFile(pendingPath, p.key, p.hard)
			if err != nil || !os.SameFile(pendingInfo, verifiedInfo) {
				return nil, errors.Join(ErrCorrupt, ownedfile.ErrReplaced, err)
			}
			if err := p.checkSuccessor(pending); err != nil {
				return nil, errors.Join(ErrCorrupt, err)
			}
		}
		if err := p.checkActive(); err != nil {
			return nil, err
		}
		currentPending, err := os.Lstat(pendingPath)
		if err != nil || !os.SameFile(pendingInfo, currentPending) {
			return nil, errors.Join(ownedfile.ErrReplaced, err)
		}
		if err := os.Remove(pendingPath); err != nil {
			return nil, err
		}
		if err := durable.SyncDirectory(dir); err != nil {
			return nil, err
		}
	} else if pendingErr != nil && !errors.Is(pendingErr, os.ErrNotExist) {
		return nil, pendingErr
	}
	if err := p.checkDirectory(); err != nil {
		return nil, err
	}
	failed = false
	return p, nil
}

func cloneSealHardLimits(h SealHardLimits) SealHardLimits {
	h.RequiredRoots = append([]SealRootSchema(nil), h.RequiredRoots...)
	return h
}

func readSealFile(path string, key []byte, hard SealHardLimits) (CandidateSeal, os.FileInfo, [32]byte, error) {
	return readSealFileWithOpen(path, key, hard, func(name string) (*os.File, error) {
		return os.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	})
}
func readSealFileWithOpen(path string, key []byte, hard SealHardLimits, open func(string) (*os.File, error)) (seal CandidateSeal, info os.FileInfo, digest [32]byte, resultErr error) {
	named, err := os.Lstat(path)
	if err != nil {
		return seal, nil, digest, err
	}
	if !named.Mode().IsRegular() || named.Size() < int64(candidateSealMACBytes+10) || named.Size() > candidateSealMaxBytes {
		return seal, nil, digest, ErrCorrupt
	}
	f, err := open(path)
	if err != nil {
		return seal, nil, digest, err
	}
	defer func() {
		if err := f.Close(); err != nil {
			resultErr = errors.Join(resultErr, ErrSealFileRetirement, err)
		}
		if resultErr != nil {
			seal = CandidateSeal{}
			info = nil
			digest = [32]byte{}
		}
	}()
	actual, err := f.Stat()
	if err != nil || !actual.Mode().IsRegular() || !os.SameFile(named, actual) || actual.Size() != named.Size() || !actual.ModTime().Equal(named.ModTime()) {
		return seal, nil, digest, errors.Join(ErrCorrupt, err)
	}
	raw := make([]byte, int(actual.Size()))
	if _, err := io.ReadFull(f, raw); err != nil {
		return seal, nil, digest, errors.Join(ErrCorrupt, err)
	}
	final, err := f.Stat()
	current, nameErr := os.Lstat(path)
	if err != nil || nameErr != nil || !os.SameFile(actual, final) || !os.SameFile(actual, current) || final.Size() != actual.Size() || current.Size() != actual.Size() || !final.ModTime().Equal(actual.ModTime()) || !current.ModTime().Equal(actual.ModTime()) {
		return seal, nil, digest, errors.Join(ErrCorrupt, ownedfile.ErrReplaced, err, nameErr)
	}
	seal, err = DecodeSealCandidate(raw, key, hard)
	if err != nil {
		return CandidateSeal{}, nil, digest, err
	}
	return seal, actual, sha256.Sum256(raw), nil
}

func (p *SealCandidatePublisher) checkDirectory() error {
	if p == nil || p.guard == nil || !p.guard.CoversDataDirectory(p.dir) {
		return ownedfile.ErrReplaced
	}
	info, err := os.Lstat(p.dir)
	if err != nil || !os.SameFile(p.dirInfo, info) || !info.IsDir() {
		return errors.Join(ownedfile.ErrReplaced, err)
	}
	return nil
}

func (p *SealCandidatePublisher) checkActive() error {
	if err := p.checkDirectory(); err != nil {
		return err
	}
	if p.activeInfo == nil {
		if _, err := os.Lstat(p.path); errors.Is(err, os.ErrNotExist) {
			return nil
		} else {
			return errors.Join(ownedfile.ErrReplaced, err)
		}
	}
	_, info, digest, err := readSealFile(p.path, p.key, p.hard)
	if err != nil || !os.SameFile(p.activeInfo, info) || digest != p.activeBytes {
		return errors.Join(ErrCorrupt, ownedfile.ErrReplaced, err)
	}
	return nil
}

func sameSealPublisherIdentity(a, b CandidateSeal) bool {
	return a.Instance == b.Instance && a.Cluster == b.Cluster && a.Member == b.Member && a.Incarnation == b.Incarnation && a.Role == b.Role && a.Term == b.Term
}

func (p *SealCandidatePublisher) checkSuccessor(next CandidateSeal) error {
	if p.activeInfo == nil {
		if next.Revision != 1 {
			return ErrSealSequence
		}
		return nil
	}
	old := p.activeSeal
	if sameSealPublisherIdentity(old, next) {
		if next.Generation < old.Generation {
			return ErrSealSequence
		}
		if next.Generation == old.Generation {
			if old.Revision == math.MaxUint64 || next.Revision != old.Revision+1 {
				return ErrSealSequence
			}
		} else if next.Revision != 1 {
			return ErrSealSequence
		}
	} else if next.Generation <= old.Generation || next.Revision != 1 {
		return ErrSealSequence
	}
	return nil
}

// Publish writes and syncs the candidate before replacing the active name. It
// publishes no partial seal; post-rename uncertainty poisons this handle until
// reopen. Callers must not use a failed receipt as a business-ready signal.
func (p *SealCandidatePublisher) Publish(ctx context.Context, next CandidateSeal) (SealPublicationReceipt, error) {
	return p.publishPrepared(ctx, next, nil, false)
}

// PublishPrepared consumes an already-created pending name and real allocated
// target from the actual publisher owner. It grants no role or root authority;
// the same successor checks, actual Close, rename and directory barrier apply.
func (p *SealCandidatePublisher) PublishPrepared(ctx context.Context, next CandidateSeal, target *resourcebudget.PhysicalCandidate) (SealPublicationReceipt, error) {
	if target == nil {
		return SealPublicationReceipt{}, resourcebudget.ErrPhysicalAllocation
	}
	return p.publishPrepared(ctx, next, target, false)
}

// PublishPreparedExchange keeps the superseded actual root as the next pending
// target. Both original names and allocated inodes survive the commit point;
// a concurrent foreign writer never gets those blocks as free space.
func (p *SealCandidatePublisher) PublishPreparedExchange(ctx context.Context, next CandidateSeal, target *resourcebudget.PhysicalCandidate) (SealPublicationReceipt, error) {
	if target == nil {
		return SealPublicationReceipt{}, resourcebudget.ErrPhysicalAllocation
	}
	return p.publishPrepared(ctx, next, target, true)
}

func (p *SealCandidatePublisher) publishPrepared(ctx context.Context, next CandidateSeal, target *resourcebudget.PhysicalCandidate, exchange bool) (receipt SealPublicationReceipt, resultErr error) {
	if p == nil || !p.mu.TryLock() {
		return SealPublicationReceipt{}, ErrBusy
	}
	defer p.mu.Unlock()
	if p.readOnly {
		return SealPublicationReceipt{}, ErrSealPublisher
	}
	if target == nil {
		target = p.physicalTarget
	}
	if p.physicalCycle != nil {
		exchange = true
	}
	if target != nil && p.activeInfo == nil {
		return SealPublicationReceipt{}, ErrSealIneligible
	}
	if p.closed || p.fault != nil {
		return SealPublicationReceipt{}, errors.Join(ErrSealPublisher, p.fault)
	}
	if err := ctx.Err(); err != nil {
		return SealPublicationReceipt{}, err
	}
	permit, err := p.budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: candidateSealMaxBytes})
	if err != nil {
		return SealPublicationReceipt{}, err
	}
	defer func() {
		if errors.Is(resultErr, ErrSealFileRetirement) {
			p.fault = errors.Join(p.fault, resultErr)
			receipt = SealPublicationReceipt{}
			return
		}
		if releaseErr := permit.Release(); releaseErr != nil {
			p.fault = errors.Join(p.fault, releaseErr)
			resultErr = errors.Join(resultErr, releaseErr)
			receipt = SealPublicationReceipt{}
		}
	}()
	if err := p.checkActive(); err != nil {
		p.fault = err
		return SealPublicationReceipt{}, err
	}
	if err := p.checkSuccessor(next); err != nil {
		return SealPublicationReceipt{}, err
	}
	if exchange {
		if err := p.checkPhysicalCurrentRoot(); err != nil {
			return SealPublicationReceipt{}, err
		}
	}
	raw, err := EncodeSealCandidate(next, p.key, p.hard)
	if err != nil {
		return SealPublicationReceipt{}, err
	}
	if p.physicalCycle != nil {
		p.physicalCycle.claims.To = sha256.Sum256(raw)
		if err := p.savePhysicalSealClaims(); err != nil {
			p.fault = errors.Join(ErrSealFileRetirement, err)
			return SealPublicationReceipt{}, p.fault
		}
		if err := p.phaseResult("physical-seal-intent-synced"); err != nil {
			p.fault = errors.Join(ErrSealFileRetirement, err)
			return SealPublicationReceipt{}, p.fault
		}
	}
	tempPath := filepath.Join(p.dir, "active.seal.pending")
	create := p.createPending
	if target != nil {
		if err := target.ValidatePrepared(tempPath, candidateSealMaxBytes, p.budget); err != nil {
			return SealPublicationReceipt{}, err
		}
		create = func(name string) (*os.File, *ownedfile.Owner, error) {
			return target.ClaimForWrite(name, candidateSealMaxBytes, int64(len(raw)))
		}
	}
	if create == nil {
		create = func(name string) (*os.File, *ownedfile.Owner, error) { return ownedfile.Create(name, os.O_RDWR, 0600) }
	}
	f, owner, err := create(tempPath)
	if err != nil {
		if errors.Is(err, ownedfile.ErrRetirement) {
			err = errors.Join(ErrSealFileRetirement, err)
		}
		return SealPublicationReceipt{}, err
	}
	closeFile := f.Close
	if target != nil {
		closeFile = target.CloseFile
	}
	closed, uncertain := false, false
	defer func() {
		if !closed {
			closed = true
			if err := closeFile(); err != nil {
				uncertain = true
				resultErr = errors.Join(resultErr, ErrSealFileRetirement, err)
			}
		}
		if uncertain {
			p.fault = errors.Join(p.fault, resultErr)
			receipt = SealPublicationReceipt{}
			return
		}
		if target != nil && resultErr != nil {
			resultErr = errors.Join(ErrSealFileRetirement, resultErr)
			p.fault = errors.Join(p.fault, resultErr)
			return
		}
		if owner != nil {
			err := p.checkDirectory()
			if err == nil {
				err = owner.Retire()
			}
			if err == nil {
				err = durable.SyncDirectory(p.dir)
			}
			if err != nil {
				resultErr = errors.Join(resultErr, ErrSealFileRetirement, err)
				p.fault = errors.Join(p.fault, resultErr)
				receipt = SealPublicationReceipt{}
			}
		}
	}()
	n, err := f.Write(raw)
	if err != nil || n != len(raw) {
		return SealPublicationReceipt{}, errors.Join(err, io.ErrShortWrite)
	}
	if err := f.Sync(); err != nil {
		return SealPublicationReceipt{}, err
	}
	if p.phase != nil {
		if err := p.phase("seal-file-synced"); err != nil {
			return SealPublicationReceipt{}, err
		}
	}
	info, err := f.Stat()
	if err != nil {
		return SealPublicationReceipt{}, err
	}
	if err := owner.VerifyFile(f); err != nil {
		return SealPublicationReceipt{}, err
	}
	if err := closeFile(); err != nil {
		closed, uncertain = true, true
		return SealPublicationReceipt{}, errors.Join(ErrSealFileRetirement, err)
	}
	closed = true
	if err := ctx.Err(); err != nil {
		return SealPublicationReceipt{}, err
	}
	if err := p.checkActive(); err != nil {
		p.fault = err
		return SealPublicationReceipt{}, err
	}
	var renameErr error
	if p.activeInfo == nil {
		alias, linkErr := owner.LinkCaptured(p.path)
		renameErr = linkErr
		if alias != nil {
			p.activeInfo, p.activeSeal, p.activeBytes = alias.Identity(), next, sha256.Sum256(raw)
			// Link occurred. Preserve that published fact through every later error.
			if renameErr != nil {
				p.fault = renameErr
				return SealPublicationReceipt{}, renameErr
			}
			if err := p.phaseResult("seal-linked"); err != nil {
				p.fault = err
				return SealPublicationReceipt{}, err
			}
			if err := owner.Retire(); err != nil {
				p.fault = errors.Join(ErrSealFileRetirement, err)
				owner = nil
				return SealPublicationReceipt{}, p.fault
			}
			owner = nil
		}
	} else {
		if exchange {
			var previous *ownedfile.Owner
			previous, renameErr = owner.ExchangeCaptured(p.path, p.activeInfo)
			if renameErr == nil && previous == nil {
				renameErr = ownedfile.ErrRetirement
			}
		} else {
			renameErr = owner.ReplaceCaptured(p.path, p.activeInfo)
		}
		if renameErr == nil {
			p.activeInfo, p.activeSeal, p.activeBytes = info, next, sha256.Sum256(raw)
			owner = nil // creation receipt now owns active.seal
		}
	}
	if renameErr != nil {
		return SealPublicationReceipt{}, renameErr
	}
	if p.phase != nil {
		if err := p.phase("seal-renamed"); err != nil {
			p.fault = err
			return SealPublicationReceipt{}, err
		}
	}
	if err := durable.SyncDirectory(p.dir); err != nil {
		p.fault = err
		return SealPublicationReceipt{}, err
	}
	if err := p.phaseResult("seal-directory-synced"); err != nil {
		p.fault = err
		return SealPublicationReceipt{}, err
	}
	if target != nil {
		if err := target.Published(p.budget, p.control); err != nil {
			p.fault = errors.Join(ErrSealFileRetirement, err)
			return SealPublicationReceipt{}, p.fault
		}
		p.physicalTarget = nil
		if p.physicalCycle != nil {
			r := p.physicalCycle
			r.claims.Current, r.claims.Pending = r.claims.Pending, r.claims.Current
			r.claims.From, r.claims.To = p.activeBytes, [32]byte{}
			if err := p.savePhysicalSealClaims(); err != nil {
				p.fault = errors.Join(ErrSealFileRetirement, err)
				return SealPublicationReceipt{}, p.fault
			}
			if err := p.phaseResult("physical-seal-renewed"); err != nil {
				p.fault = errors.Join(ErrSealFileRetirement, err)
				return SealPublicationReceipt{}, p.fault
			}
			var err error
			p.physicalTarget, err = resourcebudget.RestorePhysicalCandidateWithOwnedWorkspace(ctx, filepath.Join(p.dir, "active.seal.pending"), r.claims.Pending, p.budget, p.control, p.physicalTargetWork)
			if err != nil {
				p.fault = errors.Join(ErrSealFileRetirement, err)
				return SealPublicationReceipt{}, p.fault
			}
		}
	}
	return SealPublicationReceipt{Generation: next.Generation, Revision: next.Revision, Digest: sha256.Sum256(raw)}, nil
}

func (p *SealCandidatePublisher) phaseResult(name string) error {
	if p.phase == nil {
		return nil
	}
	return p.phase(name)
}

// Current returns the authenticated active candidate without granting role or
// readiness authority. Concurrent source audits serialize their actual reads;
// each waiting caller owns a finite IO claim before entering the lock. A busy
// immutable reader is not evidence that the independent common seal vanished.
func (p *SealCandidatePublisher) Current() (seal CandidateSeal, found bool, resultErr error) {
	return p.CurrentContext(context.Background())
}

// CurrentContext borrows only the caller's existing same-Manager workspace.
// The authenticated actual publisher still supplies selection authority.
// Ordinary Current keeps normal admission and does not activate a continuation.
func (p *SealCandidatePublisher) CurrentContext(ctx context.Context) (seal CandidateSeal, found bool, resultErr error) {
	if p == nil {
		return CandidateSeal{}, false, ErrBusy
	}
	permit, err := p.budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: candidateSealMaxBytes})
	if err != nil {
		return CandidateSeal{}, false, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	defer func() {
		if errors.Is(resultErr, ErrSealFileRetirement) {
			p.fault = errors.Join(p.fault, resultErr)
			seal, found = CandidateSeal{}, false
			return
		}
		if releaseErr := permit.Release(); releaseErr != nil {
			p.fault = errors.Join(p.fault, releaseErr)
			resultErr = errors.Join(resultErr, releaseErr)
			seal, found = CandidateSeal{}, false
		}
	}()
	if p.closed || p.fault != nil {
		return CandidateSeal{}, false, errors.Join(ErrSealPublisher, p.fault)
	}
	if err := p.checkActive(); err != nil {
		p.fault = err
		return CandidateSeal{}, false, err
	}
	return p.activeSeal, p.activeInfo != nil, nil
}

// Select returns an active seal only when it matches independently supplied
// startup authority. A mismatch is never treated as absence or as permission to
// use a caller-selected older root.
func (p *SealCandidatePublisher) Select(expected SealEligibility) (CandidateSeal, bool, error) {
	return p.SelectContext(context.Background(), expected)
}

// SelectContext preserves the complete independently supplied eligibility
// policy while using the caller's existing authenticated publisher IO loan.
// Owning a workspace does not qualify a role, root, cut or incarnation.
func (p *SealCandidatePublisher) SelectContext(ctx context.Context, expected SealEligibility) (CandidateSeal, bool, error) {
	seal, found, err := p.CurrentContext(ctx)
	if err != nil || !found {
		return CandidateSeal{}, found, err
	}
	if err := ValidateSealEligibility(seal, expected); err != nil {
		return CandidateSeal{}, false, err
	}
	return seal, true, nil
}

// SelectedReceipt describes an already durable, independently permitted seal.
// It does not publish or broaden the caller's role/cut permission.
func (p *SealCandidatePublisher) SelectedReceipt(expected SealEligibility) (SealPublicationReceipt, error) {
	seal, found, err := p.Select(expected)
	if err != nil || !found {
		return SealPublicationReceipt{}, errors.Join(ErrSealIneligible, err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.fault != nil || p.activeSeal != seal {
		return SealPublicationReceipt{}, ErrSealIneligible
	}
	return SealPublicationReceipt{Generation: seal.Generation, Revision: seal.Revision, Digest: p.activeBytes}, nil
}

func (p *SealCandidatePublisher) Close() error {
	if p == nil || !p.mu.TryLock() {
		return ErrBusy
	}
	defer p.mu.Unlock()
	if p.closed {
		return p.fault
	}
	p.closed = true
	if err := p.physicalCycle.close(); err != nil {
		p.fault = errors.Join(p.fault, ErrSealFileRetirement, err)
	}
	targetErr := p.physicalTarget.ClosePreserving()
	if targetErr != nil {
		p.fault = errors.Join(p.fault, ErrSealFileRetirement, targetErr)
	} else if p.physicalTargetWork != (resourcebudget.Permit{}) {
		p.fault = errors.Join(p.fault, p.physicalTargetWork.Release())
		p.physicalTargetWork = resourcebudget.Permit{}
	}
	if !errors.Is(p.fault, ownedfile.ErrRetirement) {
		clear(p.key)
	}
	sealPublishers.CompareAndDelete(p.dir, p)
	return p.fault
}

// DiskReservation transfers the persistent disk charge to the caller after
// Close; the charge remains until Retire removes the active seal.
func (p *SealCandidatePublisher) DiskReservation() (resourcebudget.Permit, error) {
	if p == nil || !p.mu.TryLock() {
		return resourcebudget.Permit{}, ErrBusy
	}
	defer p.mu.Unlock()
	if !p.closed || p.transferred || p.control == (resourcebudget.Permit{}) {
		return resourcebudget.Permit{}, ErrBusy
	}
	retained, err := p.control.RetainCovering(p.budget, resourcebudget.Cost{Disk: sealCandidateDiskCeiling})
	if err != nil {
		return resourcebudget.Permit{}, err
	}
	if err := p.control.Release(); err != nil {
		_ = retained.Release()
		return resourcebudget.Permit{}, err
	}
	p.control = resourcebudget.Permit{}
	p.transferred = true
	return retained, nil
}

// Retire is allowed only after Close and only while the caller still owns the
// directory lock. It removes the captured active inode before releasing Disk.
func (p *SealCandidatePublisher) Retire() error {
	if p == nil || !p.mu.TryLock() {
		return ErrBusy
	}
	defer p.mu.Unlock()
	if p.readOnly {
		return ErrSealPublisher
	}
	if errors.Is(p.fault, ErrSealFileRetirement) {
		return p.fault
	}
	if !p.closed || p.control == (resourcebudget.Permit{}) {
		return ErrBusy
	}
	if err := p.checkDirectory(); err != nil {
		return err
	}
	// A leftover pending name may be a foreign inode or the second link from
	// an interrupted first publication. Do not release the persistent charge
	// while any such name remains; Open is the only path that can authenticate
	// and reconcile the same-inode recovery case.
	if _, err := os.Lstat(filepath.Join(p.dir, "active.seal.pending")); err == nil {
		return errors.Join(ErrCorrupt, ownedfile.ErrReplaced)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if p.activeInfo != nil {
		current, err := os.Lstat(p.path)
		if err != nil || !os.SameFile(p.activeInfo, current) {
			return errors.Join(ownedfile.ErrReplaced, err)
		}
		if err := os.Remove(p.path); err != nil {
			return err
		}
		p.activeInfo = nil
		p.retireSync = true
	}
	if p.retireSync {
		if err := durable.SyncDirectory(p.dir); err != nil {
			return err
		}
		p.retireSync = false
	}
	if err := p.control.Release(); err != nil {
		return err
	}
	p.control = resourcebudget.Permit{}
	return nil
}
