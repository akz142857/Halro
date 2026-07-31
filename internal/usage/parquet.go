package usage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/parquet-go/parquet-go"
)

const parquetSchemaVersion = 2

type parquetAttempt struct {
	SchemaVersion        int32  `parquet:"schema_version"`
	EventID              string `parquet:"event_id,dict"`
	RequestID            string `parquet:"request_id,dict"`
	AttemptID            string `parquet:"attempt_id,dict"`
	Sequence             int64  `parquet:"sequence,delta"`
	AttemptNumber        int32  `parquet:"attempt_number"`
	ProjectID            string `parquet:"project_id,dict"`
	KeyID                string `parquet:"key_id,dict"`
	RouteID              string `parquet:"route_id,dict"`
	DeploymentID         string `parquet:"deployment_id,dict"`
	ProviderID           string `parquet:"provider_id,dict"`
	RequestedModel       string `parquet:"requested_model,dict"`
	ProviderModel        string `parquet:"provider_model,dict"`
	ProviderInputTokens  int64  `parquet:"provider_input_tokens"`
	ProviderOutputTokens int64  `parquet:"provider_output_tokens"`
	PreparedOutputTokens int64  `parquet:"prepared_output_tokens"`
	CostMicrosUSD        int64  `parquet:"cost_micros_usd"`
	CostEstimated        bool   `parquet:"cost_estimated"`
	TokensEstimated      bool   `parquet:"tokens_estimated"`
	StartedAtMicros      int64  `parquet:"started_at_utc,timestamp(microsecond)"`
	CompletedAtMicros    int64  `parquet:"completed_at_utc,timestamp(microsecond)"`
	Status               string `parquet:"status,dict"`
	ErrorClass           string `parquet:"error_class,dict"`
	HTTPStatus           int32  `parquet:"http_status"`
	LatencyMillis        int64  `parquet:"latency_millis"`
	RetryCount           int32  `parquet:"retry_count"`
	FallbackCount        int32  `parquet:"fallback_count"`
}

type Manifest struct {
	SchemaVersion int            `json:"schema_version"`
	LastSequence  uint64         `json:"last_sequence"`
	Files         []ManifestFile `json:"files"`
}

type ManifestFile struct {
	Path          string `json:"path"`
	Date          string `json:"date"`
	SHA256        string `json:"sha256"`
	MinSequence   uint64 `json:"min_sequence"`
	MaxSequence   uint64 `json:"max_sequence"`
	Records       int64  `json:"records"`
	InputTokens   int64  `json:"input_tokens"`
	OutputTokens  int64  `json:"output_tokens"`
	CostMicrosUSD int64  `json:"cost_micros_usd"`
}

type Exporter struct {
	root string
}

type RetentionReport struct {
	Cutoff       string `json:"cutoff"`
	FilesRemoved int    `json:"files_removed"`
	RowsRemoved  int64  `json:"rows_removed"`
}

type ReconciliationReport struct {
	LedgerRecords  int64 `json:"ledger_records"`
	ParquetRecords int64 `json:"parquet_records"`
	Missing        int64 `json:"missing"`
	Duplicates     int64 `json:"duplicates"`
	Extra          int64 `json:"extra"`
}

func NewExporter(root string) (*Exporter, error) {
	if !filepath.IsAbs(root) {
		return nil, errors.New("usage export directory must be absolute")
	}
	return &Exporter{root: filepath.Clean(root)}, nil
}

func (e *Exporter) Export(snapshot Snapshot) (Manifest, error) {
	manifest, err := e.LoadManifest()
	manifestMissing := errors.Is(err, os.ErrNotExist)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Manifest{}, err
	}
	if manifestMissing {
		manifest = Manifest{SchemaVersion: parquetSchemaVersion}
	}
	if manifest.SchemaVersion != parquetSchemaVersion {
		return Manifest{}, fmt.Errorf("usage manifest schema version %d is not supported", manifest.SchemaVersion)
	}
	pending := make([]AttemptEvent, 0)
	for _, attempt := range snapshot.Attempts {
		if attempt.Sequence > manifest.LastSequence {
			pending = append(pending, attempt)
		}
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].Sequence < pending[j].Sequence })
	if len(pending) == 0 {
		if manifestMissing {
			if err := e.commitManifest(manifest); err != nil {
				return Manifest{}, err
			}
		}
		return manifest, nil
	}
	byDate := make(map[string][]AttemptEvent)
	var dates []string
	for _, attempt := range pending {
		date := attempt.CompletedAt.UTC().Format("2006-01-02")
		if _, exists := byDate[date]; !exists {
			dates = append(dates, date)
		}
		byDate[date] = append(byDate[date], attempt)
	}
	sort.Strings(dates)
	for _, date := range dates {
		entry, err := e.publishPartition(date, byDate[date])
		if err != nil {
			return Manifest{}, err
		}
		manifest.Files = append(manifest.Files, entry)
		if entry.MaxSequence > manifest.LastSequence {
			manifest.LastSequence = entry.MaxSequence
		}
	}
	sort.Slice(manifest.Files, func(i, j int) bool {
		return manifest.Files[i].MinSequence < manifest.Files[j].MinSequence
	})
	if err := e.commitManifest(manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func (e *Exporter) LoadManifest() (Manifest, error) {
	payload, err := os.ReadFile(filepath.Join(e.root, "manifest.json"))
	if err != nil {
		return Manifest{}, err
	}
	var manifest Manifest
	if err := json.Unmarshal(payload, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode usage manifest: %w", err)
	}
	return manifest, nil
}

func (e *Exporter) Verify(snapshot *Snapshot) error {
	manifest, err := e.LoadManifest()
	if err != nil {
		return err
	}
	if manifest.SchemaVersion != parquetSchemaVersion {
		return fmt.Errorf("usage manifest schema version %d is not supported", manifest.SchemaVersion)
	}
	seen := make(map[string]struct{})
	var lastSequence uint64
	var firstRetainedSequence uint64
	for _, entry := range manifest.Files {
		path, err := e.safeManifestPath(entry.Path)
		if err != nil {
			return err
		}
		checksum, err := fileSHA256(path)
		if err != nil {
			return err
		}
		if checksum != entry.SHA256 {
			return fmt.Errorf("usage parquet checksum mismatch: %s", entry.Path)
		}
		rows, err := parquet.ReadFile[parquetAttempt](path)
		if err != nil {
			return fmt.Errorf("read usage parquet %s: %w", entry.Path, err)
		}
		if err := verifyRows(rows, entry, seen); err != nil {
			return fmt.Errorf("verify usage parquet %s: %w", entry.Path, err)
		}
		if entry.MaxSequence > lastSequence {
			lastSequence = entry.MaxSequence
		}
		if firstRetainedSequence == 0 || entry.MinSequence < firstRetainedSequence {
			firstRetainedSequence = entry.MinSequence
		}
	}
	if lastSequence > manifest.LastSequence ||
		(len(manifest.Files) > 0 && lastSequence != manifest.LastSequence) {
		return errors.New("usage manifest last sequence does not match its files")
	}
	if snapshot != nil {
		expected := make(map[string]AttemptEvent)
		for _, attempt := range snapshot.Attempts {
			if firstRetainedSequence > 0 && attempt.Sequence >= firstRetainedSequence &&
				attempt.Sequence <= manifest.LastSequence {
				expected[attempt.EventID] = attempt
			}
		}
		if len(expected) != len(seen) {
			return fmt.Errorf("usage reconciliation mismatch: parquet=%d ledger=%d", len(seen), len(expected))
		}
		for eventID := range expected {
			if _, exists := seen[eventID]; !exists {
				return fmt.Errorf("usage reconciliation missing event %s", eventID)
			}
		}
	}
	return nil
}

// Reconcile verifies checksums, schemas, summaries, duplicate Event IDs and
// the retained Ledger-to-Parquet set before returning explicit zero-difference
// counts suitable for an offline release/operations gate.
func (e *Exporter) Reconcile(snapshot Snapshot) (ReconciliationReport, error) {
	report := ReconciliationReport{}
	if err := e.Verify(&snapshot); err != nil {
		return report, err
	}
	manifest, err := e.LoadManifest()
	if err != nil {
		return report, err
	}
	var firstRetained uint64
	for _, entry := range manifest.Files {
		report.ParquetRecords += entry.Records
		if firstRetained == 0 || entry.MinSequence < firstRetained {
			firstRetained = entry.MinSequence
		}
	}
	for _, attempt := range snapshot.Attempts {
		if firstRetained > 0 && attempt.Sequence >= firstRetained && attempt.Sequence <= manifest.LastSequence {
			report.LedgerRecords++
		}
	}
	return report, nil
}

func (e *Exporter) PruneBefore(cutoff time.Time) (RetentionReport, error) {
	cutoffDate := cutoff.UTC().Format("2006-01-02")
	report := RetentionReport{Cutoff: cutoffDate}
	manifest, err := e.LoadManifest()
	if err != nil {
		return report, err
	}
	if manifest.SchemaVersion != parquetSchemaVersion {
		return report, fmt.Errorf("usage manifest schema version %d is not supported", manifest.SchemaVersion)
	}
	kept := make([]ManifestFile, 0, len(manifest.Files))
	removed := make([]ManifestFile, 0)
	for _, entry := range manifest.Files {
		if entry.Date < cutoffDate {
			removed = append(removed, entry)
			report.FilesRemoved++
			report.RowsRemoved += entry.Records
		} else {
			kept = append(kept, entry)
		}
	}
	if len(removed) == 0 {
		return report, nil
	}
	manifest.Files = kept
	if err := e.commitManifest(manifest); err != nil {
		return RetentionReport{}, fmt.Errorf("commit retained usage manifest: %w", err)
	}
	for _, entry := range removed {
		path, err := e.safeManifestPath(entry.Path)
		if err != nil {
			return report, err
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return report, fmt.Errorf("remove expired usage parquet: %w", err)
		}
		partition := filepath.Dir(path)
		if err := syncDirectory(partition); err != nil {
			return report, err
		}
		if err := os.Remove(partition); err != nil && !errors.Is(err, os.ErrNotExist) {
			// A non-empty partition can contain another retained file.
			if !isDirectoryNotEmpty(err) {
				return report, err
			}
		}
	}
	if err := syncDirectory(e.root); err != nil {
		return report, err
	}
	return report, nil
}

func (e *Exporter) publishPartition(date string, attempts []AttemptEvent) (ManifestFile, error) {
	if len(attempts) == 0 {
		return ManifestFile{}, errors.New("cannot publish an empty usage partition")
	}
	rows := make([]parquetAttempt, len(attempts))
	entry := ManifestFile{
		Date: date, MinSequence: attempts[0].Sequence,
		MaxSequence: attempts[len(attempts)-1].Sequence, Records: int64(len(attempts)),
	}
	for index, attempt := range attempts {
		rows[index] = toParquetAttempt(attempt)
		entry.InputTokens += attempt.ProviderInputTokens
		entry.OutputTokens += attempt.ProviderOutputTokens
		entry.CostMicrosUSD += attempt.CostMicrosUSD
	}
	relative := filepath.Join("date="+date,
		fmt.Sprintf("usage-%020d-%020d.parquet", entry.MinSequence, entry.MaxSequence))
	entry.Path = filepath.ToSlash(relative)
	path := filepath.Join(e.root, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return ManifestFile{}, fmt.Errorf("create usage partition: %w", err)
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := writeParquetAtomic(path, rows); err != nil {
			return ManifestFile{}, err
		}
	} else if err != nil {
		return ManifestFile{}, err
	} else {
		existing, err := parquet.ReadFile[parquetAttempt](path)
		if err != nil {
			return ManifestFile{}, fmt.Errorf("read orphan usage parquet: %w", err)
		}
		if !sameRows(existing, rows) {
			return ManifestFile{}, fmt.Errorf("existing usage parquet conflicts with export: %s", relative)
		}
	}
	checksum, err := fileSHA256(path)
	if err != nil {
		return ManifestFile{}, err
	}
	entry.SHA256 = checksum
	return entry, nil
}

func writeParquetAtomic(path string, rows []parquetAttempt) (err error) {
	temp, err := os.CreateTemp(filepath.Dir(path), ".usage-*.parquet.tmp")
	if err != nil {
		return fmt.Errorf("create usage parquet temp file: %w", err)
	}
	tempPath := temp.Name()
	defer func() {
		temp.Close()
		if err != nil {
			_ = os.Remove(tempPath)
		}
	}()
	if err = temp.Chmod(0o600); err != nil {
		return err
	}
	writer := parquet.NewGenericWriter[parquetAttempt](temp)
	if _, err = writer.Write(rows); err != nil {
		_ = writer.Close()
		return fmt.Errorf("write usage parquet: %w", err)
	}
	if err = writer.Close(); err != nil {
		return fmt.Errorf("close usage parquet writer: %w", err)
	}
	if err = temp.Sync(); err != nil {
		return fmt.Errorf("sync usage parquet: %w", err)
	}
	if err = temp.Close(); err != nil {
		return fmt.Errorf("close usage parquet: %w", err)
	}
	if err = os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("publish usage parquet: %w", err)
	}
	return syncDirectory(filepath.Dir(path))
}

func (e *Exporter) commitManifest(manifest Manifest) (err error) {
	if err := os.MkdirAll(e.root, 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(e.root, ".manifest-*.json.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer func() {
		temp.Close()
		if err != nil {
			_ = os.Remove(tempPath)
		}
	}()
	if err = temp.Chmod(0o600); err != nil {
		return err
	}
	encoder := json.NewEncoder(temp)
	encoder.SetIndent("", "  ")
	if err = encoder.Encode(manifest); err != nil {
		return err
	}
	if err = temp.Sync(); err != nil {
		return err
	}
	if err = temp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tempPath, filepath.Join(e.root, "manifest.json")); err != nil {
		return err
	}
	return syncDirectory(e.root)
}

func (e *Exporter) safeManifestPath(relative string) (string, error) {
	if relative == "" || filepath.IsAbs(relative) {
		return "", errors.New("usage manifest contains an invalid path")
	}
	clean := filepath.Clean(filepath.FromSlash(relative))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("usage manifest path escapes the usage directory")
	}
	path := filepath.Join(e.root, clean)
	if filepath.Dir(path) == e.root {
		return "", errors.New("usage parquet must be inside a date partition")
	}
	return path, nil
}

func verifyRows(rows []parquetAttempt, entry ManifestFile, seen map[string]struct{}) error {
	if int64(len(rows)) != entry.Records || len(rows) == 0 {
		return errors.New("record count mismatch")
	}
	var inputTokens, outputTokens, cost int64
	for _, row := range rows {
		if row.SchemaVersion != parquetSchemaVersion || row.Sequence <= 0 || row.EventID == "" {
			return errors.New("invalid row")
		}
		if _, exists := seen[row.EventID]; exists {
			return fmt.Errorf("duplicate event ID %s", row.EventID)
		}
		seen[row.EventID] = struct{}{}
		inputTokens += row.ProviderInputTokens
		outputTokens += row.ProviderOutputTokens
		cost += row.CostMicrosUSD
	}
	if uint64(rows[0].Sequence) != entry.MinSequence ||
		uint64(rows[len(rows)-1].Sequence) != entry.MaxSequence ||
		inputTokens != entry.InputTokens || outputTokens != entry.OutputTokens ||
		cost != entry.CostMicrosUSD {
		return errors.New("partition summary mismatch")
	}
	return nil
}

func toParquetAttempt(attempt AttemptEvent) parquetAttempt {
	return parquetAttempt{
		SchemaVersion: parquetSchemaVersion, EventID: attempt.EventID,
		RequestID: attempt.RequestID, AttemptID: attempt.AttemptID,
		Sequence: int64(attempt.Sequence), AttemptNumber: int32(attempt.AttemptNumber),
		ProjectID: attempt.ProjectID, KeyID: attempt.KeyID, RouteID: attempt.RouteID,
		DeploymentID: attempt.DeploymentID,
		ProviderID:   attempt.ProviderID, RequestedModel: attempt.RequestedModel,
		ProviderModel: attempt.ProviderModel, ProviderInputTokens: attempt.ProviderInputTokens,
		ProviderOutputTokens: attempt.ProviderOutputTokens,
		PreparedOutputTokens: attempt.PreparedOutputTokens,
		CostMicrosUSD:        attempt.CostMicrosUSD, CostEstimated: attempt.CostEstimated,
		TokensEstimated:   attempt.TokensEstimated,
		StartedAtMicros:   attempt.StartedAt.UTC().UnixMicro(),
		CompletedAtMicros: attempt.CompletedAt.UTC().UnixMicro(),
		Status:            attempt.Status, ErrorClass: attempt.ErrorClass,
		HTTPStatus: int32(attempt.HTTPStatus), LatencyMillis: attempt.LatencyMillis,
		RetryCount: int32(attempt.RetryCount), FallbackCount: int32(attempt.FallbackCount),
	}
}

func sameRows(left, right []parquetAttempt) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func isDirectoryNotEmpty(err error) bool {
	return errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST)
}
