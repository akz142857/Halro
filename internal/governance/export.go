package governance

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
)

type ExportInput struct {
	WorkUnits           []domain.WorkUnit
	Runs                []domain.Run
	Outcomes            []domain.Outcome
	Definitions         []domain.OutcomeDefinition
	AccountingWatermark ledger.Watermark
	GovernanceSequence  uint64
	GovernanceOffset    int64
}

type ExportFile struct {
	Dataset     string `json:"dataset"`
	Schema      int    `json:"schema"`
	Format      string `json:"format"`
	Path        string `json:"path"`
	SHA256      string `json:"sha256"`
	RecordCount int    `json:"record_count"`
	MinSequence uint64 `json:"min_sequence,omitempty"`
	MaxSequence uint64 `json:"max_sequence,omitempty"`
}

type ExportManifest struct {
	Version             int              `json:"version"`
	GeneratedAt         time.Time        `json:"generated_at"`
	AccountingWatermark ledger.Watermark `json:"accounting_watermark"`
	GovernanceWatermark map[string]any   `json:"governance_watermark"`
	Files               []ExportFile     `json:"files"`
}

type workUnitExport struct {
	ID                 string                        `json:"id"`
	ProjectID          string                        `json:"project_id"`
	Status             domain.WorkUnitStatus         `json:"status"`
	CreatedAt          time.Time                     `json:"created_at"`
	ClosedAt           *time.Time                    `json:"closed_at,omitempty"`
	OutcomeDefinitions []domain.OutcomeDefinitionRef `json:"outcome_definitions"`
}
type runExport struct {
	ID              string           `json:"id"`
	ProjectID       string           `json:"project_id"`
	WorkUnitID      string           `json:"work_unit_id"`
	BudgetMicrosUSD int64            `json:"budget_micros_usd"`
	Status          domain.RunStatus `json:"status"`
	CreatedAt       time.Time        `json:"created_at"`
	ExpiresAt       time.Time        `json:"expires_at"`
	ClosedAt        *time.Time       `json:"closed_at,omitempty"`
}

func WriteExport(ctx context.Context, directory string, input ExportInput) (ExportManifest, error) {
	if directory == "" {
		return ExportManifest{}, errors.New("governance export directory is required")
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return ExportManifest{}, err
	}
	manifest := ExportManifest{Version: 1, GeneratedAt: time.Now().UTC(), AccountingWatermark: input.AccountingWatermark,
		GovernanceWatermark: map[string]any{"sequence": input.GovernanceSequence, "offset": input.GovernanceOffset}}
	workUnits := make([]workUnitExport, 0, len(input.WorkUnits))
	for _, item := range input.WorkUnits {
		workUnits = append(workUnits, workUnitExport{ID: item.ID, ProjectID: item.ProjectID, Status: item.Status, CreatedAt: item.CreatedAt, ClosedAt: item.ClosedAt, OutcomeDefinitions: item.OutcomeDefinitions})
	}
	runs := make([]runExport, 0, len(input.Runs))
	for _, item := range input.Runs {
		runs = append(runs, runExport{ID: item.ID, ProjectID: item.ProjectID, WorkUnitID: item.WorkUnitID, BudgetMicrosUSD: item.BudgetMicrosUSD, Status: item.Status, CreatedAt: item.CreatedAt, ExpiresAt: item.ExpiresAt, ClosedAt: item.ClosedAt})
	}
	datasets := []struct {
		name    string
		records any
		count   int
	}{
		{"work_units", workUnits, len(workUnits)}, {"runs", runs, len(runs)}, {"outcomes", input.Outcomes, len(input.Outcomes)}, {"outcome_definitions", input.Definitions, len(input.Definitions)},
	}
	for _, dataset := range datasets {
		file, err := writeNDJSON(ctx, directory, dataset.name, dataset.records)
		if err != nil {
			return ExportManifest{}, err
		}
		file.RecordCount = dataset.count
		if dataset.name == "outcomes" && len(input.Outcomes) > 0 {
			file.MinSequence, file.MaxSequence = input.Outcomes[0].GovernanceSequence, input.Outcomes[0].GovernanceSequence
			for _, item := range input.Outcomes {
				if item.GovernanceSequence < file.MinSequence {
					file.MinSequence = item.GovernanceSequence
				}
				if item.GovernanceSequence > file.MaxSequence {
					file.MaxSequence = item.GovernanceSequence
				}
			}
		}
		manifest.Files = append(manifest.Files, file)
	}
	sort.Slice(manifest.Files, func(i, j int) bool { return manifest.Files[i].Dataset < manifest.Files[j].Dataset })
	payload, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return ExportManifest{}, err
	}
	temporary := filepath.Join(directory, ".manifest.json.tmp")
	if err := os.WriteFile(temporary, append(payload, '\n'), 0o600); err != nil {
		return ExportManifest{}, err
	}
	if err := os.Rename(temporary, filepath.Join(directory, "manifest.json")); err != nil {
		return ExportManifest{}, err
	}
	return manifest, nil
}

func writeNDJSON(ctx context.Context, directory, dataset string, records any) (ExportFile, error) {
	temporary := filepath.Join(directory, "."+dataset+".ndjson.tmp")
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return ExportFile{}, err
	}
	writer := bufio.NewWriter(file)
	encoded, err := json.Marshal(records)
	if err != nil {
		file.Close()
		return ExportFile{}, err
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(encoded, &rows); err != nil {
		file.Close()
		return ExportFile{}, err
	}
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			file.Close()
			return ExportFile{}, err
		}
		if _, err := writer.Write(append(row, '\n')); err != nil {
			file.Close()
			return ExportFile{}, err
		}
	}
	if err := writer.Flush(); err != nil {
		file.Close()
		return ExportFile{}, err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return ExportFile{}, err
	}
	if err := file.Close(); err != nil {
		return ExportFile{}, err
	}
	contents, err := os.ReadFile(temporary)
	if err != nil {
		return ExportFile{}, err
	}
	digest := sha256.Sum256(contents)
	name := dataset + ".ndjson"
	if err := os.Rename(temporary, filepath.Join(directory, name)); err != nil {
		return ExportFile{}, err
	}
	return ExportFile{Dataset: dataset, Schema: 1, Format: "ndjson", Path: name, SHA256: "sha256:" + hex.EncodeToString(digest[:])}, nil
}

func VerifyExport(directory string) (ExportManifest, error) {
	payload, err := os.ReadFile(filepath.Join(directory, "manifest.json"))
	if err != nil {
		return ExportManifest{}, err
	}
	var manifest ExportManifest
	if err := json.Unmarshal(payload, &manifest); err != nil || manifest.Version != 1 || len(manifest.Files) != 4 {
		return ExportManifest{}, errors.New("governance export manifest is invalid")
	}
	expected := map[string]struct{}{
		"work_units": {}, "runs": {}, "outcomes": {}, "outcome_definitions": {},
	}
	for _, file := range manifest.Files {
		if _, exists := expected[file.Dataset]; !exists {
			return ExportManifest{}, errors.New("governance export dataset is unexpected or duplicated")
		}
		delete(expected, file.Dataset)
		if file.Schema != 1 || file.Format != "ndjson" || file.Path != filepath.Base(file.Path) || file.Path != file.Dataset+".ndjson" {
			return ExportManifest{}, fmt.Errorf("governance export file metadata is invalid: %s", file.Dataset)
		}
		contents, err := os.ReadFile(filepath.Join(directory, file.Path))
		if err != nil {
			return ExportManifest{}, err
		}
		digest := sha256.Sum256(contents)
		if file.SHA256 != "sha256:"+hex.EncodeToString(digest[:]) {
			return ExportManifest{}, fmt.Errorf("governance export checksum mismatch: %s", file.Dataset)
		}
		count := 0
		scanner := bufio.NewScanner(strings.NewReader(string(contents)))
		for scanner.Scan() {
			var row map[string]any
			if json.Unmarshal(scanner.Bytes(), &row) != nil {
				return ExportManifest{}, errors.New("governance export row is invalid")
			}
			count++
		}
		if scanner.Err() != nil || count != file.RecordCount {
			return ExportManifest{}, errors.New("governance export record count mismatch")
		}
	}
	if len(expected) != 0 {
		return ExportManifest{}, errors.New("governance export dataset is missing")
	}
	return manifest, nil
}
