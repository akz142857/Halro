package authenticatedstate

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func hasAuthenticatedPhysicalSealBank(dir string, key []byte) (valid bool, resultErr error) {
	for i := 0; i < 2; i++ {
		path := physicalSealControlPath(dir, i)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		if err := repackOpaqueIdentity(info, physicalSealReceiptBytes, physicalSealReceiptBytes+4096, false); err != nil {
			return false, err
		}
		file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return false, err
		}
		actual, statErr := file.Stat()
		raw := make([]byte, physicalSealReceiptBytes)
		_, readErr := file.ReadAt(raw, 0)
		closeErr := ownedfile.CloseFile(file)
		if statErr != nil || !os.SameFile(info, actual) || closeErr != nil || readErr != nil && !errors.Is(readErr, io.EOF) {
			return false, errors.Join(ownedfile.ErrReplaced, statErr, readErr, closeErr)
		}
		if string(raw[:8]) == "HLRSP001" {
			mac := hmac.New(sha256.New, key)
			mac.Write(raw[:len(raw)-sha256.Size])
			if hmac.Equal(mac.Sum(nil), raw[len(raw)-sha256.Size:]) {
				valid = true
			}
		}
	}
	return valid, nil
}
func (p *SealCandidatePublisher) PhysicalPublicationRequiresNoDebt() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.physicalUnqualified || p.physicalCycle != nil && p.physicalCycle.claims.Caches == ([4]resourcebudget.PhysicalCandidateReference{})
}

// Opaque slots never lend ownership. Their names, actual inode allocation and
// finite inventory must still fit the persistent preparation charge.
func checkPhysicalSealOpaqueSlots(dir string) error {
	for slot := 0; slot < 4; slot++ {
		path := filepath.Join(dir, fmt.Sprintf("terminal-seal.unqualified-%d", slot))
		captured, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if err := repackOpaqueIdentity(captured, 0, 8<<10, true); err != nil {
			return err
		}
		file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		actual, statErr := file.Stat()
		entries, readErr := file.Readdir(8)
		closeErr := ownedfile.CloseFile(file)
		if statErr != nil || !os.SameFile(captured, actual) || readErr != nil && !errors.Is(readErr, io.EOF) || closeErr != nil || len(entries) > 7 {
			return errors.Join(ErrRecovery, statErr, readErr, closeErr)
		}
		for _, entry := range entries {
			limit := int64(0)
			switch entry.Name() {
			case "terminal.seal.0", "terminal.seal.1":
				limit = physicalSealReceiptBytes
			case "active.seal.pending":
				limit = candidateSealMaxBytes
			case "memory-audit-0.cache", "memory-audit-1.cache", "memory-governance-0.cache", "memory-governance-1.cache":
				limit = 1 << 20
			default:
				return ErrRecovery
			}
			if err := repackOpaqueIdentity(entry, limit, limit+4096, false); err != nil {
				return err
			}
			if err := repackOpaqueNameMatches(filepath.Join(path, entry.Name()), entry); err != nil {
				return err
			}
		}
		if err := repackOpaqueNameMatches(path, captured); err != nil {
			return err
		}
	}
	return nil
}

// PreserveUnqualifiedPhysicalSealAfterNoDebt is an explicit opaque-preservation
// boundary. The caller must first use the actual selected factory to prove the
// original common Ledger root equals the completely authenticated native head,
// then exhaust both pinned recovery streams with no accepted obligation. This
// moves unknown bytes without authenticating/deleting them or lending extents.
// A valid receipt with a foreign birth mismatch never reaches this branch.
func (p *SealCandidatePublisher) PreserveUnqualifiedPhysicalSealAfterNoDebt(ctx context.Context, expected CandidateSeal) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	unqualified := p.physicalUnqualified
	cachePreparation := p.physicalCycle != nil && p.physicalCycle.claims.Caches == ([4]resourcebudget.PhysicalCandidateReference{})
	if p.closed || p.fault != nil || !unqualified && !cachePreparation || p.activeSeal != expected || expected.Role != SealStandalone {
		return ErrSealIneligible
	}
	if err := errors.Join(ctx.Err(), p.checkActive()); err != nil {
		return err
	}
	if err := checkPhysicalSealOpaqueSlots(p.dir); err != nil {
		return err
	}
	valid, err := hasAuthenticatedPhysicalSealBank(p.dir, p.key)
	if err != nil {
		return err
	}
	if valid && unqualified {
		return ErrRecovery
	}
	if cachePreparation {
		if err := errors.Join(p.checkPhysicalCycleCurrent(), p.checkPhysicalSealControls(), p.physicalTarget.ValidatePrepared(filepath.Join(p.dir, "active.seal.pending"), candidateSealMaxBytes, p.budget)); err != nil {
			return err
		}
	}
	var names [5]string
	var infos [5]os.FileInfo
	count := 0
	for i := 0; i < 5; i++ {
		if i < 3 && !unqualified {
			continue
		}
		path, limit := physicalSealControlPath(p.dir, i), int64(physicalSealReceiptBytes)
		if i == 2 {
			path, limit = filepath.Join(p.dir, "active.seal.pending"), candidateSealMaxBytes
		}
		if i >= 3 {
			kind := "audit"
			if i == 4 {
				kind = "governance"
			}
			path, limit = filepath.Join(p.dir, fmt.Sprintf("memory-%s-%d.cache", kind, (expected.Revision+1)%2)), 1<<20
		}
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if err := repackOpaqueIdentity(info, limit, limit+4096, false); err != nil {
			return err
		}
		names[count], infos[count] = path, info
		count++
	}
	if count == 0 {
		p.readOnly, p.physicalUnqualified = false, false
		return nil
	}
	destination := ""
	for i := 0; i < 4; i++ {
		path := filepath.Join(p.dir, fmt.Sprintf("terminal-seal.unqualified-%d", i))
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			if destination == "" {
				destination = path
			}
			continue
		}
		if err != nil {
			return err
		}
		if err := repackOpaqueIdentity(info, 0, 8<<10, true); err != nil {
			return err
		}
	}
	if destination == "" {
		return ErrLimit
	}
	if err := os.Mkdir(destination, 0700); err != nil {
		return err
	}
	for i := 0; i < count; i++ {
		if err := repackOpaqueNameMatches(names[i], infos[i]); err != nil {
			return err
		}
		target := filepath.Join(destination, filepath.Base(names[i]))
		if err := os.Rename(names[i], target); err != nil {
			return err
		}
		if err := repackOpaqueNameMatches(target, infos[i]); err != nil {
			return err
		}
	}
	if err := errors.Join(syncDir(destination), syncDir(p.dir)); err != nil {
		p.fault = errors.Join(ErrSealFileRetirement, err)
		return p.fault
	}
	p.readOnly, p.physicalUnqualified = false, false
	return nil
}
