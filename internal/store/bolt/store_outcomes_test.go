package bolt

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	bbolt "go.etcd.io/bbolt"
)

func TestOutcomeDefinitionActiveLimitAlsoAppliesWhenReenabling(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	project, err := store.PutProject(ctx, domain.Project{ID: "prj_outcomes", Name: "outcomes", Enabled: true, CreatedAt: now, UpdatedAt: now}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	put := func(id, name string, enabled bool) domain.OutcomeDefinition {
		t.Helper()
		definition := domain.OutcomeDefinition{ID: id, ProjectID: project.ID, Name: name, Version: 1, DataType: domain.OutcomeCategorical,
			AllowedValues: []string{"accepted", "rejected"}, SuccessValues: []string{"accepted"}, Enabled: enabled, CreatedAt: now, CreatedBy: "admin"}
		stored, err := store.PutOutcomeDefinition(ctx, definition, project.Revision, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		return stored
	}
	disabled := put("odef_disabled", "disabled", false)
	for index := 0; index < domain.MaxActiveOutcomeDefinitions; index++ {
		put(fmt.Sprintf("odef_active%02d", index), fmt.Sprintf("active_%02d", index), true)
	}
	disabled.Version++
	disabled.Enabled = true
	disabled.CreatedAt = now.Add(time.Second)
	_, err = store.PutOutcomeDefinition(ctx, disabled, project.Revision, disabled.Revision, nil)
	if err == nil || !strings.Contains(err.Error(), "active outcome definition limit exceeded") {
		t.Fatalf("reenable over active limit error=%v", err)
	}
}

func TestGovernanceCheckpointSegmentsRoundTripAndReset(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	payload := bytes.Repeat([]byte("governance-checkpoint"), (governanceCheckpointSegmentSize/20)+2)
	payloadHash := sha256.Sum256(payload)
	journalHash, authentication := sha256.Sum256([]byte("journal")), sha256.Sum256([]byte("authentication"))
	if err := store.SaveGovernanceCheckpoint(7, 900, journalHash, payloadHash, authentication, payload); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadGovernanceCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Sequence != 7 || loaded.Offset != 900 || !bytes.Equal(loaded.Payload, payload) || len(loaded.Segments) < 2 {
		t.Fatalf("checkpoint=%#v payload=%d", loaded, len(loaded.Payload))
	}
	if err := store.ResetGovernanceCheckpoint(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadGovernanceCheckpoint(); err == nil {
		t.Fatal("checkpoint survived reset")
	}
}

func TestGovernanceCheckpointReclaimsUnreferencedSegments(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	journalHash, authentication := sha256.Sum256([]byte("journal")), sha256.Sum256([]byte("authentication"))
	first := bytes.Repeat([]byte("a"), governanceCheckpointSegmentSize+17)
	if err := store.SaveGovernanceCheckpoint(1, 100, journalHash, sha256.Sum256(first), authentication, first); err != nil {
		t.Fatal(err)
	}
	second := bytes.Repeat([]byte("b"), governanceCheckpointSegmentSize+17)
	if err := store.SaveGovernanceCheckpoint(2, 200, journalHash, sha256.Sum256(second), authentication, second); err != nil {
		t.Fatal(err)
	}
	err = store.db.View(func(tx *bbolt.Tx) error {
		count := 0
		if err := tx.Bucket(bucketGovernanceCheckpointSegments).ForEach(func(_, _ []byte) error { count++; return nil }); err != nil {
			return err
		}
		if count != 2 {
			t.Fatalf("checkpoint retained %d segments, want only the 2 current segments", count)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
