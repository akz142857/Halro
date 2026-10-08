package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/store/lock"
)

func prepareHAGroupRotationColdMembers(t *testing.T, ctx context.Context) (haMetadataMaintenanceFixture, []*haSelectedRuntimeFoundation) {
	t.Helper()
	f := prepareHAMetadataMaintenanceFixture(t)
	prepareHAMetadataMaintenanceArtifacts(t, f)
	completeHAMemoryActivationBoundary(t, f)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var result []*haSelectedRuntimeFoundation
	for _, cfg := range f.configs {
		guard, err := lock.Acquire(cfg.Storage.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		manager := functionalHAMetadataManager(t, cfg)
		a, err := ActivateHAMemoryRuntime(ctx, cfg, f.keys, guard, manager, logger)
		if err != nil {
			t.Fatal("activation", err)
		}
		if err := a.Close(); err != nil {
			t.Fatal(err)
		}
		owner, err := openHASelectedRuntimeFoundation(ctx, cfg, f.keys.Master, guard, manager, logger)
		if err != nil {
			t.Fatal("cold original member", err)
		}
		t.Cleanup(func() {
			if err := owner.Close(); err != nil {
				t.Error(err)
			}
			if err := guard.Close(); err != nil {
				t.Error(err)
			}
		})
		if err := owner.prepareSelectedRoots(ctx); err != nil {
			t.Fatal("seven authentic roots", err)
		}
		result = append(result, owner)
	}
	return f, result
}

func TestHAGroupRotationActualColdSourceDualKeyIntentAndCompleteGroup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f, members := prepareHAGroupRotationColdMembers(t, ctx)
	var before [2][]byte
	for n, cfg := range f.configs {
		var err error
		before[n], err = os.ReadFile(cfg.ReplicationStatePath())
		if err != nil {
			t.Fatal(err)
		}
	}
	next := bytes.Repeat([]byte{82}, 32)
	i, err := captureHAGroupRotationIntent(ctx, members, f.keys.Master, next, "group-dek-001")
	if err != nil {
		t.Fatal("actual full group capture", err)
	}
	defer i.clearKeys()
	raw, err := encodeHAGroupRotationIntent(i)
	if err != nil {
		t.Fatal("bounded dual key intent", err)
	}
	defer clear(raw)
	if len(raw) > haGroupRotationIntentLimit || len(raw) == 0 {
		t.Fatal("unbounded intent", len(raw))
	}
	for _, key := range [][]byte{f.keys.Master, next} {
		got, err := decodeHAGroupRotationIntent(raw, key)
		if err != nil {
			t.Fatal("dual key recovery", err)
		}
		if got.ClusterKey != i.ClusterKey || got.Members[0].Source.Selected != i.Members[0].Source.Selected || got.Members[1].Source.Observed != i.Members[1].Source.Observed || got.Members[0].Source.Member.Role == got.Members[1].Source.Member.Role {
			t.Fatal("captured source identity changed")
		}
		got.clearKeys()
	}
	if wrong, err := decodeHAGroupRotationIntent(raw, bytes.Repeat([]byte{83}, 32)); err == nil || wrong != nil {
		t.Fatal("wrong key acquired group intent")
	}
	for n, cfg := range f.configs {
		after, err := os.ReadFile(cfg.ReplicationStatePath())
		if err != nil || !bytes.Equal(before[n], after) {
			t.Fatal("source capture changed actual Member", n, err)
		}
	}
	if missing, err := captureHAGroupRotationIntent(ctx, members[:1], f.keys.Master, next, "group-dek-002"); err == nil || missing != nil {
		t.Fatal("single member passed group gate")
	}
	t.Run("bootstrap-key", func(t *testing.T) {
		saved := i.Members[1].Bootstrap.LedgerKey
		i.Members[1].Bootstrap.LedgerKey = nil
		defer func() { i.Members[1].Bootstrap.LedgerKey = saved }()
		if _, err := encodeHAGroupRotationIntent(i); !errors.Is(err, authenticatedstate.ErrSealIneligible) {
			t.Fatal(err)
		}
	})
	t.Run("foreign-role", func(t *testing.T) {
		saved := i.Members[1].Source.Selected.Role
		i.Members[1].Source.Selected.Role = authenticatedstate.SealPrimary
		defer func() { i.Members[1].Source.Selected.Role = saved }()
		if _, err := encodeHAGroupRotationIntent(i); !errors.Is(err, authenticatedstate.ErrSealIneligible) {
			t.Fatal(err)
		}
	})
	t.Run("different-native", func(t *testing.T) {
		saved := i.Members[1].Source.Heads[3]
		i.Members[1].Source.Heads[3][0] ^= 1
		defer func() { i.Members[1].Source.Heads[3] = saved }()
		if _, err := encodeHAGroupRotationIntent(i); err == nil {
			t.Fatal("foreign native head accepted")
		}
	})
}
