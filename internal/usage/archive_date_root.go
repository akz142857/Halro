package usage

import (
	"bytes"
	"encoding/json"
	"io"
	"time"
)

func decodeArchiveDateRetention(raw []byte) (*ArchiveDateRetention, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return nil, ErrDiskUsageCorrupt
	}
	var r ArchiveDateRetention
	var fields uint8
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return nil, ErrDiskUsageCorrupt
		}
		name, ok := token.(string)
		if !ok {
			return nil, ErrDiskUsageCorrupt
		}
		var target any
		var bit uint8
		switch name {
		case "version":
			target, bit = &r.Version, 1
		case "retain_from_date":
			target, bit = &r.RetainFromDate, 2
		case "prune_through_sequence":
			target, bit = &r.PruneThroughSequence, 4
		default:
			return nil, ErrDiskUsageCorrupt
		}
		var value json.RawMessage
		if fields&bit != 0 || d.Decode(&value) != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) || json.Unmarshal(value, target) != nil {
			return nil, ErrDiskUsageCorrupt
		}
		fields |= bit
	}
	if token, err := d.Token(); err != nil || token != json.Delim('}') || fields != 7 {
		return nil, ErrDiskUsageCorrupt
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, ErrDiskUsageCorrupt
	}
	return &r, nil
}

const archiveDateCatalogSchema = 10
const archiveFilesCursorVersion = 2

// These derived fields describe coverage, never authorize it. PruneThroughSequence
// fixes the pruning baseline: subsequently appended late old dates have higher
// sequences and remain listed until the next independently authorized sweep.
type ArchiveDateRetention struct {
	Version              int    `json:"version"`
	RetainFromDate       string `json:"retain_from_date"`
	PruneThroughSequence uint64 `json:"prune_through_sequence"`
}

func (r ArchiveDateRetention) valid(h uint64) bool {
	d, err := time.Parse("2006-01-02", r.RetainFromDate)
	return r.Version == 1 && err == nil && !d.IsZero() && d.Year() >= 1 && d.Format("2006-01-02") == r.RetainFromDate && r.PruneThroughSequence <= h
}

func validateArchiveEntryCoverage(entry ManifestFile, h uint64, retention ArchiveDateRetention) error {
	if entry.MaxSequence > h {
		return ErrDiskUsageCorrupt
	}
	if retention != (ArchiveDateRetention{}) {
		if !retention.valid(h) || validateArchivePartitionDate(entry) != nil || entry.Date < retention.RetainFromDate && entry.MinSequence <= retention.PruneThroughSequence {
			return ErrDiskUsageCorrupt
		}
	}
	return nil
}

// Used by the publisher and candidate builder so the emitted bytes and receipt
// agree exactly. Schema 10 cannot be understood by the prior schema 9 parser.
func archiveCatalogRootBytes(h uint64, complete bool, head ArchivePageRef, retention ArchiveDateRetention) ([]byte, ArchivePosition, error) {
	schema := archiveCatalogSchema
	var scope *ArchiveDateRetention
	if retention != (ArchiveDateRetention{}) {
		if !retention.valid(h) {
			return nil, ArchivePosition{}, ErrDiskUsageCorrupt
		}
		schema, complete, scope = archiveDateCatalogSchema, false, &retention
	}
	if !head.valid() {
		return nil, ArchivePosition{}, ErrDiskUsageCorrupt
	}
	root := struct {
		SchemaVersion  int                   `json:"schema_version"`
		LastSequence   uint64                `json:"last_sequence"`
		CompletePrefix bool                  `json:"complete_prefix"`
		Files          []ManifestFile        `json:"files"`
		Catalog        archiveCatalogRoot    `json:"catalog"`
		Retention      *ArchiveDateRetention `json:"retention,omitempty"`
	}{SchemaVersion: schema, LastSequence: h, CompletePrefix: complete, Catalog: archiveCatalogRoot{Version: 1, Head: head}, Retention: scope}
	raw, err := json.Marshal(root)
	position := ArchivePosition{SchemaVersion: schema, LastSequence: h, CompletePrefix: complete, Files: head.Files, CatalogVersion: 1, CatalogHead: head, Retention: retention}
	return raw, position, err
}
