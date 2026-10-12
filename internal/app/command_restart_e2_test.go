package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/backup"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/usage"
)

// E2 (board §42): each state-changing offline command, run once to a clean
// exit and once killed by SIGKILL mid-command and then rerun, leaves only
// persistent (disk) charges, a startup inventory that matches the disk, no
// leftover files and no duplicated Usage rows.

type e2Case struct {
	cfg        config.Config
	output     string // backup output directory; empty when unused
	configPath string
	archive    string // restore source
	tmp        string // this case's TMPDIR
	backupID   string
}

var e2BackupKey = bytes.Repeat([]byte{0x6b}, 32)

func e2Fixture(t *testing.T, command string) e2Case {
	t.Helper()
	var cfg config.Config
	if command == "usage-compact" {
		cfg = compactDiskFixture(t, usage.FormatNDJSON, "missing")
		budget := *budgetedBackupOperationFixture(t).ResourceBudget
		cfg.ResourceBudget, cfg.ResourcePressure = &budget, pressureConfigFixture()
	} else {
		cfg = budgetedBackupOperationFixture(t)
	}
	b := *cfg.ResourceBudget
	b.PersistentInventoryDiskBytes = b.MinimumPersistentInventoryDiskBytes() + 64<<20
	cfg.ResourceBudget = &b
	c := e2Case{cfg: cfg, output: filepath.Join(t.TempDir(), "out"), configPath: filepath.Join(t.TempDir(), "config.yaml"), tmp: t.TempDir()}
	if err := os.Mkdir(c.output, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.configPath, []byte("version: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if command == "backup-restore" {
		c.archive = filepath.Join(t.TempDir(), "source.hmbk")
		manifest, err := CreateBackup(context.Background(), cfg, c.configPath, c.archive, e2BackupKey)
		if err != nil {
			t.Fatal(err)
		}
		c.backupID = manifest.BackupID
	}
	return c
}

// e2Run runs one command to completion, calling pause at its mid-command point.
func e2Run(ctx context.Context, c e2Case, command string, pause func(string) error) error {
	switch command {
	case "backup-create":
		_, err := createBackupWithWriter(ctx, c.cfg, c.configPath, filepath.Join(c.output, "e2.hmbk"), e2BackupKey, CreateBackupOptions{}, func(o backup.CreateOptions) (backup.Manifest, error) {
			if err := pause("backup-writer"); err != nil {
				return backup.Manifest{}, err
			}
			return backup.Create(o)
		})
		return err
	case "backup-restore":
		_, err := restoreBackupWithExtract(ctx, c.cfg, c.archive, e2BackupKey, c.backupID, RestoreOptions{}, defaultKMSWrapperFactory, func(path string, key []byte, destination string) (backup.Manifest, error) {
			manifest, err := backup.Extract(path, key, destination)
			if err == nil {
				err = pause("restore-extracted")
			}
			return manifest, err
		})
		return err
	case "usage-compact":
		paused := false
		_, err := compactUsageDisk(ctx, c.cfg, smallCompactionLimits(), func(int, usage.DiskArchiveCut) error {
			if paused {
				return nil
			}
			paused = true
			return pause("compact-step")
		}, nil)
		return err
	case "usage-prune":
		// No mid-command callback: the parent kills it on TMPDIR evidence.
		_, err := PruneUsage(ctx, c.cfg, time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC))
		return err
	case "ledger-seal":
		_, err := sealLedger(ctx, c.cfg, false, func(*SealReport, *ledger.Log) error { return pause("seal-observed") })
		return err
	}
	return errors.New("unknown E2 command " + command)
}

func TestE2CommandChild(t *testing.T) {
	path := os.Getenv("HALRO_E2_CHILD_CASE")
	if path == "" {
		t.Skip("subprocess only")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var c e2Case
	var wire struct {
		Config                                     config.Config
		Output, ConfigPath, Archive, BackupID, Cmd string
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	c = e2Case{cfg: wire.Config, output: wire.Output, configPath: wire.ConfigPath, archive: wire.Archive, backupID: wire.BackupID}
	marker := os.Getenv("HALRO_E2_CHILD_MARKER")
	err = e2Run(context.Background(), c, wire.Cmd, func(phase string) error { return selectedRotationChildPause(marker, phase) })
	t.Fatal("child escaped its kill point", err)
}

func e2Kill(t *testing.T, c e2Case, command string) {
	t.Helper()
	wire := struct {
		Config                                     config.Config
		Output, ConfigPath, Archive, BackupID, Cmd string
	}{c.cfg, c.output, c.configPath, c.archive, c.backupID, command}
	raw, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	var back struct{ Config config.Config }
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if again, err := json.Marshal(struct{ Config config.Config }{back.Config}); err != nil || !bytes.Contains(raw, again[len(`{"Config":`):len(again)-1]) {
		t.Fatal("config does not survive the child round trip", err)
	}
	casePath, marker := filepath.Join(t.TempDir(), "case.json"), filepath.Join(t.TempDir(), "reached")
	if err := os.WriteFile(casePath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestE2CommandChild$", "-test.count=1")
	child.Env = append(os.Environ(), "HALRO_E2_CHILD_CASE="+casePath, "HALRO_E2_CHILD_MARKER="+marker)
	var output bytes.Buffer
	child.Stdout, child.Stderr = &output, &output
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	// Prune has no callback; it is killed when its second Usage reference
	// (the reclaim pass) appears in its private TMPDIR.
	seen := map[string]bool{}
	reached := func() bool {
		if command != "usage-prune" {
			_, err := os.Lstat(marker)
			return err == nil
		}
		names, _ := filepath.Glob(filepath.Join(c.tmp, "halro-usage-verify-*"))
		for _, name := range names {
			seen[name] = true
		}
		return len(seen) >= 2
	}
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for !reached() {
		select {
		case err := <-done:
			t.Fatal("child exited before its kill point", err, len(seen), output.String())
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
	if err := child.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("SIGKILL did not end the child")
	}
	phase, _ := os.ReadFile(marker)
	t.Log("killed at", string(phase), "references seen", len(seen))
}

// e2Persistent checks gate 1 (§42, corrected in §42.1): only disk charges
// remain, with their claims' control RAM and, by design, each command
// Metadata file owner's fixed descriptor RAM beside its disk ceiling.
func e2Persistent(t *testing.T, ctx context.Context, s resourcebudget.Stats, metadataDisk int64) {
	t.Helper()
	scope, ok := ctx.Value(commandResourceScopeKey{}).(*commandResourceScope)
	if !ok {
		t.Fatal("command ran outside its resource scope")
	}
	files := int64(0)
	for _, owner := range scope.metadataFiles {
		if owner.path != "" {
			files++
		}
	}
	claims := int64(s.NormalClaims + s.RecoveryClaims)
	for class, cost := range s.Classes {
		want := int64(0)
		switch resourcebudget.Class(class) {
		case resourcebudget.Recovery:
			want = files * commandMetadataFileOwnerRAMBytes
		case resourcebudget.Control:
			want = claims * resourcebudget.ClaimRAMBytes
		}
		if cost.RAM != want || cost.Unconfirmed != 0 {
			t.Fatal("clean exit kept non-persistent charges", class, cost, "want RAM", want, "classes", s.Classes, "metadata files", files, "claims", claims)
		}
	}
	if s.Classes[resourcebudget.Recovery].Disk < files*metadataDisk {
		t.Fatal("Metadata file owners lost their disk ceilings", s.Classes)
	}
	t.Log("persistent charges after clean exit", s.Used, "metadata files", files, "claims", claims)
}

// e2Inventory checks gate 2 and returns the charge.
func e2Inventory(t *testing.T, cfg config.Config) int64 {
	t.Helper()
	ctx, pressure, err := commandBudgetContext(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	baseline, err := preparePersistentInventory(ctx, cfg, guard, pressure)
	if err != nil {
		t.Fatal("startup inventory after the command", err)
	}
	got := baseline.Snapshot().ChargeBytes
	if err := baseline.Close(); err != nil {
		t.Fatal(err)
	}
	var want int64
	root, err := filepath.Abs(cfg.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
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

// e2AcceptedResidue is the TMPDIR residue a killed command leaves, accepted
// and documented by the user's decision (board §42.2); its reclaim is D-01/R-01.
var e2AcceptedResidue = regexp.MustCompile(`^tmp/(halro-segment-directory-#\.idx|halro-usage-verify-#(/(ledger|usage)\.db)?)$`)

var e2Random = regexp.MustCompile(`[0-9a-f]{16,}|[0-9]{4,}|[a-z0-9]{26}`)

// e2Tree lists the data and output trees with random name parts normalized.
func e2Tree(t *testing.T, c e2Case) []string {
	t.Helper()
	var names []string
	// The data directory's parent holds restore staging; TMPDIR holds
	// private references (each case gets its own).
	for label, root := range map[string]string{"parent": filepath.Dir(c.cfg.Storage.DataDir), "out": c.output, "tmp": c.tmp} {
		if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			names = append(names, label+"/"+e2Random.ReplaceAllString(filepath.ToSlash(rel), "#"))
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	sort.Strings(names)
	return names
}

func e2Usage(t *testing.T, cfg config.Config) usage.ReconciliationReport {
	t.Helper()
	report, err := VerifyUsage(context.Background(), cfg)
	if err != nil {
		t.Fatal("Usage verification after the command", err)
	}
	if report.Missing != 0 || report.Duplicates != 0 || report.Extra != 0 {
		t.Fatal("Usage rows missing or duplicated", report)
	}
	return report
}

func TestE2CommandsCleanExitAndSIGKILL(t *testing.T) {
	for _, command := range []string{"backup-create", "backup-restore", "usage-compact", "usage-prune", "ledger-seal"} {
		t.Run(command, func(t *testing.T) {
			clean := e2Fixture(t, command)
			ctx, pressure, err := commandBudgetContext(context.Background(), clean.cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("TMPDIR", clean.tmp)
			if err := e2Run(ctx, clean, command, func(string) error { return nil }); err != nil {
				t.Fatal("clean run", err)
			}
			e2Persistent(t, ctx, pressure.logicalBudget().Snapshot(), clean.cfg.ResourceBudget.MetadataStoreDiskBytes)
			cleanCharge, cleanUsage, cleanTree := e2Inventory(t, clean.cfg), e2Usage(t, clean.cfg), e2Tree(t, clean)

			killed := e2Fixture(t, command)
			t.Setenv("TMPDIR", killed.tmp)
			e2Kill(t, killed, command)
			if err := e2Run(context.Background(), killed, command, func(string) error { return nil }); err != nil {
				t.Fatal("rerun after SIGKILL", err)
			}
			killedCharge, killedUsage, killedTree := e2Inventory(t, killed.cfg), e2Usage(t, killed.cfg), e2Tree(t, killed)
			t.Log("inventory charge clean", cleanCharge, "after SIGKILL", killedCharge)
			if killedUsage.ParquetRecords != cleanUsage.ParquetRecords || killedUsage.LedgerRecords != cleanUsage.LedgerRecords {
				t.Fatal("Usage rows differ after SIGKILL", cleanUsage, killedUsage)
			}
			extra, missing := e2Diff(killedTree, cleanTree), e2Diff(cleanTree, killedTree)
			var accepted []string
			extra = slices.DeleteFunc(extra, func(name string) bool {
				if e2AcceptedResidue.MatchString(name) {
					accepted = append(accepted, name)
					return true
				}
				return false
			})
			if len(extra) != 0 || len(missing) != 0 {
				t.Fatal("files after SIGKILL differ from a clean exit; extra:", extra, "missing:", missing)
			}
			t.Log("accepted TMPDIR residue (board §42.2)", accepted)
		})
	}
}

// e2Diff returns the entries of a beyond those in b (as multisets).
func e2Diff(a, b []string) []string {
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
