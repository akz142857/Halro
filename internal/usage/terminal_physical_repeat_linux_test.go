//go:build linux

package usage

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/ownedfile"
)

func TestUsagePhysicalRepeatedEnableAfterActualCommonKeepsOriginalOwnersAndFees(t *testing.T) {
	f := newUsagePhysicalFixture(t)
	ctx := context.Background()
	raw, _ := json.Marshal(f.seal)
	q, work := f.p.physical, f.p.physical.work
	used := f.manager.Snapshot().Used
	if err := f.p.EnablePhysicalCatchup(ctx, f.native, f.owner, sha256.Sum256(raw)); err != nil {
		t.Fatal("actual same original preparation", err)
	}
	if f.p.physical != q || q.work != work || f.manager.Snapshot().Used != used {
		t.Fatal("repeat created or charged replacement owners")
	}
	if err := f.p.ActivateTypedLive(ctx, f.native, f.manager); err != nil {
		t.Fatal(err)
	}
	if err := f.lease.Close(); err != nil {
		t.Fatal(err)
	}
	f.publishAndRetire(t)
	seal, found, err := f.publisher.Current()
	if err != nil || !found {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(seal)
	used = f.manager.Snapshot().Used
	if err := f.p.EnablePhysicalCatchup(ctx, f.native, f.owner, sha256.Sum256(raw)); err != nil {
		t.Fatal("late Enable after actual new common", err)
	}
	if f.p.physical != q || q.work != work || q.remaining != 0 || !q.installed || f.manager.Snapshot().Used != used {
		t.Fatal("repeat lost original source/pool/captured controls or changed fees")
	}
	if err := f.p.EnablePhysicalCatchup(ctx, f.native, f.owner, [32]byte{99}); err == nil {
		t.Fatal("wrong independent selection admitted repeat")
	}
	if f.manager.Snapshot().Used != used {
		t.Fatal("wrong caller selection retained guessed work")
	}
}

func TestUsagePhysicalRepeatedEnableForeignControlRetainsAllOriginalDebt(t *testing.T) {
	f := newUsagePhysicalFixture(t)
	ctx := context.Background()
	raw, _ := json.Marshal(f.seal)
	control := filepath.Join(f.root, "usage", "terminal.physical.0")
	if err := os.Rename(control, control+".original"); err != nil {
		t.Fatal(err)
	}
	foreign := []byte("foreign control must survive")
	if err := os.WriteFile(control, foreign, 0600); err != nil {
		t.Fatal(err)
	}
	q, work := f.p.physical, f.p.physical.work
	used := f.manager.Snapshot().Used
	if err := f.p.EnablePhysicalCatchup(ctx, f.native, f.owner, sha256.Sum256(raw)); !errors.Is(err, ownedfile.ErrReplaced) {
		t.Fatal("replaced actual control admitted repeat", err)
	}
	afterFailure := f.manager.Snapshot().Used
	if q.failure == nil || q.closedQualified || f.p.physical != q || q.work != work || afterFailure.RAM < used.RAM || afterFailure.Disk != used.Disk {
		t.Fatalf("unknown actual control lost lifecycle: failure=%v qualified=%v sameOwner=%v sameWork=%v before=%+v after=%+v", q.failure, q.closedQualified, f.p.physical == q, q.work == work, used, f.manager.Snapshot().Used)
	}
	if err := f.p.Close(); err == nil {
		t.Fatal("unknown control Close reported known retirement")
	}
	after, err := os.ReadFile(control)
	if err != nil || string(after) != string(foreign) {
		t.Fatal("foreign control deleted or changed", err)
	}
	if f.manager.Snapshot().Used != afterFailure {
		t.Fatal("failed actual Close guessed debt refund", afterFailure, f.manager.Snapshot().Used)
	}
}
