package usage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestDiskArchiveOperationsReserveAndReleaseSharedScratch(t *testing.T) {
	f := diskExportFixture(t, 3)
	var amounts []int64
	releases := 0
	e, err := NewExporterWithOptions(filepath.Join(t.TempDir(), "archive"), Options{
		ReserveArchiveDisk: func(_ context.Context, amount int64) (func() error, error) {
			amounts = append(amounts, amount)
			return func() error { releases++; return nil }, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := e.ExportDisk(context.Background(), f.p, DefaultDiskExportLimits()); err != nil {
		t.Fatal(err)
	}
	if len(amounts) != 1 || amounts[0] != DefaultDiskExportLimits().WriteBytes || releases != 1 {
		t.Fatal("export reservation did not span the operation", amounts, releases)
	}

	amounts = nil
	if _, err := e.VerifyDiskArchive(context.Background(), f.p, DefaultArchiveVerifyLimits()); err != nil {
		t.Fatal(err)
	}
	want := DefaultArchiveVerifyLimits().IndexBytes + DefaultArchiveVerifyLimits().FileBytes
	if len(amounts) != 1 || amounts[0] != want || releases != 2 {
		t.Fatal("verification reservation did not span scratch cleanup", amounts, releases)
	}
}

func TestDiskArchiveReservationFailurePreventsWrites(t *testing.T) {
	f := diskExportFixture(t, 1)
	e, err := NewExporterWithOptions(filepath.Join(t.TempDir(), "archive"), Options{
		ReserveArchiveDisk: func(context.Context, int64) (func() error, error) {
			return nil, ErrDiskUsageLimit
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.ExportDisk(context.Background(), f.p, DefaultDiskExportLimits()); !errors.Is(err, ErrDiskUsageLimit) {
		t.Fatal("export proceeded without a shared disk reservation", err)
	}
	if _, err := e.ReadArchivePosition(context.Background()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("reservation failure published an archive position", err)
	}
}

// A date sweep retains its comparison and reference sessions together. Under
// an archive ceiling both are bounded by that ceiling rather than by the 4GiB
// index defaults, so a configured IO envelope can admit them at all.
func TestDateSweepReservationsFollowArchiveCeiling(t *testing.T) {
	f, e := dateActivationFixture(t, FormatNDJSON)
	const ceiling = 64 << 20
	e.archiveDiskCeiling = ceiling
	var ram, disk int64
	e.archiveWorkRAMReserve = func(_ context.Context, amount int64) (func() error, error) {
		ram += amount
		return func() error { ram -= amount; return nil }, nil
	}
	e.archiveDiskReserve = func(_ context.Context, amount int64) (func() error, error) {
		disk += amount
		return func() error { disk -= amount; return nil }, nil
	}
	ctx := context.Background()
	finishDateVerification(t, e, f.p, DefaultDateCatalogVerifyLimits())
	if _, err := e.ActivateRetainedDateCatalog(ctx, f.p, archiveDateCutoff); err != nil {
		t.Fatal(err)
	}
	limits := DefaultDateCatalogVerifyLimits()
	limits.Source.Rows, limits.Source.PageRows = 1, 1
	if out, err := e.VerifyPublishedDateCatalogStep(ctx, f.p, archiveDateCutoff, limits); err != nil || out.Phase == "ready" {
		t.Fatal(out, err)
	}
	finishDateReferences(t, e, DefaultDateCatalogReferenceLimits())
	wantRAM, wantDisk := DateMaintenanceWorkspace(ceiling)
	if wantRAM != ceiling+archiveVerifyDecoderRAMBytes || wantDisk != 3*ceiling {
		t.Fatal("date sweep workspace is not bounded by the archive ceiling", wantRAM, wantDisk)
	}
	if e.dateVerification == nil || e.dateReferences == nil || ram != wantRAM || disk != wantDisk {
		t.Fatal("retained date sessions did not reserve the ceiling-bounded workspace", ram, disk, wantRAM, wantDisk)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if ram != 0 || disk != 0 {
		t.Fatal("Close kept date sweep reservations", ram, disk)
	}
}

// Window verification splits the ceiling's remaining space between its index
// and one spool; the decoder workspace follows that bounded spool.
func TestWindowVerificationRAMFollowsBoundedSpool(t *testing.T) {
	f := diskExportFixture(t, 3)
	const ceiling = 64 << 20
	var amounts []int64
	e, err := NewExporterWithOptions(filepath.Join(t.TempDir(), "archive"), Options{
		ArchiveDiskBytes: ceiling,
		ReserveArchiveWorkRAM: func(_ context.Context, amount int64) (func() error, error) {
			amounts = append(amounts, amount)
			return func() error { return nil }, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.ExportDisk(context.Background(), f.p, DefaultDiskExportLimits()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.VerifyDiskArchive(context.Background(), f.p, DefaultArchiveVerifyLimits()); err != nil {
		t.Fatal(err)
	}
	if len(amounts) != 1 || amounts[0] > ceiling/2+archiveVerifyDecoderRAMBytes {
		t.Fatal("window verification reserved decoder RAM for an unbounded spool", amounts)
	}
}
