package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ownedfile"
	bbolt "go.etcd.io/bbolt"
)

func TestPrimaryGovernanceDefaultRebuildAndConfirmedReport(t *testing.T) {
	ctx := context.Background()
	r, bootstrap, _ := openBootstrappedRuntime(t)
	t.Cleanup(func() { r.Close() })
	project, err := r.store.GetProject(ctx, bootstrap.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	definition := domain.OutcomeDefinition{ID: "odef_primarydisk", ProjectID: project.ID, Name: "disk", Version: 1,
		DataType: domain.OutcomeCategorical, AllowedValues: []string{"accepted", "rejected"}, SuccessValues: []string{"accepted"}, Enabled: true, CreatedAt: now, CreatedBy: "admin"}
	if _, err := r.store.PutOutcomeDefinition(ctx, definition, project.Revision, 0, nil); err != nil {
		t.Fatal(err)
	}
	unit, _, err := r.accounting.CreateWorkUnitWithDefinitions(ctx, project.ID, bootstrap.KeyID, 10,
		[]domain.OutcomeDefinitionRef{{ID: definition.ID, Version: 1}}, budget.GovernanceIntent{Operation: "create_work_unit", IdempotencyKeyHash: outcomePageTestLabel("primary-wu"), RequestFingerprint: outcomePageTestLabel("primary-wu-fp")})
	if err != nil {
		t.Fatal(err)
	}
	input := governance.ReportInput{ProjectID: project.ID, WorkUnitID: unit.ID, DefinitionID: definition.ID, ReporterKeyID: bootstrap.KeyID, Value: "accepted", ObservedAt: now,
		Intent: governance.Intent{IdempotencyKeyHash: outcomePageTestLabel("primary-first"), RequestFingerprint: outcomePageTestLabel("primary-first-fp")}}
	first, _, err := r.governance.manager.Report(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	secondInput := input
	secondInput.Value, secondInput.SupersedesOutcomeID = "rejected", first.ID
	secondInput.Intent = governance.Intent{IdempotencyKeyHash: outcomePageTestLabel("primary-second"), RequestFingerprint: outcomePageTestLabel("primary-second-fp")}
	second, _, err := r.governance.manager.Report(ctx, secondInput)
	if err != nil {
		t.Fatal(err)
	}
	seedHead := r.governance.log.Summary()
	payload := []byte("invalid old checkpoint must not be decoded")
	if err := r.store.SaveGovernanceCheckpoint(seedHead.Records, seedHead.Bytes, seedHead.LastHash, sha256.Sum256(payload), [32]byte{1}, payload); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(standaloneGovernanceProjectionPath(r.config), []byte("untrusted crash derivative"), 0o600); err != nil {
		t.Fatal(err)
	}
	primary, replica := openPrimaryDiskPair(t, r.config)
	if primary.governance.owner == nil || primary.governance.manager.State() != nil || primary.governance.log.Summary() != seedHead {
		t.Fatal("default Primary retained State or changed native Governance")
	}
	replayed, duplicate, err := primary.governance.manager.Report(ctx, input)
	if err != nil || !duplicate || !reflect.DeepEqual(replayed, first) || primary.governance.log.Summary() != seedHead {
		t.Fatal("Primary rebuild lost durable idempotency", replayed, duplicate, err)
	}
	current, found, err := primary.governance.owner.projection.ReadCurrent(ctx, project.ID, unit.ID, definition.ID, 1)
	if err != nil || !found || current.ID != second.ID || current.Revision != 2 {
		t.Fatal("Primary rebuild lost current revision", current, found, err)
	}
	thirdInput := input
	thirdInput.SupersedesOutcomeID = second.ID
	thirdInput.Intent = governance.Intent{IdempotencyKeyHash: outcomePageTestLabel("primary-third"), RequestFingerprint: outcomePageTestLabel("primary-third-fp")}
	writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	third, duplicate, err := primary.governance.manager.Report(writeCtx, thirdInput)
	if err != nil || duplicate || third.Revision != 3 {
		t.Fatal("actual Primary confirmed Report failed", third, duplicate, err)
	}
	finalHead := primary.governance.log.Summary()
	projectedHead, err := primary.governance.owner.projection.ReadHead(ctx)
	if err != nil || projectedHead != finalHead || finalHead.Records != seedHead.Records+1 {
		t.Fatal("Report did not use selected physical owner", projectedHead, finalHead, err)
	}
	// The Metadata anchor is node-derived, so its write does not wait for HA
	// confirmation. Select the durable prefix containing this Governance fact,
	// confirm that exact target, then wait for Replica application.
	target := primary.replication.publisher.Snapshot().DurableIndex
	if err := primary.replication.coordinator.WaitConfirmed(writeCtx, target); err != nil {
		t.Fatal("Governance durable prefix was not confirmed", target, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for replica.replication.publisher.Snapshot().AppliedIndex < target {
		if time.Now().After(deadline) {
			t.Fatal("Replica did not apply confirmed Governance prefix")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := replica.governance.log.Summary(); got != finalHead {
		t.Fatal("confirmed Report absent from Replica native authority", got, finalHead)
	}
	if got, replay, err := primary.governance.manager.Report(writeCtx, thirdInput); err != nil || !replay || !reflect.DeepEqual(got, third) || primary.governance.log.Summary() != finalHead {
		t.Fatal("live Primary idempotency appended another fact", got, replay, err)
	}
	oldProjection := primary.governance.owner.projection
	if err := primary.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := oldProjection.ReadHead(ctx); err == nil {
		t.Fatal("closed Primary Governance projection remained usable")
	}
	next, err := Open(ctx, primary.config, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	current, found, err = next.governance.owner.projection.ReadCurrent(ctx, project.ID, unit.ID, definition.ID, 1)
	if err != nil || !found || current.ID != third.ID || next.governance.manager.State() != nil || next.replication.startupReady.Load() {
		t.Fatal("reopen lost confirmed history or published ready without peer", current, found, err)
	}
}

func TestPrimaryGovernanceFailedOpenClosesMetadataAndPreservesReplacement(t *testing.T) {
	r, _, _ := openBootstrappedRuntime(t)
	t.Cleanup(func() { r.Close() })
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	primary, _ := openPrimaryDiskPair(t, r.config)
	if err := primary.Close(); err != nil {
		t.Fatal(err)
	}
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := standaloneGovernanceProjectionPath(primary.config)
	replaced := false
	ctx := generationProbeContext{Context: base, probe: func() {
		if replaced {
			return
		}
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			return
		} else if err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(path, path+".owned"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("foreign replacement"), 0o600); err != nil {
			t.Fatal(err)
		}
		replaced = true
		cancel()
	}}
	failed, err := Open(ctx, primary.config, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if failed != nil || !replaced || !errors.Is(err, context.Canceled) || !errors.Is(err, ownedfile.ErrReplaced) {
		if failed != nil {
			failed.Close()
		}
		t.Fatal("failed Primary construction lost physical ownership error", failed, err)
	}
	if raw, err := os.ReadFile(path); err != nil || string(raw) != "foreign replacement" {
		t.Fatal("failed Open removed replacement", err)
	}
	// Bound the observation of the native Metadata lock: failed Open must
	// physically release it, not just release the directory lock.
	db, err := bbolt.Open(primary.config.MetadataPath(), 0o600, &bbolt.Options{ReadOnly: true, Timeout: time.Second})
	if err != nil {
		t.Fatal("failed Primary Governance leaked Metadata owner", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := Open(context.Background(), primary.config, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal("failed startup leaked native or directory owners", err)
	}
	if err := next.Close(); err != nil {
		t.Fatal(err)
	}
}
