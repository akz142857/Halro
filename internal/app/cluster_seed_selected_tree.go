package app

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/akz142857/Halro/internal/backup"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/dirscan"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// Format2 has one fixed declaration. Its canonical ordinal directory lives in
// a paid disposable catalog; neither source count nor native history occupies
// a retained slice. Unknown state and interrupted preparations are refused.
type selectedSeedTree struct {
	Files  uint64
	Bytes  uint64
	Digest [32]byte
}

func selectedSeedPathAllowed(path, metadata string) bool {
	if _, opaque := selectedSeedOpaqueControlLimit(filepath.ToSlash(path)); opaque {
		return true
	}
	top := strings.Split(filepath.ToSlash(path), "/")[0]
	if selectedSeedArchiveName(top) || selectedSeedGroupControlName(top) {
		return true
	}
	switch top {
	case "ha-group-cluster.key", metadata, metadata + ".preserved", "metadata.journal", "ledger", "usage", "audit", "governance", "provider-objects", HAMetadataBaselineDirectory, "ha-memory-state", "ha-memory-state.bootstrap", "active.seal":
		return true
	case "cluster":
		return path == "cluster" || path == "cluster/provider-object-sources" || path == "cluster/ordering.journal" || path == "cluster/selected-promotion.prepare" || path == "cluster/selected-promotion.carrier" || path == "cluster/selected-peer-promotion.prepare" || path == "cluster/selected-peer-promotion.promise" || strings.HasPrefix(filepath.ToSlash(path), "cluster/provider-object-sources/")
	}
	for i := range haSelectedCacheSlots {
		if path == filepath.Base(haSelectedCachePath("", i, false)) || path == filepath.Base(haSelectedCachePath("", i, true)) {
			return true
		}
	}
	return false
}

func hashSelectedSeedTree(ctx context.Context, cfg config.Config, root, workspace string, manager *resourcebudget.Manager) (result selectedSeedTree, resultErr error) {
	if cfg.ResourceBudget == nil || manager == nil || cfg.ResourceBudget.BackupWorkspaceDiskBytes <= 0 || workspace == "" || pathWithin(workspace, root) {
		return result, resourcebudget.ErrInvalid
	}
	ceiling := cfg.ResourceBudget.BackupWorkspaceDiskBytes
	budget, err := backup.NewWorkspaceBudget(ctx, ceiling, func(ctx context.Context, n int64) (func() error, error) {
		p, e := manager.TryAcquireRecovery(resourcebudget.Cost{RAM: n + 32<<20, Disk: n})
		if e != nil {
			return nil, e
		}
		return p.Release, nil
	})
	if err != nil {
		return result, err
	}
	ctx, err = budget.Bind(ctx)
	if err != nil {
		return result, err
	}
	catalog, err := backup.NewSourceCatalogWithContext(ctx, workspace, ceiling)
	if err != nil {
		return result, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, catalog.Close())
		if resultErr != nil {
			result = selectedSeedTree{}
		}
	}()
	err = dirscan.Tree(ctx, root, seedTreeDepth, func(rel string, file *os.File, info os.FileInfo) error {
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel == ".halro.lock" || rel == "cluster/state.json" {
			return nil
		}
		opaqueLimit, opaque := selectedSeedOpaqueControlLimit(rel)
		archived := selectedSeedArchiveName(strings.Split(rel, "/")[0])
		if opaque && (!info.Mode().IsRegular() || info.Size() < 0 || info.Size() > opaqueLimit) {
			return errSeedMemoryStateAuthority
		}
		if !selectedSeedPathAllowed(rel, cfg.Storage.MetadataFile) || (!archived && (strings.Contains(rel, "terminal.physical") || strings.Contains(rel, "terminal-root-") || strings.HasSuffix(rel, ".pending") || (strings.Contains(rel, "unqualified") && !opaque))) {
			return errSeedMemoryStateAuthority
		}
		return catalog.Add(backup.SourceFile{ArchivePath: rel, LocalPath: filepath.Join(root, rel)})
	})
	if err != nil {
		return result, err
	}
	h := sha256.New()
	h.Write([]byte("halro:seed:ordinal-tree:v2\x00"))
	buffer := make([]byte, 32<<10)
	err = catalog.Walk(ctx, func(source backup.SourceFile) (e error) {
		info, e := os.Lstat(source.LocalPath)
		if e != nil || !(info.Mode().IsRegular() || info.IsDir()) || info.Size() < 0 {
			return errors.Join(dirscan.ErrIdentity, e)
		}
		file, e := os.OpenFile(source.LocalPath, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if e != nil {
			return e
		}
		defer func() { e = errors.Join(e, ownedfile.CloseFile(file)) }()
		actual, e := file.Stat()
		if e != nil || !os.SameFile(info, actual) {
			return errors.Join(dirscan.ErrIdentity, e)
		}
		digest := sha256.New()
		var n int64
		if info.IsDir() {
			digest.Write([]byte("directory"))
		} else {
			n, e = io.CopyBuffer(digest, seedContextReader{ctx, file}, buffer)
		}
		if e != nil {
			return e
		}
		end, e := file.Stat()
		named, ne := os.Lstat(source.LocalPath)
		if e != nil || ne != nil || !info.IsDir() && n != info.Size() || !os.SameFile(info, end) || !os.SameFile(info, named) || end.Size() != info.Size() || named.Size() != info.Size() || !end.ModTime().Equal(info.ModTime()) || !named.ModTime().Equal(info.ModTime()) {
			return errors.Join(dirscan.ErrIdentity, e, ne)
		}
		if result.Files == math.MaxUint64 || uint64(n) > math.MaxUint64-result.Bytes {
			return errSeedLimit
		}
		result.Files++
		result.Bytes += uint64(n)
		var scalar [24]byte
		binary.BigEndian.PutUint64(scalar[:8], result.Files)
		binary.BigEndian.PutUint64(scalar[8:16], uint64(len(source.ArchivePath)))
		binary.BigEndian.PutUint64(scalar[16:], uint64(n))
		h.Write(scalar[:])
		h.Write([]byte(source.ArchivePath))
		h.Write(digest.Sum(nil))
		return ctx.Err()
	})
	if err != nil {
		return result, err
	}
	var trailer [16]byte
	binary.BigEndian.PutUint64(trailer[:8], result.Files)
	binary.BigEndian.PutUint64(trailer[8:], result.Bytes)
	h.Write(trailer[:])
	copy(result.Digest[:], h.Sum(nil))
	return result, nil
}
