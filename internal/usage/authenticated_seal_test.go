package usage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func TestAuthenticatedUsageBuildBindsCompleteReducerImageToLedgerCut(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	nativePath := filepath.Join(root, "ledger.wal")
	ledgerKey := bytes.Repeat([]byte{0x63}, 32)
	native, err := ledger.OpenWithOptions(nativePath, ledger.NewStatus(), ledger.Options{ChainKey: ledgerKey})
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	for _, record := range finalizationFixture(true, false) {
		if _, err := native.Append(ctx, record.Event); err != nil {
			t.Fatal(err)
		}
	}
	report, partial, err := ledger.InspectReplayAuthenticated(nativePath, ledgerKey, nil)
	if err != nil || partial {
		t.Fatal(partial, err)
	}
	limits := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: 256 << 20, Disk: 256 << 20}, MaxClaims: 32, MaxOwners: 8, MaxPressureAge: time.Hour}
	for i := range limits.Classes {
		limits.Classes[i] = limits.Total
	}
	limits.Classes[resourcebudget.Recovery] = resourcebudget.Cost{}
	limits.Classes[resourcebudget.Control] = resourcebudget.Cost{RAM: 32 * resourcebudget.ClaimRAMBytes}
	budget, err := resourcebudget.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	if err := budget.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	ledgerIdentity := sha256.Sum256([]byte("usage-seal-ledger-identity"))
	ledgerProjection, err := ledger.BuildAuthenticatedLedgerProjection(ctx, nativePath, ledgerKey, filepath.Join(root, "ledger-auth"), report, ledger.AuthenticatedLedgerOptions{
		Identity: ledgerIdentity, MaxDiskBytes: 64 << 20, MaxAppendBytes: 8 << 20, Budget: budget,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := ledgerProjection.Discard(); err != nil {
			t.Error(err)
		}
	}()
	credit, err := budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer credit.Release()
	usageProjection, err := BuildAuthenticatedUsageProjection(ctx, AuthenticatedUsageOptions{
		Native: native, Owner: ledgerProjection, Identity: sha256.Sum256([]byte("usage-seal-instance")),
		UsageKey: bytes.Repeat([]byte{0x91}, 32), ScratchPath: filepath.Join(root, "usage-scratch.db"),
		RootDirectory: filepath.Join(root, "usage-auth"), ScratchMaxDiskBytes: 16 << 20,
		RootMaxDiskBytes: 32 << 20, RootMaxAppendBytes: 4 << 20,
		ScratchReservation: credit, Budget: budget,
	})
	if err != nil {
		t.Fatal(err)
	}
	reference := usageProjection.SealReference()
	ledgerReference, err := ledgerProjection.SealReference(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if reference.Schema.Component == 0 || reference.Root.Component != reference.Schema.Component || reference.Root.Rows == 0 ||
		reference.Position.Component != reference.Schema.Component || reference.Position.Sequence != ledgerReference.Anchor.Head.Sequence ||
		reference.Position.Generation != ledgerReference.Anchor.Head.Generation || reference.Position.Offset != uint64(ledgerReference.Anchor.Head.Offset) ||
		reference.Position.Hash != ledgerReference.Anchor.Hash {
		t.Fatalf("Usage root is not bound to the selected Ledger cut: %+v", reference)
	}
	if err := usageProjection.Close(); err != nil {
		t.Fatal(err)
	}
	rootReservation, err := budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: 32 << 20})
	if err != nil {
		t.Fatal(err)
	}
	wrong := reference
	wrong.Root.Digest[0] ^= 1
	if reopened, err := OpenAuthenticatedUsageProjection(ctx, filepath.Join(root, "usage-auth"), sha256.Sum256([]byte("usage-seal-instance")), bytes.Repeat([]byte{0x91}, 32), ledgerReference, wrong, 32<<20, 4<<20, rootReservation, budget); err == nil || reopened != nil {
		t.Fatalf("reopen accepted a non-selected Usage root: reopened=%v err=%v", reopened, err)
	}
	reopened, err := OpenAuthenticatedUsageProjection(ctx, filepath.Join(root, "usage-auth"), sha256.Sum256([]byte("usage-seal-instance")), bytes.Repeat([]byte{0x91}, 32), ledgerReference, reference, 32<<20, 4<<20, rootReservation, budget)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.SealReference() != reference {
		t.Fatal("reopen changed the selected Usage receipt")
	}
	imagePath := filepath.Join(root, "restored-usage.db")
	live, err := reopened.RestoreDiskProjection(ctx, imagePath, ledgerProjection, 16<<20, credit, budget)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Discard()
	anchor, err := live.ReadReplayAnchor(ctx)
	if err != nil || anchor != ledgerReference.Anchor {
		t.Fatalf("live restored cut: %+v %v", anchor, err)
	}
	if _, err := reopened.RestoreDiskProjection(ctx, imagePath, ledgerProjection, 16<<20, credit, budget); err == nil {
		t.Fatal("existing live file replaced")
	}
	event := finalizationFixture(false, false)[0].Event
	event.EventID, event.RequestID = "after_restore_accepted", "req_after_restore"
	record, _, err := native.AppendRecordWithReceipt(ctx, event)
	if err != nil {
		t.Fatal(err)
	}
	permitted := ledger.ProjectionPrefix{Head: ledger.Watermark{Generation: record.Generation, Sequence: record.Sequence, Offset: record.Offset}, Hash: record.Hash}
	if _, err := ledgerProjection.AdvanceFromNative(ctx, native, permitted); err != nil {
		t.Fatal(err)
	}
	if err := live.ApplyContext(ctx, record); err != nil {
		t.Fatal(err)
	}
	if err := live.ApplyContext(ctx, record); err != nil {
		t.Fatalf("durable retry: %v", err)
	}
	stats, err := live.ReadStats(ctx)
	if err != nil || stats.Watermark != permitted.Head || stats.Metrics.ActiveRequests != 1 {
		t.Fatalf("restored owner did not continue: %+v %v", stats, err)
	}
	if _, err := reopened.RestoreDiskProjection(ctx, filepath.Join(root, "stale.db"), ledgerProjection, 16<<20, credit, budget); err == nil {
		t.Fatal("old image accepted under a different Ledger cut")
	}
	if _, err := os.Stat(filepath.Join(root, "stale.db")); !os.IsNotExist(err) {
		t.Fatal("cut rejection created output", err)
	}
	if err := live.Discard(); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	if err := usageProjection.Discard(); err != nil {
		t.Fatal(err)
	}
	if err := rootReservation.Release(); err != nil {
		t.Fatal(err)
	}
}
