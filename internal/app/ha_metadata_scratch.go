package app

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

const haMetadataScratchSlots = 4
const haMetadataScratchReceiptBytes = 4096

// Old crash slots are opaque inventory obligations. Neither the directory
// receipt nor DB bytes confer projection or deletion authority after restart.
// Every attempt reconstructs from native facts in one fresh exclusive slot.
type haMetadataScratch struct {
	path      string
	directory os.FileInfo
	receipt   *ownedfile.Owner
	fee       resourcebudget.Permit
	closeErr  error
}

func createHAMetadataScratch(ctx context.Context, workspace string, clusterKey []byte, a *replication.MetadataBaselineAuthority, member replication.MemberState, manager *resourcebudget.Manager) (result *haMetadataScratch, resultErr error) {
	if len(clusterKey) != 32 || a == nil || manager == nil {
		return nil, resourcebudget.ErrInvalid
	}
	s := &haMetadataScratch{}
	defer func() {
		if resultErr != nil {
			if err := s.Discard(); err != nil {
				result = s
				resultErr = errors.Join(resultErr, ownedfile.ErrRetirement, err)
			}
		}
	}()
	var err error
	s.fee, err = manager.TryAcquireRecovery(resourcebudget.Cost{RAM: haMetadataScratchReceiptBytes, Disk: haMetadataScratchReceiptBytes})
	if err != nil {
		return nil, err
	}
	for slot := 0; slot < haMetadataScratchSlots; slot++ {
		path := filepath.Join(workspace, fmt.Sprintf("ledger-maintenance.%d", slot))
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := os.Mkdir(path, 0700); err != nil {
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return nil, err
		}
		s.path = path
		info, err := os.Lstat(path)
		if err != nil {
			return s, err
		}
		if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
			return s, ownedfile.ErrReplaced
		}
		s.directory = info
		break
	}
	if s.path == "" {
		return nil, errors.New("all four HA Ledger maintenance scratch slots are retained; unqualified crash namespaces require explicit inspection")
	}
	stat, ok := s.directory.Sys().(*syscall.Stat_t)
	if !ok {
		return s, errors.New("HA scratch directory lacks actual inode evidence")
	}
	body, err := json.Marshal(struct {
		Version       uint8
		Device, Inode uint64
		Carrier       [32]byte
		Member        replication.MemberState
	}{Version: 1, Device: uint64(stat.Dev), Inode: uint64(stat.Ino), Carrier: a.Descriptor().AuthorityDigest, Member: member})
	if err != nil {
		return s, err
	}
	if len(body)+32 > haMetadataScratchReceiptBytes {
		return s, errors.New("HA maintenance scratch receipt exceeds its fixed byte bound")
	}
	mac := hmac.New(sha256.New, clusterKey)
	mac.Write([]byte("halro:cluster:metadata-maintenance-scratch:v1\x00"))
	mac.Write(body)
	file, receipt, err := ownedfile.Create(filepath.Join(s.path, "owner.receipt"), os.O_WRONLY, 0600)
	if err != nil {
		return s, err
	}
	s.receipt = receipt
	_, writeErr := file.Write(append(body, mac.Sum(nil)...))
	if err := errors.Join(writeErr, file.Sync(), ownedfile.CloseFile(file), durable.SyncDirectory(s.path), durable.SyncDirectory(workspace)); err != nil {
		if errors.Is(err, ownedfile.ErrRetirement) {
			s.closeErr = err
		}
		return s, err
	}
	if err := s.fee.ReleasePart(resourcebudget.Cost{RAM: haMetadataScratchReceiptBytes}); err != nil {
		s.closeErr = err
		return s, err
	}
	return s, nil
}

func (s *haMetadataScratch) Discard() error {
	if s == nil {
		return nil
	}
	if s.closeErr != nil {
		return errors.Join(ownedfile.ErrRetirement, s.closeErr)
	}
	// The caller joins/discards its actual Ledger DB first. No guessed filename
	// is removed here; a foreign or still-open DB prevents directory retirement.
	if err := s.receipt.Retire(); err != nil {
		s.closeErr = errors.Join(ownedfile.ErrRetirement, err)
		return s.closeErr
	}
	if s.path != "" {
		info, err := os.Lstat(s.path)
		if err != nil || s.directory == nil || !os.SameFile(s.directory, info) {
			s.closeErr = errors.Join(ownedfile.ErrRetirement, ownedfile.ErrReplaced, err)
			return s.closeErr
		}
		if err := os.Remove(s.path); err != nil {
			s.closeErr = errors.Join(ownedfile.ErrRetirement, err)
			return s.closeErr
		}
		if err := durable.SyncDirectory(filepath.Dir(s.path)); err != nil {
			s.closeErr = errors.Join(ownedfile.ErrRetirement, err)
			return s.closeErr
		}
		s.path = ""
	}
	if s.fee != (resourcebudget.Permit{}) {
		if err := s.fee.Release(); err != nil {
			s.closeErr = err
			return err
		}
		s.fee = resourcebudget.Permit{}
	}
	return nil
}
