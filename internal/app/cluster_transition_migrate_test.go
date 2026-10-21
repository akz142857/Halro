package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/store/lock"
)

func TestOfflineTransitionMigrationRequiresInspectionAuthenticationAndLock(t *testing.T) {
	cfg := testConfig(t)
	password := []byte("correct horse battery staple")
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapAdmin(context.Background(), cfg, "admin", password); err != nil {
		t.Fatal(err)
	}
	standalone, err := OpenWithOptions(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := standalone.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.Replication = &config.Replication{ClusterID: "production-a", NodeID: "halro-0", Listen: "127.0.0.1:9910",
		Peers: []config.ReplicationPeer{{Name: "halro-1", Address: "127.0.0.1:9911", SPKISHA256: "sha256:" + strings.Repeat("ab", 32)}}}
	if err := EstablishMemberState(context.Background(), cfg, replication.RolePrimary, "inc_01", 1); err != nil {
		t.Fatal(err)
	}
	options := TransitionMigrationOptions{ExpectedIncarnation: "inc_01", ExpectedRole: replication.RolePrimary,
		ExpectedTerm: 1, ExpectedPromised: 1, ExpectedApplied: 0, Username: "admin", Password: password}
	journalPath := replication.TransitionJournalPath(cfg.ReplicationStatePath())
	wrong := options
	wrong.ExpectedTerm = 2
	if _, err := MigrateMemberTransitionJournal(context.Background(), cfg, wrong); err == nil || !strings.Contains(err.Error(), "inspected") {
		t.Fatalf("stale inspection accepted: %v", err)
	}
	wrong = options
	wrong.ExpectedConfirmed = 1
	if _, err := MigrateMemberTransitionJournal(context.Background(), cfg, wrong); err == nil || !strings.Contains(err.Error(), "index changed") {
		t.Fatalf("changed confirmed index accepted: %v", err)
	}
	wrong = options
	wrong.Password = []byte("wrong password")
	if _, err := MigrateMemberTransitionJournal(context.Background(), cfg, wrong); err == nil || !strings.Contains(err.Error(), "reauthentication") {
		t.Fatalf("wrong administrator password accepted: %v", err)
	}
	if _, err := os.Lstat(journalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused migration touched journal: %v", err)
	}
	held, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateMemberTransitionJournal(context.Background(), cfg, options); err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("running member was migrated: %v", err)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	migrated, err := MigrateMemberTransitionJournal(context.Background(), cfg, options)
	if err != nil {
		t.Fatal(err)
	}
	if migrated.Version != replication.TransitionStateVersion || migrated.Transition.JournalID == "" || migrated.Transition.Sequence != 0 {
		t.Fatalf("migration result=%+v", migrated)
	}
	again, err := MigrateMemberTransitionJournal(context.Background(), cfg, options)
	if err != nil || again.Transition != migrated.Transition {
		t.Fatalf("idempotent retry=%+v err=%v", again, err)
	}
	page, err := func() (replication.DurableTransitionPage, error) {
		masterKey, err := unlockMemberMasterKey(context.Background(), cfg)
		if err != nil {
			return replication.DurableTransitionPage{}, err
		}
		defer clear(masterKey)
		clusterKey, err := replication.DeriveClusterKey(masterKey, migrated.Incarnation)
		if err != nil {
			return replication.DurableTransitionPage{}, err
		}
		defer clear(clusterKey[:])
		journal, err := replication.OpenTransitionJournal(journalPath, clusterKey[:], migrated)
		if err != nil {
			return replication.DurableTransitionPage{}, err
		}
		defer journal.Close()
		return journal.Page(0, "", 64)
	}()
	if err != nil || page.BaselineKind != "legacy_baseline" || page.CommittedSequence != 0 || page.HasMore {
		t.Fatalf("migrated baseline page=%+v err=%v", page, err)
	}
}
