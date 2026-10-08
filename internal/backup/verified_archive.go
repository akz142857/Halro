package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"os"
)

// The envelope predates the paged format. A reserved-looking name cannot tell
// us whether it is control metadata or an ordinary legacy user file. Preserve
// every entry as a fact, evaluate bounded page candidates without granting them
// authority, then select interpretation only after the authenticated root.
type catalogCandidate struct {
	declared     *observationIndex
	digest       hash.Hash
	pages, count uint64
	last         string
	invalid      error
}

func (c *catalogCandidate) observe(ctx context.Context, name string, raw []byte, actual *observationIndex) {
	if c.invalid != nil {
		return
	}
	if c.pages == ^uint64(0) || name != catalogPageName(c.pages+1) {
		c.invalid = errors.New("backup catalog page sequence is invalid")
		return
	}
	page, err := decodeCatalogPage(raw)
	if err != nil {
		c.invalid = err
		return
	}
	if page.Number != c.pages+1 {
		c.invalid = errors.New("backup catalog page number mismatch")
		return
	}
	if c.declared == nil {
		c.declared, err = newObservationIndexForWorkspace(ctx, os.TempDir(), 0)
		if err != nil {
			c.invalid = err
			return
		}
	}
	for _, file := range page.Files {
		if err := ctx.Err(); err != nil {
			c.invalid = err
			return
		}
		clean, err := safeArchivePath(file.Path)
		if err != nil || clean == "manifest.json" || isCatalogPath(clean) || file.Path <= c.last || c.count == ^uint64(0) {
			c.invalid = errors.New("backup catalog path order is invalid")
			return
		}
		found, exists, err := actual.Get(file.Path)
		if err != nil {
			c.invalid = err
			return
		}
		if !exists || found != file {
			c.invalid = fmt.Errorf("backup catalog mismatch for %q", file.Path)
			return
		}
		if err := c.declared.Add(file); err != nil {
			c.invalid = err
			return
		}
		c.count++
		c.last = file.Path
	}
	c.pages++
	hashCatalogPage(c.digest, c.pages, raw)
}

func verifyAnyArchive(ctx context.Context, input io.Reader, key []byte) (root ArchiveSummary, retained *observationIndex, err error) {
	actual, err := newObservationIndexForWorkspace(ctx, os.TempDir(), 0)
	if err != nil {
		return root, nil, err
	}
	candidate := catalogCandidate{digest: newCatalogHash()}
	defer func() {
		err = errors.Join(err, candidate.declared.Close())
		if err != nil {
			err = errors.Join(err, actual.Close())
			root = ArchiveSummary{}
			retained = nil
		}
	}()
	decrypted, err := newDecryptReader(input, key)
	if err != nil {
		return root, nil, err
	}
	reader := tar.NewReader(decrypted)
	var rootRaw []byte
	foundRoot := false
	var candidateEntries uint64
	var payloadBytes, catalogBytes int64
	for {
		if err := ctx.Err(); err != nil {
			return root, nil, err
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return root, nil, err
		}
		if foundRoot {
			return root, nil, errors.New("backup contains entries after manifest")
		}
		name, err := safeArchivePath(header.Name)
		if err != nil || header.Typeflag != tar.TypeReg || header.Size < 0 {
			return root, nil, errors.New("backup contains unsafe entry")
		}
		if name == "manifest.json" {
			if header.Size > backupManifestSizeLimit {
				return root, nil, errors.New("backup manifest exceeds size limit")
			}
			rootRaw, err = io.ReadAll(io.LimitReader(reader, header.Size+1))
			if err != nil || int64(len(rootRaw)) != header.Size {
				return root, nil, errors.Join(errors.New("read backup manifest"), err)
			}
			foundRoot = true
			continue
		}
		if _, exists, err := actual.Get(name); err != nil {
			return root, nil, err
		} else if exists {
			return root, nil, errors.New("duplicate backup payload")
		}
		digest := sha256.New()
		var candidateRaw []byte
		var size int64
		if isCatalogPath(name) {
			if candidateEntries == ^uint64(0) {
				return root, nil, errors.New("backup catalog entry count overflow")
			}
			candidateEntries++
			if header.Size <= pagedMetadataLimit && candidate.invalid == nil {
				// One page-sized copy only, also hashed as an ordinary entry until the
				// final root chooses format 5. Legacy contents are never discarded.
				candidateRaw, err = io.ReadAll(io.LimitReader(backupContextReader{ctx: ctx, reader: reader}, header.Size+1))
				size = int64(len(candidateRaw))
				_, _ = digest.Write(candidateRaw)
			} else {
				if header.Size > pagedMetadataLimit && candidate.invalid == nil {
					candidate.invalid = errors.New("paged backup metadata exceeds limit")
				}
				size, err = io.Copy(digest, backupContextReader{ctx: ctx, reader: reader})
			}
		} else {
			size, err = io.Copy(digest, backupContextReader{ctx: ctx, reader: reader})
		}
		if err != nil || size != header.Size {
			return root, nil, errors.Join(errors.New("read backup payload"), err)
		}
		if size > math.MaxInt64-payloadBytes {
			return root, nil, errors.New("backup payload byte count overflow")
		}
		payloadBytes += size
		if isCatalogPath(name) {
			if size > math.MaxInt64-catalogBytes {
				return root, nil, errors.New("backup catalog byte count overflow")
			}
			catalogBytes += size
		}
		if candidateRaw != nil {
			candidate.observe(ctx, name, candidateRaw, actual)
		}
		if err := actual.Add(File{Path: name, Size: size, SHA256: hex.EncodeToString(digest.Sum(nil))}); err != nil {
			return root, nil, err
		}
	}
	if _, err := io.Copy(io.Discard, backupContextReader{ctx: ctx, reader: decrypted}); err != nil {
		return root, nil, fmt.Errorf("verify authenticated backup ending: %w", err)
	}
	if !foundRoot {
		return root, nil, errors.New("backup manifest is missing")
	}
	var version struct {
		FormatVersion int `json:"format_version"`
	}
	if err := json.Unmarshal(rootRaw, &version); err != nil {
		return root, nil, err
	}
	if version.FormatVersion == pagedManifestVersion {
		if len(rootRaw) > pagedMetadataLimit {
			return root, nil, errors.New("paged backup root exceeds limit")
		}
		if err := validatePagedRootJSON(rootRaw); err != nil {
			return root, nil, err
		}
		decoder := json.NewDecoder(bytes.NewReader(rootRaw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&root); err != nil {
			return root, nil, err
		}
		if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
			return root, nil, errors.New("trailing backup root JSON")
		}
		if err := validatePagedManifest(root); err != nil {
			return root, nil, err
		}
		if candidate.invalid != nil {
			return root, nil, candidate.invalid
		}
		if candidate.declared == nil || root.Catalog.Pages != candidate.pages || root.Catalog.Files != candidate.count || candidateEntries != candidate.pages || candidate.count != candidate.declared.count || candidate.pages > actual.count || candidate.count != actual.count-candidate.pages || root.Catalog.SHA256 != hex.EncodeToString(candidate.digest.Sum(nil)) {
			return root, nil, errors.New("paged backup catalog completeness mismatch")
		}
		// Retain the authenticated payload-only owner directly, after a final
		// full MAC/count walk. Do not create a third index or duplicate all facts.
		if err := candidate.declared.Walk(ctx, func(File) error { return nil }); err != nil {
			return root, nil, err
		}
		if err := actual.Close(); err != nil {
			return root, nil, err
		}
		actual = candidate.declared
		candidate.declared = nil

	} else {
		var legacy Manifest
		if err := json.Unmarshal(rootRaw, &legacy); err != nil {
			return root, nil, err
		}
		if err := validateManifestHeader(legacy); err != nil {
			return root, nil, err
		}
		if err := matchManifestObservations(legacy, actual); err != nil {
			return root, nil, err
		}
		encoded, err := json.Marshal(legacy)
		if err != nil {
			return root, nil, err
		}
		manifestDigest := sha256.Sum256(encoded)
		version := legacy.FormatVersion
		legacy.Files = nil // bounded old header list is not the lifetime inventory
		root = ArchiveSummary{Manifest: legacy, CompatibilityVersion: version, Catalog: FileCatalogSummary{Files: actual.count}, legacyManifestDigest: hex.EncodeToString(manifestDigest[:])}
	}
	if err := requiredPagedFiles(root, actual); err != nil {
		return root, nil, err
	}
	if err := ctx.Err(); err != nil {
		return root, nil, err
	}
	if root.FormatVersion == pagedManifestVersion {
		payloadBytes -= catalogBytes
	}
	root.contentBytes = payloadBytes
	return root, actual, nil
}
