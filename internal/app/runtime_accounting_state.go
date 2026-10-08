package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

type runtimeAccountingReader interface {
	ledger.AccountingAdmissionReader
	ledger.AccountingLeaseReader
}

type runtimeProjectionDiskPermit struct {
	mu           sync.Mutex
	permit       resourcebudget.Permit
	released     bool
	retirableRAM int64 // Only a designated physical owner may retire this part.
}

func (p *runtimeProjectionDiskPermit) retireRAMAfterClose(closeErr error) error {
	if p == nil || closeErr != nil {
		return closeErr
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.released || p.retirableRAM == 0 {
		return nil
	}
	err := p.permit.ReleasePart(resourcebudget.Cost{RAM: p.retirableRAM})
	if err == nil {
		p.retirableRAM = 0
	}
	return err
}

func (p *runtimeProjectionDiskPermit) Reclassify(class resourcebudget.Class) error {
	if p == nil {
		return errors.New("Ledger projection disk permit is absent")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.released {
		return resourcebudget.ErrInvalid
	}
	permit, err := p.permit.Reclassify(class)
	if err == nil {
		p.permit = permit
	}
	return err
}

func (p *runtimeProjectionDiskPermit) Release() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.released {
		return nil
	}
	if err := p.permit.Release(); err != nil {
		return err
	}
	p.released = true
	return nil
}

// The reader is a diagnostic alias, not a separately selectable Manager owner.
// Keep the owned disk handle independent of that alias: native callbacks and
// Manager tickets must really join before Close releases the derived database.
// Standalone/Primary select disk accounting; Replica owns a coherent generation.
type runtimeAccountingState struct {
	retirementErr error
	recovery      *atomic.Bool // Shared across startup/maintenance copies of this exact owner.
	reader        runtimeAccountingReader
	projection    *ledger.LedgerProjection
	path          string
	file          os.FileInfo
	replica       *replicaAccountingGeneration
	startupLease  *ledger.MaintenanceLease     // Primary transfer until real confirmed recovery joins.
	diskPermit    *runtimeProjectionDiskPermit // Persistent projection ceiling; released only after owned inode removal.
}

// This reserved, node-derived file is rebuilt from the complete authenticated
// native source on every Standalone open. It is never a trusted restart cache.
// The data-directory lock excludes another runtime; a single fixed filename
// bounds crash leftovers without a directory scan or removing native facts.
func standaloneProjectionPath(cfg config.Config) string {
	return filepath.Join(cfg.Storage.DataDir, "ledger-projection.db")
}

// Configured authority/credentials may use arbitrary file names, including
// this reserved derived name. Refuse collisions BEFORE any Open mutation.
// Config File/Path fields include master/KMS/TLS/credential material; Metadata
// has a data-directory-relative filename and needs its resolved path too.
func validateStandaloneProjectionPath(ctx context.Context, cfg config.Config) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if cfg.Replication != nil {
		return nil
	}
	for _, path := range []string{standaloneProjectionPath(cfg), standaloneGovernanceProjectionPath(cfg), standaloneUsageProjectionPath(cfg)} {
		if err := validateRuntimeDerivedPath(ctx, cfg, path); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func validateRuntimeDerivedPath(ctx context.Context, cfg config.Config, candidatePath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := filepath.Abs(candidatePath)
	if err != nil {
		return err
	}
	derived, statErr := os.Stat(path)
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	check := func(candidate string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if candidate == "" {
			return nil
		}
		absolute, err := filepath.Abs(candidate)
		if err != nil {
			return err
		}
		other, statErr := os.Stat(absolute)
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return statErr
		}
		if absolute == path || derived != nil && other != nil && os.SameFile(derived, other) {
			return fmt.Errorf("reserved derivative %q conflicts with configured file %q", candidatePath, candidate)
		}
		return nil
	}
	// Computed native paths are not Config File/Path fields. A native symlink
	// can point INTO a reserved derivative, so removing that target would delete
	// authoritative history even while its existing FD remains writable.
	for _, native := range []string{cfg.MetadataPath(), cfg.LedgerPath(), cfg.GovernancePath(), cfg.AuditPath(), cfg.LogFilePath(), cfg.ErrorLogFilePath(), cfg.ReplicationStatePath(), cfg.OrderingJournalPath(), filepath.Join(cfg.Storage.DataDir, ".halro.lock"), lock.PublicationPath(cfg.Storage.DataDir)} {
		if err := check(native); err != nil {
			return err
		}
	}
	if derived != nil {
		// Native Ledger generations and Metadata journal names are computed at
		// runtime too. Inspect fixed authority directories in finite name pages;
		// never materialize a historical segment list or follow a directory tree.
		for _, directory := range []string{cfg.Storage.DataDir, filepath.Dir(cfg.MetadataPath()), filepath.Dir(cfg.LedgerPath()), filepath.Dir(cfg.GovernancePath()), filepath.Dir(cfg.AuditPath()), cfg.ClusterDirectoryPath(), filepath.Join(cfg.Storage.DataDir, "provider-objects")} {
			if err := checkDerivedNativeDirectory(ctx, directory, path, check); err != nil {
				return err
			}
		}
	}
	var visit func(reflect.Value) error
	visit = func(v reflect.Value) error {
		if v.Kind() == reflect.Pointer {
			if v.IsNil() {
				return nil
			}
			return visit(v.Elem())
		}
		if v.Kind() == reflect.Slice || v.Kind() == reflect.Array {
			for i := 0; i < v.Len(); i++ {
				if err := visit(v.Index(i)); err != nil {
					return err
				}
			}
			return nil
		}
		if v.Kind() != reflect.Struct {
			return nil
		}
		for i := 0; i < v.NumField(); i++ {
			field, name := v.Field(i), v.Type().Field(i).Name
			if field.Kind() == reflect.String && (strings.HasSuffix(name, "File") || strings.HasSuffix(name, "Path")) {
				if err := check(field.String()); err != nil {
					return err
				}
			} else if err := visit(field); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(reflect.ValueOf(cfg)); err != nil {
		return err
	}
	return ctx.Err()
}

func checkDerivedNativeDirectory(ctx context.Context, directory, reserved string, check func(string) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := os.Open(directory)
	if errors.Is(err, os.ErrNotExist) {
		return ctx.Err()
	}
	if err != nil {
		return err
	}
	defer f.Close()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		names, readErr := f.Readdirnames(64)
		for _, name := range names {
			if err := ctx.Err(); err != nil {
				return err
			}
			candidate := filepath.Join(directory, name)
			absolute, err := filepath.Abs(candidate)
			if err != nil {
				return err
			}
			if absolute != reserved {
				if err := check(candidate); err != nil {
					return err
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			return ctx.Err()
		}
		if readErr != nil {
			return readErr
		}
	}
}

func primaryProjectionPath(cfg config.Config) string {
	return filepath.Join(cfg.Storage.DataDir, "primary-ledger-projection.db")
}

func prepareRuntimeAccountingState(ctx context.Context, cfg config.Config, native *ledger.Log, pressure *resourcePressureRuntime, members ...*replicationRuntime) (result runtimeAccountingState, resultLease *ledger.MaintenanceLease, resultHead ledger.Watermark, resultErr error) {
	path := standaloneProjectionPath(cfg)
	check := func() error { return ctx.Err() }
	if cfg.Replication != nil {
		if len(members) != 1 || members[0] == nil || members[0].role != replication.RolePrimary || members[0].publisher == nil {
			return runtimeAccountingState{}, nil, ledger.Watermark{}, errors.New("Primary disk accounting requires authenticated member ownership")
		}
		member := members[0]
		before := member.publisher.Snapshot()
		check = func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if before.Term != before.PromisedTerm || before.NodeID != cfg.Replication.NodeID || before.ClusterID != cfg.Replication.ClusterID || !samePrimaryStartupAuthority(before, member.publisher.Snapshot()) {
				return errors.New("Primary accounting construction role/term/identity changed")
			}
			return nil
		}
		path = primaryProjectionPath(cfg)
		if err := validateRuntimeDerivedPath(ctx, cfg, path); err != nil {
			return runtimeAccountingState{}, nil, ledger.Watermark{}, err
		}
	} else if err := validateStandaloneProjectionPath(ctx, cfg); err != nil {
		return runtimeAccountingState{}, nil, ledger.Watermark{}, err
	}
	if err := check(); err != nil {
		return runtimeAccountingState{}, nil, ledger.Watermark{}, err
	}
	releaseWorkspace, err := acquireProjectionWorkspace(ctx, cfg, pressure)
	if err != nil {
		return runtimeAccountingState{}, nil, ledger.Watermark{}, err
	}
	defer func() {
		if !errors.Is(resultErr, ownedfile.ErrRetirement) {
			resultErr = errors.Join(resultErr, releaseWorkspace())
		}
	}()
	owner := runtimeAccountingState{path: path, recovery: &atomic.Bool{}}
	owner.diskPermit, err = acquireLedgerProjectionDisk(ctx, cfg, pressure)
	if err != nil {
		return runtimeAccountingState{}, nil, ledger.Watermark{}, err
	}
	lease, err := native.BeginMaintenance(ctx)
	if err != nil {
		return runtimeAccountingState{}, nil, ledger.Watermark{}, errors.Join(err, owner.close())
	}
	complete := false
	defer func() {
		if !complete {
			if errors.Is(resultErr, ownedfile.ErrRetirement) {
				owner.retirementErr = errors.Join(owner.retirementErr, resultErr)
			}
			// Keep native exclusivity until candidate cleanup has really joined.
			resultErr = errors.Join(resultErr, owner.close(), lease.Close())
		}
	}()
	if replicated, err := lease.ValidateOwner(native); err != nil || replicated != (cfg.Replication != nil) {
		return runtimeAccountingState{}, nil, ledger.Watermark{}, errors.Join(err, errors.New("accounting native replication mode differs from configuration"))
	}
	var head ledger.Watermark
	err = lease.WithHead(ctx, func(ctx context.Context, frozen ledger.Watermark, hash [32]byte) error {
		if err := check(); err != nil {
			return err
		}
		if err := os.Remove(owner.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("discard prior derived Ledger file: %w", err)
		}
		var p *ledger.LedgerProjection
		var prefix ledger.ProjectionPrefix
		if owner.diskPermit != nil {
			p, prefix, err = native.BuildProjectionFromCursorWithLimit(ctx, frozen.Generation, frozen.Sequence, owner.path, cfg.ResourceBudget.LedgerProjectionDiskBytes)
		} else {
			p, prefix, err = native.BuildProjectionFromCursor(ctx, frozen.Generation, frozen.Sequence, owner.path)
		}
		if err != nil {
			return err
		}
		owner.projection, owner.reader = p, p
		if owner.diskPermit != nil {
			if err := owner.diskPermit.Reclassify(resourcebudget.Background); err != nil {
				return err
			}
		}
		owner.file, err = os.Lstat(owner.path)
		if err != nil {
			return err
		}
		if prefix.Head != frozen || prefix.Hash != hash {
			return errors.New("fresh Ledger projection differs from exclusive native head")
		}
		head = frozen
		return check()
	})
	if err != nil {
		return runtimeAccountingState{}, nil, ledger.Watermark{}, err
	}
	complete = true
	return owner, lease, head, nil
}

// Called before publishing any business/background reader. The constructor's
// native lease spans fresh build, price-pin repair and two-level recovery.
// Recovery does not pass through ordinary Manager observers, so catch Usage up
// synchronously before the default runtime starts producers or returns ready.
func recoverStandaloneAccounting(ctx context.Context, cfg config.Config, native *ledger.Log, owner runtimeAccountingState, lease *ledger.MaintenanceLease, pressure *resourcePressureRuntime) error {
	r := &Runtime{config: cfg, ledger: native, runtimeResourceOwnership: runtimeResourceOwnership{resourcePressure: pressure}, accountingState: owner}
	r.draining.Store(true)
	_, err := r.recoverProjectedAccounting(ctx, lease, owner.projection, budget.RecoveryStandalone)
	return err
}

func (s *runtimeAccountingState) close() error {
	if s.replica != nil {
		err := s.replica.Close()
		s.reader, s.projection, s.replica = nil, nil, nil
		return err
	}
	err := errors.Join(s.retirementErr, s.releaseStartupLease())
	if s.projection != nil {
		discardErr := s.projection.Discard()
		err = errors.Join(err, discardErr)
		if discardErr == nil {
			s.projection = nil
		}
	}
	if s.projection == nil && s.diskPermit != nil && s.retirementErr == nil {
		removed, statErr := derivedFileRemoved(s.path)
		err = errors.Join(err, statErr)
		if statErr == nil && removed {
			releaseErr := s.diskPermit.Release()
			err = errors.Join(err, releaseErr)
			if releaseErr == nil {
				s.diskPermit = nil
			}
		} else if statErr == nil {
			err = errors.Join(err, errors.New("derived Ledger file remains; retain its Disk claim"))
		}
	}
	if s.projection == nil {
		s.reader = nil
	}
	return err
}

func derivedFileRemoved(path string) (bool, error) {
	if path == "" {
		return true, nil
	}
	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, nil
}

func (s *runtimeAccountingState) releaseStartupLease() error {
	if s.startupLease == nil {
		return nil
	}
	err := s.startupLease.Close()
	s.startupLease = nil
	return err
}
