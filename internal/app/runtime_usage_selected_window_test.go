package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/usage"
)

func TestSelectedCompleteUsageWindowVerifiesArchiveWithoutScratchRebuild(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cfg, _, _, sample := selectedLegacyExportFixture(t, ctx, true)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	opts := OpenOptions{offlineVerification: true, AllowGeneratedSetupToken: true, resourceSampleSource: sample}
	r, err := OpenWithOptions(ctx, cfg, logger, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.selectedState.generation.WaitStartupAudit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.usageExporter.ExportDiskStep(ctx, r.usage.disk, usage.DefaultDiskExportStepLimits()); err != nil {
		t.Fatal(err)
	}
	archive, err := r.usageExporter.ReadArchivePosition(ctx)
	if err != nil || archive.Files == 0 || archive.LastSequence == 0 {
		t.Fatal("fixture has no actual nonempty archive", archive, err)
	}
	stats, err := r.usage.disk.ReadStats(ctx)
	if err != nil || stats.Window.Floor != 0 || stats.Window.AttemptAfter != 0 || stats.Window.SummaryAfter != 0 || stats.Window.TrimmedAttempts != 0 {
		t.Fatal("fixture is not an actual complete selected window", stats.Window, err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	// Exhaust every disposable namespace with foreign bytes. A complete
	// authenticated Usage root must not require or read any such candidate.
	var paths []string
	var identities []os.FileInfo
	for slot := 0; slot < 4; slot++ {
		for _, suffix := range []string{"", "-scratch"} {
			path := filepath.Join(memoryStateDirectory(cfg), fmt.Sprintf("window-reference-%d%s", slot, suffix))
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(path, "opaque"), []byte("foreign input"), 0600); err != nil {
				t.Fatal(err)
			}
			info, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			paths, identities = append(paths, path), append(identities, info)
		}
	}
	again, err := OpenWithOptions(ctx, cfg, logger, opts)
	if err != nil {
		t.Fatal("complete selected archive verification rebuilt scratch history", err)
	}
	defer again.Close()
	request, found, err := again.usage.disk.ReadRequest(ctx, "req_before_legacy_export_migration")
	if err != nil || !found || request.CostMicrosUSD != 7 {
		t.Fatal("archive verification lost selected business", request, found, err)
	}
	for i, path := range paths {
		info, err := os.Lstat(path)
		if err != nil || !os.SameFile(info, identities[i]) {
			t.Fatal("verification moved opaque candidate", path, err)
		}
		if raw, err := os.ReadFile(filepath.Join(path, "opaque")); err != nil || string(raw) != "foreign input" {
			t.Fatal("verification changed opaque candidate", path, err)
		}
	}
}
