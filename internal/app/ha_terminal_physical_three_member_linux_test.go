//go:build linux

package app

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/store/lock"
	"golang.org/x/sys/unix"
)

// The target deployment is three nodes. One Primary and two Replicas run real
// TLS physical credit, replicate facts to both peers with online checkpoints
// between them, then finish and close with every member at full pressure,
// where any new normal admission is refused. A restart must serve the facts on
// both Replicas. Functional Managers, not cfg.Validate or OS pressure.
func TestHAPhysicalThreeMemberPressuredTerminalAndRestart(t *testing.T) {
	ctx, f, logger := prepareHAPhysicalThreeMemberFixture(t, 6*time.Minute)
	written, revisions := runHAPhysicalOnlineGeneration(t, ctx, f.configs[:], f.keys.Master, logger, 0, nil, nil, haPhysicalGenerationPressuredClose)
	logHAPhysicalFixtureSpace(t, 0)
	runHAPhysicalOnlineGeneration(t, ctx, f.configs[:], f.keys.Master, logger, 1, written, revisions, haPhysicalGenerationNormal)
}

// With two Replicas a ticket holds one promise per peer, so a lost reply can
// leave one peer known and the other unknown; see
// TestHAPhysicalLostCreditRepliesResolveByResend.
func TestHAPhysicalThreeMemberLostCreditRepliesResolveByResend(t *testing.T) {
	ctx, f, logger := prepareHAPhysicalThreeMemberFixture(t, 6*time.Minute)
	written, revisions := runHAPhysicalOnlineGeneration(t, ctx, f.configs[:], f.keys.Master, logger, 0, nil, nil, haPhysicalGenerationLostReplies)
	runHAPhysicalOnlineGeneration(t, ctx, f.configs[:], f.keys.Master, logger, 1, written, revisions, haPhysicalGenerationNormal)
}

func prepareHAPhysicalThreeMemberFixture(t *testing.T, timeout time.Duration) (context.Context, haThreeGroupFixture, *slog.Logger) {
	t.Helper()
	root := os.Getenv("HALRO_EXT4_ROOT")
	if root == "" {
		t.Skip("requires isolated ext4 with three members' original bank capacity")
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(root, &fs); err != nil || fs.Type != unix.EXT4_SUPER_MAGIC {
		t.Fatal("actual ext4", err)
	}
	if uint64(fs.Blocks)*uint64(fs.Bsize) < 3800<<20 {
		t.Skip("two Replicas' full Ledger and Usage banks need at least 3800 MiB usable ext4 capacity (4096 MiB backing)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)
	standalone, _, _ := openBootstrappedRuntime(t)
	f := prepareHAThreeGroupFixture(t, standalone)
	// The independent baseline binds this ceiling; set it before publication.
	for n, cfg := range f.configs {
		limits := *cfg.ResourceBudget
		limits.MetadataJournalDiskBytes = 576 << 20
		cfg.ResourceBudget = &limits
		f.configs[n] = cfg
	}
	prepareHAThreeGroupArtifacts(t, f)
	completeHAThreeGroupBoundary(t, f)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	for n, cfg := range f.configs {
		guard, err := lock.Acquire(cfg.Storage.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		activation, err := ActivateHAMemoryRuntime(ctx, cfg, f.keys, guard, functionalHAMetadataManager(t, cfg), logger)
		if err != nil {
			t.Fatal("three-member activation", n, err)
		}
		if err := errors.Join(activation.Close(), guard.Close()); err != nil {
			t.Fatal(err)
		}
	}
	return ctx, f, logger
}
