//go:build linux

package resourcebudget

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// A fixed slot's control claim is retained past the caller's loan, so preparing
// or restoring it inside a parent workspace must still pay ordinary admission.
// A restarted HA start restores its bound cache targets inside the Directory
// catch-up workspace; a loan child there refused every restart after a failed one.
func TestPhysicalCandidateControlClaimIgnoresParentWorkspace(t *testing.T) {
	dir := t.TempDir()
	var fs unix.Statfs_t
	if err := unix.Statfs(dir, &fs); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("HALRO_EXT4_ROOT") == "" || fs.Type != unix.EXT4_SUPER_MAGIC {
		t.Skip("isolated ext4 fixture required")
	}
	l := fixtureLimits()
	l.Total.Disk = 1 << 20
	for i := range l.Classes {
		if Class(i) != Recovery && Class(i) != Control {
			l.Classes[i] = l.Total
		}
	}
	m, err := New(l)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SetPressure(Healthy); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "fixed-slot")
	const bytes = 64 << 10
	parent, err := m.TryAcquire(Background, Cost{Disk: bytes})
	if err != nil {
		t.Fatal(err)
	}
	w, err := m.TryAcquireCompletableWork(Active, Cost{RAM: 1}, Cost{RAM: 4096}, []WorkspaceRequirement{{Background, 4096}})
	if err != nil {
		t.Fatal(err)
	}
	var retained [2]Permit
	err = w.WithWorkspace(context.Background(), func(ctx context.Context) error {
		prepared, err := PreparePhysicalCandidate(ctx, path, bytes, m, parent)
		if err != nil {
			return err
		}
		if retained[0], err = prepared.RetainIndependentWorkspace(m); err != nil {
			return errors.Join(errors.New("prepared slot borrowed the parent loan"), err)
		}
		ref, err := prepared.Reference()
		if err != nil {
			return err
		}
		if err := prepared.ClosePreserving(); err != nil {
			return err
		}
		restored, err := RestorePhysicalCandidate(ctx, path, ref, m, parent)
		if err != nil {
			return err
		}
		if retained[1], err = restored.RetainIndependentWorkspace(m); err != nil {
			return errors.Join(errors.New("restored slot borrowed the parent loan"), err)
		}
		return restored.ClosePreserving()
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range retained {
		if err := p.Release(); err != nil {
			t.Fatal(err)
		}
	}
	if err := errors.Join(w.Close(), w.Active.Release()); err != nil {
		t.Fatal(err)
	}
}
