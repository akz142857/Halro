package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/ownedfile"
)

func TestPagedBackupLargeCatalogRoundTripAndLegacyRejection(t *testing.T) {
	options := manifestLimitOptions(t)
	catalog, err := NewSourceCatalog(filepath.Dir(options.OutputPath))
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range options.Files {
		if err := catalog.Add(source); err != nil {
			t.Fatal(err)
		}
	}
	local := options.Files[0].LocalPath
	prefix := "data/" + strings.Repeat("a", 200) + "/" + strings.Repeat("b", 200) + "/" + strings.Repeat("c", 200) + "/"
	for n := 0; n < 1500; n++ {
		if err := catalog.Add(SourceFile{ArchivePath: fmt.Sprintf("%s%04d_%s", prefix, n, strings.Repeat("d", 150)), LocalPath: local}); err != nil {
			t.Fatal(err)
		}
	}
	options.Files = nil
	options.SourceCatalog = catalog
	created, err := CreatePaged(options)
	if err != nil {
		t.Fatal(err)
	}
	if created.FormatVersion != 5 || created.CompatibilityVersion != 2 || created.SourceRole != "standalone" || created.Files != nil || created.Catalog.Files != 1504 || created.Catalog.Pages < 24 {
		t.Fatal(created.Catalog)
	}
	encoded, err := json.Marshal(created)
	if err != nil || len(encoded) > pagedMetadataLimit {
		t.Fatal(len(encoded), err)
	}
	handle, err := OpenPaged(context.Background(), options.OutputPath, options.BackupKey)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	observed := 0
	legacyBytes := 0
	if err := handle.FilesEach(context.Background(), func(file File) error {
		raw, err := json.Marshal(file)
		if err != nil {
			return err
		}
		legacyBytes += len(raw) + 1
		observed++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if observed != 1504 || legacyBytes <= backupManifestSizeLimit {
		t.Fatal(observed, legacyBytes)
	}
	if handle.Summary().Catalog != created.Catalog {
		t.Fatal("verified catalog changed")
	}
	destination := filepath.Join(filepath.Dir(options.OutputPath), "extracted")
	if err := handle.Extract(context.Background(), destination); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"config/config.yaml", prefix + "0000_" + strings.Repeat("d", 150), prefix + "1499_" + strings.Repeat("d", 150)} {
		data, err := os.ReadFile(filepath.Join(destination, filepath.FromSlash(name)))
		if err != nil || string(data) != "native fact" {
			t.Fatal(name, string(data), err)
		}
	}
	if err := handle.FilesEach(context.Background(), func(file File) error {
		raw, err := os.ReadFile(filepath.Join(destination, filepath.FromSlash(file.Path)))
		if err != nil {
			return err
		}
		if string(raw) != "native fact" {
			return errors.New("extracted payload changed")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(destination, strings.TrimSuffix(catalogPrefix, "/"))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("metadata catalog was extracted as user data", err)
	}
	if result, err := Verify(options.OutputPath, options.BackupKey); err == nil || result.BackupID != "" {
		t.Fatal("legacy collecting reader accepted v5", err)
	}
	legacyDestination := filepath.Join(filepath.Dir(options.OutputPath), "legacy-extracted")
	if result, err := Extract(options.OutputPath, options.BackupKey, legacyDestination); err == nil || result.BackupID != "" {
		t.Fatal("legacy extraction accepted v5", err)
	}
	if _, err := os.Stat(legacyDestination); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("legacy extractor wrote before rejecting new format", err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	if len(handle.key) != 0 || handle.input != nil || handle.observed != nil || handle.Summary().BackupID != "" {
		t.Fatal("closed handle retained authority/key")
	}
}

func TestPagedBackupRoleCompatibilityAndRequiredSourceGate(t *testing.T) {
	for _, version := range []int{2, 3, 4} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			options := manifestLimitOptions(t)
			local := options.Files[0].LocalPath
			if version >= 3 {
				options.GovernanceFormatVersion = 1
				options.Files = append(options.Files, SourceFile{ArchivePath: "data/governance/governance.journal", LocalPath: local})
			}
			if version == 4 {
				options.ClusterID = "cluster"
				options.ClusterIncarnation = "incarnation"
				options.SourceNodeID = "node"
				options.SourceRole = "replica"
				options.Term = 2
				options.MetadataJournalEpoch = 3
				options.MetadataJournalSequence = 4
				options.PerStoreHead = map[string]StoreHead{"ledger": {}, "audit": {}, "governance": {}, "metadata": {Generation: 3, Sequence: 4}}
				for _, name := range []string{"data/cluster/state.json", "data/cluster/ordering.journal", "data/metadata.journal"} {
					options.Files = append(options.Files, SourceFile{ArchivePath: name, LocalPath: local})
				}
			}
			created, err := CreatePaged(options)
			if err != nil {
				t.Fatal(err)
			}
			if created.CompatibilityVersion != version {
				t.Fatal(created.CompatibilityVersion)
			}
			handle, err := OpenPaged(context.Background(), options.OutputPath, options.BackupKey)
			if err != nil {
				t.Fatal(err)
			}
			if version == 4 {
				altered := handle.Summary()
				altered.PerStoreHead["metadata"] = StoreHead{}
				if handle.Summary().PerStoreHead["metadata"].Generation != 3 {
					t.Fatal("summary mutated owned prefix map")
				}
			}
			if err := handle.Close(); err != nil {
				t.Fatal(err)
			}
			options.OutputPath = filepath.Join(filepath.Dir(options.OutputPath), "missing.hmbk")
			options.Files = options.Files[1:]
			if result, err := CreatePaged(options); err == nil || result.BackupID != "" {
				t.Fatal("missing required source published", err)
			}
			if _, err := os.Stat(options.OutputPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
		})
	}
}

type pagedTestEntry struct {
	name string
	raw  []byte
}

func readPagedTestEntries(t *testing.T, path string, key []byte) []pagedTestEntry {
	t.Helper()
	input, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	decrypted, err := newDecryptReader(input, key)
	if err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(decrypted)
	var result []pagedTestEntry
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, pagedTestEntry{header.Name, raw})
	}
	if _, err := io.Copy(io.Discard, decrypted); err != nil {
		t.Fatal(err)
	}
	return result
}
func writePagedTestEntries(t *testing.T, path string, key []byte, entries []pagedTestEntry) {
	t.Helper()
	output, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	encrypted, err := newEncryptWriter(output, key)
	if err != nil {
		t.Fatal(err)
	}
	writer := tar.NewWriter(encrypted)
	for _, entry := range entries {
		if err := writer.WriteHeader(&tar.Header{Name: entry.name, Mode: 0600, Size: int64(len(entry.raw)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(entry.raw); err != nil {
			t.Fatal(err)
		}
	}
	if err := errors.Join(writer.Close(), encrypted.Close()); err != nil {
		t.Fatal(err)
	}
}
func updatePagedTestRoot(t *testing.T, entries []pagedTestEntry, change func(*PagedManifest)) {
	t.Helper()
	for n, entry := range entries {
		if entry.name == "manifest.json" {
			var root PagedManifest
			if err := json.Unmarshal(entry.raw, &root); err != nil {
				t.Fatal(err)
			}
			change(&root)
			raw, err := json.Marshal(root)
			if err != nil {
				t.Fatal(err)
			}
			entries[n].raw = raw
			return
		}
	}
	t.Fatal("missing test root")
}
func TestPagedBackupRejectsCatalogCompletenessAndEndingDamage(t *testing.T) {
	options := manifestLimitOptions(t)
	local := options.Files[0].LocalPath
	for n := 0; n < 70; n++ {
		options.Files = append(options.Files, SourceFile{ArchivePath: fmt.Sprintf("data/extra/%04d", n), LocalPath: local})
	}
	if _, err := CreatePaged(options); err != nil {
		t.Fatal(err)
	}
	original := readPagedTestEntries(t, options.OutputPath, options.BackupKey)
	for _, kind := range []string{"page-number", "page-delete", "page-swap", "catalog-count", "catalog-hash", "catalog-file-drop", "root-role", "duplicate-field", "root-wide", "root-duplicate", "root-nested-duplicate", "late-payload", "extra-payload", "ending", "wrong-key"} {
		t.Run(kind, func(t *testing.T) {
			entries := append([]pagedTestEntry(nil), original...)
			pageIndices := []int{}
			for n, entry := range entries {
				if isCatalogPath(entry.name) {
					pageIndices = append(pageIndices, n)
				}
			}
			switch kind {
			case "page-number":
				entries[pageIndices[0]].name = catalogPageName(2)
			case "page-delete":
				n := pageIndices[0]
				entries = append(entries[:n:n], entries[n+1:]...)
			case "page-swap":
				a, b := pageIndices[0], pageIndices[1]
				entries[a], entries[b] = entries[b], entries[a]
			case "catalog-count":
				updatePagedTestRoot(t, entries, func(root *PagedManifest) { root.Catalog.Files-- })
			case "catalog-hash":
				updatePagedTestRoot(t, entries, func(root *PagedManifest) { root.Catalog.SHA256 = strings.Repeat("0", 64) })
			case "root-role":
				updatePagedTestRoot(t, entries, func(root *PagedManifest) { root.SourceRole = "replica" })
			case "duplicate-field":
				n := pageIndices[0]
				entries[n].raw = bytes.Replace(entries[n].raw, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1)
			case "root-duplicate":
				n := len(entries) - 1
				entries[n].raw = bytes.Replace(entries[n].raw, []byte(`"compatibility_version":2`), []byte(`"compatibility_version":2,"compatibility_version":2`), 1)
			case "root-nested-duplicate":
				n := len(entries) - 1
				entries[n].raw = bytes.Replace(entries[n].raw, []byte(`"txid":1`), []byte(`"txid":1,"txid":1`), 1)
			case "root-wide":
				entries[len(entries)-1].raw = bytes.Repeat([]byte(" "), pagedMetadataLimit+1)
			case "catalog-file-drop":
				n := pageIndices[0]
				var page catalogPage
				if err := json.Unmarshal(entries[n].raw, &page); err != nil {
					t.Fatal(err)
				}
				page.Files = page.Files[1:]
				raw, err := json.Marshal(page)
				if err != nil {
					t.Fatal(err)
				}
				entries[n].raw = raw
				// Recompute the claimed digest so actual/declared bijection, rather
				// than only checksum disagreement, must detect the omission.
				h := newCatalogHash()
				for _, n := range pageIndices {
					var p catalogPage
					if err := json.Unmarshal(entries[n].raw, &p); err != nil {
						t.Fatal(err)
					}
					hashCatalogPage(h, p.Number, entries[n].raw)
				}
				updatePagedTestRoot(t, entries, func(root *PagedManifest) { root.Catalog.Files--; root.Catalog.SHA256 = hex.EncodeToString(h.Sum(nil)) })
			case "late-payload":
				entries = append(entries, pagedTestEntry{"data/late", []byte("late")})
			case "extra-payload":
				entries = append([]pagedTestEntry{{"data/extra-unclaimed", []byte("extra")}}, entries...)
			}
			output := filepath.Join(t.TempDir(), "bad.hmbk")
			writePagedTestEntries(t, output, options.BackupKey, entries)
			if kind == "ending" {
				raw, err := os.ReadFile(output)
				if err != nil {
					t.Fatal(err)
				}
				raw[len(raw)-1] ^= 1
				if err := os.WriteFile(output, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			key := options.BackupKey
			if kind == "wrong-key" {
				key = bytes.Repeat([]byte{17}, 32)
			}
			if handle, err := OpenPaged(context.Background(), output, key); err == nil || handle != nil {
				if handle != nil {
					handle.Close()
				}
				t.Fatal("malformed authenticated container accepted", kind)
			}
		})
	}
}

func TestPagedBackupPinnedCiphertextAndOwnedLifetime(t *testing.T) {
	for _, kind := range []string{"path", "same-inode", "index", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			options := manifestLimitOptions(t)
			if _, err := CreatePaged(options); err != nil {
				t.Fatal(err)
			}
			handle, err := OpenPaged(context.Background(), options.OutputPath, options.BackupKey)
			if err != nil {
				t.Fatal(err)
			}
			defer handle.Close()
			ctx := context.Background()
			indexName := handle.observed.db.Path()
			switch kind {
			case "path", "same-inode":
				second := options
				second.OutputPath = filepath.Join(filepath.Dir(options.OutputPath), "second.hmbk")
				if _, err := CreatePaged(second); err != nil {
					t.Fatal(err)
				}
				if kind == "path" {
					if err := os.Rename(second.OutputPath, options.OutputPath); err != nil {
						t.Fatal(err)
					}
				} else {
					raw, err := os.ReadFile(second.OutputPath)
					if err != nil {
						t.Fatal(err)
					}
					if int64(len(raw)) != handle.identity.Size() {
						t.Fatal("ciphertext fixture length changed")
					}
					if err := os.WriteFile(options.OutputPath, raw, 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.Chtimes(options.OutputPath, handle.identity.ModTime(), handle.identity.ModTime()); err != nil {
						t.Fatal(err)
					}
				}
			case "index":
				if err := os.Rename(indexName, indexName+".old"); err != nil {
					t.Fatal(err)
				}
				defer os.Remove(indexName + ".old")
				if err := os.WriteFile(indexName, []byte("foreign"), 0600); err != nil {
					t.Fatal(err)
				}
			case "cancel":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			destination := filepath.Join(filepath.Dir(options.OutputPath), "stage")
			err = handle.Extract(ctx, destination)
			if err == nil {
				t.Fatal("changed/canceled handle extracted successfully")
			}
			if kind == "same-inode" && !strings.Contains(err.Error(), "between verification") {
				t.Fatal("same-inode rewrite did not reach full digest fence", err)
			}
			if kind == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			closeErr := handle.Close()
			if kind == "index" {
				if !errors.Is(closeErr, ownedfile.ErrReplaced) {
					t.Fatal(closeErr)
				}
				raw, err := os.ReadFile(indexName)
				if err != nil || string(raw) != "foreign" {
					t.Fatal(string(raw), err)
				}
			} else if closeErr != nil {
				t.Fatal(closeErr)
			}
		})
	}
}

func TestPagedCatalogDecoderRejectsWideArraysAndDuplicateFileFields(t *testing.T) {
	file := File{Path: "data/a", Size: 1, SHA256: strings.Repeat("0", 64)}
	page := catalogPage{Version: 1, Number: 1, Files: make([]File, catalogPageRows+1)}
	for n := range page.Files {
		page.Files[n] = file
	}
	raw, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeCatalogPage(raw); err == nil {
		t.Fatal("wide array decoded beyond row limit")
	}
	page.Files = page.Files[:1]
	raw, err = json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.Replace(raw, []byte(`"size":1`), []byte(`"size":1,"size":1`), 1)
	if _, err := decodeCatalogPage(raw); err == nil {
		t.Fatal("duplicate file field accepted")
	}
}

func TestPagedOpenFailureCleansAllPrivateIndexes(t *testing.T) {
	options := manifestLimitOptions(t)
	if _, err := CreatePaged(options); err != nil {
		t.Fatal(err)
	}
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	bad := readPagedTestEntries(t, options.OutputPath, options.BackupKey)
	updatePagedTestRoot(t, bad, func(root *PagedManifest) { root.Catalog.Files++ })
	filename := filepath.Join(t.TempDir(), "bad.hmbk")
	writePagedTestEntries(t, filename, options.BackupKey, bad)
	if handle, err := OpenPaged(context.Background(), filename, options.BackupKey); err == nil || handle != nil {
		t.Fatal("invalid container returned a handle")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := OpenPaged(ctx, options.OutputPath, options.BackupKey); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := OpenPaged(context.Background(), options.OutputPath, bytes.Repeat([]byte{1}, 33)); err == nil {
		t.Fatal("invalid key width accepted")
	}
	files, err := os.ReadDir(scratch)
	if err != nil || len(files) != 0 {
		t.Fatal("failed open leaked private indexes", files, err)
	}
}
