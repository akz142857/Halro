package governance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
)

func shaLabel(value string) string {
	digest := sha256.Sum256([]byte(value))
	return "sha256:" + fmt.Sprintf("%x", digest)
}

type definitionReader struct{ definition domain.OutcomeDefinition }

func (r definitionReader) GetOutcomeDefinition(context.Context, string, string, uint64) (domain.OutcomeDefinition, error) {
	return r.definition, nil
}

type checkpointRecorder struct {
	mu       sync.Mutex
	sequence uint64
	payload  []byte
}

func (r *checkpointRecorder) SaveGovernanceCheckpoint(sequence uint64, _ int64, payload []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sequence = sequence
	r.payload = append([]byte(nil), payload...)
	return nil
}

func testManager(t *testing.T) (*Manager, *Log, *ledger.State, domain.OutcomeDefinition) {
	t.Helper()
	now := time.Date(2026, 9, 4, 8, 0, 0, 0, time.UTC)
	definition := domain.OutcomeDefinition{ID: "odef_result", ProjectID: "prj_1", Name: "resolved", Version: 1, DataType: domain.OutcomeCategorical,
		AllowedValues: []string{"accepted", "rejected"}, SuccessValues: []string{"accepted"}, Enabled: true, CreatedAt: now, CreatedBy: "admin", Revision: 1}
	state := ledger.NewState()
	event := ledger.Event{EventID: "evt_wu", Kind: ledger.EventWorkUnitCreated, WorkUnitID: "wku_unit", ProjectID: "prj_1", KeyID: "key_1", PeriodID: "2026-09-04", PeriodTimezoneVersion: 1,
		OccurredAt: now, Operation: "work_unit.create", IdempotencyKeyHash: shaLabel("wu"), RequestFingerprint: shaLabel("wu-fp"), OutcomeDefinitions: []domain.OutcomeDefinitionRef{{ID: definition.ID, Version: 1}}}
	if err := state.Apply(ledger.Record{Generation: 1, Sequence: 1, Offset: 1, Event: event}); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	for index := range key {
		key[index] = byte(index + 1)
	}
	log, err := Open(filepath.Join(t.TempDir(), "governance.journal"), key)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := &checkpointRecorder{}
	manager := NewManager(log, NewState(), state, definitionReader{definition}, checkpoint)
	manager.now = func() time.Time { return now.Add(time.Hour) }
	return manager, log, state, definition
}

func reportInput(value, supersedes, idem string) ReportInput {
	return ReportInput{ProjectID: "prj_1", WorkUnitID: "wku_unit", DefinitionID: "odef_result", Value: value, ReporterKeyID: "key_1",
		ObservedAt: time.Date(2026, 9, 4, 7, 0, 0, 0, time.UTC), SupersedesOutcomeID: supersedes,
		Intent: Intent{IdempotencyKeyHash: shaLabel(idem), RequestFingerprint: shaLabel(value + "|" + supersedes)}}
}

func TestManagerSerializesCurrentHeadAndPersistsIdempotency(t *testing.T) {
	manager, log, _, _ := testManager(t)
	defer log.Close()
	first, replay, err := manager.Report(context.Background(), reportInput("accepted", "", "first"))
	if err != nil || replay {
		t.Fatalf("first report = %#v replay=%v err=%v", first, replay, err)
	}
	replayed, replay, err := manager.Report(context.Background(), reportInput("accepted", "", "first"))
	if err != nil || !replay || replayed.ID != first.ID {
		t.Fatalf("idempotent replay = %#v replay=%v err=%v", replayed, replay, err)
	}
	restored := NewState()
	if err := restored.Restore(manager.State().Snapshot()); err != nil {
		t.Fatal(err)
	}
	if idem, ok := restored.Idempotency("prj_1", shaLabel("first")); !ok || idem.OutcomeID != first.ID {
		t.Fatalf("checkpoint lost idempotency: %#v ok=%v", idem, ok)
	}
	var successes, conflicts int
	var mu sync.Mutex
	var wait sync.WaitGroup
	for index, value := range []string{"accepted", "rejected"} {
		wait.Add(1)
		go func(index int, value string) {
			defer wait.Done()
			_, _, err := manager.Report(context.Background(), reportInput(value, first.ID, fmt.Sprintf("revision-%d", index)))
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				successes++
			} else if errors.Is(err, ErrRevisionConflict) {
				conflicts++
			} else {
				t.Errorf("unexpected revision error: %v", err)
			}
		}(index, value)
	}
	wait.Wait()
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes=%d conflicts=%d", successes, conflicts)
	}
	if log.Summary().Records != 2 {
		t.Fatalf("journal records=%d, want 2", log.Summary().Records)
	}
}

func TestJournalRejectsTamperingAndWrongKey(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "governance.journal")
	key := make([]byte, 32)
	key[0] = 1
	log, err := Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	event := Event{EventID: "gov_1", ProjectID: "prj_1", WorkUnitID: "wku_unit", DefinitionID: "odef_result", DefinitionVersion: 1, OutcomeID: "out_result", Value: "accepted", ReporterKeyID: "key_1", ObservedAt: time.Now().UTC(), IngestedAt: time.Now().UTC(), Revision: 1, IdempotencyKeyHash: shaLabel("one"), RequestFingerprint: shaLabel("two")}
	if _, err := log.Append(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	log.Close()
	wrong := make([]byte, 32)
	wrong[0] = 2
	if _, err := Verify(path, wrong); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("wrong key error=%v", err)
	}
	payload, _ := os.ReadFile(path)
	payload[len(payload)-1] ^= 1
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(path, key); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("tamper error=%v", err)
	}
}

func TestEvidenceAndExportKeepCostsOutOfGovernanceDatasets(t *testing.T) {
	for _, value := range []string{"https://example.test/evidence", "Bearer secret", "token=secret", "line\nbreak", "gw_secret"} {
		if domain.ValidateEvidenceRef(value) == nil {
			t.Errorf("accepted unsafe evidence_ref %q", value)
		}
	}
	directory := filepath.Join(t.TempDir(), "export")
	manifest, err := WriteExport(context.Background(), directory, ExportInput{WorkUnits: []domain.WorkUnit{{ID: "wku_unit", ProjectID: "prj_1", Status: domain.WorkUnitOpen, CreatedAt: time.Now()}}, Runs: []domain.Run{{ID: "run_one", ProjectID: "prj_1", WorkUnitID: "wku_unit", BudgetMicrosUSD: 100, Status: domain.RunActive, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}}, AccountingWatermark: ledger.Watermark{Generation: 1, Sequence: 2, Offset: 3}})
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Files) != 4 {
		t.Fatalf("files=%d", len(manifest.Files))
	}
	if _, err := VerifyExport(directory); err != nil {
		t.Fatal(err)
	}
	manifestPayload, err := os.ReadFile(filepath.Join(directory, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Run("rejects unsafe path", func(t *testing.T) {
		var changed ExportManifest
		if err := json.Unmarshal(manifestPayload, &changed); err != nil {
			t.Fatal(err)
		}
		changed.Files[0].Path = "../" + changed.Files[0].Path
		payload, _ := json.Marshal(changed)
		if err := os.WriteFile(filepath.Join(directory, "manifest.json"), payload, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := VerifyExport(directory); err == nil {
			t.Fatal("unsafe export path was accepted")
		}
	})
	if err := os.WriteFile(filepath.Join(directory, "manifest.json"), manifestPayload, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Run("rejects unexpected dataset", func(t *testing.T) {
		var changed ExportManifest
		if err := json.Unmarshal(manifestPayload, &changed); err != nil {
			t.Fatal(err)
		}
		changed.Files[0].Dataset = "unexpected"
		payload, _ := json.Marshal(changed)
		if err := os.WriteFile(filepath.Join(directory, "manifest.json"), payload, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := VerifyExport(directory); err == nil {
			t.Fatal("unexpected export dataset was accepted")
		}
	})
	if err := os.WriteFile(filepath.Join(directory, "manifest.json"), manifestPayload, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"work_units.ndjson", "runs.ndjson", "outcomes.ndjson", "outcome_definitions.ndjson"} {
		payload, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(payload, []byte("committed_micros_usd")) || bytes.Contains(payload, []byte("cost_micros_usd")) {
			t.Fatalf("%s duplicates additive cost", name)
		}
	}
}

func TestOutcomeDefinitionRejectsDuplicateSuccessValues(t *testing.T) {
	now := time.Now().UTC()
	definition := domain.OutcomeDefinition{ID: "odef_result", ProjectID: "prj_1", Name: "result", Version: 1, DataType: domain.OutcomeCategorical,
		AllowedValues: []string{"accepted", "rejected"}, SuccessValues: []string{"accepted", "accepted"}, Enabled: true, CreatedAt: now, CreatedBy: "admin"}
	if err := definition.Validate(); err == nil {
		t.Fatal("duplicate success values were accepted")
	}
}
