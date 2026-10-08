package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ledger"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

func replicaRuntimeOwnerFixture(t *testing.T) (config.Config, *replicationRuntime, *ledger.Log, *boltstore.Store) {
	t.Helper()
	member, native, metadata, candidate := replicaGenerationFixture(t)
	cfg := testConfig(t)
	cfg.Storage.DataDir = filepath.Dir(candidate)
	cfg.Storage.MetadataFile = "metadata.db"
	// Give the real native file its ordinary configured directory alias. Metadata
	// already has the real standard name; the fixture's authenticated owners stay
	// unchanged and include a durable late settlement outside the applied cut.
	if err := os.Symlink("replica", filepath.Join(cfg.Storage.DataDir, "ledger")); err != nil {
		t.Fatal(err)
	}
	cfg.Replication = &config.Replication{ClusterID: "production-a", NodeID: "halro-1"}
	return cfg, member, native, metadata
}

func TestReplicaRuntimeOwnerDiscardsSavedBytesAndSelectsAppliedPrefix(t *testing.T) {
	cfg, member, native, metadata := replicaRuntimeOwnerFixture(t)
	output := replicaRuntimeProjectionPath(cfg)
	for _, path := range []string{output, output + ".usage"} {
		if err := os.WriteFile(path, []byte("untrusted old derivative"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	g, err := prepareReplicaRuntimeOwner(context.Background(), cfg, member, native, metadata, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close() })
	head, _, err := g.ledger.ReadPosition(context.Background())
	stats, statsErr := g.usageDisk.ReadStats(context.Background())
	if err != nil || statsErr != nil || head.Sequence != 4 || stats.Watermark.Sequence != 4 || stats.Totals.Attempts != 0 || stats.Metrics.ActiveRequests != 0 || g.usage.Aggregate() != nil {
		t.Fatal("excluded durable settlement entered default generation", head, stats, err, statsErr)
	}
}

func TestReplicaRuntimeOwnerRefusesProtectedPathsBeforeDiscard(t *testing.T) {
	for _, target := range []string{"ledger", "metadata", "credential"} {
		t.Run(target, func(t *testing.T) {
			cfg, member, native, metadata := replicaRuntimeOwnerFixture(t)
			output := replicaRuntimeProjectionPath(cfg)
			switch target {
			case "ledger":
				if err := os.Link(cfg.LedgerPath(), output); err != nil {
					t.Fatal(err)
				}
			case "metadata":
				if err := os.Symlink(cfg.MetadataPath(), output+".usage"); err != nil {
					t.Fatal(err)
				}
			case "credential":
				cfg.Storage.MasterKey.File = output
				if err := os.WriteFile(output, bytes.Repeat([]byte{0x41}, 32), 0600); err != nil {
					t.Fatal(err)
				}
			}
			before := replicaOwnerTree(t, cfg.Storage.DataDir)
			if g, err := prepareReplicaRuntimeOwner(context.Background(), cfg, member, native, metadata, nil); g != nil || err == nil {
				t.Fatal("protected path was discarded", g, err)
			}
			if !reflect.DeepEqual(before, replicaOwnerTree(t, cfg.Storage.DataDir)) {
				t.Fatal("refusal changed authority or prior derivative")
			}
		})
	}
}

// WalkDir never follows directory symlinks. Record the link itself and every
// real file so authority alias fixtures also detect link/target replacement.
func replicaOwnerTree(t *testing.T, root string) map[string][32]byte {
	t.Helper()
	result := make(map[string][32]byte)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		var payload []byte
		if entry.Type()&os.ModeSymlink != 0 {
			var target string
			target, err = os.Readlink(path)
			payload = []byte("symlink:" + target)
		} else {
			payload, err = os.ReadFile(path)
		}
		if err == nil {
			result[path] = sha256.Sum256(payload)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestReplicaRuntimeOwnerCancellationPreservesReplacementDuringBuild(t *testing.T) {
	cfg, member, native, metadata := replicaRuntimeOwnerFixture(t)
	output := replicaRuntimeProjectionPath(cfg)
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	var replaced bool
	foreign := []byte("replacement during native build")
	ctx := generationProbeContext{Context: base, probe: func() {
		if replaced {
			return
		}
		if _, err := os.Stat(output); err != nil {
			return
		}
		replaced = true
		if err := os.Rename(output, output+".moved"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(output, foreign, 0600); err != nil {
			t.Fatal(err)
		}
		cancel()
	}}
	g, err := prepareReplicaRuntimeOwner(ctx, cfg, member, native, metadata, nil)
	if g != nil || !errors.Is(err, context.Canceled) || !replaced {
		t.Fatal("build cancellation returned usable generation", g, err)
	}
	if actual, err := os.ReadFile(output); err != nil || !bytes.Equal(actual, foreign) {
		t.Fatal("failed build removed replacement", err)
	}
	if _, err := os.Stat(output + ".moved"); err != nil {
		t.Fatal("failed build removed moved inode", err)
	}
}
