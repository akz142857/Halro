package usage

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func windowAuthorityFixture(t *testing.T) (*diskFailureFixture, *Exporter) {
	t.Helper()
	f := newDiskFailureFixture(t, windowRequest("authority", false, false))
	e, err := NewExporterWithOptions(t.TempDir(), Options{Format: FormatNDJSON})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.ExportDisk(context.Background(), f.p, DefaultDiskExportLimits()); err != nil {
		t.Fatal(err)
	}
	// Reopening deliberately removes the publisher's ephemeral receipt so
	// verifier tests begin with an existing, untrusted root.
	e, err = NewExporterWithOptions(e.root, Options{Format: FormatNDJSON})
	if err != nil {
		t.Fatal(err)
	}
	return f, e
}
func TestArchiveWindowRequiresActualGenesisProofAndPreservesNewFacts(t *testing.T) {
	f, e := windowAuthorityFixture(t)
	ctx := context.Background()
	digest := windowDigest(t, f.p, diskUsageBuckets)
	if out, err := e.PruneVerifiedWindowStep(ctx, f.p, windowTestCutoff, windowTestLimits); !errors.Is(err, ErrArchiveWindowProof) || out != (DiskWindowProgress{}) || windowDigest(t, f.p, diskUsageBuckets) != digest {
		t.Fatal(out, err)
	}
	if _, err := e.VerifyDiskArchive(ctx, f.p, DefaultArchiveVerifyLimits()); err != nil {
		t.Fatal(err)
	}
	for _, event := range windowRequest("unexported", false, false) {
		appendDiskDetailEvent(t, f, event)
	}
	for i := 0; i < 100; i++ {
		out, err := e.PruneVerifiedWindowStep(ctx, f.p, windowTestCutoff, windowTestLimits)
		if err != nil {
			t.Fatal(out, err)
		}
		if out.Done {
			break
		}
		if i == 99 {
			t.Fatal("no cleanup progress")
		}
	}
	stats, err := f.p.ReadStats(ctx)
	if err != nil || stats.Window.Attempts != 2 || stats.Window.Summaries != 1 {
		t.Fatal(stats, err)
	}
	if found, err := f.p.RequestDetailPage(ctx, DiskRequestDetailQuery{RequestID: "unexported", Limit: 10}, DiskRequestDetailCursor{}, diskTestLimits); err != nil || !found.Found {
		t.Fatal(found, err)
	}
}
func TestArchiveWindowOwnerReplacementReopenAndRootChangesRefuseProof(t *testing.T) {
	for _, kind := range []string{"owner", "exporter", "identical_inode_replacement", "directory", "frontier"} {
		t.Run(kind, func(t *testing.T) {
			f, e := windowAuthorityFixture(t)
			ctx := context.Background()
			if _, err := e.VerifyDiskArchive(ctx, f.p, DefaultArchiveVerifyLimits()); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "owner":
				f = newDiskFailureFixture(t, windowRequest("authority", false, false))
			case "exporter":
				var err error
				e, err = NewExporter(e.root)
				if err != nil {
					t.Fatal(err)
				}
			case "identical_inode_replacement":
				path := filepath.Join(e.root, "manifest.json")
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				replacement := path + ".replacement"
				if err := os.WriteFile(replacement, raw, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(replacement, path); err != nil {
					t.Fatal(err)
				}
			case "directory":
				old := e.root + ".old"
				if err := os.Rename(e.root, old); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(e.root, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(filepath.Join(old, "manifest.json"), filepath.Join(e.root, "manifest.json")); err != nil {
					t.Fatal(err)
				}
			case "frontier":
				path := filepath.Join(e.root, "manifest.json")
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var root map[string]any
				if err := json.Unmarshal(raw, &root); err != nil {
					t.Fatal(err)
				}
				root["last_sequence"] = float64(f.a.Watermark().Sequence + 1)
				raw, err = json.Marshal(root)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, raw, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			digest := windowDigest(t, f.p, diskUsageBuckets)
			out, err := e.PruneVerifiedWindowStep(ctx, f.p, windowTestCutoff, windowTestLimits)
			if !errors.Is(err, ErrArchiveWindowProof) || out != (DiskWindowProgress{}) || windowDigest(t, f.p, diskUsageBuckets) != digest {
				t.Fatal(out, err)
			}
		})
	}
}
func TestArchiveWindowFailedOrScopedVerificationAndPublicationCannotMintProof(t *testing.T) {
	for _, kind := range []string{"cancelled", "scoped", "corrupt_file", "unknown_publication"} {
		t.Run(kind, func(t *testing.T) {
			f, e := windowAuthorityFixture(t)
			ctx := context.Background()
			if _, err := e.VerifyDiskArchive(ctx, f.p, DefaultArchiveVerifyLimits()); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "cancelled":
				c, cancel := context.WithCancel(ctx)
				cancel()
				if _, err := e.VerifyDiskArchive(c, f.p, DefaultArchiveVerifyLimits()); err == nil {
					t.Fatal("cancel accepted")
				}
			case "scoped":
				if _, err := e.VerifyDiskArchiveAfter(ctx, f.p, 1, DefaultArchiveVerifyLimits()); err != nil {
					t.Fatal(err)
				}
			case "corrupt_file":
				page, err := e.ReadArchiveFiles(ctx, "", 1)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(e.root, filepath.FromSlash(page.Files[0].Path)), []byte("bad"), 0o600); err != nil {
					t.Fatal(err)
				}
				if _, err := e.VerifyDiskArchive(ctx, f.p, DefaultArchiveVerifyLimits()); err == nil {
					t.Fatal("corruption accepted")
				}
			case "unknown_publication":
				var err error
				e, err = NewExporterWithOptions(e.root, Options{Format: FormatNDJSON})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := e.ExportDisk(ctx, f.p, DefaultDiskExportLimits()); err != nil {
					t.Fatal(err)
				}
			}
			if out, err := e.PruneVerifiedWindowStep(ctx, f.p, windowTestCutoff, windowTestLimits); !errors.Is(err, ErrArchiveWindowProof) || out != (DiskWindowProgress{}) {
				t.Fatal(out, err)
			}
		})
	}
}

func TestArchiveWindowAuthorityJoinsActualRootCallbackOnCancellationAndClose(t *testing.T) {
	f, e := windowAuthorityFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := e.VerifyDiskArchive(ctx, f.p, DefaultArchiveVerifyLimits()); err != nil {
		t.Fatal(err)
	}
	entered, release, done := make(chan *os.File, 1), make(chan struct{}), make(chan error, 1)
	go func() {
		out, err := e.pruneVerifiedWindowStep(ctx, f.p, windowTestCutoff, windowTestLimits, func(root *archiveManifestInput) error {
			entered <- root.file
			<-release
			var raw [4]byte
			_, err := root.file.ReadAt(raw[:], 0)
			return err
		})
		if out != (DiskWindowProgress{}) {
			done <- errors.New("cancel published progress")
			return
		}
		done <- err
	}()
	file := <-entered
	cancel()
	if _, err := file.Stat(); err != nil {
		close(release)
		<-done
		t.Fatal("root descriptor disappeared before callback exit", err)
	}
	closed := make(chan error, 1)
	closeStarted := make(chan struct{})
	go func() { close(closeStarted); closed <- f.p.Close() }()
	<-closeStarted
	select {
	case err := <-closed:
		close(release)
		<-done
		t.Fatal("physical owner closed before root callback", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatal("root descriptor retained after operation", err)
	}
}
