package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/store/lock"
)

func TestSelectedSeedAfterActualKMSGroupRotationRebindsTargetUnlockAndSevenRoots(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	fixture, members, h := prepareHAGroupKMSColdMembers(t, ctx)
	originalBinding, originalEvidence := members[0].bootstrap.Binding, members[0].bootstrap.Evidence
	for _, member := range members {
		if err := errors.Join(member.Close(), member.owners.Components.Guard.Close()); err != nil {
			t.Fatal(err)
		}
	}
	next := bytes.Repeat([]byte{119}, 32)
	options := kmsRotationOptions{operationID: "seed-kms-group-current", factory: h.factory, random: bytes.NewReader(next), now: time.Now}
	if _, err := rotateHAGroupKeyWithOptions(ctx, fixture.configs[:], "", options.operationID, nil, functionalHAGroupPressure, &options); err != nil {
		t.Fatal("actual KMS group rotation", err)
	}
	source := fixture.configs[0]
	manifestPath := filepath.Join(t.TempDir(), "seed.json")
	manifest, err := CreateSeedManifest(selectedSeedCommandTestContext(t, ctx, source), source, "halro-1", manifestPath, "admin", []byte("correct horse battery staple"), "")
	if err != nil {
		t.Fatal("actual KMS-selected source approval", err)
	}
	target := fixture.configs[1]
	target.Storage.DataDir = filepath.Join(t.TempDir(), "replica")
	stage := target.Storage.DataDir + ".stage"
	copyTestTree(t, source.Storage.DataDir, stage)
	if _, err = InstallSeedSnapshot(selectedSeedCommandTestContext(t, ctx, target), target, stage, manifestPath); err != nil {
		t.Fatal("actual KMS target install", err)
	}
	manager := functionalHAMetadataManager(t, target)
	actual, p, err := unlockHAMemoryCandidate(ctx, target, manager, h.factory)
	if err != nil {
		t.Fatal("target node independent unlock", err)
	}
	defer clear(actual)
	if !bytes.Equal(actual, next) || p.NodeID != target.Replication.NodeID {
		t.Fatal("source node or old candidate remained target unlock permission")
	}
	member, key, err := readMemberState(target, actual)
	defer clear(key[:])
	if err != nil || member.Role != replication.RoleReplica || member.AppliedIndex != manifest.Index {
		t.Fatal("target actual Member", member, err)
	}
	if _, oldKey, e := readMemberState(target, fixture.keys.Master); e == nil {
		clear(oldKey[:])
		t.Fatal("old DEK qualified target")
	}
	guard, err := lock.Acquire(target.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	f, err := openHASelectedRuntimeFoundation(ctx, target, actual, guard, manager, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal("target true fresh factory", err)
	}
	if err = f.prepareSelectedServices(ctx); err != nil {
		t.Fatal("target actual business consumer", err)
	}
	if err = p.validateSelected(ctx, f); err != nil {
		t.Fatal("target unlock lacked actual selected descriptor authority", err)
	}
	if f.bootstrap.Binding != originalBinding || f.bootstrap.Evidence != originalEvidence || f.reopened.Seal.RootCount != 7 || f.reopened.Seal.Role != authenticatedstate.SealReplica {
		t.Fatal("KMS rebinding altered original native identity or target roots")
	}
	if _, err = f.metadataStore.GetAdminUser(ctx, "admin"); err != nil {
		t.Fatal(err)
	}
	if err = errors.Join(p.Close(), f.Close()); err != nil {
		t.Fatal("target actual joins", err)
	}
}
