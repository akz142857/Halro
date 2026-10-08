package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestUnifiedVerifiedArchiveFormatsOneThroughFive(t *testing.T) {
	for _, version := range []int{1, 2, 3, 4, 5} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			options := manifestLimitOptions(t)
			local := options.Files[0].LocalPath
			if version == 3 || version == 4 {
				options.GovernanceFormatVersion = 1
				options.Files = append(options.Files, SourceFile{ArchivePath: "data/governance/governance.journal", LocalPath: local})
			}
			if version == 4 {
				options.ClusterID = "cluster"
				options.ClusterIncarnation = "incarnation"
				options.SourceNodeID = "node"
				options.SourceRole = "replica"
				options.Term = 1
				options.MetadataJournalEpoch = 1
				options.MetadataJournalSequence = 2
				options.PerStoreHead = map[string]StoreHead{"ledger": {}, "audit": {}, "governance": {}, "metadata": {Generation: 1, Sequence: 2}}
				for _, name := range []string{"data/cluster/state.json", "data/cluster/ordering.journal", "data/metadata.journal"} {
					options.Files = append(options.Files, SourceFile{ArchivePath: name, LocalPath: local})
				}
			}
			if version == 5 {
				if _, err := CreatePaged(options); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := Create(options); err != nil {
					t.Fatal(err)
				}
				if version == 1 {
					entries := readPagedTestEntries(t, options.OutputPath, options.BackupKey)
					for n, entry := range entries {
						if entry.name == "manifest.json" {
							var manifest Manifest
							if err := json.Unmarshal(entry.raw, &manifest); err != nil {
								t.Fatal(err)
							}
							manifest.FormatVersion = 1
							raw, err := json.Marshal(manifest)
							if err != nil {
								t.Fatal(err)
							}
							entries[n].raw = raw
						}
					}
					writePagedTestEntries(t, options.OutputPath, options.BackupKey, entries)
				}
			}
			handle, err := OpenVerified(context.Background(), options.OutputPath, options.BackupKey)
			if err != nil {
				t.Fatal(err)
			}
			defer handle.Close()
			root := handle.Summary()
			if root.FormatVersion != version || root.Files != nil || root.Catalog.Files != uint64(len(options.Files)) {
				t.Fatal(root.FormatVersion, root.Catalog, len(options.Files))
			}
			compatible := version
			if version == 5 {
				compatible = 2
			}
			if root.CompatibilityVersion != compatible {
				t.Fatal(root.CompatibilityVersion)
			}
			var expectedPayloadBytes int64
			for _, file := range options.Files {
				info, err := os.Stat(file.LocalPath)
				if err != nil {
					t.Fatal(err)
				}
				expectedPayloadBytes += info.Size()
			}
			if got, err := handle.PayloadBytes(); err != nil || got != expectedPayloadBytes {
				t.Fatalf("verified payload bytes=%d want=%d err=%v", got, expectedPayloadBytes, err)
			}
			if version < 5 && (root.Catalog.Version != 0 || root.Catalog.Pages != 0 || root.Catalog.SHA256 != "") {
				t.Fatal("legacy inventory fabricated a saved catalog root")
			}
			count := 0
			if err := handle.FilesEach(context.Background(), func(file File) error { count++; return nil }); err != nil {
				t.Fatal(err)
			}
			if count != len(options.Files) {
				t.Fatal(count)
			}
			stage := filepath.Join(filepath.Dir(options.OutputPath), "stage")
			if err := handle.Extract(context.Background(), stage); err != nil {
				t.Fatal(err)
			}
			for _, source := range options.Files {
				raw, err := os.ReadFile(filepath.Join(stage, filepath.FromSlash(source.ArchivePath)))
				if err != nil || string(raw) != "native fact" {
					t.Fatal(source.ArchivePath, err)
				}
			}
			if err := handle.Close(); err != nil {
				t.Fatal(err)
			}
			if version < 5 {
				if result, err := OpenPaged(context.Background(), options.OutputPath, options.BackupKey); err == nil || result != nil {
					if result != nil {
						result.Close()
					}
					t.Fatal("paged-only API accepted legacy container")
				}
			}
		})
	}
}

func TestUnifiedLegacyCatalogNamespaceIsOrdinaryPayload(t *testing.T) {
	for _, kind := range []string{"plain", "empty", "wide", "valid-page", "bad-page"} {
		t.Run(kind, func(t *testing.T) {
			options := manifestLimitOptions(t)
			var content []byte
			switch kind {
			case "plain":
				content = []byte("ordinary user contents")
			case "empty":
				content = []byte{}
			case "wide":
				content = bytes.Repeat([]byte("z"), pagedMetadataLimit+17)
			case "bad-page":
				content = []byte(`{"version":1,"number":2,"files":[]}`)
			case "valid-page":
				digest := sha256.Sum256([]byte("native fact"))
				var files []File
				for _, source := range options.Files {
					files = append(files, File{Path: source.ArchivePath, Size: int64(len("native fact")), SHA256: hex.EncodeToString(digest[:])})
				}
				slices.SortFunc(files, func(a, b File) int { return strings.Compare(a.Path, b.Path) })
				var err error
				content, err = json.Marshal(catalogPage{Version: 1, Number: 1, Files: files})
				if err != nil {
					t.Fatal(err)
				}
			}
			local := filepath.Join(filepath.Dir(options.OutputPath), "ordinary.json")
			if err := os.WriteFile(local, content, 0600); err != nil {
				t.Fatal(err)
			}
			name := catalogPageName(1)
			options.Files = append(options.Files, SourceFile{ArchivePath: name, LocalPath: local})
			if _, err := Create(options); err != nil {
				t.Fatal(err)
			}
			if kind == "valid-page" {
				// Make the page candidate fully valid: its four declared payloads precede
				// it. Only the final legacy root distinguishes this from v5 control data.
				entries := readPagedTestEntries(t, options.OutputPath, options.BackupKey)
				var ordinary, root pagedTestEntry
				var ordered []pagedTestEntry
				for _, entry := range entries {
					if entry.name == name {
						ordinary = entry
					} else if entry.name == "manifest.json" {
						root = entry
					} else {
						ordered = append(ordered, entry)
					}
				}
				ordered = append(ordered, ordinary, root)
				writePagedTestEntries(t, options.OutputPath, options.BackupKey, ordered)
			}
			handle, err := OpenVerified(context.Background(), options.OutputPath, options.BackupKey)
			if err != nil {
				t.Fatal(err)
			}
			defer handle.Close()
			if handle.Summary().FormatVersion != 2 || handle.Summary().Catalog.Files != 5 {
				t.Fatal(handle.Summary().Catalog)
			}
			found := false
			if err := handle.FilesEach(context.Background(), func(file File) error {
				if file.Path == name {
					found = true
					if file.Size != int64(len(content)) {
						t.Fatal(file.Size)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if !found {
				t.Fatal("legacy reserved-looking file was dropped")
			}
			stage := filepath.Join(filepath.Dir(options.OutputPath), "stage")
			if err := handle.Extract(context.Background(), stage); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(filepath.Join(stage, filepath.FromSlash(name)))
			if err != nil || !bytes.Equal(raw, content) {
				t.Fatal("legacy file changed or was skipped", err)
			}
		})
	}
}

func TestUnifiedLegacyWideRootAndWrongFinalSource(t *testing.T) {
	options := manifestLimitOptions(t)
	options.Build.Version = strings.Repeat("v", pagedMetadataLimit+17)
	if _, err := Create(options); err != nil {
		t.Fatal(err)
	}
	h, err := OpenVerified(context.Background(), options.OutputPath, options.BackupKey)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if len(h.Summary().Build.Version) <= pagedMetadataLimit {
		t.Fatal("old wide root was truncated")
	}
	second := options
	second.OutputPath = filepath.Join(filepath.Dir(options.OutputPath), "second.hmbk")
	if _, err := Create(second); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(second.OutputPath)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(raw)) != h.identity.Size() {
		t.Fatal("ciphertext fixture size changed")
	}
	if err := os.WriteFile(options.OutputPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(options.OutputPath, h.identity.ModTime(), h.identity.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := h.Extract(context.Background(), filepath.Join(filepath.Dir(options.OutputPath), "stage")); err == nil || !strings.Contains(err.Error(), "between verification") {
		t.Fatal("legacy source rewrite bypassed full ciphertext fence", err)
	}
}

// A counting Reader proves format discrimination and full authentication take
// exactly one pass, with no rewind or fallback parser reopening the source.
type countedBackupInput struct {
	input io.Reader
	bytes int64
}

func (r *countedBackupInput) Read(p []byte) (int, error) {
	n, err := r.input.Read(p)
	r.bytes += int64(n)
	return n, err
}
func TestUnifiedVerificationSinglePassAndCleanup(t *testing.T) {
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	for _, paged := range []bool{false, true} {
		options := manifestLimitOptions(t)
		if paged {
			if _, err := CreatePaged(options); err != nil {
				t.Fatal(err)
			}
		} else {
			if _, err := Create(options); err != nil {
				t.Fatal(err)
			}
		}
		raw, err := os.ReadFile(options.OutputPath)
		if err != nil {
			t.Fatal(err)
		}
		counted := &countedBackupInput{input: bytes.NewReader(raw)}
		summary, index, err := verifyAnyArchive(context.Background(), counted, options.BackupKey)
		if err != nil {
			t.Fatal(err)
		}
		if counted.bytes != int64(len(raw)) || summary.Catalog.Files != 4 || index.count != 4 {
			t.Fatal(counted.bytes, len(raw), summary.Catalog, index.count)
		}
		if err := index.Close(); err != nil {
			t.Fatal(err)
		}
		broken := bytes.Clone(raw)
		broken[len(broken)-1] ^= 1
		if root, index, err := verifyAnyArchive(context.Background(), bytes.NewReader(broken), options.BackupKey); err == nil || index != nil || root.BackupID != "" {
			if index != nil {
				index.Close()
			}
			t.Fatal("bad final record returned authority", err)
		}
	}
	files, err := os.ReadDir(scratch)
	if err != nil || len(files) != 0 {
		t.Fatal("unified verification leaked scratch", files, err)
	}
}

func TestUnifiedLegacyMissingAndDuplicateFactsAreRejected(t *testing.T) {
	options := manifestLimitOptions(t)
	if _, err := Create(options); err != nil {
		t.Fatal(err)
	}
	entries := readPagedTestEntries(t, options.OutputPath, options.BackupKey)
	for _, kind := range []string{"missing", "duplicate", "extra", "late", "bad-count"} {
		t.Run(kind, func(t *testing.T) {
			mutated := append([]pagedTestEntry(nil), entries...)
			switch kind {
			case "missing":
				mutated = mutated[1:]
			case "duplicate":
				mutated = append([]pagedTestEntry{mutated[0]}, mutated...)
			case "extra":
				mutated = append([]pagedTestEntry{{"data/extra", []byte("extra")}}, mutated...)
			case "late":
				mutated = append(mutated, pagedTestEntry{"data/late", []byte("late")})
			case "bad-count":
				for n, entry := range mutated {
					if entry.name == "manifest.json" {
						var root Manifest
						if err := json.Unmarshal(entry.raw, &root); err != nil {
							t.Fatal(err)
						}
						root.Files = root.Files[1:]
						raw, err := json.Marshal(root)
						if err != nil {
							t.Fatal(err)
						}
						mutated[n].raw = raw
					}
				}
			}
			filename := filepath.Join(t.TempDir(), "bad.hmbk")
			writePagedTestEntries(t, filename, options.BackupKey, mutated)
			if h, err := OpenVerified(context.Background(), filename, options.BackupKey); err == nil || h != nil {
				if h != nil {
					h.Close()
				}
				t.Fatal("legacy bad facts accepted", err)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(filename), "stage")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
		})
	}
}
