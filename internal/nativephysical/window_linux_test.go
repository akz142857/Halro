//go:build linux

package nativephysical

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"golang.org/x/sys/unix"
)

func physicalFixture(t *testing.T) (*os.File, Options, []byte) {
	t.Helper()
	if os.Getenv("HALRO_EXT4_ROOT") == "" {
		t.Skip("isolated actual ext4 fixture required")
	}
	path := filepath.Join(t.TempDir(), "native")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	o, key := optionsForActualFile(t, f)
	return f, o, key
}
func optionsForActualFile(t *testing.T, f *os.File) (Options, []byte) {
	t.Helper()
	l := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: 128 << 20, Disk: 64 << 20}, MaxClaims: 32, MaxOwners: 16, MaxPressureAge: time.Hour}
	for i := range l.Classes {
		l.Classes[i] = l.Total
	}
	l.Classes[resourcebudget.Recovery] = resourcebudget.Cost{}
	l.Classes[resourcebudget.Control] = resourcebudget.Cost{RAM: 32 * resourcebudget.ClaimRAMBytes}
	m, err := resourcebudget.New(l)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	d, err := m.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: 32 << 20})
	if err != nil {
		t.Fatal(err)
	}
	c, err := m.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: ControlDiskBytes})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Release(); _ = c.Release() })
	key := bytes.Repeat([]byte{0x32}, 32)
	o := Options{Manager: m, NativeDisk: d, ControlDisk: c, MaxDiskBytes: 32 << 20, AppendBytes: 4096, WindowBytes: 8192, RAMBytes: 1 << 20, Identity: [32]byte{1}}
	o.Authenticate = func(ctx context.Context, p Position) error {
		if p.EOF == 0 && p.Sequence == 0 && p.Hash == ([32]byte{}) {
			return ctx.Err()
		}
		if p.EOF != 4096 || p.Sequence != 1 {
			return ErrOwnership
		}
		var raw [4096]byte
		if _, err := io.ReadFull(io.NewSectionReader(f, 0, 4096), raw[:]); err != nil {
			return err
		}
		h := hmac.New(sha256.New, key)
		h.Write(raw[:4064])
		if !hmac.Equal(h.Sum(nil), raw[4064:]) || sha256.Sum256(raw[:]) != p.Hash {
			return ErrOwnership
		}
		return ctx.Err()
	}
	return o, key
}
func fillActualFilesystem(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(filepath.Dir(path), "foreign"), os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_SYNC, 0600)
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 1<<20)
	n := len(chunk)
	for {
		_, err := f.Write(chunk[:n])
		if err == nil {
			continue
		}
		if !errors.Is(err, syscall.ENOSPC) {
			t.Fatal(err)
		}
		if n > 4096 {
			n = 4096
			continue
		}
		break
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(path, &fs); err != nil || fs.Type != unix.EXT4_SUPER_MAGIC || fs.Bavail != 0 {
		t.Fatal("not actual full ext4", fs.Bavail, err)
	}
	return f
}
func TestOriginalPhysicalWindowFullFilesystemFiniteEscrowAndRestart(t *testing.T) {
	f, o, key := physicalFixture(t)
	ctx := context.Background()
	w, err := Prepare(ctx, f.Name(), f, key, Position{}, o)
	if err != nil {
		t.Fatal(err)
	}
	before := o.Manager.Snapshot()
	r1, err := w.Reserve(Position{}, 4096)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := w.Reserve(Position{}, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Reserve(Position{}, 1); !errors.Is(err, resourcebudget.ErrPhysicalAllocation) {
		t.Fatal("unbounded admission", err)
	}
	filler := fillActualFilesystem(t, f.Name())
	if err = o.Manager.SetPressure(resourcebudget.Pressured); err != nil {
		t.Fatal(err)
	}
	var raw [4096]byte
	copy(raw[:], "actual authenticated native frame")
	h := hmac.New(sha256.New, key)
	h.Write(raw[:4064])
	copy(raw[4064:], h.Sum(nil))
	if err = r1.BeforeWrite(Position{}, 4096); err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteAt(raw[:], 0); err != nil {
		t.Fatal(err)
	}
	if err = f.Sync(); err != nil {
		t.Fatal(err)
	}
	if err = r1.Durable(); err != nil {
		t.Fatal(err)
	}
	if err = r2.Release(); err != nil {
		t.Fatal(err)
	}
	head := Position{Sequence: 1, Hash: sha256.Sum256(raw[:]), EOF: 4096}
	if err = w.Checkpoint(ctx, head); err != nil {
		t.Fatal("actual full filesystem receipt-bank renewal", err)
	}
	if after := o.Manager.Snapshot(); after.Used != before.Used || after.NormalClaims != before.NormalClaims {
		t.Fatal("renew acquired fresh fee", before, after)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	if err = w.CloseAfterNative(nil); err != nil {
		t.Fatal(err)
	}
	if err = filler.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filler.Name()); err != nil {
		t.Fatal(err)
	}
	if err = o.Manager.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	f, err = os.OpenFile(f.Name(), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	o.Restart = true
	o.Authenticate = func(ctx context.Context, p Position) error {
		var actual [4096]byte
		_, err := io.ReadFull(io.NewSectionReader(f, 0, 4096), actual[:])
		if err != nil || p != head || actual != raw {
			return errors.Join(ErrOwnership, err)
		}
		return ctx.Err()
	}
	w, err = Prepare(ctx, f.Name(), f, key, head, o)
	if err != nil {
		t.Fatal("authenticated original birth/prefix restart", err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	if err = w.CloseAfterNative(nil); err != nil {
		t.Fatal(err)
	}
}
func TestPhysicalWindowForeignReplacementRetainsOriginalDebt(t *testing.T) {
	f, o, key := physicalFixture(t)
	w, err := Prepare(context.Background(), f.Name(), f, key, Position{}, o)
	if err != nil {
		t.Fatal(err)
	}
	before := o.Manager.Snapshot().Used
	if err := os.Rename(w.controlPath(0), w.controlPath(0)+".original"); err != nil {
		t.Fatal(err)
	}
	foreign := []byte("foreign control")
	if err := os.WriteFile(w.controlPath(0), foreign, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Reserve(Position{}, 1); !errors.Is(err, ownedfile.ErrReplaced) {
		t.Fatal("foreign bank allowed", err)
	}
	w.Hold(ownedfile.ErrReplaced)
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	if err = w.CloseAfterNative(nil); !errors.Is(err, ownedfile.ErrRetirement) {
		t.Fatal("unknown ownership erased", err)
	}
	if o.Manager.Snapshot().Used != before || w.key == ([32]byte{}) {
		t.Fatal("unknown debt or key returned")
	}
	got, err := os.ReadFile(w.controlPath(0))
	if err != nil || !bytes.Equal(got, foreign) {
		t.Fatal("foreign file touched", err)
	}
}
func TestPhysicalWindowInterruptedCreatorPreservedInFiniteOpaqueSlots(t *testing.T) {
	f, o, key := physicalFixture(t)
	o.Phase = func(stage string) error {
		if stage == "bank-0-created" {
			return errors.New("interrupted creator")
		}
		return nil
	}
	w, err := Prepare(context.Background(), f.Name(), f, key, Position{}, o)
	if err == nil || w == nil {
		t.Fatal("missing partial owner")
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	if err = w.CloseAfterNative(nil); err != nil {
		t.Fatal(err)
	}
	f, err = os.OpenFile(f.Name(), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	o.Restart = true
	o.Phase = nil
	w, err = Prepare(context.Background(), f.Name(), f, key, Position{}, o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.Name()+".physical-unqualified-0", "bank-0")); err != nil {
		t.Fatal("unknown creator was not preserved", err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	if err = w.CloseAfterNative(nil); err != nil {
		t.Fatal(err)
	}
}

func TestPhysicalWindowActualSIGKILLRecovery(t *testing.T) {
	if os.Getenv("HALRO_EXT4_ROOT") == "" {
		t.Skip("isolated actual ext4 fixture required")
	}
	for _, stage := range []string{"bank-0-created", "bank-1-created", "receipt-synced", "native-allocated"} {
		t.Run(stage, func(t *testing.T) {
			f, o, key := physicalFixture(t)
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(exe, "-test.run", "^TestPhysicalWindowSIGKILLChild$")
			cmd.Env = append(os.Environ(), "HALRO_NATIVE_PHYSICAL_KILL_PATH="+f.Name(), "HALRO_NATIVE_PHYSICAL_KILL_STAGE="+stage)
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
				t.Fatalf("not actual SIGKILL: %v %s", err, out)
			}
			o.Restart = true
			w, err := Prepare(context.Background(), f.Name(), f, key, Position{}, o)
			if err != nil {
				t.Fatal("fresh original FD authenticated recovery", err)
			}
			if err = f.Close(); err != nil {
				t.Fatal(err)
			}
			if err = w.CloseAfterNative(nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestPhysicalWindowSIGKILLChild(t *testing.T) {
	path := os.Getenv("HALRO_NATIVE_PHYSICAL_KILL_PATH")
	stage := os.Getenv("HALRO_NATIVE_PHYSICAL_KILL_STAGE")
	if path == "" || stage == "" {
		t.Skip("actual child only")
	}
	f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err != nil {
		t.Fatal(err)
	}
	o, key := optionsForActualFile(t, f)
	o.Phase = func(s string) error {
		if s != stage {
			return nil
		}
		if err := unix.Kill(os.Getpid(), unix.SIGKILL); err != nil {
			return err
		}
		select {}
	}
	_, err = Prepare(context.Background(), path, f, key, Position{}, o)
	t.Fatalf("actual kill stage was not reached: %s %v", stage, err)
}
