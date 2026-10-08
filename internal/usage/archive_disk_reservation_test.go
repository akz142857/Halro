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
