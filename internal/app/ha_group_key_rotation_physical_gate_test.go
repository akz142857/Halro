package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ownedfile"
)

func TestHAGroupRotationDisabledPhysicalFlagCannotBypassRetainedReceipts(t *testing.T) {
	for _, mode := range []string{"file", "key_slots"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			var f haMetadataMaintenanceFixture
			var owners []*haSelectedRuntimeFoundation
			var h *kmsAppHarness
			if mode == "file" {
				f, owners = prepareHAGroupRotationColdMembers(t, ctx)
			} else {
				f, owners, h = prepareHAGroupKMSColdMembers(t, ctx)
			}
			for _, o := range owners {
				if err := o.Close(); err != nil {
					t.Fatal(err)
				}
				if err := o.owners.Components.Guard.Close(); err != nil {
					t.Fatal(err)
				}
			}
			cfg := f.configs[0]
			if cfg.ResourceBudget.PhysicalTerminalReserve {
				t.Fatal("fixture must test actual false configuration")
			}
			sourcePaths := []string{cfg.ReplicationStatePath(), haMemoryStateBootstrapPath(cfg), haGroupKeyDestination(cfg), cfg.OrderingJournalPath(), filepath.Join(cfg.Storage.DataDir, HAMetadataBaselineDirectory, "metadata.journal")}
			source := make([][]byte, len(sourcePaths))
			for n, path := range sourcePaths {
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				source[n] = raw
				defer clear(raw)
			}
			paths := []string{
				filepath.Join(haMemoryStateDirectory(cfg), "ledger", "terminal.physical.0"),
				filepath.Join(haMemoryStateDirectory(cfg), "usage", "terminal.physical.1"),
				filepath.Join(haMemoryStateDirectory(cfg), "directory", "terminal.physical.0"),
				filepath.Join(haMemoryStateDirectory(cfg), "governance", "terminal.physical.1"),
				filepath.Join(cfg.Storage.DataDir, HAMetadataBaselineDirectory, "metadata-root", "terminal.physical.0"),
				cfg.AuditPath() + ".physical-window-1", cfg.GovernancePath() + ".physical-window-0",
				cfg.ReplicationStatePath() + ".physical-bank", cfg.ReplicationStatePath() + ".physical.json",
			}
			target := filepath.Join(t.TempDir(), "next.key")
			if err := os.WriteFile(target, bytes.Repeat([]byte{132}, 32), 0600); err != nil {
				t.Fatal(err)
			}
			for _, path := range paths {
				file, owner, err := ownedfile.Create(path, os.O_RDWR, 0600)
				if err != nil {
					t.Fatal(err)
				}
				opaque := []byte("foreign opaque preparation; no authority")
				if _, err := file.Write(opaque); err != nil {
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
				pressureCalls := 0
				pressure := func(c config.Config) (*resourcePressureRuntime, error) {
					pressureCalls++
					return functionalHAGroupPressure(c)
				}
				if mode == "file" {
					_, err = rotateHAGroupMasterKeyWithPressureFactory(ctx, f.configs[:], target, "physical-receipt-refusal", nil, pressure)
				} else {
					o := kmsRotationOptions{operationID: "physical-receipt-refusal", factory: h.factory, random: bytes.NewReader(nil), now: time.Now}
					_, err = rotateHAGroupKeyWithOptions(ctx, f.configs[:], "", o.operationID, nil, pressure, &o)
				}
				if !errors.Is(err, authenticatedstate.ErrRecovery) || pressureCalls != 0 {
					t.Fatal("physical receipt bypass reached pressure/key/source initialization", path, err, pressureCalls)
				}
				raw, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(raw, opaque) {
					t.Fatal("foreign control altered", path, err)
				}
				clear(raw)
				for n, p := range sourcePaths {
					raw, err := os.ReadFile(p)
					if err != nil || !bytes.Equal(raw, source[n]) {
						t.Fatal("source altered before physical refusal", p, err)
					}
					clear(raw)
				}
				for _, c := range f.configs {
					for slot := range haGroupRotationIntentSlots {
						if _, err := os.Lstat(haGroupRotationIntentPath(c.Storage.DataDir, slot)); !errors.Is(err, os.ErrNotExist) {
							t.Fatal("intent created before physical refusal", err)
						}
					}
				}
				if err := owner.Retire(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
