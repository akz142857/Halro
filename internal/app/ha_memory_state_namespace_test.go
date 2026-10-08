package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

func TestHAMemoryActivationRetryPreservesFourOpaqueCandidatesAndRefusesFifth(t *testing.T) {
	f := prepareHAMetadataMaintenanceFixture(t)
	prepareHAMetadataMaintenanceArtifacts(t, f)
	completeHAMemoryActivationBoundary(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cfg := f.configs[0]
	guard, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	manager := functionalHAMetadataManager(t, cfg)
	m, err := OpenHAMetadataMaintenance(ctx, cfg, f.keys, guard, manager, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	for i := 0; i <= haMemoryActivationOpaqueSlots; i++ {
		root := haMemoryStateDirectory(cfg)
		if err := os.Mkdir(root, 0700); err != nil {
			t.Fatal(err)
		}
		payload := bytes.Repeat([]byte{byte(i + 1)}, 123+i)
		if err := os.WriteFile(filepath.Join(root, "untrusted-input"), payload, 0600); err != nil {
			t.Fatal(err)
		}
		original, err := os.Lstat(root)
		if err != nil {
			t.Fatal(err)
		}
		fee, err := preserveHAMemoryActivationCandidates(ctx, cfg, m, f.keys.Ledger, guard, manager)
		if i == haMemoryActivationOpaqueSlots {
			if !errors.Is(err, resourcebudget.ErrExhausted) {
				t.Fatal("fifth opaque candidate did not refuse", err)
			}
			if after, err := os.Lstat(root); err != nil || !os.SameFile(original, after) {
				t.Fatal("exhaustion moved the remaining candidate", err)
			}
			if actual, err := os.ReadFile(filepath.Join(root, "untrusted-input")); err != nil || !bytes.Equal(actual, payload) {
				t.Fatal("exhaustion changed opaque bytes", err)
			}
			continue
		}
		if err != nil || fee == (resourcebudget.Permit{}) {
			t.Fatal("qualified preservation failed or lost persistent fee", err)
		}
		// Persistent opaque fees deliberately stay charged; this helper never
		// infers deletion permission or an authenticated root from those bytes.
		preserved := filepath.Join(cfg.Storage.DataDir, "ha-memory-state.unqualified-"+string(rune('0'+i)), "roots")
		if info, err := os.Lstat(preserved); err != nil || !os.SameFile(original, info) {
			t.Fatal("preservation replaced the actual candidate inode", err)
		}
		if actual, err := os.ReadFile(filepath.Join(preserved, "untrusted-input")); err != nil || !bytes.Equal(actual, payload) {
			t.Fatal("preservation changed opaque bytes", err)
		}
	}
}

func TestHAMemoryActivationRefusesForeignEmptyGovernanceKeyBeforePreservation(t *testing.T) {
	f := prepareHAMetadataMaintenanceFixture(t)
	prepareHAMetadataMaintenanceArtifacts(t, f)
	completeHAMemoryActivationBoundary(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cfg := f.configs[0]
	guard, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	root := haMemoryStateDirectory(cfg)
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "opaque"), []byte("preserve me"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	keys := f.keys
	keys.Governance = bytes.Clone(keys.Governance)
	keys.Governance[0] ^= 1
	a, err := ActivateHAMemoryRuntimeWithOptions(ctx, cfg, keys, guard, functionalHAMetadataManager(t, cfg), slog.New(slog.NewTextHandler(io.Discard, nil)), HAMemoryActivationOptions{Restart: true})
	if a != nil {
		defer a.Close()
	}
	if err == nil {
		t.Fatal("empty stream certified a foreign Governance key")
	}
	if after, err := os.Lstat(root); err != nil || !os.SameFile(before, after) {
		t.Fatal("refused key moved the opaque candidate", err)
	}
	if raw, err := os.ReadFile(filepath.Join(root, "opaque")); err != nil || string(raw) != "preserve me" {
		t.Fatal("refused key changed opaque bytes", err)
	}
	if _, err := os.Lstat(filepath.Join(cfg.Storage.DataDir, "ha-memory-state.unqualified-0")); !os.IsNotExist(err) {
		t.Fatal("refused key created a preservation slot", err)
	}
}
