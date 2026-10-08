// Package diskinventory rebuilds startup Disk accounting from a quiescent,
// exclusively locked namespace. It neither authenticates file contents nor
// grants deletion authority or reserves filesystem blocks.
package diskinventory

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/akz142857/Halro/internal/resourcebudget"
	storelock "github.com/akz142857/Halro/internal/store/lock"
)

const (
	MaxDepth = 32
	// Each live os.File directory iterator can hold an 8 KiB readdir buffer.
	// Reserve above the complete 32-level FD stack plus names/stat/close errors;
	// this is a calibrated logical envelope, not an allocator interception.
	WorkspaceRAMBytes int64 = 1 << 20
)

var (
	ErrNamespace  = errors.New("disk inventory namespace is not qualified")
	ErrLimit      = errors.New("disk inventory exceeds configured ceiling")
	ErrRetirement = errors.New("disk inventory actual descriptor retirement is uncertain")
)

type Options struct {
	DataDir string
	Guard   *storelock.Lock
	Budget  *resourcebudget.Manager
	// Ceiling includes all existing files/directories, including native facts,
	// old derivatives, private candidates and failed replacement artifacts.
	Ceiling  int64
	Deadline time.Duration
}

// Snapshot is an observation, never an authorization to trust, trim or unlink
// data. ChargeBytes counts each qualified file's maximum logical/allocated
// bytes and allocated directory blocks. Nonexclusive hardlinks are refused.
type Snapshot struct {
	Files, Directories                        uint64
	LogicalBytes, AllocatedBytes, ChargeBytes int64
}

// Baseline holds the startup charge after the actual scanning FD group exits.
// Close cannot refund Disk: persisted bytes outlive process owners, and a
// disappeared pathname cannot establish that an old inode stopped consuming
// space. Consumers continue to reserve growth/replacement ceilings separately.
// This can conservatively double count existing consumer ceilings.
type Baseline struct {
	mu          sync.Mutex
	observation Snapshot
	credit      resourcebudget.Permit
	cleanupErr  error
	published   bool
}

func (b *Baseline) Snapshot() Snapshot {
	if b == nil {
		return Snapshot{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.observation
}

func (b *Baseline) Close() error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cleanupErr
}

// Publish moves already owned persistent bytes out of the dedicated Recovery
// pool. It admits no new bytes and does not assert runtime readiness. Failure
// preserves the original charge; Unknown pressure does not erase ownership.
func (b *Baseline) Publish() error {
	if b == nil {
		return ErrNamespace
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cleanupErr != nil {
		return b.cleanupErr
	}
	if b.published {
		return nil
	}
	p, err := b.credit.Reclassify(resourcebudget.Background)
	if err == nil {
		b.credit = p
		b.published = true
	}
	return err
}

// Reconcile must run before native Open/tail repair, background workers and
// readiness, with all in-process writers excluded. It reserves the full disk
// ceiling before scanning and shrinks it only after known physical FD exit.
// Every process rescans actual names; no saved count/checkpoint is trusted.
// A nonnil result with error retains fees for uncertain actual descriptor exit
// and must never be published. This API never modifies the inspected files.
func Reconcile(ctx context.Context, opts Options) (*Baseline, error) {
	return reconcile(ctx, opts, nil)
}
