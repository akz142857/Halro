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

const parquetSchemaVersion = 3
const adjustmentParquetSchemaVersion = 2

// Export format (ADR 0017): what new partitions are written as. Existing
// partitions are never rewritten to a different format.
const (
	FormatParquet = "parquet"
	FormatNDJSON  = "ndjson"
)

type parquetAdjustment struct {
	SchemaVersion               int32  `parquet:"schema_version" json:"schema_version"`
	EventID                     string `parquet:"event_id,dict" json:"event_id"`
	Sequence                    int64  `parquet:"sequence,delta" json:"sequence"`
	RequestID                   string `parquet:"request_id,dict" json:"request_id"`
	AttemptID                   string `parquet:"attempt_id,dict" json:"attempt_id"`
	ProjectID                   string `parquet:"project_id,dict" json:"project_id"`
	DeploymentID                string `parquet:"deployment_id,dict" json:"deployment_id"`
	ProviderID                  string `parquet:"provider_id,dict" json:"provider_id"`
	Mode                        string `parquet:"mode,dict" json:"mode"`
	AdjustmentSequence          int64  `parquet:"adjustment_sequence" json:"adjustment_sequence"`
	IdempotencyKeyDigest        string `parquet:"idempotency_key_digest" json:"idempotency_key_digest"`
	BaseCostMicrosUSD           int64  `parquet:"base_cost_micros_usd" json:"base_cost_micros_usd"`
	BaseCostKnown               bool   `parquet:"base_cost_known" json:"base_cost_known"`
	NetCostBeforeMicrosUSD      int64  `parquet:"net_cost_before_micros_usd" json:"net_cost_before_micros_usd"`
	DeltaMicrosUSD              int64  `parquet:"delta_micros_usd" json:"delta_micros_usd"`
	NetCostAfterMicrosUSD       int64  `parquet:"net_cost_after_micros_usd" json:"net_cost_after_micros_usd"`
	ServicePeriodID             string `parquet:"service_period_id,dict" json:"service_period_id"`
	OriginalCompletedAtMicros   int64  `parquet:"original_completed_at_utc,timestamp(microsecond)" json:"original_completed_at_utc"`
	PostedPeriodID              string `parquet:"posted_period_id,dict" json:"posted_period_id"`
	PostedAtMicros              int64  `parquet:"posted_at_utc,timestamp(microsecond)" json:"posted_at_utc"`
	CorrectionPriceSnapshotJSON string `parquet:"correction_price_snapshot_json" json:"correction_price_snapshot_json"`
	ReasonCode                  string `parquet:"reason_code,dict" json:"reason_code"`
	EvidenceDigest              string `parquet:"evidence_digest" json:"evidence_digest"`
	CreatedBy                   string `parquet:"created_by,dict" json:"created_by"`
	Reason                      string `parquet:"reason" json:"reason"`
	OriginalSettlementEventID   string `parquet:"original_settlement_event_id" json:"original_settlement_event_id"`
	OriginalSettlementDigest    string `parquet:"original_settlement_digest" json:"original_settlement_digest"`
	AdjustmentRequestDigest     string `parquet:"adjustment_request_digest" json:"adjustment_request_digest"`
}

type parquetAttempt struct {
	SchemaVersion        int32  `parquet:"schema_version" json:"schema_version"`
	EventID              string `parquet:"event_id,dict" json:"event_id"`
	RequestID            string `parquet:"request_id,dict" json:"request_id"`
	AttemptID            string `parquet:"attempt_id,dict" json:"attempt_id"`
	Sequence             int64  `parquet:"sequence,delta" json:"sequence"`
	AttemptNumber        int32  `parquet:"attempt_number" json:"attempt_number"`
	ProjectID            string `parquet:"project_id,dict" json:"project_id"`
	KeyID                string `parquet:"key_id,dict" json:"key_id"`
	RouteID              string `parquet:"route_id,dict" json:"route_id"`
	DeploymentID         string `parquet:"deployment_id,dict" json:"deployment_id"`
	ProviderID           string `parquet:"provider_id,dict" json:"provider_id"`
	RequestedModel       string `parquet:"requested_model,dict" json:"requested_model"`
	ProviderModel        string `parquet:"provider_model,dict" json:"provider_model"`
	ProviderInputTokens  int64  `parquet:"provider_input_tokens" json:"provider_input_tokens"`
	ProviderOutputTokens int64  `parquet:"provider_output_tokens" json:"provider_output_tokens"`
	PreparedOutputTokens int64  `parquet:"prepared_output_tokens" json:"prepared_output_tokens"`
	CostMicrosUSD        int64  `parquet:"cost_micros_usd" json:"cost_micros_usd"`
	CostKnown            bool   `parquet:"cost_known" json:"cost_known"`
	PriceEvidenceStatus  string `parquet:"price_evidence_status,dict" json:"price_evidence_status"`
	CostValueStatus      string `parquet:"cost_value_status,dict" json:"cost_value_status"`
	BillingMode          string `parquet:"billing_mode,dict" json:"billing_mode"`
	PriceSnapshotJSON    string `parquet:"price_snapshot_json" json:"price_snapshot_json"`
	InputCostMicrosUSD   int64  `parquet:"input_cost_micros_usd" json:"input_cost_micros_usd"`
	OutputCostMicrosUSD  int64  `parquet:"output_cost_micros_usd" json:"output_cost_micros_usd"`
	FixedCostMicrosUSD   int64  `parquet:"fixed_cost_micros_usd" json:"fixed_cost_micros_usd"`
	TokenUsageSource     string `parquet:"token_usage_source,dict" json:"token_usage_source"`
	CostEstimated        bool   `parquet:"cost_estimated" json:"cost_estimated"`
	TokensEstimated      bool   `parquet:"tokens_estimated" json:"tokens_estimated"`
	StartedAtMicros      int64  `parquet:"started_at_utc,timestamp(microsecond)" json:"started_at_utc"`
	CompletedAtMicros    int64  `parquet:"completed_at_utc,timestamp(microsecond)" json:"completed_at_utc"`
	Status               string `parquet:"status,dict" json:"status"`
	ErrorClass           string `parquet:"error_class,dict" json:"error_class"`
	HTTPStatus           int32  `parquet:"http_status" json:"http_status"`
	LatencyMillis        int64  `parquet:"latency_millis" json:"latency_millis"`
	RetryCount           int32  `parquet:"retry_count" json:"retry_count"`
	FallbackCount        int32  `parquet:"fallback_count" json:"fallback_count"`
}

type parquetAttemptV2 struct {
	SchemaVersion        int32  `parquet:"schema_version" json:"schema_version"`
	EventID              string `parquet:"event_id,dict" json:"event_id"`
	RequestID            string `parquet:"request_id,dict" json:"request_id"`
	AttemptID            string `parquet:"attempt_id,dict" json:"attempt_id"`
	Sequence             int64  `parquet:"sequence,delta" json:"sequence"`
	AttemptNumber        int32  `parquet:"attempt_number" json:"attempt_number"`
	ProjectID            string `parquet:"project_id,dict" json:"project_id"`
	KeyID                string `parquet:"key_id,dict" json:"key_id"`
	RouteID              string `parquet:"route_id,dict" json:"route_id"`
	DeploymentID         string `parquet:"deployment_id,dict" json:"deployment_id"`
	ProviderID           string `parquet:"provider_id,dict" json:"provider_id"`
	RequestedModel       string `parquet:"requested_model,dict" json:"requested_model"`
	ProviderModel        string `parquet:"provider_model,dict" json:"provider_model"`
	ProviderInputTokens  int64  `parquet:"provider_input_tokens" json:"provider_input_tokens"`
	ProviderOutputTokens int64  `parquet:"provider_output_tokens" json:"provider_output_tokens"`
	PreparedOutputTokens int64  `parquet:"prepared_output_tokens" json:"prepared_output_tokens"`
	CostMicrosUSD        int64  `parquet:"cost_micros_usd" json:"cost_micros_usd"`
	CostEstimated        bool   `parquet:"cost_estimated" json:"cost_estimated"`
	TokensEstimated      bool   `parquet:"tokens_estimated" json:"tokens_estimated"`
	StartedAtMicros      int64  `parquet:"started_at_utc,timestamp(microsecond)" json:"started_at_utc"`
	CompletedAtMicros    int64  `parquet:"completed_at_utc,timestamp(microsecond)" json:"completed_at_utc"`
	Status               string `parquet:"status,dict" json:"status"`
	ErrorClass           string `parquet:"error_class,dict" json:"error_class"`
	HTTPStatus           int32  `parquet:"http_status" json:"http_status"`
	LatencyMillis        int64  `parquet:"latency_millis" json:"latency_millis"`
	RetryCount           int32  `parquet:"retry_count" json:"retry_count"`
	FallbackCount        int32  `parquet:"fallback_count" json:"fallback_count"`
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

type AdjustmentManifest struct {
	SchemaVersion int                      `json:"schema_version"`
	LastSequence  uint64                   `json:"last_sequence"`
	Files         []AdjustmentManifestFile `json:"files"`
}

type AdjustmentManifestFile struct {
	Path           string `json:"path"`
	Date           string `json:"date"`
	SHA256         string `json:"sha256"`
	MinSequence    uint64 `json:"min_sequence"`
	MaxSequence    uint64 `json:"max_sequence"`
	Records        int64  `json:"records"`
	DeltaMicrosUSD int64  `json:"delta_micros_usd"`
	// Format mirrors ManifestFile.Format; see FormatParquet/FormatNDJSON.
	Format string `json:"format,omitempty"`
}

func (f AdjustmentManifestFile) format() string {
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
	if manifest.SchemaVersion != 2 && manifest.SchemaVersion != parquetSchemaVersion {
		return Manifest{}, fmt.Errorf("usage manifest schema version %d is not supported", manifest.SchemaVersion)
	}
	manifestUpgraded := manifest.SchemaVersion == 2
	if manifestUpgraded {
		for index := range manifest.Files {
			manifest.Files[index].SchemaVersion = 2
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
		if err := e.exportAdjustments(snapshot.Adjustments); err != nil {
			return Manifest{}, err
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
	if err := e.exportAdjustments(snapshot.Adjustments); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func (e *Exporter) exportAdjustments(adjustments []CostAdjustmentEvent) error {
	manifest, err := e.LoadAdjustmentManifest()
	missing := errors.Is(err, os.ErrNotExist)
	if err != nil && !missing {
		return err
	}
	if missing {
		manifest = AdjustmentManifest{SchemaVersion: adjustmentParquetSchemaVersion}
	}
	if manifest.SchemaVersion != adjustmentParquetSchemaVersion {
		return fmt.Errorf("adjustment manifest schema version %d is not supported", manifest.SchemaVersion)
	}
	var pending []CostAdjustmentEvent
	for _, adjustment := range adjustments {
		if adjustment.Sequence > manifest.LastSequence {
			pending = append(pending, adjustment)
		}
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].Sequence < pending[j].Sequence })
	if len(pending) == 0 {
		if missing {
			return e.commitAdjustmentManifest(manifest)
		}
		return nil
	}
	byDate := make(map[string][]CostAdjustmentEvent)
	var dates []string
	for _, adjustment := range pending {
		date := adjustment.PostedAt.UTC().Format("2006-01-02")
		if _, ok := byDate[date]; !ok {
			dates = append(dates, date)
		}
		byDate[date] = append(byDate[date], adjustment)
	}
	sort.Strings(dates)
	for _, date := range dates {
		entry, err := e.publishAdjustmentPartition(date, byDate[date])
		if err != nil {
			return err
		}
		manifest.Files = append(manifest.Files, entry)
		if entry.MaxSequence > manifest.LastSequence {
			manifest.LastSequence = entry.MaxSequence
		}
	}
	sort.Slice(manifest.Files, func(i, j int) bool { return manifest.Files[i].MinSequence < manifest.Files[j].MinSequence })
	return e.commitAdjustmentManifest(manifest)
}

func (e *Exporter) LoadAdjustmentManifest() (AdjustmentManifest, error) {
	payload, err := os.ReadFile(filepath.Join(e.root, "cost_adjustments", "manifest.json"))
	if err != nil {
		return AdjustmentManifest{}, err
	}
	var manifest AdjustmentManifest
	if err := json.Unmarshal(payload, &manifest); err != nil {
		return AdjustmentManifest{}, fmt.Errorf("decode adjustment manifest: %w", err)
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
	if manifest.SchemaVersion != 2 && manifest.SchemaVersion != parquetSchemaVersion {
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
		entryVersion := entry.SchemaVersion
		if entryVersion == 0 {
			entryVersion = manifest.SchemaVersion
		}
		if entryVersion == 2 {
			var rows []parquetAttemptV2
			var readErr error
			if entry.format() == FormatNDJSON {
				rows, readErr = readNDJSONFile[parquetAttemptV2](path)
			} else {
				rows, readErr = parquet.ReadFile[parquetAttemptV2](path)
			}
			if readErr != nil {
				return fmt.Errorf("read legacy usage partition %s: %w", entry.Path, readErr)
			}
			if err := verifyRowsV2(rows, entry, seen); err != nil {
				return fmt.Errorf("verify legacy usage partition %s: %w", entry.Path, err)
			}
		} else {
			rows, readErr := readAttemptRows(path, entry.format())
			if readErr != nil {
				return fmt.Errorf("read usage partition %s: %w", entry.Path, readErr)
			}
			if err := verifyRows(rows, entry, seen, canonicalRows); err != nil {
				return fmt.Errorf("verify usage partition %s: %w", entry.Path, err)
			}
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
			if row, exists := canonicalRows[eventID]; exists && row != toParquetAttempt(expected[eventID]) {
				return fmt.Errorf("usage reconciliation content mismatch for event %s", eventID)
			}
		}
	}
	return e.verifyAdjustments(snapshot)
}

func (e *Exporter) verifyAdjustments(snapshot *Snapshot) error {
	manifest, err := e.LoadAdjustmentManifest()
	if errors.Is(err, os.ErrNotExist) && (snapshot == nil || len(snapshot.Adjustments) == 0) {
		return nil
	}
	if err != nil {
		return err
	}
	if manifest.SchemaVersion != adjustmentParquetSchemaVersion {
		return errors.New("unsupported adjustment manifest schema")
	}
	seen := make(map[string]parquetAdjustment)
	for _, entry := range manifest.Files {
		path, err := e.safeManifestPath(entry.Path)
		if err != nil {
			return err
		}
		checksum, err := fileSHA256(path)
		if err != nil || checksum != entry.SHA256 {
			return fmt.Errorf("adjustment partition checksum mismatch: %s", entry.Path)
		}
		rows, err := readAdjustmentRows(path, entry.format())
		if err != nil {
			return err
		}
		if int64(len(rows)) != entry.Records || len(rows) == 0 {
			return errors.New("adjustment record count mismatch")
		}
		var delta int64
		for _, row := range rows {
			if row.SchemaVersion != adjustmentParquetSchemaVersion || row.EventID == "" || row.Sequence <= 0 {
				return errors.New("invalid adjustment row")
			}
			if _, ok := seen[row.EventID]; ok {
				return errors.New("duplicate adjustment event")
			}
			seen[row.EventID] = row
			if err := addInt64(&delta, row.DeltaMicrosUSD); err != nil {
				return err
			}
		}
		if delta != entry.DeltaMicrosUSD || uint64(rows[0].Sequence) != entry.MinSequence || uint64(rows[len(rows)-1].Sequence) != entry.MaxSequence {
			return errors.New("adjustment partition summary mismatch")
		}
	}
	if snapshot != nil {
		expected := 0
		for _, item := range snapshot.Adjustments {
			if item.Sequence <= manifest.LastSequence {
				expected++
				row, ok := seen[item.EventID]
				if !ok {
					return fmt.Errorf("missing adjustment event %s", item.EventID)
				}
				if row != toParquetAdjustment(item) {
					return fmt.Errorf("adjustment content mismatch for event %s", item.EventID)
				}
			}
		}
		if expected != len(seen) {
			return errors.New("adjustment reconciliation mismatch")
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
	if manifest.SchemaVersion != 2 && manifest.SchemaVersion != parquetSchemaVersion {
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
		if cost, ok := originalAttemptCost(attempt); ok {
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

func (e *Exporter) publishAdjustmentPartition(date string, adjustments []CostAdjustmentEvent) (AdjustmentManifestFile, error) {
	if len(adjustments) == 0 {
		return AdjustmentManifestFile{}, errors.New("cannot publish empty adjustment partition")
	}
	rows := make([]parquetAdjustment, len(adjustments))
	entry := AdjustmentManifestFile{
		Date: date, MinSequence: adjustments[0].Sequence, MaxSequence: adjustments[len(adjustments)-1].Sequence,
		Records: int64(len(adjustments)), Format: e.format,
	}
	for index, item := range adjustments {
		rows[index] = toParquetAdjustment(item)
		if err := addInt64(&entry.DeltaMicrosUSD, item.DeltaMicrosUSD); err != nil {
			return AdjustmentManifestFile{}, err
		}
	}
	relative := filepath.Join("cost_adjustments", "date="+date, fmt.Sprintf("adjustments-%020d-%020d.%s", entry.MinSequence, entry.MaxSequence, partitionExtension(e.format)))
	entry.Path = filepath.ToSlash(relative)
	path := filepath.Join(e.root, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return AdjustmentManifestFile{}, err
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if e.format == FormatNDJSON {
			if err := writeNDJSONAtomic(path, rows); err != nil {
				return AdjustmentManifestFile{}, err
			}
		} else if err := writeAdjustmentParquetAtomic(path, rows); err != nil {
			return AdjustmentManifestFile{}, err
		}
	} else if err != nil {
		return AdjustmentManifestFile{}, err
	} else {
		existing, err := readAdjustmentRows(path, e.format)
		if err != nil {
			return AdjustmentManifestFile{}, fmt.Errorf("read orphan adjustment partition: %w", err)
		}
		if !sameAdjustmentRows(existing, rows) {
			return AdjustmentManifestFile{}, fmt.Errorf("existing adjustment partition conflicts with export: %s", relative)
		}
	}
	checksum, err := fileSHA256(path)
	if err != nil {
		return AdjustmentManifestFile{}, err
	}
	entry.SHA256 = checksum
	return entry, nil
}

func readAdjustmentRows(path, format string) ([]parquetAdjustment, error) {
	if format == FormatNDJSON {
		return readNDJSONFile[parquetAdjustment](path)
	}
	return parquet.ReadFile[parquetAdjustment](path)
}

func toParquetAdjustment(item CostAdjustmentEvent) parquetAdjustment {
	var snapshotJSON string
	if item.CorrectionPriceSnapshot != nil {
		if encoded, err := json.Marshal(item.CorrectionPriceSnapshot); err == nil {
			snapshotJSON = string(encoded)
		}
	}
	return parquetAdjustment{SchemaVersion: adjustmentParquetSchemaVersion, EventID: item.EventID, Sequence: int64(item.Sequence), RequestID: item.RequestID, AttemptID: item.AttemptID, ProjectID: item.ProjectID,
		DeploymentID: item.DeploymentID, ProviderID: item.ProviderID, Mode: string(item.Mode), AdjustmentSequence: int64(item.AdjustmentSequence), IdempotencyKeyDigest: item.IdempotencyKeyDigest,
		BaseCostMicrosUSD: item.BaseCostMicrosUSD, BaseCostKnown: item.BaseCostKnown, NetCostBeforeMicrosUSD: item.NetCostBeforeMicrosUSD, DeltaMicrosUSD: item.DeltaMicrosUSD, NetCostAfterMicrosUSD: item.NetCostAfterMicrosUSD,
		ServicePeriodID: item.ServicePeriodID, OriginalCompletedAtMicros: item.OriginalCompletedAt.UTC().UnixMicro(), PostedPeriodID: item.PostedPeriodID, PostedAtMicros: item.PostedAt.UTC().UnixMicro(),
		CorrectionPriceSnapshotJSON: snapshotJSON, ReasonCode: item.ReasonCode, EvidenceDigest: item.EvidenceDigest, CreatedBy: item.CreatedBy,
		Reason: item.Reason, OriginalSettlementEventID: item.OriginalSettlementEventID, OriginalSettlementDigest: item.OriginalSettlementDigest, AdjustmentRequestDigest: item.AdjustmentRequestDigest}
}

func writeAdjustmentParquetAtomic(path string, rows []parquetAdjustment) (err error) {
	temp, err := os.CreateTemp(filepath.Dir(path), ".adjustments-*.parquet.tmp")
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
	writer := parquet.NewGenericWriter[parquetAdjustment](temp)
	if _, err = writer.Write(rows); err != nil {
		_ = writer.Close()
		return err
	}
	if err = writer.Close(); err != nil {
		return err
	}
	if err = temp.Sync(); err != nil {
		return err
	}
	if err = temp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tempPath, path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func (e *Exporter) commitAdjustmentManifest(manifest AdjustmentManifest) (err error) {
	root := filepath.Join(e.root, "cost_adjustments")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(root, ".manifest-*.json.tmp")
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
	if err = os.Rename(tempPath, filepath.Join(root, "manifest.json")); err != nil {
		return err
	}
	return syncDirectory(root)
}

// writeNDJSONAtomic follows the exact durability sequence writeParquetAtomic
// and writeAdjustmentParquetAtomic already use — temp file in the target
// directory, fsync, atomic rename, directory fsync. A partition's durability
// story does not depend on what container is inside it.
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
	return syncDirectory(filepath.Dir(path))
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

func verifyRows(rows []parquetAttempt, entry ManifestFile, seen map[string]struct{}, canonical map[string]parquetAttempt) error {
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

func verifyRowsV2(rows []parquetAttemptV2, entry ManifestFile, seen map[string]struct{}) error {
	if int64(len(rows)) != entry.Records || len(rows) == 0 {
		return errors.New("record count mismatch")
	}
	var inputTokens, outputTokens, cost int64
	for _, row := range rows {
		if row.SchemaVersion != 2 || row.Sequence <= 0 || row.EventID == "" {
			return errors.New("invalid legacy row")
		}
		if _, exists := seen[row.EventID]; exists {
			return fmt.Errorf("duplicate event ID %s", row.EventID)
		}
		seen[row.EventID] = struct{}{}
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
	if uint64(rows[0].Sequence) != entry.MinSequence || uint64(rows[len(rows)-1].Sequence) != entry.MaxSequence || inputTokens != entry.InputTokens || outputTokens != entry.OutputTokens || cost != entry.CostMicrosUSD {
		return errors.New("partition summary mismatch")
	}
	return nil
}

func toParquetAttempt(attempt AttemptEvent) parquetAttempt {
	cost, costKnown := originalAttemptCost(attempt)
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
		DeploymentID: attempt.DeploymentID,
		ProviderID:   attempt.ProviderID, RequestedModel: attempt.RequestedModel,
		ProviderModel: attempt.ProviderModel, ProviderInputTokens: attempt.ProviderInputTokens,
		ProviderOutputTokens: attempt.ProviderOutputTokens,
		PreparedOutputTokens: attempt.PreparedOutputTokens,
		CostMicrosUSD:        cost, CostKnown: costKnown,
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
		HTTPStatus: int32(attempt.HTTPStatus), LatencyMillis: attempt.LatencyMillis,
		RetryCount: int32(attempt.RetryCount), FallbackCount: int32(attempt.FallbackCount),
	}
}

func originalAttemptCost(attempt AttemptEvent) (int64, bool) {
	if attempt.OriginalCostMicrosUSD != nil {
		return *attempt.OriginalCostMicrosUSD, true
	}
	if attempt.CostMicrosUSD != nil {
		return *attempt.CostMicrosUSD, true
	}
	return 0, false
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

func sameAdjustmentRows(left, right []parquetAdjustment) bool {
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
