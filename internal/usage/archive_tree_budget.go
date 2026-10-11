package usage

import (
	"context"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
)

const archiveInventoryBatch = 128
const archiveInventoryMaxDepth = 64

type archiveTreeBudget struct {
	mu      sync.Mutex
	ceiling int64
	used    int64
}

func inventoryArchiveTree(ctx context.Context, root string, ceiling int64) (int64, error) {
	if ceiling < 1 {
		return 0, ErrDiskUsageLimit
	}
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return 0, err
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return 0, ErrDiskUsageCorrupt
	}
	var total int64
	var walk func(string, int, os.FileInfo) error
	walk = func(path string, depth int, expected os.FileInfo) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		openedInfo, err := f.Stat()
		if err != nil || !os.SameFile(expected, openedInfo) || !openedInfo.IsDir() {
			_ = f.Close()
			return errors.Join(err, ErrDiskUsageCorrupt)
		}
		var walkErr error
		for {
			if err := ctx.Err(); err != nil {
				walkErr = err
				break
			}
			entries, readErr := f.ReadDir(archiveInventoryBatch)
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				walkErr = readErr
				break
			}
			for _, entry := range entries {
				if err := ctx.Err(); err != nil {
					walkErr = err
					break
				}
				child := filepath.Join(path, entry.Name())
				info, statErr := os.Lstat(child)
				if statErr != nil {
					walkErr = statErr
					break
				}
				switch {
				case info.IsDir() && info.Mode()&os.ModeSymlink == 0:
					if depth >= archiveInventoryMaxDepth {
						walkErr = ErrDiskUsageLimit
						break
					}
					walkErr = walk(child, depth+1, info)
				case info.Mode().IsRegular():
					if info.Size() < 0 || info.Size() > math.MaxInt64-total {
						walkErr = ErrDiskUsageLimit
						break
					}
					total += info.Size()
					if total > ceiling {
						walkErr = ErrDiskUsageLimit
					}
				default:
					walkErr = ErrDiskUsageCorrupt
				}
				if walkErr != nil {
					break
				}
			}
			if walkErr != nil {
				break
			}
			if errors.Is(readErr, io.EOF) || len(entries) == 0 {
				break
			}
		}
		if err := f.Close(); err != nil {
			walkErr = errors.Join(walkErr, err)
		}
		return walkErr
	}
	if err := walk(root, 0, rootInfo); err != nil {
		return 0, err
	}
	currentRoot, err := os.Lstat(root)
	if err != nil {
		return 0, err
	}
	if !os.SameFile(rootInfo, currentRoot) || !currentRoot.IsDir() {
		return 0, ErrDiskUsageCorrupt
	}
	return total, nil
}

// archiveVerifyDecoderRAMBytes is the decoder workspace an archive comparison
// holds beside one spooled file.
const archiveVerifyDecoderRAMBytes int64 = 32 << 20

// clampArchiveScratch bounds one scratch limit by the archive tree ceiling.
// Every archive file and scratch index lives under that tree, which charges
// them against the ceiling, so a larger limit can never be used; reserving it
// would only make the step inadmissible under the configured IO envelope.
// Zero is the uncapped tree.
func clampArchiveScratch(limit, ceiling int64) int64 {
	if ceiling > 0 {
		return min(limit, ceiling)
	}
	return limit
}

// DateMaintenanceWorkspace is the IO work a Usage date sweep holds at once
// under an archive ceiling (zero: none): the retained comparison session's
// decoder RAM, spool and index beside the retained reference index. A window
// verification, which runs only before any date policy exists, needs less.
func DateMaintenanceWorkspace(archiveCeiling int64) (ram, disk int64) {
	verify, references := DefaultDateCatalogVerifyLimits(), DefaultDateCatalogReferenceLimits()
	file := clampArchiveScratch(verify.FileBytes, archiveCeiling)
	return file + archiveVerifyDecoderRAMBytes, file + clampArchiveScratch(verify.IndexBytes, archiveCeiling) + clampArchiveScratch(references.IndexBytes, archiveCeiling)
}

func newArchiveTreeBudget(ctx context.Context, root string, ceiling int64) (*archiveTreeBudget, error) {
	if ceiling == 0 {
		return nil, nil
	}
	used, err := inventoryArchiveTree(ctx, root, ceiling)
	if err != nil {
		return nil, err
	}
	return &archiveTreeBudget{ceiling: ceiling, used: used}, nil
}

func (e *Exporter) ensureArchiveWorkTree(ctx context.Context, work *archiveWork) error {
	if e == nil || work == nil {
		return ErrDiskUsageLimit
	}
	if work.tree != nil || e.archiveDiskCeiling == 0 {
		return nil
	}
	tree, err := newArchiveTreeBudget(ctx, e.root, e.archiveDiskCeiling)
	if err != nil {
		return err
	}
	work.tree = tree
	return nil
}

func (e *Exporter) newArchiveWork(ctx context.Context, remaining int64) (*archiveWork, error) {
	work := &archiveWork{remaining: remaining}
	if err := e.ensureArchiveWorkTree(ctx, work); err != nil {
		return nil, err
	}
	return work, nil
}

func (e *Exporter) reserveArchiveDisk(ctx context.Context, amount int64) (func() error, error) {
	if e == nil || amount < 0 {
		return nil, ErrDiskUsageLimit
	}
	if amount == 0 || e.archiveDiskReserve == nil {
		return func() error { return nil }, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	release, err := e.archiveDiskReserve(ctx, amount)
	if err != nil {
		return nil, err
	}
	if release == nil {
		return nil, ErrDiskUsageCorrupt
	}
	return release, nil
}

func (e *Exporter) reserveArchiveWorkRAM(ctx context.Context, amount int64) (func() error, error) {
	if e == nil || amount <= 0 {
		return nil, ErrDiskUsageLimit
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if e.archiveWorkRAMReserve == nil {
		return func() error { return nil }, nil
	}
	release, err := e.archiveWorkRAMReserve(ctx, amount)
	if err != nil {
		return nil, err
	}
	if release == nil {
		return nil, ErrDiskUsageCorrupt
	}
	return release, nil
}

func archiveTreeWriter(ctx context.Context, out io.Writer, tree *archiveTreeBudget) *archiveWriter {
	work := &archiveWork{remaining: math.MaxInt64, tree: tree}
	return &archiveWriter{ctx: ctx, out: out, remaining: math.MaxInt64, work: work}
}

func (b *archiveTreeBudget) take(n int) error {
	return b.takeBytes(int64(n))
}

func (b *archiveTreeBudget) takeBytes(n int64) error {
	if b == nil || n == 0 {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if n < 0 || n > math.MaxInt64-b.used || b.used+n > b.ceiling {
		return ErrDiskUsageLimit
	}
	b.used += n
	return nil
}
