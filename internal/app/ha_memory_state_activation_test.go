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
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/statecut"
	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/vault"
)

// Initial roots are constructed only after this actual mTLS/receiver/native
// fsync/application/MemberState boundary. No ordering fields or roots are made
// up by the fixture.
func completeHAMemoryActivationBoundary(t *testing.T, f haMetadataMaintenanceFixture) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	var guards [2]*lock.Lock
	var owners [2]*HAMetadataMaintenance
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for i, cfg := range f.configs {
		var err error
		guards[i], err = lock.Acquire(cfg.Storage.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		defer guards[i].Close()
		owners[i], err = OpenHAMetadataMaintenance(ctx, cfg, f.keys, guards[i], functionalHAMetadataManager(t, cfg), logger)
		if err != nil {
			t.Fatal(err)
		}
		defer owners[i].Close()
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan error, 2)
	complete := make(chan replication.MemberState, 2)
	for _, owner := range owners {
		go func(m *HAMetadataMaintenance) {
			done <- m.Run(runCtx, func(s replication.MemberState) error { complete <- s; return nil })
		}(owner)
	}
	for range 2 {
		select {
		case state := <-complete:
			if state.DurableIndex != f.oldCut.DurableIndex+1 || state.ConfirmedIndex != state.DurableIndex || state.AppliedIndex != state.DurableIndex {
				t.Fatal("actual group boundary differs", state)
			}
		case err := <-done:
			t.Fatal("maintenance exited before actual boundary", err)
		case <-ctx.Done():
			t.Fatal("actual HA boundary did not complete", ctx.Err())
		}
	}
	stop()
	for range 2 {
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
	for _, owner := range owners {
		if err := owner.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestHAMemoryActivationPublishesActualSevenRootsAndIndependentBootstrap(t *testing.T) {
	f := prepareHAMetadataMaintenanceFixture(t)
	prepareHAMetadataMaintenanceArtifacts(t, f)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	// N alone has no permission to create a seven-root namespace.
	guard, err := lock.Acquire(f.configs[0].Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(f.configs[0].ReplicationStatePath())
	if err != nil {
		t.Fatal(err)
	}
	owner, err := ActivateHAMemoryRuntime(ctx, f.configs[0], f.keys, guard, functionalHAMetadataManager(t, f.configs[0]), logger)
	if err == nil {
		owner.Close()
		t.Fatal("unconfirmed format2 boundary created roots")
	}
	if _, err := os.Lstat(haMemoryStateDirectory(f.configs[0])); !os.IsNotExist(err) {
		t.Fatal("refused source changed root namespace", err)
	}
	after, err := os.ReadFile(f.configs[0].ReplicationStatePath())
	if err != nil || string(before) != string(after) {
		t.Fatal("refused source changed actual member", err)
	}
	if owner != nil {
		if err := owner.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	completeHAMemoryActivationBoundary(t, f)
	for i, cfg := range f.configs {
		t.Run(string([]byte{'0' + byte(i)}), func(t *testing.T) {
			guard, err := lock.Acquire(cfg.Storage.DataDir)
			if err != nil {
				t.Fatal(err)
			}
			defer guard.Close()
			manager := functionalHAMetadataManager(t, cfg)
			a, err := ActivateHAMemoryRuntime(ctx, cfg, f.keys, guard, manager, logger)
			if err != nil {
				t.Fatal("actual member seven-root activation", err)
			}
			defer a.Close()
			if a.Receipt.Generation != 1 || a.Receipt.Revision != 1 || a.Receipt.Digest == ([32]byte{}) || a.BootstrapPath != haMemoryStateBootstrapPath(cfg) {
				t.Fatal("missing durable complete activation", a.Receipt, a.BootstrapPath)
			}
			var selected authenticatedstate.CandidateSeal
			if err := statecut.WithHASealAuthority(ctx, a.owners, func(_ context.Context, authority authenticatedstate.SealEligibility) error {
				seal, found, err := a.owners.Components.Publisher.Select(authority)
				if err != nil || !found {
					return errors.Join(authenticatedstate.ErrSealIneligible, err)
				}
				selected = seal
				return nil
			}); err != nil {
				t.Fatal("actual independent member/source selection", err)
			}
			expectedRole := authenticatedstate.SealPrimary
			if i == 1 {
				expectedRole = authenticatedstate.SealReplica
			}
			if selected.Role != expectedRole || selected.RootCount != 7 || selected.OrderingIndex != f.oldCut.DurableIndex+1 || selected.Term != f.oldCut.Term {
				t.Fatalf("actual seven-root role/cut differs: role=%d roots=%d index=%d term=%d", selected.Role, selected.RootCount, selected.OrderingIndex, selected.Term)
			}
			if err := a.Close(); err != nil {
				t.Fatal("actual activation joins", err)
			}
			verifyHAMemoryActivationBootstrap(t, ctx, cfg, f, manager)
			fresh, reopened, err := openHAMemoryActivationRead(ctx, cfg, f.keys, guard, manager, logger, haMemoryStateBootstrapPath(cfg))
			if err != nil {
				t.Fatal("fresh actual seven-root HA selected reopening", err)
			}
			if reopened.Seal != selected {
				t.Fatal("fresh reopening changed common selection")
			}
			if err := reopened.Close(); err != nil {
				t.Fatal("fresh derived descriptor join", err)
			}
			if err := fresh.Close(); err != nil {
				t.Fatal("fresh native/lease/publisher joins", err)
			}
			if _, err := os.Lstat(haMemoryStateBootstrapPath(cfg) + ".pending"); !os.IsNotExist(err) {
				t.Fatal("known joined pending bootstrap not retired", err)
			}
			for _, path := range []string{"ledger", "usage", "governance", "directory"} {
				if _, err := os.Stat(filepath.Join(haMemoryStateDirectory(cfg), path, "active.root")); err != nil {
					t.Fatal(path, err)
				}
			}
		})
	}
}

func verifyHAMemoryActivationBootstrap(t *testing.T, ctx context.Context, cfg config.Config, f haMetadataMaintenanceFixture, manager *resourcebudget.Manager) {
	t.Helper()
	member, clusterKey, err := readMemberState(cfg, f.keys.Master)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(clusterKey[:])
	carrier, authority, err := readHAMetadataCarrier(ctx, filepath.Join(cfg.Storage.DataDir, HAMetadataBaselineDirectory, "carrier.json"), clusterKey[:], member)
	if err != nil {
		t.Fatal(err)
	}
	v, err := vault.New(f.keys.Master)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	b, err := readHAMemoryStateBootstrap(ctx, cfg, v, manager, member, authority, carrier.Preparation.Evidence)
	if err != nil || b == nil {
		t.Fatal("independent cross-owner bootstrap qualification", err)
	}
	if b.Version != 2 || !bytes.Equal(b.LegacyMetadataKey, f.keys.Metadata) {
		t.Fatal("bootstrap omitted the independently Vault-certified legacy Metadata key")
	}
	b.Version = 1
	if err := b.validate(cfg, member, authority, carrier.Preparation.Evidence); err == nil {
		t.Fatal("old bootstrap version acquired a new source-key grant")
	}
	b.Version = 2
	legacy := b.LegacyMetadataKey
	b.LegacyMetadataKey = nil
	if err := b.validate(cfg, member, authority, carrier.Preparation.Evidence); err == nil {
		t.Fatal("bootstrap inferred a missing legacy Metadata key")
	}
	b.LegacyMetadataKey = legacy
	b.clearKeys()
	if !bytes.Equal(b.LegacyMetadataKey, make([]byte, 32)) {
		t.Fatal("bootstrap retirement retained legacy source key bytes")
	}
	other := member
	other.NodeID = "another-member"
	if b, err := readHAMemoryStateBootstrap(ctx, cfg, v, manager, other, authority, carrier.Preparation.Evidence); err == nil || b != nil {
		t.Fatal("bootstrap grants another member authority", err)
	}
	path := haMemoryStateBootstrapPath(cfg)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	changed := append([]byte(nil), raw...)
	changed[len(changed)-1] ^= 1
	if err := os.WriteFile(path, changed, 0600); err != nil {
		t.Fatal(err)
	}
	if b, err := readHAMemoryStateBootstrap(ctx, cfg, v, manager, member, authority, carrier.Preparation.Evidence); err == nil || b != nil {
		t.Fatal("tampered member bootstrap accepted", err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}
