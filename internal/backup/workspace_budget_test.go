package backup

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/accountinglimits"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	bolt "go.etcd.io/bbolt"
)

func backupWorkspaceFixture(t *testing.T, parent context.Context, limit int64) (context.Context, *WorkspaceBudget, *resourcebudget.Manager) {
	t.Helper()
	limits := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: 1 << 30, Disk: 1 << 30}, RecoveryReserve: resourcebudget.Cost{RAM: 512 << 20, Disk: 512 << 20}, MaxClaims: 8, RecoveryClaims: 4, MaxOwners: 4, MaxPressureAge: time.Second}
	limits.Classes[resourcebudget.Recovery] = limits.RecoveryReserve
	limits.Classes[resourcebudget.Control] = resourcebudget.Cost{RAM: 8 * resourcebudget.ClaimRAMBytes}
	manager, err := resourcebudget.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	budget, err := NewWorkspaceBudget(parent, limit, func(ctx context.Context, bytes int64) (func() error, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ram, err := accountinglimits.ProjectionMappingRAMBytes(bytes)
		if err != nil {
			return nil, err
		}
		permit, err := manager.TryAcquireRecovery(resourcebudget.Cost{RAM: ram, Disk: bytes})
		if err != nil {
			return nil, err
		}
		return permit.Release, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := budget.Bind(parent)
	if err != nil {
		t.Fatal(err)
	}
	return ctx, budget, manager
}

func TestBackupWorkspaceIndexAdmissionAndRealReaderRetirement(t *testing.T) {
	ctx, _, manager := backupWorkspaceFixture(t, context.Background(), 1<<20)
	directory := t.TempDir()
	index, err := newObservationIndexForWorkspace(ctx, directory, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := index.Add(observationFact("data/one")); err != nil {
		t.Fatal(err)
	}
	if s := manager.Snapshot(); s.Classes[resourcebudget.Recovery].RAM != 33<<20 || s.Classes[resourcebudget.Recovery].Disk != 1<<20 {
		t.Fatal("private index escaped shared mapping/file fee", s)
	}
	db := index.db
	entered, resume, readDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		readDone <- db.View(func(*bolt.Tx) error { close(entered); <-resume; return nil })
	}()
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- index.Close() }()
	select {
	case err := <-closed:
		close(resume)
		t.Fatal("physical reader was not joined", err)
	case <-time.After(20 * time.Millisecond):
	}
	if s := manager.Snapshot(); s.RecoveryClaims != 1 || s.Classes[resourcebudget.Recovery].RAM != 33<<20 {
		close(resume)
		t.Fatal("blocked Close refunded live mapping", s)
	}
	close(resume)
	if err := <-readDone; err != nil {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 0 || manager.Snapshot().Used != (resourcebudget.Cost{}) {
		t.Fatal("actual retirement retained file or fee", entries, err, manager.Snapshot())
	}
}

func TestBackupWorkspaceConstructorDebtKeepsOriginalReservation(t *testing.T) {
	ctx, _, manager := backupWorkspaceFixture(t, context.Background(), 1<<20)
	directory := t.TempDir()
	var created string
	create := func(path string, flags int, mode os.FileMode) (*os.File, *ownedfile.Owner, error) {
		file, _, err := ownedfile.Create(path, flags, mode)
		if err != nil {
			t.Fatal(err)
		}
		created = path
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		return nil, nil, errors.Join(ownedfile.ErrRetirement, file.Close())
	}
	index, err := newObservationIndexWithCreate(ctx, directory, 0, create)
	if index != nil || !errors.Is(err, ownedfile.ErrRetirement) {
		t.Fatal("constructor debt disappeared", index, err)
	}
	if got := manager.Snapshot(); got.RecoveryClaims != 1 || got.Used != (resourcebudget.Cost{RAM: (33 << 20) + resourcebudget.ClaimRAMBytes, Disk: 1 << 20}) {
		t.Fatal("nil engine refunded original mapping/file fees", got)
	}
	if _, err := os.Stat(created); err != nil {
		t.Fatal("unknown constructed inode removed", err)
	}
}

func TestBackupWorkspaceRefusalReplacementAndMissingNameKeepOwnership(t *testing.T) {
	for _, mode := range []string{"refusal", "quota", "replacement", "rename"} {
		t.Run(mode, func(t *testing.T) {
			limit := int64(1 << 20)
			if mode == "quota" {
				limit = 4096
			}
			ctx, _, manager := backupWorkspaceFixture(t, context.Background(), limit)
			directory := t.TempDir()
			if mode == "refusal" {
				occupied, err := manager.TryAcquireRecovery(resourcebudget.Cost{RAM: 500 << 20})
				if err != nil {
					t.Fatal(err)
				}
				if index, err := newObservationIndexForWorkspace(ctx, directory, 0); index != nil || !errors.Is(err, resourcebudget.ErrExhausted) {
					t.Fatal("unpaid index reached scratch IO", index, err)
				}
				if err := occupied.Release(); err != nil {
					t.Fatal(err)
				}
			} else {
				index, err := newObservationIndexForWorkspace(ctx, directory, 0)
				if mode == "quota" {
					if index != nil || err == nil {
						t.Fatal("undersized index file ceiling admitted", index, err)
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					path := index.path
					if err := os.Rename(path, path+".owned"); err != nil {
						t.Fatal(err)
					}
					if mode == "replacement" {
						if err := os.WriteFile(path, []byte("foreign"), 0600); err != nil {
							t.Fatal(err)
						}
					}
					for n := 0; n < 2; n++ {
						if err := index.Close(); !errors.Is(err, ownedfile.ErrReplaced) || manager.Snapshot().RecoveryClaims != 1 {
							t.Fatal("uncertain/repeated retirement returned success or refunded", err, manager.Snapshot())
						}
					}
					if _, err := os.Stat(path + ".owned"); err != nil {
						t.Fatal("renamed owned file removed without its receipt", err)
					}
					if mode == "replacement" {
						raw, err := os.ReadFile(path)
						if err != nil || string(raw) != "foreign" {
							t.Fatal("foreign replacement removed", err)
						}
					}
					return
				}
			}
			entries, err := os.ReadDir(directory)
			if err != nil || len(entries) != 0 || manager.Snapshot().Used != (resourcebudget.Cost{}) {
				t.Fatal("admission/constructor refusal leaked scratch or fees", entries, err, manager.Snapshot())
			}
		})
	}
}

func TestBackupWorkspacePinnedHandleAndReportKeepOriginalBudget(t *testing.T) {
	options := manifestLimitOptions(t)
	if _, err := CreatePaged(options); err != nil {
		t.Fatal(err)
	}
	root, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx, _, manager := backupWorkspaceFixture(t, root, 1<<20)
	h, err := OpenVerified(ctx, options.OutputPath, options.BackupKey)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if manager.Snapshot().RecoveryClaims != 1 {
		t.Fatal("verification did not retire speculative index", manager.Snapshot())
	}
	if err := h.Extract(context.Background(), filepath.Join(t.TempDir(), "stage")); err != nil {
		t.Fatal(err)
	}
	if s := manager.Snapshot(); s.RecoveryClaims != 1 || s.Acquired[resourcebudget.Recovery] != 3 {
		t.Fatal("method context escaped original index budget", s)
	}
	other, _, _ := backupWorkspaceFixture(t, context.Background(), 1<<20)
	destination := filepath.Join(t.TempDir(), "other")
	if err := h.Extract(other, destination); err == nil {
		t.Fatal("substituted budget was accepted")
	}
	if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed identity check reached destination IO", err)
	}
	cancel()
	if err := h.Extract(context.Background(), destination); !errors.Is(err, context.Canceled) {
		t.Fatal("method lost original job cancellation", err)
	}
	if err := h.Close(); err != nil || manager.Snapshot().Used != (resourcebudget.Cost{}) {
		t.Fatal("cancelled owner did not physically retire", err, manager.Snapshot())
	}
	ctx, _, manager = backupWorkspaceFixture(t, context.Background(), 1<<20)
	var output bytes.Buffer
	if err := WriteVerifiedReport(ctx, options.OutputPath, options.BackupKey, &output); err != nil || output.Len() == 0 || manager.Snapshot().Used != (resourcebudget.Cost{}) || manager.Snapshot().Acquired[resourcebudget.Recovery] != 3 {
		t.Fatal("report indexes/spool escaped shared actual retirement", err, manager.Snapshot())
	}
}

func TestBackupWorkspaceLateIndexRetirementErrorSuppressesReport(t *testing.T) {
	options := manifestLimitOptions(t)
	if _, err := CreatePaged(options); err != nil {
		t.Fatal(err)
	}
	ctx, original, manager := backupWorkspaceFixture(t, context.Background(), 1<<20)
	injected := errors.New("index fee release failed")
	budget, err := NewWorkspaceBudget(context.Background(), 1<<20, func(ctx context.Context, n int64) (func() error, error) {
		release, err := original.reserve(ctx, n)
		if err != nil {
			return nil, err
		}
		return func() error { return errors.Join(release(), injected) }, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := budget.Bind(ctx); err == nil {
		t.Fatal("nested scope substituted the original callback/manager")
	}
	ctx, err = budget.Bind(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := WriteVerifiedReport(ctx, options.OutputPath, options.BackupKey, &output); !errors.Is(err, injected) || output.Len() != 0 || manager.Snapshot().Used != (resourcebudget.Cost{}) {
		t.Fatal("failed candidate retirement published a report", err, output.Len(), manager.Snapshot())
	}
}
