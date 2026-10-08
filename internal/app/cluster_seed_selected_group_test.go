package app

import (
	"bytes"
	"context"
	"errors"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/store/lock"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSelectedSeedAfterActualGroupDEKRotationRebindsOnlyStableKeyCarrier(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	fixture, members := prepareHAGroupRotationColdMembers(t, ctx)
	for _, member := range members {
		if err := errors.Join(member.Close(), member.owners.Components.Guard.Close()); err != nil {
			t.Fatal(err)
		}
	}
	next := bytes.Repeat([]byte{115}, 32)
	newKey := filepath.Join(t.TempDir(), "new.key")
	if err := os.WriteFile(newKey, next, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := rotateHAGroupMasterKeyWithPressureFactory(ctx, fixture.configs[:], newKey, "seed-group-current", nil, functionalHAGroupPressure); err != nil {
		t.Fatal("real whole-group rotation", err)
	}
	next = bytes.Repeat([]byte{116}, 32)
	newKey = filepath.Join(t.TempDir(), "new-next.key")
	if err := os.WriteFile(newKey, next, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := rotateHAGroupMasterKeyWithPressureFactory(ctx, fixture.configs[:], newKey, "seed-group-next", nil, functionalHAGroupPressure); err != nil {
		t.Fatal("real repeated group rotation", err)
	}
	source := fixture.configs[0]
	history, err := os.ReadFile(haGroupHistoryPath(source))
	if err != nil {
		t.Fatal("actual completed group key history", err)
	}
	archiveSuffix, err := selectedSeedArchiveSuffix(source)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(t.TempDir(), "seed.json")
	manifest, err := CreateSeedManifest(selectedSeedCommandTestContext(t, ctx, source), source, "halro-1", manifestPath, "admin", []byte("correct horse battery staple"), "")
	if err != nil {
		t.Fatal("actual new-DEK source approval", err)
	}
	target := fixture.configs[1]
	target.Storage.DataDir = filepath.Join(t.TempDir(), "replica")
	stage := target.Storage.DataDir + ".stage"
	copyTestTree(t, source.Storage.DataDir, stage)
	if _, err = InstallSeedSnapshot(selectedSeedCommandTestContext(t, ctx, target), target, stage, manifestPath); err != nil {
		t.Fatal("actual new-DEK cold seed", err)
	}
	archivedHistory, err := os.ReadFile(filepath.Join(haMemoryStateDirectory(target)+archiveSuffix, "seed-group-controls", "ha-group-key-history"))
	if err != nil || !bytes.Equal(archivedHistory, history) {
		t.Fatal("source history key-only bytes changed", err)
	}
	if _, err := os.Lstat(haGroupHistoryPath(target)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("source key history became target operation permission", err)
	}
	member, key, err := readMemberState(target, next)
	if err != nil || member.Role != replication.RoleReplica || member.NodeID != target.Replication.NodeID || member.AppliedIndex != manifest.Index {
		t.Fatal("target stable carrier/Member", member, err)
	}
	defer clear(key[:])
	if _, _, err = readMemberState(source, next); err != nil {
		t.Fatal(err)
	}
	if _, _, err = readMemberState(target, fixture.keys.Master); err == nil {
		t.Fatal("old DEK became target key permission")
	}
	if err = checkHAGroupKeyRotationPending(ctx, target, next, functionalHAMetadataManager(t, target)); err != nil {
		t.Fatal("source operation was mistaken for target authorization", err)
	}
	guard, err := lock.Acquire(target.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	f, err := openHASelectedRuntimeFoundation(ctx, target, next, guard, functionalHAMetadataManager(t, target), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal("true target seven-root selection", err)
	}
	if err = f.prepareSelectedServices(ctx); err != nil {
		t.Fatal("new-DEK issued business facade", err)
	}
	if _, err = f.metadataStore.GetAdminUser(ctx, "admin"); err != nil {
		t.Fatal(err)
	}
	if f.bootstrap.Binding != members[0].bootstrap.Binding || f.bootstrap.Evidence != members[0].bootstrap.Evidence || f.reopened.Seal.RootCount != 7 {
		t.Fatal("DEK carrier invented native generation/instance")
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSelectedSeedFixedArchiveSlotsKeepForeignAndRefuseExhaustion(t *testing.T) {
	cfg := testConfig(t)
	cfg.Storage.DataDir = t.TempDir()
	for n, suffix := range selectedSeedArchiveSuffixes {
		actual, err := selectedSeedArchiveSuffix(cfg)
		if err != nil || actual != suffix {
			t.Fatal(n, actual, err)
		}
		path := haMemoryStateDirectory(cfg) + suffix
		if err = os.WriteFile(path, []byte("foreign opaque bytes"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := selectedSeedArchiveSuffix(cfg); !errors.Is(err, authenticatedstate.ErrLimit) {
		t.Fatal("full fixed slots were reused", err)
	}
	for _, suffix := range selectedSeedArchiveSuffixes {
		raw, err := os.ReadFile(haMemoryStateDirectory(cfg) + suffix)
		if err != nil || string(raw) != "foreign opaque bytes" {
			t.Fatal("opaque archive changed", err)
		}
	}
}
