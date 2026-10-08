package backup

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"strings"
	"time"
)

const pagedManifestVersion = 5
const pagedMetadataLimit = 64 << 10
const catalogPageRows = 64
const catalogPrefix = "__halro_backup_catalog__/"
const catalogDigestDomain = "halro:backup-file-catalog:v1\x00"

type FileCatalogSummary struct {
	Version int    `json:"version"`
	Pages   uint64 `json:"pages"`
	Files   uint64 `json:"files"`
	SHA256  string `json:"sha256"`
}

// PagedManifest contains only a bounded root. Files must be nil. The catalog
// is consumed through VerifiedPagedArchive, never a successful empty legacy list.
type ArchiveSummary struct {
	legacyManifestDigest string
	contentBytes         int64
	Manifest
	CompatibilityVersion int                `json:"compatibility_version"`
	Catalog              FileCatalogSummary `json:"file_catalog"`
}

// PagedManifest is the format-5 root spelling. Unified readers also return
// ArchiveSummary for legacy formats, with Version=0 catalog inventory counts.
type PagedManifest = ArchiveSummary

type catalogPage struct {
	Version int    `json:"version"`
	Number  uint64 `json:"number"`
	Files   []File `json:"files"`
}

func isCatalogPath(name string) bool       { return strings.HasPrefix(name, catalogPrefix) }
func catalogPageName(number uint64) string { return fmt.Sprintf("%s%020d.json", catalogPrefix, number) }
func newCatalogHash() hash.Hash {
	h := sha256.New()
	_, _ = h.Write([]byte(catalogDigestDomain))
	return h
}
func hashCatalogPage(h hash.Hash, number uint64, raw []byte) {
	var frame [16]byte
	binary.BigEndian.PutUint64(frame[:8], number)
	binary.BigEndian.PutUint64(frame[8:], uint64(len(raw)))
	_, _ = h.Write(frame[:])
	_, _ = h.Write(raw)
}
func validatePagedManifest(root PagedManifest) error {
	if root.FormatVersion != pagedManifestVersion || root.CompatibilityVersion < 2 || root.CompatibilityVersion > 4 || root.Files != nil || root.Catalog.Version != 1 || !validSHA256(root.Catalog.SHA256) {
		return errors.New("paged backup root is invalid")
	}
	legacy := root.Manifest
	legacy.FormatVersion = root.CompatibilityVersion
	if err := validateManifestHeader(legacy); err != nil {
		return err
	}
	if root.CompatibilityVersion != 4 && (root.SourceRole != "standalone" || root.ClusterID != "" || root.ClusterIncarnation != "" || root.SourceNodeID != "" || root.Term != 0 || root.AppliedIndex != 0 || root.MetadataJournalEpoch != 0 || root.MetadataJournalSequence != 0 || len(root.PerStoreHead) != 0) {
		return errors.New("paged Standalone backup contains cluster prefix fields")
	}
	return nil
}

type catalogWriter struct {
	archive      *tar.Writer
	createdAt    time.Time
	digest       hash.Hash
	files        []File
	bytes        int
	pages, count uint64
	last         string
}

func newCatalogWriter(archive *tar.Writer, createdAt time.Time) *catalogWriter {
	return &catalogWriter{archive: archive, createdAt: createdAt, digest: newCatalogHash(), files: make([]File, 0, catalogPageRows)}
}
func (w *catalogWriter) Add(file File) error {
	clean, err := safeArchivePath(file.Path)
	if err != nil || clean == "manifest.json" || isCatalogPath(clean) || file.Path <= w.last || file.Size < 0 || !validSHA256(file.SHA256) || w.count == math.MaxUint64 {
		return errors.New("paged backup file fact is invalid")
	}
	encoded, err := json.Marshal(file)
	if err != nil {
		return err
	}
	for {
		if len(w.files) == 0 {
			if w.pages == math.MaxUint64 {
				return errors.New("backup catalog page count overflow")
			}
			base, err := json.Marshal(catalogPage{Version: 1, Number: w.pages + 1, Files: []File{}})
			if err != nil {
				return err
			}
			w.bytes = len(base)
		}
		size := len(encoded)
		if len(w.files) > 0 {
			size++
		}
		if size > pagedMetadataLimit-w.bytes || len(w.files) == catalogPageRows {
			if len(w.files) == 0 {
				return errors.New("backup catalog file fact exceeds page limit")
			}
			if err := w.flush(); err != nil {
				return err
			}
			continue
		}
		w.bytes += size
		w.files = append(w.files, file)
		w.count++
		w.last = file.Path
		return nil
	}
}
func (w *catalogWriter) flush() error {
	if len(w.files) == 0 {
		return nil
	}
	number := w.pages + 1
	raw, err := json.Marshal(catalogPage{Version: 1, Number: number, Files: w.files})
	if err != nil {
		return err
	}
	if len(raw) > pagedMetadataLimit || len(raw) != w.bytes {
		return errors.New("backup catalog page exceeds size accounting")
	}
	if err := w.archive.WriteHeader(&tar.Header{Name: catalogPageName(number), Mode: 0600, Size: int64(len(raw)), ModTime: w.createdAt, Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	if _, err := w.archive.Write(raw); err != nil {
		return err
	}
	hashCatalogPage(w.digest, number, raw)
	w.pages++
	clear(w.files)
	w.files = w.files[:0]
	w.bytes = 0
	return nil
}
func (w *catalogWriter) Finish() (FileCatalogSummary, error) {
	if err := w.flush(); err != nil {
		return FileCatalogSummary{}, err
	}
	return FileCatalogSummary{Version: 1, Pages: w.pages, Files: w.count, SHA256: hex.EncodeToString(w.digest.Sum(nil))}, nil
}

// Decode enforces the row ceiling before allocating each next File. Duplicate
// page and file fields, unknown fields and trailing JSON are rejected.
func decodeCatalogPage(raw []byte) (catalogPage, error) {
	var page catalogPage
	if len(raw) > pagedMetadataLimit {
		return page, errors.New("backup catalog page exceeds limit")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return page, errors.New("invalid backup catalog page")
	}
	var seen uint8
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return page, err
		}
		field, ok := token.(string)
		if !ok {
			return page, errors.New("invalid catalog field")
		}
		var bit uint8
		switch field {
		case "version":
			bit = 1
		case "number":
			bit = 2
		case "files":
			bit = 4
		default:
			return page, errors.New("unknown catalog field")
		}
		if seen&bit != 0 {
			return page, errors.New("duplicate catalog field")
		}
		seen |= bit
		switch field {
		case "version":
			err = d.Decode(&page.Version)
		case "number":
			err = d.Decode(&page.Number)
		case "files":
			token, err = d.Token()
			if err != nil || token != json.Delim('[') {
				return page, errors.New("invalid catalog file array")
			}
			for d.More() {
				if len(page.Files) >= catalogPageRows {
					return page, errors.New("backup catalog page row limit exceeded")
				}
				file, readErr := decodeCatalogFile(d)
				if readErr != nil {
					return page, readErr
				}
				page.Files = append(page.Files, file)
			}
			token, err = d.Token()
			if err != nil || token != json.Delim(']') {
				return page, errors.New("invalid catalog file array ending")
			}
		}
		if err != nil {
			return page, err
		}
	}
	token, err = d.Token()
	if err != nil || token != json.Delim('}') || seen != 7 || page.Version != 1 || page.Number == 0 || len(page.Files) == 0 {
		return page, errors.New("invalid catalog page ending")
	}
	if token, err = d.Token(); !errors.Is(err, io.EOF) {
		return page, fmt.Errorf("trailing catalog JSON: %v %v", token, err)
	}
	return page, nil
}
func decodeCatalogFile(d *json.Decoder) (File, error) {
	var file File
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return file, errors.New("invalid catalog file")
	}
	var seen uint8
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return file, err
		}
		field, ok := token.(string)
		if !ok {
			return file, errors.New("invalid catalog file field")
		}
		var bit uint8
		switch field {
		case "path":
			bit = 1
		case "size":
			bit = 2
		case "sha256":
			bit = 4
		default:
			return file, errors.New("unknown catalog file field")
		}
		if seen&bit != 0 {
			return file, errors.New("duplicate catalog file field")
		}
		seen |= bit
		switch field {
		case "path":
			err = d.Decode(&file.Path)
		case "size":
			err = d.Decode(&file.Size)
		case "sha256":
			err = d.Decode(&file.SHA256)
		}
		if err != nil {
			return file, err
		}
	}
	token, err = d.Token()
	if err != nil || token != json.Delim('}') || seen != 7 {
		return file, errors.New("incomplete catalog file")
	}
	return file, nil
}

// The root has a fixed schema and <=64KiB payload. Preflight rejects ambiguous
// duplicate fields at every nesting level before typed decoding. The bounds
// also keep malformed JSON from growing a deep stack or a large field table.
func validatePagedRootJSON(raw []byte) error {
	if len(raw) > pagedMetadataLimit {
		return errors.New("paged backup root exceeds limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := scanPagedJSONValue(decoder, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing backup root JSON")
	}
	return nil
}
func scanPagedJSONValue(decoder *json.Decoder, depth int) error {
	if depth > 16 {
		return errors.New("backup root JSON nesting exceeds limit")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			token, err := decoder.Token()
			if err != nil {
				return err
			}
			field, ok := token.(string)
			if !ok {
				return errors.New("invalid root JSON field")
			}
			if _, exists := seen[field]; exists || len(seen) >= 128 {
				return errors.New("duplicate or excessive root JSON fields")
			}
			seen[field] = struct{}{}
			if err := scanPagedJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		token, err := decoder.Token()
		if err != nil || token != json.Delim('}') {
			return errors.New("invalid root JSON object ending")
		}
	case '[':
		count := 0
		for decoder.More() {
			if count >= 128 {
				return errors.New("root JSON array exceeds limit")
			}
			if err := scanPagedJSONValue(decoder, depth+1); err != nil {
				return err
			}
			count++
		}
		token, err := decoder.Token()
		if err != nil || token != json.Delim(']') {
			return errors.New("invalid root JSON array ending")
		}
	default:
		return errors.New("invalid root JSON delimiter")
	}
	return nil
}
