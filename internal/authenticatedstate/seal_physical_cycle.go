package authenticatedstate

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

const physicalSealReceiptBytes = 32 << 10

// PhysicalSealDiskBytes covers both retained publication targets and both
// bounded receipt banks. It is a logical ceiling; Enable separately verifies
// the real target ranges before lending either inode.
const PhysicalSealDiskBytes = sealCandidateDiskCeiling + 2*physicalSealReceiptBytes + 4*(8<<10+2*physicalSealReceiptBytes+candidateSealMaxBytes+4*(1<<20))

type physicalSealClaims struct {
	Serial           uint64
	From, To         [32]byte
	Current, Pending resourcebudget.PhysicalCandidateReference
	Controls         [2]resourcebudget.PhysicalIdentity
	Caches           [4]resourcebudget.PhysicalCandidateReference
}
type physicalSealCycle struct {
	claims    physicalSealClaims
	files     [2]*os.File
	owners    [2]*ownedfile.Owner
	bank      int
	fee, work resourcebudget.Permit
	closed    bool
	closeErr  error
}

func physicalSealControlPath(dir string, bank int) string {
	return filepath.Join(dir, fmt.Sprintf("terminal.seal.%d", bank))
}
func encodePhysicalSealClaims(c physicalSealClaims, key []byte) ([]byte, error) {
	var body bytes.Buffer
	if err := binary.Write(&body, binary.BigEndian, c); err != nil {
		return nil, err
	}
	if body.Len()+8+sha256.Size > physicalSealReceiptBytes {
		return nil, ErrLimit
	}
	raw := make([]byte, physicalSealReceiptBytes)
	copy(raw, "HLRSP001")
	copy(raw[8:], body.Bytes())
	mac := hmac.New(sha256.New, key)
	mac.Write(raw[:len(raw)-sha256.Size])
	copy(raw[len(raw)-sha256.Size:], mac.Sum(nil))
	return raw, nil
}
func decodePhysicalSealClaims(raw, key []byte) (physicalSealClaims, error) {
	var c physicalSealClaims
	if len(raw) != physicalSealReceiptBytes || string(raw[:8]) != "HLRSP001" {
		return c, ErrCorrupt
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(raw[:len(raw)-sha256.Size])
	if !hmac.Equal(mac.Sum(nil), raw[len(raw)-sha256.Size:]) {
		return c, ErrCorrupt
	}
	if err := binary.Read(bytes.NewReader(raw[8:]), binary.BigEndian, &c); err != nil {
		return c, err
	}
	if c.Serial == 0 || c.From == ([32]byte{}) || c.Current.Bytes != candidateSealMaxBytes || c.Pending.Bytes != candidateSealMaxBytes || c.Current.File == c.Pending.File || c.Current.Parent != c.Pending.Parent {
		return c, ErrCorrupt
	}
	return c, nil
}

// OpenPhysicalSealCandidatePublisher authenticates the actual active envelope
// under the independent Standalone identity/version policy before restoring any
// preparation. Saved positions belong to that signed common envelope; its
// actual native/root owners still have to authenticate them before business.
// Receipt bytes authorize only exact prepared inodes, never a pending root.
func OpenPhysicalSealCandidatePublisher(ctx context.Context, dir string, guard *lock.Lock, key []byte, hard SealHardLimits, budget *resourcebudget.Manager, reservation resourcebudget.Permit, identity SealEligibility) (publisher *SealCandidatePublisher, resultErr error) {
	return openQualifiedPhysicalSealCandidatePublisher(ctx, dir, guard, key, hard, budget, reservation, identity, nil, false)
}

// OpenHAQualifiedPhysicalSealCandidatePublisher consumes the exact common
// envelope already selected inside the actual Member/Ordering/four-source
// freeze. It restores physical births only; the live HA factory retains source
// permission and must requalify every derived owner before exposing business.
// Unlike the Standalone compatibility entry point it never copies candidate
// positions into the independent eligibility supplied by that factory.
func OpenHAQualifiedPhysicalSealCandidatePublisher(ctx context.Context, dir string, guard *lock.Lock, key []byte, hard SealHardLimits, budget *resourcebudget.Manager, reservation resourcebudget.Permit, selected CandidateSeal, authority SealEligibility) (*SealCandidatePublisher, error) {
	return openQualifiedPhysicalSealCandidatePublisher(ctx, dir, guard, key, hard, budget, reservation, authority, &selected, true)
}

// OpenPhysicalSealCandidateReader observes only the actual MACed active
// envelope. It preserves every physical control/pending name and cannot publish,
// retire or restore a target. HA uses it to establish independent Member and
// Ordering eligibility before opening the separate qualified physical writer.
func OpenPhysicalSealCandidateReader(ctx context.Context, dir string, guard *lock.Lock, key []byte, hard SealHardLimits, budget *resourcebudget.Manager, reservation resourcebudget.Permit) (publisher *SealCandidatePublisher, resultErr error) {
	if guard == nil || !guard.CoversDataDirectory(dir) || budget == nil || len(key) != 32 {
		return nil, ErrSealIneligible
	}
	read, err := budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: candidateSealMaxBytes})
	if err != nil {
		return nil, err
	}
	defer func() {
		if !errors.Is(resultErr, ownedfile.ErrRetirement) {
			resultErr = errors.Join(resultErr, read.Release())
		}
	}()
	selected, _, _, err := readSealFile(filepath.Join(dir, "active.seal"), key, hard)
	if err != nil {
		return nil, err
	}
	return openSealCandidatePublisherPrepared(dir, guard, key, hard, budget, reservation, nil, true, &selected, nil, true)
}

func openQualifiedPhysicalSealCandidatePublisher(ctx context.Context, dir string, guard *lock.Lock, key []byte, hard SealHardLimits, budget *resourcebudget.Manager, reservation resourcebudget.Permit, identity SealEligibility, expected *CandidateSeal, ha bool) (publisher *SealCandidatePublisher, resultErr error) {
	defer func() {
		if publisher != nil && physicalPreparationObserver(ctx) != nil {
			publisher.phase = physicalPreparationObserver(ctx)
		}
	}()
	validRole := identity.Role == SealStandalone && !ha || ha && (identity.Role == SealPrimary || identity.Role == SealReplica) && expected != nil
	if guard == nil || !guard.CoversDataDirectory(dir) || budget == nil || !validRole || identity.Coverage != SealCoverageAuthenticated {
		return nil, ErrSealIneligible
	}
	if err := checkPhysicalSealOpaqueSlots(dir); err != nil {
		return nil, err
	}
	read, err := budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: 2*physicalSealReceiptBytes + candidateSealMaxBytes})
	if err != nil {
		return nil, err
	}
	defer func() {
		if !errors.Is(resultErr, ownedfile.ErrRetirement) {
			resultErr = errors.Join(resultErr, read.Release())
		}
	}()
	selected, _, digest, err := readSealFile(filepath.Join(dir, "active.seal"), key, hard)
	if err != nil {
		return nil, err
	}
	if ha {
		if selected != *expected {
			return nil, ErrSealIneligible
		}
	} else {
		copy(identity.Positions[:], selected.Positions[:])
	}
	if err := ValidateSealEligibility(selected, identity); err != nil {
		return nil, err
	}
	present := false
	for i := 0; i < 2; i++ {
		_, err := os.Lstat(physicalSealControlPath(dir, i))
		if err == nil {
			present = true
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	if !present {
		return OpenSealCandidatePublisher(dir, guard, key, hard, budget, reservation)
	}
	valid, err := hasAuthenticatedPhysicalSealBank(dir, key)
	if err != nil {
		return nil, err
	}
	if !valid {
		return openSealCandidatePublisherPrepared(dir, guard, key, hard, budget, reservation, nil, true, &selected, nil, true)
	}
	cycle, err := restorePhysicalSealCycle(ctx, dir, key, budget, reservation, digest)
	if err != nil {
		return nil, err
	}
	pending := cycle.claims.Pending
	if digest == cycle.claims.To {
		pending = cycle.claims.Current
	}
	target, err := resourcebudget.RestorePhysicalCandidate(ctx, filepath.Join(dir, "active.seal.pending"), pending, budget, reservation)
	if err != nil {
		return nil, errors.Join(err, cycle.close(), target.ClosePreserving())
	}
	publisher, err = OpenSealCandidatePublisherPrepared(dir, guard, key, hard, budget, reservation, selected, target)
	if err != nil {
		return nil, errors.Join(err, target.ClosePreserving(), cycle.close())
	}
	publisher.physicalCycle = cycle
	publisher.physicalTargetWork, err = target.RetainIndependentWorkspace(budget)
	if err != nil {
		return publisher, errors.Join(err, publisher.Close())
	}
	// A commit can precede the final receipt bank update. The exact active digest
	// plus both birth identities prove the actual exchange; preserve both inodes.
	if digest == cycle.claims.To {
		cycle.claims.Current, cycle.claims.Pending = cycle.claims.Pending, cycle.claims.Current
		cycle.claims.From, cycle.claims.To = digest, [32]byte{}
	}
	if err := publisher.checkPhysicalCycleCurrent(); err != nil {
		return nil, errors.Join(err, publisher.Close())
	}
	return publisher, nil
}

func restorePhysicalSealCycle(ctx context.Context, dir string, key []byte, budget *resourcebudget.Manager, reservation resourcebudget.Permit, digest [32]byte) (cycle *physicalSealCycle, resultErr error) {
	fee, err := reservation.RetainCovering(budget, resourcebudget.Cost{Disk: 2 * physicalSealReceiptBytes})
	if err != nil {
		return nil, err
	}
	work, err := budget.TryAcquireContext(ctx, resourcebudget.Background, resourcebudget.Cost{RAM: 4096})
	if err != nil {
		return nil, errors.Join(err, fee.Release())
	}
	r := &physicalSealCycle{fee: fee, work: work}
	cycle = r // all opened descriptors and persistent claims survive failure
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, r.close())
		}
	}()
	var best physicalSealClaims
	found := false
	var ids [2]resourcebudget.PhysicalIdentity
	for i := 0; i < 2; i++ {
		path := physicalSealControlPath(dir, i)
		f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		r.files[i] = f
		if err != nil {
			return r, err
		}
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() < 0 || info.Size() > physicalSealReceiptBytes {
			return r, errors.Join(ownedfile.ErrReplaced, err)
		}
		ids[i], err = resourcebudget.CapturePhysicalIdentity(f)
		if err != nil {
			return r, err
		}
		raw := make([]byte, physicalSealReceiptBytes)
		if _, err := f.ReadAt(raw, 0); err != nil && !errors.Is(err, io.EOF) {
			return r, err
		}
		c, err := decodePhysicalSealClaims(raw, key)
		if err != nil {
			continue
		} // one bank may contain a torn, never selected write
		if found && c.Controls != best.Controls {
			return r, ErrRecovery
		}
		if !found || c.Serial > best.Serial {
			best = c
			r.bank = i
			found = true
		}
	}
	if !found || best.Controls != ids || (digest != best.From && digest != best.To) {
		return r, ErrRecovery
	}
	r.claims = best
	for i := 0; i < 2; i++ {
		parent, parentInfo, err := physicalPathIdentity(dir)
		if err != nil || parent != best.Current.Parent {
			return r, errors.Join(ownedfile.ErrReplaced, err)
		}
		info, err := r.files[i].Stat()
		if err != nil {
			return r, err
		}
		r.owners[i], err = ownedfile.RestoreAuthenticated(physicalSealControlPath(dir, i), r.files[i], info, parentInfo)
		if err != nil {
			return r, err
		}
		if err := errors.Join(resourcebudget.CheckPhysicalRange(r.files[i], 0, physicalSealReceiptBytes), resourcebudget.CheckPhysicalTail(r.files[i], info.Size(), physicalSealReceiptBytes)); err != nil {
			return r, err
		}
	}
	return r, nil
}

// EnablePhysicalSealPreparation is called only after complete selected-native
// audits, before upstream admission. It prepares both actual retained targets
// and both receipt banks. Interrupted unknown names remain charged and intact.
func (p *SealCandidatePublisher) EnablePhysicalSealPreparation(ctx context.Context, selected CandidateSeal, reservation resourcebudget.Permit) (resultErr error) {
	return p.enableQualifiedPhysicalSealPreparation(ctx, selected, reservation, false)
}

// EnableHAQualifiedPhysicalSealPreparation is called while the actual HA
// source freeze is still live. Independent eligibility must select this exact
// envelope; a copied role/header or an allocation receipt is insufficient.
func (p *SealCandidatePublisher) EnableHAQualifiedPhysicalSealPreparation(ctx context.Context, selected CandidateSeal, reservation resourcebudget.Permit, authority SealEligibility) error {
	if p == nil || (authority.Role != SealPrimary && authority.Role != SealReplica) {
		return ErrSealIneligible
	}
	if err := ValidateSealEligibility(selected, authority); err != nil {
		return err
	}
	actual, found, err := p.SelectContext(ctx, authority)
	if err != nil || !found || actual != selected {
		return errors.Join(ErrSealIneligible, err)
	}
	return p.enableQualifiedPhysicalSealPreparation(ctx, selected, reservation, true)
}

func (p *SealCandidatePublisher) enableQualifiedPhysicalSealPreparation(ctx context.Context, selected CandidateSeal, reservation resourcebudget.Permit, ha bool) (resultErr error) {
	validRole := selected.Role == SealStandalone && !ha || ha && (selected.Role == SealPrimary || selected.Role == SealReplica)
	if p == nil || !validRole {
		return ErrSealIneligible
	}
	p.mu.Lock()
	alreadyPrepared := p.physicalCycle != nil
	p.mu.Unlock()
	if alreadyPrepared {
		return p.ValidatePhysicalSealPreparation(ctx, selected)
	}
	current, err := p.prepareCurrentQualifiedPhysicalRoot(ctx, selected, ha)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.readOnly || p.fault != nil || p.activeSeal != selected || !validRole {
		return ErrSealIneligible
	}
	fee, err := reservation.RetainCovering(p.budget, resourcebudget.Cost{Disk: 2 * physicalSealReceiptBytes})
	if err != nil {
		return err
	}
	work, err := p.budget.TryAcquireContext(ctx, resourcebudget.Background, resourcebudget.Cost{RAM: 4096})
	if err != nil {
		return errors.Join(err, fee.Release())
	}
	r := &physicalSealCycle{fee: fee, work: work}
	p.physicalCycle = r
	defer func() {
		if resultErr != nil {
			p.fault = errors.Join(p.fault, ErrSealFileRetirement, resultErr)
		}
	}()
	r.claims.Current = current
	r.claims.From = p.activeBytes
	for i := 0; i < 2; i++ {
		f, owner, err := ownedfile.Create(physicalSealControlPath(p.dir, i), os.O_RDWR, 0600)
		r.files[i], r.owners[i] = f, owner
		if err != nil {
			return err
		}
		if err := p.phaseResult(fmt.Sprintf("physical-seal-control-created-%d", i)); err != nil {
			return err
		}
		if err := resourcebudget.AllocatePhysicalRange(f, 0, physicalSealReceiptBytes); err != nil {
			return err
		}
		if err := p.phaseResult(fmt.Sprintf("physical-seal-control-allocated-%d", i)); err != nil {
			return err
		}
		r.claims.Controls[i], err = resourcebudget.CapturePhysicalIdentity(f)
		if err != nil {
			return err
		}
	}
	target, err := resourcebudget.PreparePhysicalCandidateObserved(ctx, filepath.Join(p.dir, "active.seal.pending"), candidateSealMaxBytes, p.budget, reservation, func(phase string) error { return p.phaseResult("physical-seal-" + phase) })
	p.physicalTarget = target
	if err != nil {
		return err
	}
	p.physicalTargetWork, err = target.RetainIndependentWorkspace(p.budget)
	if err != nil {
		return err
	}
	r.claims.Pending, err = target.Reference()
	if err != nil {
		return err
	}
	if err := p.savePhysicalSealClaims(); err != nil {
		return err
	}
	if err := p.phaseResult("physical-seal-first-valid-receipt"); err != nil {
		return err
	}
	if err := p.savePhysicalSealClaims(); err != nil {
		return err
	}
	return syncDir(p.dir)
}
func (p *SealCandidatePublisher) savePhysicalSealClaims() error {
	r := p.physicalCycle
	if r == nil || r.closed || r.claims.Serial == ^uint64(0) {
		return ErrRecovery
	}
	if err := p.checkPhysicalSealControls(); err != nil {
		return err
	}
	r.claims.Serial++
	raw, err := encodePhysicalSealClaims(r.claims, p.key)
	if err != nil {
		return err
	}
	bank := 1 - r.bank
	if err := r.owners[bank].VerifyFile(r.files[bank]); err != nil {
		return err
	}
	n, err := r.files[bank].WriteAt(raw, 0)
	if err != nil || n != len(raw) {
		return errors.Join(err, io.ErrShortWrite)
	}
	if err := r.files[bank].Sync(); err != nil {
		return err
	}
	if err := r.owners[bank].VerifyFile(r.files[bank]); err != nil {
		return err
	}
	r.bank = bank
	return nil
}
func (p *SealCandidatePublisher) checkPhysicalSealControls() error {
	r := p.physicalCycle
	if r == nil || r.closed {
		return ErrRecovery
	}
	for i := 0; i < 2; i++ {
		if err := r.owners[i].VerifyFile(r.files[i]); err != nil {
			return err
		}
		id, err := resourcebudget.CapturePhysicalIdentity(r.files[i])
		if err != nil || id != r.claims.Controls[i] {
			return errors.Join(ownedfile.ErrReplaced, err)
		}
		info, err := r.files[i].Stat()
		if err != nil {
			return err
		}
		if err := errors.Join(resourcebudget.CheckPhysicalRange(r.files[i], 0, physicalSealReceiptBytes), resourcebudget.CheckPhysicalTail(r.files[i], info.Size(), physicalSealReceiptBytes)); err != nil {
			return err
		}
	}
	return nil
}

func (p *SealCandidatePublisher) checkPhysicalCycleCurrent() error {
	r := p.physicalCycle
	if r == nil || r.closed {
		return ErrRecovery
	}
	id, _, err := physicalPathIdentity(p.path)
	if err != nil || id != r.claims.Current.File {
		return errors.Join(ownedfile.ErrReplaced, err)
	}
	parent, _, err := physicalPathIdentity(p.dir)
	if err != nil || parent != r.claims.Current.Parent {
		return errors.Join(ownedfile.ErrReplaced, err)
	}
	return p.checkPhysicalCurrentRoot()
}
func (p *SealCandidatePublisher) ValidatePhysicalSealPreparation(ctx context.Context, selected CandidateSeal) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.readOnly || p.fault != nil || p.activeSeal != selected {
		return errors.Join(ErrRecovery, p.fault)
	}
	if err := errors.Join(ctx.Err(), p.checkActive(), p.checkPhysicalCycleCurrent(), p.checkPhysicalSealControls()); err != nil {
		return err
	}
	return p.physicalTarget.ValidatePrepared(filepath.Join(p.dir, "active.seal.pending"), candidateSealMaxBytes, p.budget)
}
func (r *physicalSealCycle) close() error {
	if r == nil {
		return nil
	}
	if r.closed {
		return r.closeErr
	}
	r.closed = true
	for _, f := range r.files {
		r.closeErr = errors.Join(r.closeErr, ownedfile.CloseFile(f))
	}
	if r.closeErr == nil {
		r.closeErr = r.work.Release()
	}
	// Persistent banks are deliberately retained. Close is no deletion receipt.
	return r.closeErr
}

// PhysicalCacheTargetReferences lends only the four fixed cache target birth
// records from this same independently selected common envelope. Indexes are
// audit slots 0/1, then governance slots 0/1. The caller supplies each actual
// role-specific maximum and native/cache content proof; these are no roots.
func (p *SealCandidatePublisher) PhysicalCacheTargetReferences(selected CandidateSeal) (references [4]resourcebudget.PhysicalCandidateReference, found bool, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.fault != nil || p.physicalCycle == nil || p.activeSeal != selected {
		return references, false, errors.Join(ErrRecovery, p.fault)
	}
	if err := errors.Join(p.checkActive(), p.checkPhysicalCycleCurrent(), p.checkPhysicalSealControls()); err != nil {
		return references, false, err
	}
	references = p.physicalCycle.claims.Caches
	empty := 0
	for _, r := range references {
		if r == (resourcebudget.PhysicalCandidateReference{}) {
			empty++
		} else if r.File.Inode == 0 || r.Parent != p.physicalCycle.claims.Current.Parent || r.Bytes <= 0 {
			return references, false, ErrRecovery
		}
	}
	if empty != 0 && empty != len(references) {
		return references, false, ErrRecovery
	}
	return references, empty == 0, nil
}

// BindPhysicalCacheTargets persists the actual preparation's finite ownership
// receipt before upstream admission. An existing vector cannot be replaced by
// new caller fields or a newly scanned inode. Reuse keeps the original births.
func (p *SealCandidatePublisher) BindPhysicalCacheTargets(selected CandidateSeal, references [4]resourcebudget.PhysicalCandidateReference) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.fault != nil || p.physicalCycle == nil || p.activeSeal != selected {
		return errors.Join(ErrRecovery, p.fault)
	}
	if err := errors.Join(p.checkActive(), p.checkPhysicalCycleCurrent()); err != nil {
		return err
	}
	for i, r := range references {
		if r.File.Inode == 0 || r.Parent != p.physicalCycle.claims.Current.Parent || r.Bytes <= 0 {
			return ErrRecovery
		}
		old := p.physicalCycle.claims.Caches[i]
		if old != (resourcebudget.PhysicalCandidateReference{}) && old != r {
			return ownedfile.ErrReplaced
		}
		for j := 0; j < i; j++ {
			if references[j].File == r.File {
				return ownedfile.ErrReplaced
			}
		}
	}
	p.physicalCycle.claims.Caches = references
	if err := p.savePhysicalSealClaims(); err != nil {
		p.fault = errors.Join(ErrSealFileRetirement, err)
		return p.fault
	}
	return nil
}
