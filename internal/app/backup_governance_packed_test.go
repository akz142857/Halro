package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

// These are functional actual native/derivative/Monitor ownership tests, not
// physical capacity evidence. OS capacity uses the isolated Linux experiment.
func TestPackedBackupGovernanceValidatesNativeDomainWithoutMappingHistory(t *testing.T) {
	for _, mode := range []string{"clean", "revision-gap", "wrong-supersedes", "duplicate-outcome", "missing-anchor", "invalid-anchor", "wrong-key", "native-tamper", "wrong-head", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			cfg := budgetedBackupOperationFixture(t)
			cfg.ResourceBudget.MemoryStateRequired = true
			// A large history ceiling must charge Disk rather than mmap-sized RAM.
			cfg.ResourceBudget.GovernanceProjectionDiskBytes = 512 << 20
			cfg.ResourceBudget.IORAMBytes = 256 << 20
			cfg.ResourceBudget.BackgroundRAMBytes = 256 << 20
			pressure, err := newResourcePressureWithSource(cfg, pressureSampleFunc(func(context.Context) resourcebudget.Sample { return runtimeLowSample() }))
			if err != nil {
				t.Fatal(err)
			}
			base, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			ctx := context.WithValue(base, commandResourceScopeKey{}, &commandResourceScope{pressure: pressure, budget: *cfg.ResourceBudget, policy: *cfg.ResourcePressure})
			metadata, native, source, key := openGovernanceRecoveryFixture(t)
			defer metadata.Close()
			defer native.Close()
			first := governanceRecoveryEvent("out_first", "accepted", "", "first", 1)
			second := governanceRecoveryEvent("out_second", "rejected", first.OutcomeID, "second", 2)
			switch mode {
			case "revision-gap":
				second.Revision = 3
			case "wrong-supersedes":
				second.SupersedesOutcomeID = "missing"
			case "duplicate-outcome":
				second.OutcomeID = first.OutcomeID
			}
			if _, err = native.AppendBatch(ctx, []governance.Event{first, second}); err != nil {
				t.Fatal(err)
			}
			expected, auth := native.AuthenticatedHead()
			if mode == "invalid-anchor" {
				auth[0] ^= 1
			}
			if mode != "missing-anchor" {
				if err = metadata.PutGovernanceJournalAnchor(expected.Records, expected.Bytes, expected.LastHash, auth); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "wrong-key" {
				key = bytes.Repeat([]byte{0x21}, 32)
			}
			if mode == "wrong-head" {
				expected.Records++
			}
			if mode == "native-tamper" {
				f, e := os.OpenFile(source, os.O_RDWR, 0)
				if e != nil {
					t.Fatal(e)
				}
				_, e = f.WriteAt([]byte{0xFF}, 1)
				if e = errors.Join(e, f.Close()); e != nil {
					t.Fatal(e)
				}
			}
			before, e := os.ReadFile(source)
			if e != nil {
				t.Fatal(e)
			}
			metadataPath := filepath.Join(filepath.Dir(source), "metadata.db")
			metadataBefore, e := os.ReadFile(metadataPath)
			if e != nil {
				t.Fatal(e)
			}
			if mode == "cancel" {
				cancel()
			}
			parent := t.TempDir()
			err = validateBackupGovernanceProjection(ctx, cfg, metadata, native, source, key, expected, parent)
			if (mode == "clean") != (err == nil) {
				t.Fatal(mode, err, pressure.logicalBudget().Snapshot())
			}
			if mode == "revision-gap" && !errors.Is(err, governance.ErrRevisionConflict) {
				t.Fatal("domain error hidden", err)
			}
			if mode == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			after, e := os.ReadFile(source)
			if e != nil || !bytes.Equal(before, after) {
				t.Fatal("native changed", e)
			}
			metadataAfter, e := os.ReadFile(metadataPath)
			if e != nil || !bytes.Equal(metadataBefore, metadataAfter) {
				t.Fatal("authoritative Metadata changed", e)
			}
			entries, e := os.ReadDir(parent)
			if e != nil || len(entries) != 0 {
				t.Fatal("temporary files survived", entries, e, err)
			}
			if used := pressure.logicalBudget().Snapshot().Used; used != (resourcebudget.Cost{}) {
				t.Fatal("known cleanup fee leaked", used, err)
			}
			if pressure.monitor.Snapshot().Running {
				t.Fatal("finite monitor leaked")
			}
			if mode == "clean" && pressure.monitor.Snapshot().Samples < 2 {
				t.Fatal("no actual sampler cooldown")
			}
		})
	}
}

func TestPackedBackupGovernanceCandidateCancellationAndForeignCleanupRetainActualOwners(t *testing.T) {
	for _, mode := range []string{"cancel-before-build", "cancel-after-build", "foreign-root", "foreign-publication", "foreign-directory", "closed-publication", "closed-directory"} {
		t.Run(mode, func(t *testing.T) {
			cfg := budgetedBackupOperationFixture(t)
			cfg.ResourceBudget.MemoryStateRequired = true
			pressure, e := newResourcePressureWithSource(cfg, pressureSampleFunc(func(context.Context) resourcebudget.Sample { return runtimeLowSample() }))
			if e != nil {
				t.Fatal(e)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			metadata, native, source, key := openGovernanceRecoveryFixture(t)
			defer metadata.Close()
			defer native.Close()
			if _, e = native.Append(ctx, governanceRecoveryEvent("out_first", "accepted", "", "first", 1)); e != nil {
				t.Fatal(e)
			}
			expected := native.Summary()
			before, e := os.ReadFile(source)
			if e != nil {
				t.Fatal(e)
			}
			parent := t.TempDir()
			var foreign string
			var entered bool
			err := validatePackedBackupGovernanceProjection(ctx, cfg, native, key, expected, pressure, parent, func(phase string, d *packedBackupGovernanceFailure, directory string) error {
				if phase == "publication-created" {
					if mode == "cancel-before-build" {
						entered = true
						cancel()
					}
					if mode == "closed-publication" {
						entered = true
						return d.PublicationFD.Close()
					}
				}
				if phase != "candidate-built" {
					return nil
				}
				if mode == "cancel-before-build" || mode == "closed-publication" {
					return errors.New("unexpected built candidate")
				}
				entered = true
				if d.Projection == nil || d.Native != native {
					t.Fatal("actual owner lost")
				}
				current, found, e := d.Projection.ReadCurrent(ctx, "prj_1", "wku_unit", "odef_result", 1)
				if e == nil && (!found || current.ID != "out_first" || current.Revision != 1) {
					t.Fatal("domain reduction changed", current, found)
				}
				if e != nil {
					return e
				}
				s := pressure.logicalBudget().Snapshot()
				if s.Pressure != resourcebudget.Healthy || s.Classes[resourcebudget.Background].Disk == 0 {
					t.Fatal("root uncharged", s)
				}
				switch mode {
				case "cancel-after-build":
					cancel()
					return nil
				case "closed-directory":
					return d.Directory.Close()
				case "foreign-root":
					foreign = filepath.Join(directory, "governance")
				case "foreign-publication":
					foreign = lock.PublicationPath(filepath.Join(directory, "governance"))
				case "foreign-directory":
					foreign = directory
				}
				if e := os.Rename(foreign, foreign+".original"); e != nil {
					return e
				}
				if mode == "foreign-directory" {
					if e := os.Mkdir(foreign, 0700); e != nil {
						return e
					}
					return os.WriteFile(filepath.Join(foreign, "foreign"), []byte("foreign"), 0600)
				}
				return os.WriteFile(foreign, []byte("foreign"), 0600)
			})
			if !entered || err == nil {
				t.Fatal(mode, "fault not exercised", err)
			}
			after, e := os.ReadFile(source)
			if e != nil || !bytes.Equal(before, after) {
				t.Fatal("borrowed native changed", e)
			}
			if strings.HasPrefix(mode, "cancel-") {
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
				entries, e := os.ReadDir(parent)
				if e != nil || len(entries) != 0 {
					t.Fatal(entries, e, err)
				}
				if used := pressure.logicalBudget().Snapshot().Used; used != (resourcebudget.Cost{}) {
					t.Fatal("canceled known close leaked", used)
				}
			} else {
				var debt *packedBackupGovernanceFailure
				if !errors.As(err, &debt) || !errors.Is(err, ownedfile.ErrRetirement) || debt.Native != native || debt.Fee == (resourcebudget.Permit{}) || pressure.logicalBudget().Snapshot().Used == (resourcebudget.Cost{}) {
					t.Fatal("actual owner/fees lost", err)
				}
				if mode == "closed-publication" && (debt.PublicationFD == nil || debt.Publication == nil) {
					t.Fatal("actual Close debt lost")
				}
				if mode == "closed-directory" && debt.Directory == nil {
					t.Fatal("actual directory debt lost")
				}
				if strings.HasPrefix(mode, "foreign-") {
					path := foreign
					if mode == "foreign-directory" {
						path = filepath.Join(foreign, "foreign")
					}
					raw, e := os.ReadFile(path)
					if e != nil || string(raw) != "foreign" {
						t.Fatal("foreign removed", string(raw), e)
					}
				}
				// Test owns every temporary. Restore original names only after proving
				// refusal, then join the retained directory FDs to keep test FD counts sane.
				if foreign != "" {
					if mode == "foreign-directory" {
						_ = os.Remove(filepath.Join(foreign, "foreign"))
					}
					_ = os.Remove(foreign)
					_ = os.Rename(foreign+".original", foreign)
				}
				if debt.Projection != nil {
					_ = debt.Projection.Discard()
				}
				if debt.Directory != nil {
					_ = debt.Directory.Close()
				}
				if debt.Parent != nil {
					_ = debt.Parent.Close()
				}
			}
			if pressure.monitor.Snapshot().Running {
				t.Fatal("monitor did not join")
			}
		})
	}
}
