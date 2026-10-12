package main

import (
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/backup"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/diskinventory"
	"github.com/akz142857/Halro/internal/metadatastate"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

// E2 (board §42) for Metadata migration. Gate 1 runs the complete command to
// a clean exit. Gates 2 and 3 run the runtime-directory preparation once to
// the end and once killed by a real SIGKILL at a boundary, then rerun with
// restart; the startup inventory must match the disk and the trees must
// match. The fixture has no Usage, so gate 4 does not apply.
func TestE2MetadataMigrationCleanExitAndSIGKILL(t *testing.T) {
	if os.Getenv("HALRO_E2") != "1" {
		t.Skip("E2 evidence harness (board §42.1): set HALRO_E2=1; its open findings fail it")
	}
	run := func(t *testing.T, cfg config.Config, a metadataMigrationArguments) *resourcebudget.Manager {
		m := migrationRestartManager(t)
		ctx, err := backup.NewWorkspaceBudget(context.Background(), 16<<20, func(ctx context.Context, size int64) (func() error, error) {
			p, err := m.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: 32 << 20, Disk: size})
			if err != nil {
				return nil, err
			}
			return p.Release, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		bound, err := ctx.Bind(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		backupKey, err := metadataMigrationKeyFile(a.backupKey)
		if err != nil {
			t.Fatal(err)
		}
		defer clear(backupKey)
		archive, err := backup.OpenVerified(bound, a.backup, backupKey)
		if err != nil {
			t.Fatal(err)
		}
		ledgerKey, err := metadataMigrationKeyFile(a.ledgerKey)
		if err != nil {
			t.Fatal(err)
		}
		defer clear(ledgerKey)
		journalKey, err := metadataMigrationKeyFile(a.sourceJournalKey)
		if err != nil {
			t.Fatal(err)
		}
		defer clear(journalKey)
		guard, err := lock.Acquire(cfg.Storage.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		if err := prepareRuntimeMigrationDirectory(bound, cfg, a, archive, ledgerKey, m, guard); err != nil {
			t.Fatal(err)
		}
		o := metadatastate.BaselineOptions{Identity: a.identity, InstanceID: a.instanceID, KeyGeneration: a.keyGeneration, Budget: m, MaxSourceBytes: 32 << 20, MaxDiskBytes: 32 << 20, MaxAppendBytes: 8 << 20}
		p, err := metadatastate.PrepareFormat2FromBackup(bound, archive, ledgerKey, journalKey, cfg.Storage.DataDir, a.output, guard, o, 16<<20)
		if err != nil {
			t.Fatal("Metadata preparation", err)
		}
		if err := p.Close(); err != nil {
			t.Fatal(err)
		}
		if err := archive.Close(); err != nil {
			t.Fatal(err)
		}
		if err := guard.Close(); err != nil {
			t.Fatal(err)
		}
		return m
	}
	inventory := func(t *testing.T, cfg config.Config) int64 {
		guard, err := lock.Acquire(cfg.Storage.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		defer guard.Close()
		root, err := filepath.Abs(cfg.Storage.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		b, err := diskinventory.Reconcile(context.Background(), diskinventory.Options{DataDir: root, Guard: guard, Budget: migrationRestartManager(t), Ceiling: 64 << 20, Deadline: time.Minute})
		if err != nil {
			t.Fatal("startup inventory after migration", err)
		}
		got := b.Snapshot().ChargeBytes
		if err := b.Close(); err != nil {
			t.Fatal(err)
		}
		var want int64
		if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			var st syscall.Stat_t
			if err := syscall.Lstat(path, &st); err != nil {
				return err
			}
			charge := st.Blocks * 512
			if !d.IsDir() && st.Size > charge {
				charge = st.Size
			}
			want += charge
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatal("startup inventory differs from the disk", got, want)
		}
		return got
	}
	random := regexp.MustCompile(`[0-9a-f]{16,}|[0-9]{4,}`)
	tree := func(t *testing.T, cfg config.Config) []string {
		root := filepath.Dir(cfg.Storage.DataDir)
		var names []string
		if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			names = append(names, random.ReplaceAllString(filepath.ToSlash(rel), "#"))
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		sort.Strings(names)
		return names
	}
	fixture := func(t *testing.T) (config.Config, metadataMigrationArguments) {
		cfg, a, _, _, _ := metadataMigrationCommandFixture(t)
		cfg.ResourceBudget.PersistentInventoryDiskBytes = 64 << 20
		a.activateRuntime, a.output = true, filepath.Join(cfg.Storage.DataDir, "memory-state")
		return cfg, a
	}

	// Gate 1 on the complete command's clean exit (candidate output).
	commandCfg, commandArgs, commandManager, _, _ := metadataMigrationCommandFixture(t)
	if err := prepareMetadataMigrationCommand(context.Background(), commandCfg, commandArgs, commandManager, io.Discard); err != nil {
		t.Fatal("clean migration command", err)
	}
	s := commandManager.Snapshot()
	claims := int64(s.NormalClaims + s.RecoveryClaims)
	for class, cost := range s.Classes {
		want := int64(0)
		if resourcebudget.Class(class) == resourcebudget.Control {
			want = claims * resourcebudget.ClaimRAMBytes
		}
		if cost.RAM != want || cost.Unconfirmed != 0 {
			t.Error("clean migration kept non-persistent charges", class, cost, s.Classes)
		}
	}
	t.Log("persistent charges after the clean migration command", s.Used, "claims", claims)

	// Gates 2 and 3 compare the runtime-directory preparation run to the end
	// with the same preparation killed at a boundary and rerun with restart.
	cleanCfg, cleanArgs := fixture(t)
	run(t, cleanCfg, cleanArgs)
	cleanCharge, cleanTree := inventory(t, cleanCfg), tree(t, cleanCfg)

	killedCfg, killedArgs := fixture(t)
	killMigrationNamespaceBoundary(t, killedCfg, killedArgs, "staging-synced", false)
	killedArgs.restartPreparation = true
	run(t, killedCfg, killedArgs)
	killedCharge, killedTree := inventory(t, killedCfg), tree(t, killedCfg)
	t.Log("inventory charge clean", cleanCharge, "after SIGKILL", killedCharge)
	extra, missing := e2MigrationDiff(killedTree, cleanTree), e2MigrationDiff(cleanTree, killedTree)
	if len(extra) != 0 || len(missing) != 0 {
		t.Fatal("files after SIGKILL differ from a clean exit; extra:", extra, "missing:", missing)
	}
}

func e2MigrationDiff(a, b []string) []string {
	count := map[string]int{}
	for _, name := range b {
		count[name]++
	}
	var out []string
	for _, name := range a {
		if count[name] > 0 {
			count[name]--
			continue
		}
		out = append(out, name)
	}
	return out
}
