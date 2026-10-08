package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/usage"
)

func TestStandaloneUsageOwnerFreshRebuildAndAuthenticatedSuffixAcrossRoll(t *testing.T) {
	r, _ := summaryRuntime(t, 2)
	r.stopBackgroundWork()
	if err := r.usage.closeOwner(); err != nil {
		t.Fatal(err)
	}
	path := standaloneUsageProjectionPath(r.config)
	if err := os.WriteFile(path, []byte("untrusted crash leftover"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := installDiskUsageQueries(t, r)
	ctx := context.Background()
	first, err := p.ReadStats(ctx)
	if err != nil || first.Metrics.RequestsSuccess != 2 || r.usage.Aggregate != nil {
		t.Fatal("fresh owner did not replace untrusted history", first, err)
	}
	for _, id := range []string{"before_roll", "after_roll"} {
		seedLedgerActivity(t, r, id)
		if err := r.usageCollector.CatchUp(ctx); err != nil {
			t.Fatal(err)
		}
		if id == "before_roll" {
			rolled, err := r.ledger.Roll()
			if err != nil || !rolled.Rolled {
				t.Fatal(rolled, err)
			}
			// An empty new generation changes the native physical head, not
			// the last applied Usage record. Repeated catch-up must remain valid.
			for i := 0; i < 2; i++ {
				if err := r.usageCollector.CatchUp(ctx); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	want, err := p.ReadStats(ctx)
	anchor, anchorErr := p.ReadReplayAnchor(ctx)
	head, hash, _ := r.ledger.ChainHead()
	if err != nil || anchorErr != nil || want.Metrics.RequestsSuccess != 4 || anchor.Head != head || anchor.Hash != hash {
		t.Fatal(want, anchor, head, err, anchorErr)
	}
	if err := r.usage.closeOwner(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("owned database survived actual close", err)
	}
	// Rebuild in the same locked Runtime from the complete authenticated
	// source, including sealed EOF/roll links, without using old Usage bytes.
	p = installDiskUsageQueries(t, r)
	got, err := p.ReadStats(ctx)
	if err != nil || !reflect.DeepEqual(got.Metrics, want.Metrics) || got.Totals != want.Totals || got.Watermark != want.Watermark {
		t.Fatal("reconstruction changed cumulative facts", got, want, err)
	}
}

func TestStandaloneUsageOwnerRefusesAuthorityAliasBeforeDeletion(t *testing.T) {
	for _, kind := range []string{"master-path", "native-hardlink", "native-symlink"} {
		t.Run(kind, func(t *testing.T) {
			r, _ := summaryRuntime(t, 1)
			r.stopBackgroundWork()
			if err := r.usage.closeOwner(); err != nil {
				t.Fatal(err)
			}
			cfg := r.config
			path := standaloneUsageProjectionPath(cfg)
			file := cfg.LedgerPath()
			switch kind {
			case "master-path":
				file = cfg.Storage.MasterKey.File
				if err := os.Link(file, path); err != nil {
					t.Fatal(err)
				}
				cfg.Storage.MasterKey.File = path
			case "native-hardlink":
				if err := os.Link(file, path); err != nil {
					t.Fatal(err)
				}
			case "native-symlink":
				if err := os.Symlink(file, path); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			lease, err := r.ledger.BeginMaintenance(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			view, collector, err := prepareStandaloneUsage(context.Background(), cfg, r.ledger, r.accountingState.projection, lease, r.resourcePressure)
			if closeErr := lease.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			if err == nil || view != nil || collector != nil {
				t.Fatal("authority alias accepted", view, collector, err)
			}
			after, err := os.ReadFile(file)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("refusal changed native/key bytes", err)
			}
			if _, err := os.Lstat(path); err != nil {
				t.Fatal("refusal removed authority alias", err)
			}
		})
	}
}

func TestStandaloneUsageOwnerClosePreservesReplacementFile(t *testing.T) {
	r, _ := summaryRuntime(t, 1)
	installDiskUsageQueries(t, r)
	path := standaloneUsageProjectionPath(r.config)
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	const replacement = "replacement must survive stale owner"
	if err := os.WriteFile(path, []byte(replacement), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.usage.closeOwner(); err == nil {
		t.Fatal("replaced file accepted as owned")
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != replacement {
		t.Fatal("stale owner deleted replacement", err)
	}
	// Terminal Runtime reports the identity failure; its cleanup must still
	// join and release all other stores and the directory lock.
	if err := r.Close(); err == nil {
		t.Fatal("Runtime hid selected-owner cleanup failure")
	}
	guard, err := lock.AcquireExistingReadOnly(r.config.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStandaloneUsageOwnerTerminalCloseJoinsActualDiskReader(t *testing.T) {
	r, _ := summaryRuntime(t, 1)
	p := installDiskUsageQueries(t, r)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	blocked := &blockedProjectionReadContext{Context: context.Background(), entered: entered, release: release}
	read := make(chan error, 1)
	go func() { _, err := p.ReadStats(blocked); read <- err }()
	awaitDrainSignal(t, entered)
	closed := make(chan error, 1)
	go func() { closed <- r.Close() }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := p.ReadStats(context.Background())
		if errors.Is(err, usage.ErrDiskUsageClosed) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("disk owner did not withdraw admission", err)
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-closed:
		t.Fatal("Runtime closed before actual disk reader", err)
	default:
	}
	if _, _, err := r.accountingState.projection.ReadPosition(context.Background()); err != nil {
		t.Fatal("Ledger dependency closed before Usage reader", err)
	}
	if next, err := lock.AcquireExistingReadOnly(r.config.Storage.DataDir); err == nil {
		_ = next.Close()
		t.Fatal("data lock released before Usage reader")
	}
	unblock()
	if err := <-read; err != nil {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(standaloneUsageProjectionPath(r.config)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("owned file remained after join", err)
	}
}

func TestStandaloneUsageOwnerRequiresMatchingFrozenLedgerPrefix(t *testing.T) {
	r, _ := summaryRuntime(t, 1)
	r.stopBackgroundWork()
	before, err := os.Lstat(standaloneUsageProjectionPath(r.config))
	if err != nil {
		t.Fatal(err)
	}
	beforeStats, err := r.usage.disk.ReadStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// An actual durable native fact is absent from the selected projection.
	// The constructor must refuse before constructing/publishing a Usage DB.
	_, _, err = r.ledger.AppendRecordWithReceipt(context.Background(), ledger.Event{
		EventID: "unapplied-usage-prefix", Kind: ledger.EventRequestAccepted,
		RequestID: "unapplied", ProjectID: "project_1", KeyID: "key_1",
		PeriodID: "2026-10-04", PeriodTimezone: "UTC", PeriodTimezoneVersion: 1,
		OccurredAt: time.Now().UTC(), RequestedModel: "chat",
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := r.ledger.BeginMaintenance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	view, collector, err := prepareStandaloneUsage(context.Background(), r.config, r.ledger, r.accountingState.projection, lease, r.resourcePressure)
	_ = lease.Close()
	if err == nil || view != nil || collector != nil {
		t.Fatal("unapplied prefix published", view, collector, err)
	}
	after, statErr := os.Lstat(standaloneUsageProjectionPath(r.config))
	afterStats, readErr := r.usage.disk.ReadStats(context.Background())
	if statErr != nil || readErr != nil || !os.SameFile(before, after) || beforeStats != afterStats {
		t.Fatal("failed candidate changed the existing selected owner", statErr, readErr)
	}
}

func TestStandaloneUsageOwnerLiveSuffixIntegrityFailureDoesNotAdvance(t *testing.T) {
	r, _ := summaryRuntime(t, 1)
	p := installDiskUsageQueries(t, r)
	before, err := p.ReadReplayAnchor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	seedLedgerActivity(t, r, "suffix-tamper")
	nativeHead, _, _ := r.ledger.ChainHead()
	file, err := os.OpenFile(r.config.LedgerPath(), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	info, err := file.Stat()
	var value [1]byte
	if err == nil {
		_, err = file.ReadAt(value[:], info.Size()-1)
	}
	if err == nil {
		value[0] ^= 1
		_, err = file.WriteAt(value[:], info.Size()-1)
	}
	if err = errors.Join(err, file.Close()); err != nil {
		t.Fatal(err)
	}
	if err := r.usageCollector.CatchUp(context.Background()); err == nil {
		t.Fatal("unauthenticated native suffix applied")
	}
	after, err := p.ReadReplayAnchor(context.Background())
	// A valid earlier frame may commit before the corrupt final frame is
	// reached. Only the fully authenticated prefix can be reported/applied.
	if err != nil || after.Head.Sequence < before.Head.Sequence || after.Head.Sequence >= nativeHead.Sequence {
		t.Fatal("integrity failure advanced through bad native frame", before, after, err)
	}
}

func TestStandaloneUsageOwnerWindowMaintenanceRequiresProofAndAdvancesFinitePrefixes(t *testing.T) {
	r, _ := summaryRuntime(t, 0)
	seedRetentionUsage(t, r, 129, time.Now().UTC().AddDate(0, 0, -60))
	p := installDiskUsageQueries(t, r)
	ctx := context.Background()
	before, err := p.ReadStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r.pruneUsageWindow()
	unverified, err := p.ReadStats(ctx)
	if err != nil || unverified.Window != before.Window {
		t.Fatal("unknown export root authorized removal", unverified, before, err)
	}
	r.exportUsageParquet()
	if _, err := r.usageExporter.VerifyDiskArchive(ctx, p, usage.DefaultArchiveVerifyLimits()); err != nil {
		t.Fatal(err)
	}
	rootPath := r.config.UsagePath() + "/manifest.json"
	root, err := os.ReadFile(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	r.replication = &replicationRuntime{role: replication.RoleReplica}
	r.pruneUsageWindow()
	r.replication = nil
	member, err := p.ReadStats(ctx)
	if err != nil || member.Window != before.Window {
		t.Fatal("Standalone proof authorized Replica retention", member, err)
	}
	r.pruneUsageWindow()
	first, err := p.ReadStats(ctx)
	if err != nil || first.Window.Attempts == 0 || first.Window.Attempts >= 129 || first.Window.Summaries >= 129 {
		t.Fatal("one tick claimed entire backlog", first, err)
	}
	for i := 0; i < 8; i++ {
		r.pruneUsageWindow()
	}
	after, err := p.ReadStats(ctx)
	if err != nil || after.Window.Attempts != 0 || after.Window.Summaries != 0 || after.Window.TrimmedAttempts != 129 || after.Window.Floor != before.Watermark.Sequence+1 || after.Watermark != before.Watermark || after.Totals != before.Totals || after.Metrics != before.Metrics {
		t.Fatal("window maintenance changed cumulative facts or left backlog", before, after, err)
	}
	actualRoot, err := os.ReadFile(rootPath)
	if err != nil || !bytes.Equal(root, actualRoot) {
		t.Fatal("window trimming mutated archive", err)
	}
}
