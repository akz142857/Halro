package governance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ownedfile"
	bbolt "go.etcd.io/bbolt"
)

type projectionOwnerProbeContext struct {
	context.Context
	probe func()
}

func (c projectionOwnerProbeContext) Err() error { c.probe(); return c.Context.Err() }

func TestGovernanceProjectionFailedBuildPreservesReplacementAndClosesActualFile(t *testing.T) {
	_, log, _, _, _ := testManager(t)
	if _, err := log.Append(context.Background(), projectionEvent(1)); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "view.db")
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	replaced := false
	ctx := projectionOwnerProbeContext{Context: base, probe: func() {
		if replaced {
			return
		}
		if _, err := os.Stat(output); errors.Is(err, os.ErrNotExist) {
			return
		} else if err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(output, output+".owned"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(output, []byte("foreign replacement"), 0o600); err != nil {
			t.Fatal(err)
		}
		replaced = true
		cancel()
	}}
	p, err := BuildGovernanceProjection(ctx, log.path, log.key, output, log.Summary())
	if p != nil || !replaced || !errors.Is(err, context.Canceled) || !errors.Is(err, ownedfile.ErrReplaced) {
		t.Fatal("failed constructor lost cancellation or physical identity", p, err)
	}
	if raw, err := os.ReadFile(output); err != nil || string(raw) != "foreign replacement" {
		t.Fatal("cleanup removed replacement", err)
	}
	// The moved original inode must be physically closed, not merely hidden
	// behind a nil projection. Another bbolt opener can acquire its lock.
	db, err := bbolt.Open(output+".owned", 0o600, &bbolt.Options{ReadOnly: true, Timeout: time.Second})
	if err != nil {
		t.Fatal("failed constructor left actual file open", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestGovernanceProjectionDiscardPreservesReplacementAfterPhysicalClose(t *testing.T) {
	_, log, _, _, _ := testManager(t)
	output := filepath.Join(t.TempDir(), "view.db")
	p, err := BuildGovernanceProjection(context.Background(), log.path, log.key, output, log.Summary())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err := os.Rename(output, output+".owned"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(output, []byte("foreign replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := p.Discard(); !errors.Is(err, ownedfile.ErrReplaced) {
			t.Fatal("replacement removal was not refused", err)
		}
	}
	if _, err := p.ReadHead(context.Background()); err == nil {
		t.Fatal("retired physical projection still usable")
	}
	if raw, err := os.ReadFile(output); err != nil || string(raw) != "foreign replacement" {
		t.Fatal("replacement did not survive repeated cleanup", err)
	}
}

func TestGovernanceProjectionDataFileLimitRejectsAndRemovesCandidate(t *testing.T) {
	_, log, _, _, _ := testManager(t)
	output := filepath.Join(t.TempDir(), "limited.db")
	if p, err := BuildGovernanceProjectionWithLimit(context.Background(), log.path, log.key, output, log.Summary(), -1); p != nil || !errors.Is(err, ErrProjectionDiskLimitInvalid) {
		t.Fatal("negative Governance projection ceiling was not rejected", p, err)
	}
	p, err := BuildGovernanceProjectionWithLimit(context.Background(), log.path, log.key, output, log.Summary(), 4<<10)
	if p != nil || !errors.Is(err, ErrProjectionDiskLimit) {
		t.Fatal("undersized Governance projection ceiling did not fail closed", p, err)
	}
	if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed Governance candidate was not removed", err)
	}
}

func TestGovernanceProjectionDataFileLimitRejectsGrowthAtomically(t *testing.T) {
	_, log, _, _, _ := testManager(t)
	output := filepath.Join(t.TempDir(), "sized.db")
	baseline, err := BuildGovernanceProjection(context.Background(), log.path, log.key, output, log.Summary())
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(output)
	if err != nil {
		t.Fatal(err)
	}
	if err := baseline.Discard(); err != nil {
		t.Fatal(err)
	}
	bounded, err := BuildGovernanceProjectionWithLimit(context.Background(), log.path, log.key, output, log.Summary(), info.Size())
	if err != nil {
		t.Fatal(err)
	}
	defer bounded.Discard()
	var applied Summary
	var rejected bool
	for i := 1; i <= 10000; i++ {
		record, err := log.Append(context.Background(), projectionEvent(i))
		if err != nil {
			t.Fatal(err)
		}
		if err := bounded.Apply(context.Background(), record); err != nil {
			if !errors.Is(err, ErrProjectionDiskLimit) {
				t.Fatal("live Governance projection failed for a non-capacity reason", err)
			}
			rejected = true
			break
		}
		applied = log.Summary()
	}
	if !rejected {
		t.Fatal("live Governance projection never reached its file ceiling")
	}
	if head, err := bounded.ReadHead(context.Background()); err != nil || head != applied {
		t.Fatal("rejected projection transaction advanced its native position", head, applied, err)
	}
}
