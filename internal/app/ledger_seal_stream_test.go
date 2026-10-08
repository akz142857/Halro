package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

func sealOutputFixture(t *testing.T, generations int, active bool) config.Config {
	t.Helper()
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	log, store, key := openSealingLedger(t, cfg)
	defer clear(key)
	for i := 0; i < generations; i++ {
		appendSealingEvents(t, log, i+1, 1)
		if _, err := log.Roll(); err != nil {
			t.Fatal(err)
		}
	}
	if active {
		appendSealingEvents(t, log, generations+1, 1)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func enableLedgerSegmentIndexBudget(cfg config.Config) config.Config {
	b := runtimeRAMBudgetFixture()
	b.TotalDiskBytes, b.BackgroundDiskBytes = 256<<20, 256<<20
	b.RecoveryDiskBytes, b.RecoveryReserveDiskBytes = 128<<20, 128<<20
	b.IODiskBytes = 64
	b.LedgerSegmentIndexDiskBytes = 1 << 20
	b.RecoveryClaims = 7
	b.MaxClaims++
	b.ControlRAMBytes += resourcebudget.ClaimRAMBytes
	cfg.ResourcePressure = pressureConfigFixture()
	cfg.ResourceBudget = &b
	return cfg
}

type sealBoundedDestination struct {
	bytes.Buffer
	max int
}

func (w *sealBoundedDestination) Write(p []byte) (int, error) {
	if len(p) > 32<<10 {
		return 0, errors.New("report exceeded fixed output chunk")
	}
	if len(p) > w.max {
		w.max = len(p)
	}
	return w.Buffer.Write(p)
}

func assertSealSpoolRetired(t *testing.T, scratch string, cfg config.Config) {
	t.Helper()
	files, err := os.ReadDir(scratch)
	if err != nil || len(files) != 0 {
		t.Fatalf("scratch leaked: %v %v", files, err)
	}
	owned, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		t.Fatalf("offline lock not retired: %v", err)
	}
	if err := owned.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWriteSealLedgerPreservesCompleteJSONWithFixedOutputChunks(t *testing.T) {
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	cfg := enableLedgerSegmentIndexBudget(sealOutputFixture(t, 100, false))
	expected, err := SealLedger(context.Background(), cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	var destination sealBoundedDestination
	if err := WriteSealLedger(context.Background(), cfg, false, &destination); err != nil {
		t.Fatal(err)
	}
	var actual SealReport
	if err := json.Unmarshal(destination.Bytes(), &actual); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actual, expected) || bytes.Count(destination.Bytes(), []byte(`"generation":`)) != 100 {
		t.Fatalf("reports differ: actual=%+v expected=%+v", actual, expected)
	}
	if destination.Len() <= 32<<10 || destination.max != 32<<10 {
		t.Fatalf("bytes=%d max=%d", destination.Len(), destination.max)
	}
	assertSealSpoolRetired(t, scratch, cfg)
}

func TestWriteSealLedgerEmptyAndNewCompressedGeneration(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty", true: "compressed new generation"}[active], func(t *testing.T) {
			scratch := t.TempDir()
			t.Setenv("TMPDIR", scratch)
			cfg := sealOutputFixture(t, 0, active)
			var output bytes.Buffer
			if err := WriteSealLedger(context.Background(), cfg, true, &output); err != nil {
				t.Fatal(err)
			}
			var report SealReport
			if err := json.Unmarshal(output.Bytes(), &report); err != nil {
				t.Fatal(err)
			}
			if active {
				if !report.Rolled || report.Compacted == nil || report.SealedGenerations != 1 || bytes.Count(output.Bytes(), []byte(`"generation":`)) != 3 || !bytes.Contains(output.Bytes(), []byte(`"compressed":true`)) {
					t.Fatalf("report=%+v", report)
				}
			} else if report.Rolled || report.SealedGenerations != 0 || bytes.Contains(output.Bytes(), []byte(`"verified"`)) {
				t.Fatalf("empty report=%+v", report)
			}
			assertSealSpoolRetired(t, scratch, cfg)
		})
	}
}

type sealFailedDestination struct{ short bool }

func (w sealFailedDestination) Write(p []byte) (int, error) {
	if w.short {
		return len(p) - 1, nil
	}
	return 0, io.ErrClosedPipe
}

func TestWriteSealLedgerFailuresDoNotLeakOwnersOrClaimSuccess(t *testing.T) {
	for _, name := range []string{"corrupt history", "cancel during preparation", "destination failure", "short destination"} {
		t.Run(name, func(t *testing.T) {
			scratch := t.TempDir()
			t.Setenv("TMPDIR", scratch)
			cfg := sealOutputFixture(t, 3, false)
			var output bytes.Buffer
			ctx := context.Background()
			var destination io.Writer = &output
			var expected error
			switch name {
			case "corrupt history":
				path := filepath.Join(filepath.Dir(cfg.LedgerPath()), "ledger-3.wal")
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				raw[len(raw)-1] ^= 1
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "cancel during preparation":
				base, cancel := context.WithCancel(context.Background())
				defer cancel()
				ctx = generationProbeContext{Context: base, probe: func() {
					matches, err := filepath.Glob(filepath.Join(scratch, "halro-seal-report_*.json"))
					if err != nil {
						t.Fatal(err)
					}
					if len(matches) > 0 {
						cancel()
					}
				}}
				expected = context.Canceled
			case "destination failure":
				destination = sealFailedDestination{}
				expected = io.ErrClosedPipe
			case "short destination":
				destination = sealFailedDestination{short: true}
				expected = io.ErrShortWrite
			}
			err := WriteSealLedger(ctx, cfg, false, destination)
			if err == nil || expected != nil && !errors.Is(err, expected) {
				t.Fatalf("err=%v expected=%v", err, expected)
			}
			if output.Len() != 0 {
				t.Fatalf("preparation failure published %d bytes", output.Len())
			}
			assertSealSpoolRetired(t, scratch, cfg)
		})
	}
}

func TestWriteSealLedgerPreservesForeignSpoolReplacementOnCancellation(t *testing.T) {
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	cfg := sealOutputFixture(t, 2, false)
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	var replacedPath string
	ctx := generationProbeContext{Context: base, probe: func() {
		if replacedPath != "" {
			return
		}
		matches, err := filepath.Glob(filepath.Join(scratch, "halro-seal-report_*.json"))
		if err != nil {
			t.Fatal(err)
		}
		if len(matches) == 0 {
			return
		}
		replacedPath = matches[0]
		if err := os.Rename(replacedPath, replacedPath+".owned"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(replacedPath, []byte("foreign replacement"), 0600); err != nil {
			t.Fatal(err)
		}
		cancel()
	}}
	var destination bytes.Buffer
	err := WriteSealLedger(ctx, cfg, false, &destination)
	if replacedPath == "" || !errors.Is(err, context.Canceled) || !errors.Is(err, ownedfile.ErrReplaced) || destination.Len() != 0 {
		t.Fatalf("path=%q bytes=%d err=%v", replacedPath, destination.Len(), err)
	}
	raw, readErr := os.ReadFile(replacedPath)
	if readErr != nil || string(raw) != "foreign replacement" {
		t.Fatalf("removed foreign inode: %q %v", raw, readErr)
	}
	dataLock, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		t.Fatal("failed report leaked offline lock", err)
	}
	if err := dataLock.Close(); err != nil {
		t.Fatal(err)
	}
}
