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

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/store/lock"
)

func selectedSeedCommandTestContext(t *testing.T, ctx context.Context, cfg config.Config) context.Context {
	t.Helper()
	p := &resourcePressureRuntime{budget: functionalHAMetadataManager(t, cfg)}
	return context.WithValue(ctx, commandResourceScopeKey{}, &commandResourceScope{pressure: p, budget: *cfg.ResourceBudget, policy: *cfg.ResourcePressure})
}

func TestSelectedSeedApprovalColdReplicaSevenRootsAndFreshBusiness(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	fixture, owners, _ := prepareHASelectedTransportFixture(t, ctx)
	for _, f := range owners {
		if err := f.Close(); err != nil {
			t.Fatal("source close", err)
		}
		if err := f.owners.Components.Guard.Close(); err != nil {
			t.Fatal(err)
		}
	}
	source := fixture.configs[0]
	manifestPath := filepath.Join(t.TempDir(), "seed-v2.json")
	manifest, err := CreateSeedManifest(selectedSeedCommandTestContext(t, ctx, source), source, "halro-1", manifestPath, "admin", []byte("correct horse battery staple"), "")
	if err != nil {
		t.Fatal("actual selected source approval", err)
	}
	if manifest.Version != 2 || manifest.Selected == nil || len(manifest.Files) != 0 || manifest.Selected.SourceSeal.RootCount != 7 {
		t.Fatalf("not complete fixed format2 declaration: %+v", manifest)
	}
	payload, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = decodeSeedManifest(payload); err != nil {
		t.Fatalf("actual emitted manifest roundtrip bytes=%d: %v", len(payload), err)
	}
	target := fixture.configs[1]
	target.Storage.DataDir = filepath.Join(t.TempDir(), "replica")
	stage := target.Storage.DataDir + ".staging"
	copyTestTree(t, source.Storage.DataDir, stage)
	// Approval failure precedes target source repair, Member writes or derived builds.
	bad := manifest
	copySelected := *manifest.Selected
	bad.Selected = &copySelected
	bad.TargetNode = "foreign"
	_, err = installSelectedSeedSnapshot(selectedSeedCommandTestContext(t, ctx, target), target, stage, bad)
	if !errors.Is(err, authenticatedstate.ErrSealIneligible) {
		t.Fatalf("wrong target granted permission: %v", err)
	}
	if _, err = os.Lstat(target.Storage.DataDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed seed published target: %v", err)
	}
	for _, name := range []string{"native_changed", "foreign_file", "external_hardlink", "bootstrap_changed"} {
		t.Run(name, func(t *testing.T) {
			badCfg := fixture.configs[1]
			badCfg.Storage.DataDir = filepath.Join(t.TempDir(), "target")
			badStage := badCfg.Storage.DataDir + ".stage"
			copyTestTree(t, source.Storage.DataDir, badStage)
			marker := filepath.Join(badStage, "active.seal")
			original, e := os.ReadFile(marker)
			if e != nil {
				t.Fatal(e)
			}
			switch name {
			case "foreign_file":
				if e = os.WriteFile(filepath.Join(badStage, "ha-memory-state", "foreign"), []byte("foreign remains"), 0600); e != nil {
					t.Fatal(e)
				}
			case "external_hardlink":
				external := filepath.Join(t.TempDir(), "external")
				if e = os.WriteFile(external, original, 0600); e != nil {
					t.Fatal(e)
				}
				if e = os.Remove(marker); e != nil {
					t.Fatal(e)
				}
				if e = os.Link(external, marker); e != nil {
					t.Fatal(e)
				}
			default:
				path := filepath.Join(badStage, HAMetadataBaselineDirectory, "metadata.journal")
				if name == "bootstrap_changed" {
					path = filepath.Join(badStage, "ha-memory-state.bootstrap")
				}
				raw, e := os.ReadFile(path)
				if e != nil || len(raw) == 0 {
					t.Fatal(e)
				}
				raw[0] ^= 1
				if e = os.WriteFile(path, raw, 0600); e != nil {
					t.Fatal(e)
				}
			}
			if _, e = InstallSeedSnapshot(selectedSeedCommandTestContext(t, ctx, badCfg), badCfg, badStage, manifestPath); e == nil {
				t.Fatal("foreign staged source became qualified")
			}
			if _, e = os.Lstat(badCfg.Storage.DataDir); !errors.Is(e, os.ErrNotExist) {
				t.Fatal("rejected source published target", e)
			}
			actual, e := os.ReadFile(marker)
			if e != nil || !bytes.Equal(original, actual) {
				t.Fatal("rejected foreign state changed", e)
			}
		})
	}
	for _, name := range []string{"root", "term", "incarnation", "member", "baseline"} {
		bad := manifest
		selected := *manifest.Selected
		bad.Selected = &selected
		switch name {
		case "root":
			bad.Selected.SourceSeal.Roots[0].Digest[0] ^= 1
		case "term":
			bad.Term++
		case "incarnation":
			bad.Incarnation += "-foreign"
		case "member":
			bad.Selected.SourceMember.NodeID = "foreign"
		case "baseline":
			bad.Selected.BaselineDigest[0] ^= 1
		}
		if _, e := installSelectedSeedSnapshot(selectedSeedCommandTestContext(t, ctx, target), target, stage, bad); e == nil {
			t.Fatalf("changed %s became authority", name)
		}
	}
	state, err := InstallSeedSnapshot(selectedSeedCommandTestContext(t, ctx, target), target, stage, manifestPath)
	if err != nil {
		t.Fatal("actual cold target install", err)
	}
	if state.Role != replication.RoleReplica || state.NodeID != target.Replication.NodeID || state.DurableIndex != manifest.Index || state.ConfirmedIndex != manifest.Index || state.AppliedIndex != manifest.Index {
		t.Fatalf("wrong independent target member: %+v", state)
	}
	guard, err := lock.Acquire(target.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	f, err := openHASelectedRuntimeFoundation(ctx, target, fixture.keys.Master, guard, functionalHAMetadataManager(t, target), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal("actual fresh Replica selection", err)
	}
	defer f.Close()
	if err = f.prepareSelectedServices(ctx); err != nil {
		t.Fatal("actual fresh Replica business owners", err)
	}
	seal := f.reopened.Seal
	if seal.Role != authenticatedstate.SealReplica || seal.RootCount != 7 || seal.OrderingIndex != manifest.Index || seal.Generation <= manifest.Selected.SourceSeal.Generation {
		t.Fatalf("target did not genuinely publish successor seven roots: %+v", seal)
	}
	user, err := f.metadataStore.GetAdminUser(ctx, "admin")
	if err != nil || user.Username != "admin" {
		t.Fatalf("actual issued selected Metadata read: %+v %v", user, err)
	}
	if err = f.Close(); err != nil {
		t.Fatal("actual fresh owner join", err)
	}
}
