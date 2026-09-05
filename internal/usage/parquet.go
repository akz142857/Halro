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
	"github.com/parquet-go/parquet-go/compress/zstd"

	"github.com/akz142857/Halro/internal/durable"
)

// Schema 6 adds nullable Work Unit and Run attribution. Rows written under
// schema 5 decode with both columns empty, preserving ungoverned history.
//
// Schema 5 adds the upstream's own identifiers for a failed attempt — the
// provider code, the provider request ID, and the phase the failure happened in
// — so history keeps what a support ticket to the upstream is built out of.
// Rows written under schema 4 decode with those columns empty, which reads as
// "not recorded", and the console says exactly that rather than showing an
// "unknown" nobody can act on.
//
// Schema 4 added the provider token tiers: the cache-read, cache-write, and
// reasoning spans that partition the input and output totals. Rows written under
// schema 3 decode with those columns zero, which reads correctly as "no tier
// reported" rather than as a tier of size zero.
const parquetSchemaVersion = 6

// parquetSchemaMinReadable is the oldest manifest this build still opens. Every
// version from here to parquetSchemaVersion is accepted and upgraded in place;
// gating on an explicit pair instead is how a bump silently locks out the
// installs written by the version before it.
const parquetSchemaMinReadable = 3

func supportedManifestSchema(version int) bool {
	return version >= parquetSchemaMinReadable && version <= parquetSchemaVersion
}

// Export format (ADR 0017): what new partitions are written as. Existing
// partitions are never rewritten to a different format.
const (
	FormatParquet = "parquet"
	FormatNDJSON  = "ndjson"
)

type parquetAttempt struct {
	SchemaVersion                 int32  `parquet:"schema_version" json:"schema_version"`
	EventID                       string `parquet:"event_id,dict" json:"event_id"`
	RequestID                     string `parquet:"request_id,dict" json:"request_id"`
	AttemptID                     string `parquet:"attempt_id,dict" json:"attempt_id"`
	Sequence                      int64  `parquet:"sequence,delta" json:"sequence"`
	AttemptNumber                 int32  `parquet:"attempt_number" json:"attempt_number"`
	ProjectID                     string `parquet:"project_id,dict" json:"project_id"`
	WorkUnitID                    string `parquet:"work_unit_id,dict" json:"work_unit_id"`
	RunID                         string `parquet:"run_id,dict" json:"run_id"`
	KeyID                         string `parquet:"key_id,dict" json:"key_id"`
	RouteID                       string `parquet:"route_id,dict" json:"route_id"`
	DeploymentID                  string `parquet:"deployment_id,dict" json:"deployment_id"`
	ProviderID                    string `parquet:"provider_id,dict" json:"provider_id"`
	RequestedModel                string `parquet:"requested_model,dict" json:"requested_model"`
	ProviderModel                 string `parquet:"provider_model,dict" json:"provider_model"`
	ProviderInputTokens           int64  `parquet:"provider_input_tokens" json:"provider_input_tokens"`
	ProviderOutputTokens          int64  `parquet:"provider_output_tokens" json:"provider_output_tokens"`
	ProviderCachedInputTokens     int64  `parquet:"provider_cached_input_tokens" json:"provider_cached_input_tokens"`
	ProviderCacheWriteInputTokens int64  `parquet:"provider_cache_write_input_tokens" json:"provider_cache_write_input_tokens"`
	ProviderReasoningTokens       int64  `parquet:"provider_reasoning_tokens" json:"provider_reasoning_tokens"`
	PreparedOutputTokens          int64  `parquet:"prepared_output_tokens" json:"prepared_output_tokens"`
	CostMicrosUSD                 int64  `parquet:"cost_micros_usd" json:"cost_micros_usd"`
	CostKnown                     bool   `parquet:"cost_known" json:"cost_known"`
	PriceEvidenceStatus           string `parquet:"price_evidence_status,dict" json:"price_evidence_status"`
	CostValueStatus               string `parquet:"cost_value_status,dict" json:"cost_value_status"`
	BillingMode                   string `parquet:"billing_mode,dict" json:"billing_mode"`
	PriceSnapshotJSON             string `parquet:"price_snapshot_json" json:"price_snapshot_json"`
	InputCostMicrosUSD            int64  `parquet:"input_cost_micros_usd" json:"input_cost_micros_usd"`
	OutputCostMicrosUSD           int64  `parquet:"output_cost_micros_usd" json:"output_cost_micros_usd"`
	FixedCostMicrosUSD            int64  `parquet:"fixed_cost_micros_usd" json:"fixed_cost_micros_usd"`
	TokenUsageSource              string `parquet:"token_usage_source,dict" json:"token_usage_source"`
	CostEstimated                 bool   `parquet:"cost_estimated" json:"cost_estimated"`
	TokensEstimated               bool   `parquet:"tokens_estimated" json:"tokens_estimated"`
	StartedAtMicros               int64  `parquet:"started_at_utc,timestamp(microsecond)" json:"started_at_utc"`
	CompletedAtMicros             int64  `parquet:"completed_at_utc,timestamp(microsecond)" json:"completed_at_utc"`
	Status                        string `parquet:"status,dict" json:"status"`
	ErrorClass                    string `parquet:"error_class,dict" json:"error_class"`
	HTTPStatus                    int32  `parquet:"http_status" json:"http_status"`
	ProviderCode                  string `parquet:"provider_code,dict" json:"provider_code"`
	ProviderRequestID             string `parquet:"provider_request_id" json:"provider_request_id"`
	FailurePhase                  string `parquet:"failure_phase,dict" json:"failure_phase"`
	LatencyMillis                 int64  `parquet:"latency_millis" json:"latency_millis"`
	RetryCount                    int32  `parquet:"retry_count" json:"retry_count"`
	FallbackCount                 int32  `parquet:"fallback_count" json:"fallback_count"`
}

type Manifest struct {
	SchemaVersion int            `json:"schema_version"`
	LastSequence  uint64         `json:"last_sequence"`
	Files         []ManifestFile `json:"files"`
}

type ManifestFile struct {
	SchemaVersion int    `json:"schema_version,omitempty"`
	Path          string `json:"path"`
	Date          string `json:"date"`
	SHA256        string `json:"sha256"`
	MinSequence   uint64 `json:"min_sequence"`
	MaxSequence   uint64 `json:"max_sequence"`
	Records       int64  `json:"records"`
	InputTokens   int64  `json:"input_tokens"`
	OutputTokens  int64  `json:"output_tokens"`
	CostMicrosUSD int64  `json:"cost_micros_usd"`
	// Format is the physical container this partition is stored in (ADR
	// 0017). Empty decodes as FormatParquet — every manifest written before
	// this field existed only ever held Parquet partitions.
	Format string `json:"format,omitempty"`
}

func (f ManifestFile) format() string {
	if f.Format == "" {
		return FormatParquet
	}
	return f.Format
}

type Exporter struct {
	root   string
	format string
}

// Options configures what NewExporterWithOptions writes new partitions as.
// The zero value writes Parquet, matching NewExporter's historical behavior.
type Options struct {
	Format string
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
	return NewExporterWithOptions(root, Options{})
}

func NewExporterWithOptions(root string, options Options) (*Exporter, error) {
	if !filepath.IsAbs(root) {
		return nil, errors.New("usage export directory must be absolute")
	}
	format := options.Format
	if format == "" {
		format = FormatParquet
	}
	if format != FormatParquet && format != FormatNDJSON {
		return nil, fmt.Errorf("usage export format %q is not supported", format)
	}
	return &Exporter{root: filepath.Clean(root), format: format}, nil
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
	if !supportedManifestSchema(manifest.SchemaVersion) {
		return Manifest{}, fmt.Errorf("usage manifest schema version %d is not supported", manifest.SchemaVersion)
	}
	manifestUpgraded := manifest.SchemaVersion != parquetSchemaVersion
	if manifestUpgraded {
		// Stamp the files with the version they were actually written under
		// before the manifest moves forward, so a reader can still tell which
		// columns a given partition is expected to carry.
		previous := manifest.SchemaVersion
		for index := range manifest.Files {
			if manifest.Files[index].SchemaVersion == 0 {
				manifest.Files[index].SchemaVersion = previous
			}
		}
		manifest.SchemaVersion = parquetSchemaVersion
	}
	pending := make([]AttemptEvent, 0)
	for _, attempt := range snapshot.Attempts {
		if attempt.Sequence > manifest.LastSequence {
			pending = append(pending, attempt)
		}
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].Sequence < pending[j].Sequence })
	if len(pending) == 0 {
		if manifestMissing || manifestUpgraded {
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
	if !supportedManifestSchema(manifest.SchemaVersion) {
		return fmt.Errorf("usage manifest schema version %d is not supported", manifest.SchemaVersion)
	}
	seen := make(map[string]struct{})
	canonicalRows := make(map[string]parquetAttempt)
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
		rows, readErr := readAttemptRows(path, entry.format())
		if readErr != nil {
			return fmt.Errorf("read usage partition %s: %w", entry.Path, readErr)
		}
		if err := verifyRows(rows, entry, seen, canonicalRows); err != nil {
			return fmt.Errorf("verify usage partition %s: %w", entry.Path, err)
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
		// The window's lower edge. Below it the aggregate has been pruned, so
		// there is nothing on this side to compare — and the rows on the
		// Parquet side come out of the comparison with it, by row rather than
		// by file, because one partition covers a whole day and a floor almost
		// always falls inside a file rather than between two.
		//
		// This narrows the comparison; it does not relax it. Everything at or
		// above the floor is still matched one for one, by count and by
		// content. A snapshot that dropped records without declaring a floor is
		// still refused, which is what keeps this from becoming a way to hide
		// a lossy export.
		floor := max(firstRetainedSequence, snapshot.Floor)
		if snapshot.Floor > 0 {
			for eventID, row := range canonicalRows {
				if uint64(row.Sequence) < floor {
					delete(seen, eventID)
				}
			}
		}
		expected := make(map[string]AttemptEvent)
		for _, attempt := range snapshot.Attempts {
			if firstRetainedSequence > 0 && attempt.Sequence >= floor &&
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
			if row, exists := canonicalRows[eventID]; exists && row != narrowToSchema(toParquetAttempt(expected[eventID]), row.SchemaVersion) {
				return fmt.Errorf("usage reconciliation content mismatch for event %s", eventID)
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
		if err := addInt64(&report.ParquetRecords, entry.Records); err != nil {
			return report, err
		}
		if firstRetained == 0 || entry.MinSequence < firstRetained {
			firstRetained = entry.MinSequence
		}
	}
	// The comparison runs over the range both sides still claim to hold, which
	// is not the same as the range Parquet holds. The aggregate serves a
	// console window and is pruned behind it, so below its floor there is
	// nothing left to compare — counting those as missing would report the
	// window working as a reconciliation failure, and doctor would refuse.
	//
	// Parquet's rows below the floor come off the same way, and by row rather
	// than by file: one partition covers a whole day, so a floor almost always
	// falls inside a file rather than between two. Subtracting whole files
	// either kept rows the aggregate no longer has or discarded rows it does.
	floor := max(firstRetained, snapshot.Floor)
	if snapshot.Floor > 0 {
		below, err := e.rowsBelow(manifest, floor)
		if err != nil {
			return report, err
		}
		if err := addInt64(&report.ParquetRecords, -below); err != nil {
			return report, err
		}
	}
	for _, attempt := range snapshot.Attempts {
		if firstRetained > 0 && attempt.Sequence >= floor && attempt.Sequence <= manifest.LastSequence {
			report.LedgerRecords++
		}
	}
	return report, nil
}

// rowsBelow counts the exported rows the aggregate's window no longer covers.
//
// Only the partitions that straddle or precede the floor are read; the ones
// entirely above it are answered from the manifest, which is what keeps a long
// archive from being re-read on every reconciliation.
func (e *Exporter) rowsBelow(manifest Manifest, floor uint64) (int64, error) {
	var below int64
	for _, entry := range manifest.Files {
		if entry.MinSequence >= floor {
			continue
		}
		if entry.MaxSequence < floor {
			if err := addInt64(&below, entry.Records); err != nil {
				return 0, err
			}
			continue
		}
		path, err := e.safeManifestPath(entry.Path)
		if err != nil {
			return 0, err
		}
		rows, err := readAttemptRows(path, entry.format())
		if err != nil {
			return 0, err
		}
		for _, row := range rows {
			if uint64(row.Sequence) < floor {
				below++
			}
		}
	}
	return below, nil
}

func (e *Exporter) PruneBefore(cutoff time.Time) (RetentionReport, error) {
	cutoffDate := cutoff.UTC().Format("2006-01-02")
	report := RetentionReport{Cutoff: cutoffDate}
	manifest, err := e.LoadManifest()
	if err != nil {
		return report, err
	}
	if !supportedManifestSchema(manifest.SchemaVersion) {
		return report, fmt.Errorf("usage manifest schema version %d is not supported", manifest.SchemaVersion)
	}
	kept := make([]ManifestFile, 0, len(manifest.Files))
	removed := make([]ManifestFile, 0)
	for _, entry := range manifest.Files {
		if entry.Date < cutoffDate {
			removed = append(removed, entry)
			report.FilesRemoved++
			if err := addInt64(&report.RowsRemoved, entry.Records); err != nil {
				return report, err
			}
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
		if err := durable.SyncDirectory(partition); err != nil {
			return report, err
		}
		if err := os.Remove(partition); err != nil && !errors.Is(err, os.ErrNotExist) {
			// A non-empty partition can contain another retained file.
			if !isDirectoryNotEmpty(err) {
				return report, err
			}
		}
	}
	if err := durable.SyncDirectory(e.root); err != nil {
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
		SchemaVersion: parquetSchemaVersion, Date: date, MinSequence: attempts[0].Sequence,
		MaxSequence: attempts[len(attempts)-1].Sequence, Records: int64(len(attempts)),
		Format: e.format,
	}
	for index, attempt := range attempts {
		rows[index] = toParquetAttempt(attempt)
		if err := addInt64(&entry.InputTokens, attempt.ProviderInputTokens); err != nil {
			return ManifestFile{}, err
		}
		if err := addInt64(&entry.OutputTokens, attempt.ProviderOutputTokens); err != nil {
			return ManifestFile{}, err
		}
		if cost, ok := attempt.KnownCostMicrosUSD(); ok {
			if err := addInt64(&entry.CostMicrosUSD, cost); err != nil {
				return ManifestFile{}, err
			}
		}
	}
	relative := filepath.Join("date="+date,
		fmt.Sprintf("usage-%020d-%020d.%s", entry.MinSequence, entry.MaxSequence, partitionExtension(e.format)))
	entry.Path = filepath.ToSlash(relative)
	path := filepath.Join(e.root, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return ManifestFile{}, fmt.Errorf("create usage partition: %w", err)
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if e.format == FormatNDJSON {
			if err := writeNDJSONAtomic(path, rows); err != nil {
				return ManifestFile{}, err
			}
		} else if err := writeParquetAtomic(path, rows); err != nil {
			return ManifestFile{}, err
		}
	} else if err != nil {
		return ManifestFile{}, err
	} else {
		existing, err := readAttemptRows(path, e.format)
		if err != nil {
			return ManifestFile{}, fmt.Errorf("read orphan usage partition: %w", err)
		}
		if !sameRows(existing, rows) {
			return ManifestFile{}, fmt.Errorf("existing usage partition conflicts with export: %s", relative)
		}
	}
	checksum, err := fileSHA256(path)
	if err != nil {
		return ManifestFile{}, err
	}
	entry.SHA256 = checksum
	return entry, nil
}

func partitionExtension(format string) string {
	if format == FormatNDJSON {
		return "ndjson"
	}
	return "parquet"
}

func readAttemptRows(path, format string) ([]parquetAttempt, error) {
	if format == FormatNDJSON {
		return readNDJSONFile[parquetAttempt](path)
	}
	return parquet.ReadFile[parquetAttempt](path)
}

// writeNDJSONAtomic follows the exact durability sequence writeParquetAtomic
// already uses — temp file in the target directory, fsync, atomic rename,
// directory fsync. A partition's durability story does not depend on what
// container is inside it.
func writeNDJSONAtomic[T any](path string, rows []T) (err error) {
	temp, err := os.CreateTemp(filepath.Dir(path), ".ndjson-*.tmp")
	if err != nil {
		return fmt.Errorf("create usage ndjson temp file: %w", err)
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
	for _, row := range rows {
		if err = encoder.Encode(row); err != nil {
			return fmt.Errorf("write usage ndjson: %w", err)
		}
	}
	if err = temp.Sync(); err != nil {
		return fmt.Errorf("sync usage ndjson: %w", err)
	}
	if err = temp.Close(); err != nil {
		return fmt.Errorf("close usage ndjson: %w", err)
	}
	if err = os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("publish usage ndjson: %w", err)
	}
	return durable.SyncDirectory(filepath.Dir(path))
}

func readNDJSONFile[T any](path string) ([]T, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var rows []T
	decoder := json.NewDecoder(file)
	for decoder.More() {
		var row T
		if err := decoder.Decode(&row); err != nil {
			return nil, fmt.Errorf("decode usage ndjson row: %w", err)
		}
		rows = append(rows, row)
	}
	return rows, nil
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
	// Explicit rather than the library's default, and zstd rather than snappy:
	// these columns are identifiers, enumerations and timestamps, which
	// dictionary-encode well and then compress well again, and a partition is
	// written once and read rarely. Existing partitions are never rewritten, so
	// this only changes what is written from here on — a reader handles both,
	// because the codec is recorded per column chunk in the file itself.
	writer := parquet.NewGenericWriter[parquetAttempt](temp, parquet.Compression(&zstd.Codec{}))
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
	return durable.SyncDirectory(filepath.Dir(path))
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
	return durable.SyncDirectory(e.root)
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

// narrowToSchema restates a freshly derived row as the given schema version
// would have written it, so it can be compared against a partition that is
// older than the current schema.
//
// Partitions are frozen once published. Reconciliation re-derives what a row
// should contain from the Ledger, which necessarily produces the current
// schema, so every column added after a partition was written would read as a
// content mismatch — the row on disk is not wrong, it is just older. Only
// columns the row's own schema could express take part in the comparison.
func narrowToSchema(row parquetAttempt, version int32) parquetAttempt {
	row.SchemaVersion = version
	if version < 6 {
		row.WorkUnitID = ""
		row.RunID = ""
	}
	if version < 5 {
		// Schema 5 introduced the upstream's own failure identifiers.
		row.ProviderCode = ""
		row.ProviderRequestID = ""
		row.FailurePhase = ""
	}
	if version < 4 {
		// Schema 4 introduced the provider token tiers.
		row.ProviderCachedInputTokens = 0
		row.ProviderCacheWriteInputTokens = 0
		row.ProviderReasoningTokens = 0
	}
	return row
}

func verifyRows(rows []parquetAttempt, entry ManifestFile, seen map[string]struct{}, canonical map[string]parquetAttempt) error {
	if int64(len(rows)) != entry.Records || len(rows) == 0 {
		return errors.New("record count mismatch")
	}
	var inputTokens, outputTokens, cost int64
	for _, row := range rows {
		// The readable range, not the current version. Partitions are never
		// rewritten, so a bump leaves every row already on disk at the older
		// version; demanding the current one here fails verification for
		// exactly the installs that have history. This is the same
		// exact-equality trap the manifest gate above documents, one level
		// further down.
		if !supportedManifestSchema(int(row.SchemaVersion)) || row.Sequence <= 0 || row.EventID == "" {
			return errors.New("invalid row")
		}
		if _, exists := seen[row.EventID]; exists {
			return fmt.Errorf("duplicate event ID %s", row.EventID)
		}
		seen[row.EventID] = struct{}{}
		canonical[row.EventID] = row
		if err := addInt64(&inputTokens, row.ProviderInputTokens); err != nil {
			return err
		}
		if err := addInt64(&outputTokens, row.ProviderOutputTokens); err != nil {
			return err
		}
		if err := addInt64(&cost, row.CostMicrosUSD); err != nil {
			return err
		}
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
	cost, costKnown := attempt.KnownCostMicrosUSD()
	inputCost, outputCost, fixedCost := int64(0), int64(0), int64(0)
	if attempt.InputCostMicrosUSD != nil {
		inputCost = *attempt.InputCostMicrosUSD
	}
	if attempt.OutputCostMicrosUSD != nil {
		outputCost = *attempt.OutputCostMicrosUSD
	}
	if attempt.FixedCostMicrosUSD != nil {
		fixedCost = *attempt.FixedCostMicrosUSD
	}
	var snapshotJSON string
	if attempt.PriceSnapshot != nil {
		if encoded, err := json.Marshal(attempt.PriceSnapshot); err == nil {
			snapshotJSON = string(encoded)
		}
	}
	return parquetAttempt{
		SchemaVersion: parquetSchemaVersion, EventID: attempt.EventID,
		RequestID: attempt.RequestID, AttemptID: attempt.AttemptID,
		Sequence: int64(attempt.Sequence), AttemptNumber: int32(attempt.AttemptNumber),
		ProjectID: attempt.ProjectID, KeyID: attempt.KeyID, RouteID: attempt.RouteID,
		WorkUnitID: attempt.WorkUnitID, RunID: attempt.RunID,
		DeploymentID: attempt.DeploymentID,
		ProviderID:   attempt.ProviderID, RequestedModel: attempt.RequestedModel,
		ProviderModel: attempt.ProviderModel, ProviderInputTokens: attempt.ProviderInputTokens,
		ProviderOutputTokens:          attempt.ProviderOutputTokens,
		ProviderCachedInputTokens:     attempt.ProviderCachedInputTokens,
		ProviderCacheWriteInputTokens: attempt.ProviderCacheWriteInputTokens,
		ProviderReasoningTokens:       attempt.ProviderReasoningTokens,
		PreparedOutputTokens:          attempt.PreparedOutputTokens,
		CostMicrosUSD:                 cost, CostKnown: costKnown,
		PriceEvidenceStatus: string(attempt.PriceEvidenceStatus), CostValueStatus: string(attempt.CostValueStatus),
		BillingMode: func() string {
			if attempt.PriceSnapshot != nil {
				return string(attempt.PriceSnapshot.BillingMode)
			}
			return ""
		}(),
		PriceSnapshotJSON: snapshotJSON, InputCostMicrosUSD: inputCost, OutputCostMicrosUSD: outputCost, FixedCostMicrosUSD: fixedCost,
		TokenUsageSource: string(attempt.TokenUsageSource), CostEstimated: attempt.CostEstimated,
		TokensEstimated:   attempt.TokensEstimated,
		StartedAtMicros:   attempt.StartedAt.UTC().UnixMicro(),
		CompletedAtMicros: attempt.CompletedAt.UTC().UnixMicro(),
		Status:            attempt.Status, ErrorClass: attempt.ErrorClass,
		HTTPStatus:        int32(attempt.HTTPStatus),
		ProviderCode:      attempt.ProviderCode,
		ProviderRequestID: attempt.ProviderRequestID,
		FailurePhase:      attempt.FailurePhase,
		LatencyMillis:     attempt.LatencyMillis,
		RetryCount:        int32(attempt.RetryCount), FallbackCount: int32(attempt.FallbackCount),
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

func isDirectoryNotEmpty(err error) bool {
	return errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST)
}
