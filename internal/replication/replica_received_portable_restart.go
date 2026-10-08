package replication

import (
	"bytes"
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func portableReceivedOpaquePath(path string, slot int) string {
	return fmt.Sprintf("%s.unqualified-%d", path, slot)
}

// Inventory never parses retained candidates or attributes creator ownership.
// Every actual file is covered by the original fixed five-file Disk permit.
func checkPortableReceivedInventory(path string) error {
	for slot := -1; slot < portableReceivedOpaqueSlots; slot++ {
		name := path
		if slot >= 0 {
			name = portableReceivedOpaquePath(path, slot)
		}
		info, err := os.Lstat(name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > PortableReceivedDiskBytes {
			return ErrMemberAuthority
		}
	}
	return nil
}

func (o *PortableReceivedPublication) preserveUnqualified(ctx context.Context, path string, orderingID resourcebudget.PhysicalIdentity) (resultErr error) {
	raw, id, readErr := readPortableReceivedReceiptIdentity(path)
	if errors.Is(readErr, os.ErrNotExist) {
		return nil
	}
	if errors.Is(readErr, ownedfile.ErrRetirement) {
		return readErr
	}
	if readErr == nil {
		var header portableReceivedHeader
		decoder := json.NewDecoder(bytes.NewReader(raw[:receiveReceiptHeaderBytes]))
		decoder.DisallowUnknownFields()
		decodeErr := decoder.Decode(&header)
		mac, macErr := portableReceivedMAC(header, o.publisher.key[:])
		member := o.publisher.state
		if decodeErr == nil && macErr == nil && hmac.Equal(mac, header.MAC) && header.Version == 1 && header.Receipt == id && header.Ordering == orderingID && header.ClusterID == member.ClusterID && header.Incarnation == member.Incarnation && header.NodeID == member.NodeID && header.Baseline == o.baseline.Descriptor().AuthorityDigest {
			return nil
		}
	}
	// An opaque candidate has no root, cut, accepted-record or deletion permission.
	// Capture the actual name/FD only to preserve that stopped namespace intact.
	parent, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return err
	}
	if !parent.IsDir() || parent.Mode()&os.ModeSymlink != 0 {
		return ErrMemberAuthority
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if closeErr := ownedfile.CloseFile(f); closeErr != nil {
		return errors.Join(err, ownedfile.CloseError(f, closeErr))
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > PortableReceivedDiskBytes {
		return ErrMemberAuthority
	}
	var destination string
	for slot := 0; slot < portableReceivedOpaqueSlots; slot++ {
		target := portableReceivedOpaquePath(path, slot)
		if _, err := os.Lstat(target); errors.Is(err, os.ErrNotExist) {
			destination = target
			break
		} else if err != nil {
			return err
		}
	}
	if destination == "" {
		return ErrReceivedOpaqueSlots
	}
	named, err := os.Lstat(path)
	if err != nil || !os.SameFile(named, info) || named.Size() != info.Size() {
		return errors.Join(ownedfile.ErrReplaced, err)
	}
	namedParent, err := os.Lstat(filepath.Dir(path))
	if err != nil || !os.SameFile(namedParent, parent) {
		return errors.Join(ownedfile.ErrReplaced, err)
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = renamePortableReceivedOpaque(path, destination); err != nil {
		return errors.Join(ownedfile.ErrRetirement, err)
	}
	if err = durable.SyncDirectory(filepath.Dir(path)); err != nil {
		return errors.Join(ownedfile.ErrRetirement, err)
	}
	return nil
}

var ErrReceivedOpaqueSlots = errors.New("all four received receipt preservation slots are occupied")
