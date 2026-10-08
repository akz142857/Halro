package replication

import (
	"context"
	"errors"
	"fmt"
	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"os"
	"path/filepath"
)

// Explicit restart preserves a partially created bank/receipt as opaque files.
// It never reads the bank or claims its creator/delete rights. The caller has
// authenticated current Member/Ordering and rejected any unexplained tail.
func preserveOpaquePublicationFiles(ctx context.Context, statePath string) error {
	type entry struct {
		path, name string
		info       os.FileInfo
	}
	entries := []entry{{path: statePath + ".physical-bank", name: "bank"}, {path: statePath + ".physical.json", name: "receipt"}}
	any := false
	for i := range entries {
		info, err := os.Lstat(entries[i].path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		max := int64(MaxMemberStateJSON)
		if i == 1 {
			max = physicalPublicationReceiptBytes
		}
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > max {
			return errors.Join(ownedfile.ErrReplaced, err)
		}
		entries[i].info = info
		any = true
	}
	if !any {
		return nil
	}
	var directory string
	for slot := 0; slot < physicalPublicationOpaqueSlots; slot++ {
		candidate := fmt.Sprintf("%s.physical.unqualified-%d", statePath, slot)
		if _, err := os.Lstat(candidate); errors.Is(err, os.ErrNotExist) {
			directory = candidate
			break
		} else if err != nil {
			return err
		}
	}
	if directory == "" {
		return resourcebudget.ErrExhausted
	}
	if err := os.Mkdir(directory, 0700); err != nil {
		return err
	}
	parent, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	if err := durable.SyncDirectory(filepath.Dir(statePath)); err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.info == nil {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		actual, err := os.Lstat(entry.path)
		if err != nil || !os.SameFile(actual, entry.info) {
			return errors.Join(ownedfile.ErrReplaced, err)
		}
		named, err := os.Lstat(directory)
		if err != nil || !os.SameFile(named, parent) {
			return errors.Join(ownedfile.ErrReplaced, err)
		}
		if err := renameOpaquePhysicalPublication(entry.path, filepath.Join(directory, entry.name)); err != nil {
			return err
		}
		if err := errors.Join(durable.SyncDirectory(directory), durable.SyncDirectory(filepath.Dir(statePath))); err != nil {
			return err
		}
	}
	return nil
}
