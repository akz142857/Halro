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

	"github.com/akz142857/Halro/internal/replication"
)

func TestHAGroupFileRotationActualOriginalTLSSevenRootsAndNewKeyReopen(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	f, members := prepareHAGroupRotationColdMembers(t, ctx)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	next := bytes.Repeat([]byte{94}, 32)
	r, err := prepareHAGroupFileRotation(ctx, members, f.keys.Master, next, "group-file-001")
	if err != nil {
		t.Fatal("whole group prepare", err)
	}
	for n, cfg := range f.configs {
		if err := checkHAGroupKeyRotationPending(ctx, cfg, f.keys.Master, members[n].owners.Components.Budget); !errors.Is(err, ErrHAGroupKeyRotationPending) {
			t.Fatal("pending intent admitted Runtime", n, err)
		}
	}
	if err := r.execute(ctx, logger); err != nil {
		t.Fatal("actual complete group File chain", err)
	}
	var verified []*haSelectedRuntimeFoundation
	for n, cfg := range f.configs {
		actual, key, err := readMemberState(cfg, next)
		if err != nil || key != r.intent.ClusterKey || actual.Role != ([]replication.Role{replication.RolePrimary, replication.RoleReplica})[n] || actual.Term != r.intent.Members[n].Source.Member.Term || actual.DurableIndex <= r.intent.Members[n].Source.Member.DurableIndex || actual.AppliedIndex != actual.DurableIndex || actual.ConfirmedIndex != actual.DurableIndex {
			t.Fatal("new DEK changed independent native role/key/cut", n, actual, err)
		}
		clear(key[:])
		if _, key, err := readMemberState(cfg, f.keys.Master); err == nil {
			clear(key[:])
			t.Fatal("old DEK reopened new cluster-key carrier", n)
		}
		loaded, err := os.ReadFile(cfg.Storage.MasterKey.File)
		if err != nil || !bytes.Equal(loaded, next) {
			t.Fatal("actual current File DEK", n, err)
		}
		clear(loaded)
		proofCtx := context.WithValue(ctx, haGroupKeyVerificationKey{}, &haGroupKeyVerification{rotation: r, member: n})
		fresh, err := openHASelectedRuntimeFoundation(proofCtx, cfg, next, members[n].owners.Components.Guard, functionalHAMetadataManager(t, cfg), logger)
		if err != nil {
			t.Fatal("actual independent new-key seven-root reopen", n, err)
		}
		if err := fresh.prepareSelectedRoots(proofCtx); err != nil {
			t.Fatal("new-key original roots factory", n, err)
		}
		fresh.metadataStore, err = fresh.reopened.OpenHAMetadataStore(proofCtx)
		if err != nil {
			t.Fatal("new-key original Metadata facade", n, err)
		}
		if err := verifyHAGroupNewMaterial(proofCtx, fresh, r.intent, n); err != nil {
			t.Fatal("actual new-key Vault/native material", n, err)
		}
		if fresh.reopened.Seal.RootCount != 7 || fresh.reopened.Seal.OrderingIndex != actual.AppliedIndex || fresh.bootstrap.Binding != r.intent.Members[n].Bootstrap.Binding {
			t.Fatal("new DEK substituted common/baseline source")
		}
		verified = append(verified, fresh)
		t.Cleanup(func() {
			if err := fresh.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	if err := r.completeVerified(ctx, verified); err != nil {
		t.Fatal("complete actual verified whole group", err)
	}
	for n, fresh := range verified {
		if err := fresh.Close(); err != nil {
			t.Fatal("new-key actual owner join", n, err)
		}
	}
	for n, cfg := range f.configs {
		if err := checkHAGroupKeyRotationPending(ctx, cfg, next, members[n].owners.Components.Budget); err != nil {
			t.Fatal("completed group gate", n, err)
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
