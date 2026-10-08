package usage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestArchiveScratchRetirementPreservesUnknownAndReplacementInodes(t *testing.T) {
	for _, fault := range []string{"unknown-entry", "index-replacement", "directory-replacement", "already-closed"} {
		t.Run(fault, func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), "owned")
			if err := os.Mkdir(directory, 0700); err != nil {
				t.Fatal(err)
			}
			identity, err := os.Lstat(directory)
			if err != nil {
				t.Fatal(err)
			}
			index, err := newArchiveVerifyIndex(filepath.Join(directory, "index.bin"), 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			var released int
			s := archiveVerifyScratch{directory: directory, identity: identity, index: index,
				releaseShared: func() error { released++; return nil },
				releaseRAM:    func() error { released++; return nil }}
			foreign := filepath.Join(directory, "foreign")
			switch fault {
			case "unknown-entry":
				err = os.WriteFile(foreign, []byte("keep"), 0600)
			case "index-replacement":
				err = os.Rename(index.file.Name(), index.file.Name()+".owned")
				foreign = index.file.Name()
				if err == nil {
					err = os.WriteFile(foreign, []byte("keep"), 0600)
				}
			case "directory-replacement":
				err = os.Rename(directory, directory+".owned")
				if err == nil {
					err = os.Mkdir(directory, 0700)
				}
				if err == nil {
					err = os.WriteFile(foreign, []byte("keep"), 0600)
				}
			case "already-closed":
				err = index.file.Close()
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := s.closeScratch(); err == nil || released != 0 {
				t.Fatal("unretired scratch refunded its whole-job fees", err, released)
			}
			if fault != "already-closed" {
				raw, err := os.ReadFile(foreign)
				if err != nil || string(raw) != "keep" {
					t.Fatal("retirement removed foreign bytes", err)
				}
			}
		})
	}
}

func TestArchiveComparisonWorkspaceAdmissionAndDateSessionRetirement(t *testing.T) {
	f := diskExportFixture(t, 2)
	e, err := NewExporterWithOptions(t.TempDir(), Options{Format: FormatNDJSON})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if _, err := e.ExportDisk(context.Background(), f.p, DefaultDiskExportLimits()); err != nil {
		t.Fatal(err)
	}
	refusal := errors.New("shared workspace refused")
	var requested int64
	e.archiveWorkRAMReserve = func(_ context.Context, amount int64) (func() error, error) {
		requested = amount
		return nil, refusal
	}
	if report, err := e.VerifyDiskArchive(context.Background(), f.p, DefaultArchiveVerifyLimits()); !errors.Is(err, refusal) || report != (ArchiveVerification{}) || requested != (288<<20) {
		t.Fatal("workspace was not refused before scratch IO", report, err, requested)
	}
	matches, err := filepath.Glob(filepath.Join(e.root, ".archive-verify-*"))
	if err != nil || len(matches) != 0 {
		t.Fatal("refusal created scratch", matches, err)
	}
	e.archiveWorkRAMReserve = func(_ context.Context, amount int64) (func() error, error) {
		requested = amount
		return func() error {
			matches, err := filepath.Glob(filepath.Join(e.root, ".archive-verify-*"))
			if err != nil || len(matches) != 0 {
				t.Fatal("fee released before actual scratch deletion", matches, err)
			}
			requested = 0
			return nil
		}, nil
	}
	if _, err := e.VerifyDiskArchive(context.Background(), f.p, DefaultArchiveVerifyLimits()); err != nil || requested != 0 {
		t.Fatal(err, requested)
	}
	f2, e2 := dateArchiveFixture(t, FormatNDJSON, false)
	finishDateCandidate(t, e2, archiveDateCutoff)
	var held int64
	e2.archiveWorkRAMReserve = func(_ context.Context, amount int64) (func() error, error) {
		held += amount
		return func() error { held -= amount; return nil }, nil
	}
	limits := DefaultDateCatalogVerifyLimits()
	limits.Files, limits.Source.Rows, limits.Source.PageRows = 1, 1, 1
	if out, err := e2.VerifyRetainedDateCatalogStep(context.Background(), f2.p, archiveDateCutoff, limits); err != nil || out.Phase == "ready" || held != 288<<20 {
		t.Fatal("resumable comparison lost its job fee", err, held)
	}
	if err := e2.Close(); err != nil || held != 0 {
		t.Fatal("actual Close failed to retire comparison workspace", err, held)
	}
}

func TestArchiveComparisonLateFeeFailureNeverMintsWindowProof(t *testing.T) {
	for _, fee := range []string{"RAM", "Disk"} {
		t.Run(fee, func(t *testing.T) {
			f := diskExportFixture(t, 2)
			e, err := NewExporterWithOptions(t.TempDir(), Options{Format: FormatNDJSON})
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			if _, err := e.ExportDisk(context.Background(), f.p, DefaultDiskExportLimits()); err != nil {
				t.Fatal(err)
			}
			injected := errors.New("actual fee retirement failed")
			reserve := func(context.Context, int64) (func() error, error) {
				return func() error { return injected }, nil
			}
			if fee == "RAM" {
				e.archiveWorkRAMReserve = reserve
			} else {
				e.archiveDiskReserve = reserve
			}
			out, err := e.VerifyDiskArchive(context.Background(), f.p, DefaultArchiveVerifyLimits())
			if !errors.Is(err, injected) || out != (ArchiveVerification{}) || e.windowProof != nil || e.windowPending != nil {
				t.Fatal("late cleanup error published success or deletion authority", out, err, e.windowProof)
			}
		})
	}
}
