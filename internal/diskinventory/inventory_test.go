//go:build linux || darwin

package diskinventory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/resourcebudget"
	storelock "github.com/akz142857/Halro/internal/store/lock"
	"golang.org/x/sys/unix"
)

func inventoryManager(t *testing.T) *resourcebudget.Manager {
	t.Helper()
	l := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: 16 << 20, Disk: 3 << 30}, RecoveryReserve: resourcebudget.Cost{RAM: 8 << 20, Disk: 1 << 30}, MaxClaims: 16, RecoveryClaims: 4, MaxOwners: 4, MaxPressureAge: time.Minute}
	for i := range l.Classes {
		l.Classes[i] = l.Total
	}
	l.Classes[resourcebudget.Recovery] = l.RecoveryReserve
	l.Classes[resourcebudget.Control] = resourcebudget.Cost{RAM: int64(l.MaxClaims) * resourcebudget.ClaimRAMBytes}
	m, err := resourcebudget.New(l)
	if err != nil {
		t.Fatal(err)
	}
	return m // Unknown pressure deliberately exercises dedicated startup reserve
}

func inventoryFixture(t *testing.T) Options {
	t.Helper()
	dir := t.TempDir()
	g, err := storelock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := g.Close(); err != nil {
			t.Error(err)
		}
	})
	return Options{DataDir: dir, Guard: g, Budget: inventoryManager(t), Ceiling: 1 << 30, Deadline: time.Minute}
}

func assertNoUsage(t *testing.T, m *resourcebudget.Manager) {
	t.Helper()
	if s := m.Snapshot(); s.Used != (resourcebudget.Cost{}) || s.Owners != 0 || s.NormalClaims != 0 || s.RecoveryClaims != 0 {
		t.Fatalf("remaining fee %+v", s)
	}
}

func TestInventoryRebuildsExistingTreeAndKeepsPersistentFeesAfterClose(t *testing.T) {
	o := inventoryFixture(t)
	if err := os.MkdirAll(filepath.Join(o.DataDir, "old", "private"), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(o.DataDir, "old", "private", "replacement.pending")
	if err := os.WriteFile(path, []byte("unpublished old replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(o.DataDir, "sparse.wal"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(64 << 20); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := Reconcile(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	s := b.Snapshot()
	if s.Files != 3 || s.Directories != 3 || s.LogicalBytes < 64<<20 || s.ChargeBytes < s.LogicalBytes || s.ChargeBytes < s.AllocatedBytes {
		t.Fatalf("wrong actual tree %+v", s)
	}
	used := o.Budget.Snapshot()
	if used.Used != (resourcebudget.Cost{RAM: resourcebudget.ClaimRAMBytes, Disk: s.ChargeBytes}) || used.RecoveryClaims != 1 {
		t.Fatalf("wrong baseline fee %+v", used)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if o.Budget.Snapshot().Used != used.Used {
		t.Fatal("Close or disappeared name refunded persistent bytes")
	}
	if err := b.Publish(); err != nil {
		t.Fatal(err)
	}
	if err := b.Publish(); err != nil {
		t.Fatal(err)
	}
	published := o.Budget.Snapshot()
	if published.Used != used.Used || published.RecoveryClaims != 0 || published.NormalClaims != 1 || published.Classes[resourcebudget.Background].Disk != s.ChargeBytes {
		t.Fatalf("ownership changed bytes %+v", published)
	}
	// A fresh process Manager observes ACTUAL remaining names, not old counts or
	// copies of the preceding manager's in-memory Permit.
	o.Budget = inventoryManager(t)
	newOwner, err := Reconcile(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if got := newOwner.Snapshot(); got.Files != 2 || got.LogicalBytes >= s.LogicalBytes {
		t.Fatalf("stale inventory inherited %+v", got)
	}
}

func TestInventoryRefusesUnqualifiedAliasesAndObjectsWithoutMutation(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			o := inventoryFixture(t)
			original := filepath.Join(o.DataDir, "native")
			if err := os.WriteFile(original, []byte("native authority"), 0600); err != nil {
				t.Fatal(err)
			}
			foreign := filepath.Join(o.DataDir, "alias")
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink(original, foreign)
			case "hardlink":
				err = os.Link(original, foreign)
			case "fifo":
				err = unix.Mkfifo(foreign, 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			b, err := Reconcile(context.Background(), o)
			if b != nil || !errors.Is(err, ErrNamespace) {
				t.Fatalf("unqualified object accepted %v %v", b, err)
			}
			assertNoUsage(t, o.Budget)
			got, err := os.ReadFile(original)
			if err != nil || string(got) != "native authority" {
				t.Fatalf("read-only inventory mutated authority %q %v", got, err)
			}
			if _, err := os.Lstat(foreign); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestInventoryCeilingAndDepthRejectWithoutPartialClaim(t *testing.T) {
	for _, mode := range []string{"ceiling", "depth"} {
		t.Run(mode, func(t *testing.T) {
			o := inventoryFixture(t)
			if mode == "ceiling" {
				o.Ceiling = 1
			} else {
				dir := o.DataDir
				for i := 0; i < MaxDepth; i++ {
					dir = filepath.Join(dir, "d")
					if err := os.Mkdir(dir, 0700); err != nil {
						t.Fatal(err)
					}
				}
			}
			b, err := Reconcile(context.Background(), o)
			if b != nil || !errors.Is(err, ErrLimit) {
				t.Fatalf("limit accepted %v %v", b, err)
			}
			assertNoUsage(t, o.Budget)
		})
	}
}

func TestInventoryWrongGuardFailsBeforeIOAndCharge(t *testing.T) {
	o := inventoryFixture(t)
	other := inventoryFixture(t)
	o.Guard = other.Guard
	b, err := Reconcile(context.Background(), o)
	if b != nil || !errors.Is(err, ErrNamespace) {
		t.Fatalf("foreign guard accepted %v %v", b, err)
	}
	assertNoUsage(t, o.Budget)
}

func TestInventoryUnknownActualCloseKeepsWholeEnvelope(t *testing.T) {
	o := inventoryFixture(t)
	actualErr := errors.New("actual close outcome unknown")
	var held *os.File
	b, err := reconcile(context.Background(), o, func(f *os.File) error {
		if held == nil {
			held = f
			return actualErr
		}
		return f.Close()
	})
	if held == nil {
		t.Fatal("no actual descriptor")
	}
	defer held.Close()
	if b == nil || !errors.Is(err, ErrRetirement) || !errors.Is(err, actualErr) {
		t.Fatalf("lost actual IO owner %v %v", b, err)
	}
	if got := o.Budget.Snapshot().Used; got != (resourcebudget.Cost{RAM: WorkspaceRAMBytes + resourcebudget.ClaimRAMBytes, Disk: o.Ceiling}) {
		t.Fatalf("uncertain fee returned %+v", got)
	}
	if b.Snapshot() != (Snapshot{}) {
		t.Fatal("uncertain result became usable inventory")
	}
	if !errors.Is(b.Publish(), ErrRetirement) || !errors.Is(b.Close(), actualErr) {
		t.Fatal("uncertain result became healthy")
	}
}

func TestInventoryCancellationJoinsActualBlockedClose(t *testing.T) {
	for _, last := range []bool{false, true} {
		t.Run(strconv.FormatBool(last), func(t *testing.T) { testInventoryCancellationJoinsActualBlockedClose(t, last) })
	}
}

func testInventoryCancellationJoinsActualBlockedClose(t *testing.T, last bool) {
	o := inventoryFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var once sync.Once
	go func() {
		_, err := reconcile(ctx, o, func(f *os.File) error {
			if !last || f.Name() == o.DataDir {
				once.Do(func() { close(entered); <-release })
			}
			return f.Close()
		})
		done <- err
	}()
	<-entered
	cancel()
	select {
	case err := <-done:
		t.Fatalf("returned before actual Close %v", err)
	default:
	}
	if got := o.Budget.Snapshot().Used; got != (resourcebudget.Cost{RAM: WorkspaceRAMBytes + resourcebudget.ClaimRAMBytes, Disk: o.Ceiling}) {
		t.Fatalf("cancel retired physical job %+v", got)
	}
	close(release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	assertNoUsage(t, o.Budget)
}

func TestInventoryChangedDirectoryRefusesObservation(t *testing.T) {
	for _, last := range []bool{false, true} {
		t.Run(strconv.FormatBool(last), func(t *testing.T) { testInventoryChangedDirectoryRefusesObservation(t, last) })
	}
}

func testInventoryChangedDirectoryRefusesObservation(t *testing.T, last bool) {
	o := inventoryFixture(t)
	var once sync.Once
	b, err := reconcile(context.Background(), o, func(f *os.File) error {
		if !last || f.Name() == o.DataDir {
			once.Do(func() {
				if err := os.WriteFile(filepath.Join(o.DataDir, "late"), []byte("replacement"), 0600); err != nil {
					t.Error(err)
				}
			})
		}
		return f.Close()
	})
	if b != nil || !errors.Is(err, ErrNamespace) {
		t.Fatalf("moving tree accepted %v %v", b, err)
	}
	assertNoUsage(t, o.Budget)
}

func TestInventoryWideTreeUsesOnePersistentClaim(t *testing.T) {
	o := inventoryFixture(t)
	for i := 0; i < 2000; i++ {
		if err := os.WriteFile(filepath.Join(o.DataDir, strconv.Itoa(i)), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	b, err := Reconcile(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if b.Snapshot().Files != 2001 || o.Budget.Snapshot().Owners != 1 || o.Budget.Snapshot().RecoveryClaims != 1 {
		t.Fatalf("inventory retained per-file claims %+v %+v", b.Snapshot(), o.Budget.Snapshot())
	}
}

func TestInventoryFailedPublicationPreservesBaselineFee(t *testing.T) {
	o := inventoryFixture(t)
	b, err := Reconcile(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	before := o.Budget.Snapshot().Used
	o.Budget.CloseAdmission()
	if err := b.Publish(); !errors.Is(err, resourcebudget.ErrInvalid) {
		t.Fatal(err)
	}
	if o.Budget.Snapshot().Used != before {
		t.Fatal("failed role change erased persistent fee")
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestInventoryUnlinkedOpenOldInodeNeverRefundsBaseline(t *testing.T) {
	o := inventoryFixture(t)
	path := filepath.Join(o.DataDir, "native.wal")
	const oldBytes = "old still-open native bytes"
	if err := os.WriteFile(path, []byte(oldBytes), 0600); err != nil {
		t.Fatal(err)
	}
	old, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	b, err := Reconcile(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Publish(); err != nil {
		t.Fatal(err)
	}
	baseline := o.Budget.Snapshot().Used
	if err := o.Budget.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	// The replacement reserves an ADDITIONAL consumer growth envelope before
	// creating any bytes. The inventory baseline is never its replacement fee.
	growth, err := o.Budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("new native bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	var bytes [len(oldBytes)]byte
	if _, err := old.ReadAt(bytes[:], 0); err != nil {
		t.Fatal(err)
	}
	if string(bytes[:]) != oldBytes {
		t.Fatalf("old descriptor ceased to own actual bytes %q", bytes)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if got := o.Budget.Snapshot().Used; got.Disk != baseline.Disk+4096 {
		t.Fatalf("unlink/name replacement refunded old inode %+v", got)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := growth.Release(); err != nil {
		t.Fatal(err)
	}
	if got := o.Budget.Snapshot().Used; got != baseline {
		t.Fatalf("growth receipt erased baseline %+v", got)
	}
}
