package governance

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/metadatajournal"
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

// Export limits bound one encoded row and the fixed four-file manifest, not
// shared RAM, total disk consumption or producer work. Sources must page outside
// DB transactions and retain their fixed-prefix ownership until they return.
const ExportRowMaxBytes = 1 << 20
const ExportDefinitionRowMaxBytes = metadatajournal.MaxPayloadSize
const ExportManifestMaxBytes = 64 << 10

var ErrExportRowLimit = errors.New("governance export row exceeds its byte or domain boundary")
var ErrExportStreamClosed = errors.New("governance export stream is closed")

// A producer yields borrowed values synchronously. It must stop on yield errors
// and may not retain yield or create unbounded parallel work. The sink latches
// ignored errors and refuses late callbacks, so neither can publish a prefix.
type ExportProducer[T any] func(context.Context, func(T) error) error

type ExportStreamInput struct {
	WorkUnits           ExportProducer[domain.WorkUnit]
	Runs                ExportProducer[domain.Run]
	Outcomes            ExportProducer[domain.Outcome]
	Definitions         ExportProducer[domain.OutcomeDefinition]
	AccountingWatermark ledger.Watermark
	GovernanceSequence  uint64
	GovernanceOffset    int64
	// Runs once after every producer has returned and all dataset files have
	// been synced, immediately before manifest publication. Owners must seal
	// their complete fixed cuts here; errors discard this call's output.
	BeforePublish func(context.Context) error
}

func exportSliceProducer[T any](rows []T) ExportProducer[T] {
	return func(ctx context.Context, yield func(T) error) error {
		for _, row := range rows {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := yield(row); err != nil {
				return err
			}
		}
		return ctx.Err()
	}
}

// Compatibility entry point: it does not allocate dataset copies, but callers
// that supply complete slices still retain those slices for the operation.
func WriteExport(ctx context.Context, directory string, input ExportInput) (ExportManifest, error) {
	return WriteExportStream(ctx, directory, ExportStreamInput{
		WorkUnits: exportSliceProducer(input.WorkUnits), Runs: exportSliceProducer(input.Runs),
		Outcomes: exportSliceProducer(input.Outcomes), Definitions: exportSliceProducer(input.Definitions),
		AccountingWatermark: input.AccountingWatermark, GovernanceSequence: input.GovernanceSequence, GovernanceOffset: input.GovernanceOffset,
	})
}

// WriteExportStream creates an exclusive directory. manifest.json is the commit
// marker, published only after all producers finish, datasets are flushed/synced
// and dataset names are synced. An existing export is never overwritten. Errors
// clean only this call's owned directory and return a zero manifest. A crash can
// leave an incomplete directory without a manifest; it must not be consumed.
func WriteExportStream(ctx context.Context, directory string, input ExportStreamInput) (result ExportManifest, err error) {
	if err := ctx.Err(); err != nil {
		return ExportManifest{}, err
	}
	if directory == "" {
		return ExportManifest{}, errors.New("governance export directory is required")
	}
	directory = filepath.Clean(directory)
	parent := filepath.Dir(directory)
	if err := ensureExportParent(ctx, parent, syncExportDirectory); err != nil {
		return ExportManifest{}, err
	}
	if err := os.Mkdir(directory, 0o700); err != nil {
		return ExportManifest{}, err
	}
	published := false
	defer func() {
		if !published {
			err = errors.Join(err, os.RemoveAll(directory))
			result = ExportManifest{}
		}
	}()
	manifest := ExportManifest{Version: 1, GeneratedAt: time.Now().UTC(), AccountingWatermark: input.AccountingWatermark,
		GovernanceWatermark: map[string]any{"sequence": input.GovernanceSequence, "offset": input.GovernanceOffset}}
	add := func(file ExportFile, err error) error {
		if err != nil {
			return err
		}
		manifest.Files = append(manifest.Files, file)
		return nil
	}
	if err := add(writeExportDataset(ctx, directory, "work_units", input.WorkUnits, encodeExportWorkUnit)); err != nil {
		return ExportManifest{}, err
	}
	if err := add(writeExportDataset(ctx, directory, "runs", input.Runs, encodeExportRun)); err != nil {
		return ExportManifest{}, err
	}
	governanceSequence := input.GovernanceSequence
	if err := add(writeExportDataset(ctx, directory, "outcomes", input.Outcomes, func(row domain.Outcome) ([]byte, uint64, error) {
		if row.GovernanceSequence == 0 || row.GovernanceSequence > governanceSequence {
			return nil, 0, ErrCorrupt
		}
		if err := exportFieldLimit(row.ID, row.ProjectID, row.WorkUnitID, row.DefinitionID, row.Value, row.ReporterKeyID, row.EvidenceSHA256, row.EvidenceRef, row.SupersedesOutcomeID); err != nil {
			return nil, 0, err
		}
		raw, err := json.Marshal(row)
		return raw, row.GovernanceSequence, err
	})); err != nil {
		return ExportManifest{}, err
	}
	if err := add(writeExportDataset(ctx, directory, "outcome_definitions", input.Definitions, encodeExportDefinition)); err != nil {
		return ExportManifest{}, err
	}
	sort.Slice(manifest.Files, func(i, j int) bool { return manifest.Files[i].Dataset < manifest.Files[j].Dataset })
	if err := ctx.Err(); err != nil {
		return ExportManifest{}, err
	}
	if err := syncExportDirectory(directory); err != nil {
		return ExportManifest{}, err
	}
	payload, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return ExportManifest{}, err
	}
	if len(payload)+1 > ExportManifestMaxBytes {
		return ExportManifest{}, ErrExportRowLimit
	}
	temporary := filepath.Join(directory, ".manifest.json.tmp")
	if err := writeExportManifest(temporary, append(payload, '\n')); err != nil {
		return ExportManifest{}, err
	}
	if input.BeforePublish != nil {
		if err := input.BeforePublish(ctx); err != nil {
			return ExportManifest{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return ExportManifest{}, err
	}
	if err := os.Rename(temporary, filepath.Join(directory, "manifest.json")); err != nil {
		return ExportManifest{}, err
	}
	if err := syncExportDirectory(directory); err != nil {
		return ExportManifest{}, err
	}
	if err := syncExportDirectory(parent); err != nil {
		return ExportManifest{}, err
	}
	published = true
	return manifest, nil
}

func exportFieldLimit(fields ...string) error {
	return exportFieldsWithin(ExportRowMaxBytes, fields...)
}
func exportFieldsWithin(limit int, fields ...string) error {
	size := 0
	for _, field := range fields {
		if len(field) > limit-size {
			return ErrExportRowLimit
		}
		size += len(field)
	}
	return nil
}

func encodeExportWorkUnit(row domain.WorkUnit) ([]byte, uint64, error) {
	if len(row.OutcomeDefinitions) > domain.MaxDefinitionsPerWorkUnit {
		return nil, 0, ErrExportRowLimit
	}
	if err := exportFieldLimit(row.ID, row.ProjectID, string(row.Status)); err != nil {
		return nil, 0, err
	}
	for _, ref := range row.OutcomeDefinitions {
		if len(ref.ID) > domain.MaxGovernanceIDLength {
			return nil, 0, ErrExportRowLimit
		}
	}
	raw, err := json.Marshal(workUnitExport{ID: row.ID, ProjectID: row.ProjectID, Status: row.Status, CreatedAt: row.CreatedAt, ClosedAt: row.ClosedAt, OutcomeDefinitions: row.OutcomeDefinitions})
	return raw, 0, err
}
func encodeExportRun(row domain.Run) ([]byte, uint64, error) {
	if err := exportFieldLimit(row.ID, row.ProjectID, row.WorkUnitID, string(row.Status)); err != nil {
		return nil, 0, err
	}
	raw, err := json.Marshal(runExport{ID: row.ID, ProjectID: row.ProjectID, WorkUnitID: row.WorkUnitID, BudgetMicrosUSD: row.BudgetMicrosUSD, Status: row.Status, CreatedAt: row.CreatedAt, ExpiresAt: row.ExpiresAt, ClosedAt: row.ClosedAt})
	return raw, 0, err
}
func encodeExportDefinition(row domain.OutcomeDefinition) ([]byte, uint64, error) {
	if len(row.AllowedValues) > 16 || len(row.SuccessValues) > 16 {
		return nil, 0, ErrExportRowLimit
	}
	if err := exportFieldsWithin(ExportDefinitionRowMaxBytes, row.ID, row.ProjectID, row.Name, string(row.DataType), row.Unit, row.Description, row.CreatedBy); err != nil {
		return nil, 0, err
	}
	for _, values := range [][]string{row.AllowedValues, row.SuccessValues} {
		for _, value := range values {
			if len(value) > 32 {
				return nil, 0, ErrExportRowLimit
			}
		}
	}
	raw, err := json.Marshal(row)
	return raw, 0, err
}

func writeExportDataset[T any](ctx context.Context, directory, dataset string, source ExportProducer[T], encode func(T) ([]byte, uint64, error)) (ExportFile, error) {
	name := dataset + ".ndjson"
	temporary := filepath.Join(directory, "."+name+".tmp")
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return ExportFile{}, err
	}
	defer file.Close()
	digest := sha256.New()
	writer := bufio.NewWriterSize(io.MultiWriter(file, digest), 64<<10)
	meta := ExportFile{Dataset: dataset, Schema: 1, Format: "ndjson", Path: name}
	var mu sync.Mutex
	var sinkErr error
	closed := false
	yield := func(row T) error {
		mu.Lock()
		defer mu.Unlock()
		if closed {
			return ErrExportStreamClosed
		}
		if sinkErr != nil {
			return sinkErr
		}
		sinkErr = ctx.Err()
		if sinkErr != nil {
			return sinkErr
		}
		raw, seq, err := encode(row)
		if err == nil && (len(raw) > exportDatasetRowLimit(dataset) || meta.RecordCount == math.MaxInt) {
			err = ErrExportRowLimit
		}
		if err != nil {
			sinkErr = err
			return err
		}
		if _, err := writer.Write(raw); err != nil {
			sinkErr = err
			return err
		}
		if err := writer.WriteByte('\n'); err != nil {
			sinkErr = err
			return err
		}
		meta.RecordCount++
		if seq != 0 {
			if meta.MinSequence == 0 || seq < meta.MinSequence {
				meta.MinSequence = seq
			}
			if seq > meta.MaxSequence {
				meta.MaxSequence = seq
			}
		}
		return nil
	}
	var producerErr error
	if source != nil {
		producerErr = source(ctx, yield)
	}
	mu.Lock()
	closed = true // joins entered callbacks; all later callbacks fail before IO.
	err = errors.Join(producerErr, sinkErr, ctx.Err())
	if err == nil {
		err = writer.Flush()
	}
	writer = nil // retained callbacks cannot retain buffers or owner dependencies.
	ctx = nil
	encode = nil
	mu.Unlock()
	if err != nil {
		return ExportFile{}, err
	}
	if err := file.Sync(); err != nil {
		return ExportFile{}, err
	}
	if err := file.Close(); err != nil {
		return ExportFile{}, err
	}
	if err := os.Rename(temporary, filepath.Join(directory, name)); err != nil {
		return ExportFile{}, err
	}
	meta.SHA256 = "sha256:" + hex.EncodeToString(digest.Sum(nil))
	return meta, nil
}

func writeExportManifest(path string, payload []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(payload); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	return f.Close()
}

// Sync the complete parent chain before claiming the export directory. This
// also covers retries that encounter parents left by a failed first-run sync.
// All handles are opened read-only and held one at a time.
func ensureExportParent(ctx context.Context, path string, syncDir func(string) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := syncDir(path); err != nil {
			return err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return nil
		}
		path = parent
	}
}
func exportDatasetRowLimit(dataset string) int {
	if dataset == "outcome_definitions" {
		return ExportDefinitionRowMaxBytes
	}
	return ExportRowMaxBytes
}

func syncExportDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func VerifyExport(directory string) (ExportManifest, error) {
	return VerifyExportContext(context.Background(), directory)
}

// Verification reads bounded lines and hashes the same open descriptor; it
// never loads the whole dataset. Checksums prove file/manifest consistency,
// not authenticated journal content, key sets or cross-journal atomicity.
func VerifyExportContext(ctx context.Context, directory string) (ExportManifest, error) {
	if err := ctx.Err(); err != nil {
		return ExportManifest{}, err
	}
	path := filepath.Join(directory, "manifest.json")
	file, err := os.Open(path)
	if err != nil {
		return ExportManifest{}, err
	}
	defer file.Close()
	initial, err := file.Stat()
	if err != nil {
		return ExportManifest{}, err
	}
	payload, err := io.ReadAll(io.LimitReader(file, ExportManifestMaxBytes+1))
	if err == nil {
		err = exportFileIdentity(file, path, initial)
	}
	if err != nil {
		return ExportManifest{}, err
	}
	if len(payload) > ExportManifestMaxBytes {
		return ExportManifest{}, ErrExportRowLimit
	}
	var manifest ExportManifest
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if err := decoder.Decode(&manifest); err != nil || manifest.Version != 1 || len(manifest.Files) != 4 {
		return ExportManifest{}, errors.New("governance export manifest is invalid")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return ExportManifest{}, errors.New("governance export manifest has trailing data")
	}
	sequence, ok := manifest.GovernanceWatermark["sequence"].(json.Number)
	if !ok {
		return ExportManifest{}, errors.New("governance export watermark is invalid")
	}
	var head uint64
	if err := json.Unmarshal([]byte(sequence), &head); err != nil {
		return ExportManifest{}, err
	}
	expected := map[string]struct{}{"work_units": {}, "runs": {}, "outcomes": {}, "outcome_definitions": {}}
	for _, meta := range manifest.Files {
		if _, exists := expected[meta.Dataset]; !exists {
			return ExportManifest{}, errors.New("governance export dataset is unexpected or duplicated")
		}
		delete(expected, meta.Dataset)
		if meta.Schema != 1 || meta.Format != "ndjson" || meta.Path != filepath.Base(meta.Path) || meta.Path != meta.Dataset+".ndjson" || meta.RecordCount < 0 {
			return ExportManifest{}, fmt.Errorf("governance export file metadata is invalid: %s", meta.Dataset)
		}
		if err := verifyExportDataset(ctx, filepath.Join(directory, meta.Path), meta, head); err != nil {
			return ExportManifest{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return ExportManifest{}, err
	}
	if err := exportFileIdentity(file, path, initial); err != nil {
		return ExportManifest{}, err
	}
	return manifest, nil
}

func exportFileIdentity(file *os.File, path string, initial os.FileInfo) error {
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	named, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !opened.Mode().IsRegular() || !named.Mode().IsRegular() || !os.SameFile(opened, named) || !os.SameFile(opened, initial) || opened.Size() != named.Size() || opened.Size() != initial.Size() || !opened.ModTime().Equal(initial.ModTime()) {
		return errors.New("governance export source was replaced or is not a regular file")
	}
	return nil
}

func verifyExportDataset(ctx context.Context, path string, meta ExportFile, head uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	initial, err := file.Stat()
	if err != nil {
		return err
	}
	if err := exportFileIdentity(file, path, initial); err != nil {
		return err
	}
	digest := sha256.New()
	scanner := bufio.NewScanner(io.TeeReader(file, digest))
	rowLimit := exportDatasetRowLimit(meta.Dataset)
	scanner.Buffer(make([]byte, 64<<10), rowLimit+2)
	count := 0
	var minSequence, maxSequence uint64
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		row := bytes.TrimSpace(scanner.Bytes())
		if len(scanner.Bytes()) > rowLimit {
			return ErrExportRowLimit
		}
		if len(row) == 0 || row[0] != '{' || !json.Valid(row) {
			return errors.New("governance export row is invalid")
		}
		if count == math.MaxInt {
			return ErrExportRowLimit
		}
		count++
		if meta.Dataset == "outcomes" {
			var outcome struct {
				Sequence uint64 `json:"governance_sequence"`
			}
			if err := json.Unmarshal(row, &outcome); err != nil || outcome.Sequence == 0 || outcome.Sequence > head {
				return errors.New("governance export outcome sequence is invalid")
			}
			if minSequence == 0 || outcome.Sequence < minSequence {
				minSequence = outcome.Sequence
			}
			if outcome.Sequence > maxSequence {
				maxSequence = outcome.Sequence
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := exportFileIdentity(file, path, initial); err != nil {
		return err
	}
	if meta.SHA256 != "sha256:"+hex.EncodeToString(digest.Sum(nil)) {
		return fmt.Errorf("governance export checksum mismatch: %s", meta.Dataset)
	}
	if count != meta.RecordCount || minSequence != meta.MinSequence || maxSequence != meta.MaxSequence {
		return errors.New("governance export record count or sequence range mismatch")
	}
	return nil
}
