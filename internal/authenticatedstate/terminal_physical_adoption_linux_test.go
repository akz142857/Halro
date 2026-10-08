//go:build linux

package authenticatedstate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/ownedfile"
)

func TestPhysicalClosedBorrowerTransfersOpaqueFeeToActualCreator(t *testing.T) {
	requirePhysicalReceiptExt4(t)
	creator, o := testStoreWithoutCleanup(t, 3)
	ctx := context.Background()
	root, err := creator.RootIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = creator.Close(); err != nil {
		t.Fatal(err)
	}
	o.ExpectedRoot = &root
	o.DiskReservation, err = creator.DiskReservation()
	if err != nil {
		t.Fatal(err)
	}
	defer o.DiskReservation.Release()
	borrower, err := OpenStore(ctx, creator.dir, o)
	if err != nil {
		t.Fatal(err)
	}
	nativePath := filepath.Join(filepath.Dir(creator.dir), "native")
	if err = os.WriteFile(nativePath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	native, verify := receiptNativeFixture(t, nativePath)
	if _, err = borrower.EnablePhysicalTerminalQualified(ctx, nextCut(99), native, verify); err != nil {
		t.Fatal(err)
	}
	// This bounded opaque name is never authenticated or deleted. Its real
	// existence must keep the original aggregate Disk fee after Close/adoption.
	opaque := filepath.Join(creator.dir, "terminal-unqualified-0")
	if err = os.Mkdir(opaque, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(opaque, "terminal.physical.0"), []byte("opaque"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := borrower.physical.fee.Cost()
	if err != nil {
		t.Fatal(err)
	}
	originalOwner := borrower.physical
	if err = borrower.Close(); err != nil {
		t.Fatal(err)
	}
	if borrower.physical.feeRetired {
		t.Fatal("opaque persistent targets lost fee")
	}
	if err = creator.AdoptClosedSuccessor(borrower); err != nil {
		t.Fatal(err)
	}
	if creator.physical != originalOwner || borrower.physical != nil {
		t.Fatal("actual physical owner was dropped/copied rather than transferred")
	}
	fee, err := creator.physical.fee.Cost()
	if err != nil || fee.Disk != before.Disk || fee.RAM != 0 {
		t.Fatal("transferred fee changed", fee, err)
	}
	if err = creator.Discard(); !errors.Is(err, ownedfile.ErrReplaced) {
		t.Fatal("opaque inode gained deletion authority", err)
	}
	raw, err := os.ReadFile(filepath.Join(opaque, "terminal.physical.0"))
	if err != nil || string(raw) != "opaque" {
		t.Fatal("opaque inode changed", err)
	}
	if _, err = creator.physical.fee.Cost(); err != nil {
		t.Fatal("failed unknown retirement refunded original fee", err)
	}
}
