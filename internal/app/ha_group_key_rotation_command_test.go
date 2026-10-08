package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func TestHAGroupRotationPublicFileCommandActualMonitorsAndDefaultRuntime(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	f, members := prepareHAGroupRotationColdMembers(t, ctx)
	for n, m := range members {
		if err := m.Close(); err != nil {
			t.Fatal(n, err)
		}
		if err := m.owners.Components.Guard.Close(); err != nil {
			t.Fatal(n, err)
		}
	}
	next := bytes.Repeat([]byte{95}, 32)
	target := filepath.Join(t.TempDir(), "new.key")
	if err := os.WriteFile(target, next, 0600); err != nil {
		t.Fatal(err)
	}
	result, err := rotateHAGroupMasterKeyWithPressureFactory(ctx, f.configs[:], target, "public-group-file-001", nil, functionalHAGroupPressure)
	if err != nil {
		t.Fatal("public original-monitor File transaction", err)
	}
	if result.Members != 2 || result.RecoveredPending || result.NewKeyVersion != result.OldKeyVersion+1 || result.NewFingerprint != keyFingerprint(next) {
		t.Fatal(result)
	}
	for n, cfg := range f.configs {
		actual, key, err := readMemberState(cfg, next)
		if err != nil || actual.Role != ([]replication.Role{replication.RolePrimary, replication.RoleReplica})[n] || actual.DurableIndex != actual.AppliedIndex || actual.ConfirmedIndex != actual.AppliedIndex {
			t.Fatal(n, actual, err)
		}
		clear(key[:])
		if _, key, err := readMemberState(cfg, f.keys.Master); err == nil {
			clear(key[:])
			t.Fatal("old File key admitted", n)
		}
		if err := os.Rename(cfg.MetadataPath(), cfg.MetadataPath()+".preserved-group"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(cfg.MetadataPath(), 0700); err != nil {
			t.Fatal(err)
		}
	}
	f.keys.Master = bytes.Clone(next)
	proveDefaultHASelectedRuntime(t, ctx, f, [2]*haSelectedRuntimeFoundation{members[0], members[1]})
}

func TestHAGroupRotationPublicRefusesPartialAndWrongOperationBeforeWrites(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f, members := prepareHAGroupRotationColdMembers(t, ctx)
	next := bytes.Repeat([]byte{96}, 32)
	r, err := prepareHAGroupFileRotation(ctx, members, f.keys.Master, next, "pending-public-group")
	if err != nil {
		t.Fatal(err)
	}
	for n, m := range members {
		if err := m.Close(); err != nil {
			t.Fatal(n, err)
		}
		if err := m.owners.Components.Guard.Close(); err != nil {
			t.Fatal(n, err)
		}
	}
	target := filepath.Join(t.TempDir(), "new.key")
	if err := os.WriteFile(target, next, 0600); err != nil {
		t.Fatal(err)
	}
	var before [2][]byte
	for n, cfg := range f.configs {
		before[n], err = os.ReadFile(haGroupRotationIntentPath(cfg.Storage.DataDir, r.slot))
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rotateHAGroupMasterKeyWithPressureFactory(ctx, f.configs[:1], target, "pending-public-group", nil, functionalHAGroupPressure); err == nil {
		t.Fatal("partial group admitted")
	}
	if _, err := rotateHAGroupMasterKeyWithPressureFactory(ctx, f.configs[:], target, "foreign-operation", nil, functionalHAGroupPressure); !errors.Is(err, ErrHAGroupKeyRotationPending) {
		t.Fatal("foreign operation admitted", err)
	}
	for n, cfg := range f.configs {
		actual, err := os.ReadFile(haGroupRotationIntentPath(cfg.Storage.DataDir, r.slot))
		if err != nil || !bytes.Equal(actual, before[n]) {
			t.Fatal("foreign operation mutated actual intent", n, err)
		}
	}
}

// Actual finite sampling/controller/lifecycle with functional values. This
// verifies the protocol, not Linux filesystem/cgroup capacity or pressure.
func functionalHAGroupPressure(cfg config.Config) (*resourcePressureRuntime, error) {
	return newResourcePressureWithSource(cfg, pressureSampleFunc(func(context.Context) resourcebudget.Sample { return runtimeLowSample() }))
}
