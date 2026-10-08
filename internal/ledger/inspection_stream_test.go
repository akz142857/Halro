package ledger

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// Construct authentic history without repeated Roll's whole-manifest writes.
// Each descriptor and its exact native frame are emitted once, no history slice.
func inspectionStreamFixture(tb testing.TB, count int) string {
	tb.Helper()
	root := tb.TempDir()
	path := filepath.Join(root, "ledger.wal")
	manifest, err := os.Create(segmentManifestPath(root))
	if err != nil {
		tb.Fatal(err)
	}
	if _, err := manifest.WriteString(`{"version":1,"segments":[`); err != nil {
		tb.Fatal(err)
	}
	encoder := json.NewEncoder(manifest)
	event := validReservation("inspection-event", "inspection-attempt")
	payload, err := json.Marshal(event)
	if err != nil {
		tb.Fatal(err)
	}
	var previous [32]byte
	for generation := 1; generation <= count; generation++ {
		frame, next := encodeChainFrame(testChainKey, uint64(generation), event.Kind, payload, previous)
		name := fmt.Sprintf("ledger-%d.wal", generation)
		if err := os.WriteFile(filepath.Join(root, name), frame, 0o600); err != nil {
			tb.Fatal(err)
		}
		sum := sha256.Sum256(frame)
		segment := Segment{Generation: uint64(generation), File: name, FirstSequence: uint64(generation), LastSequence: uint64(generation), Length: int64(len(frame)), StoredLength: int64(len(frame)), StartHash: encodeChainHash(previous), EndHash: encodeChainHash(next), PlainChecksum: hex.EncodeToString(sum[:]), StoredChecksum: hex.EncodeToString(sum[:]), EndEpoch: frameVersionLedgerIntegrity, SealedAt: time.Date(2026, 10, 2, 1, 0, 0, 0, time.UTC)}
		if generation > 1 {
			if _, err := manifest.WriteString(","); err != nil {
				tb.Fatal(err)
			}
		}
		if err := encoder.Encode(segment); err != nil {
			tb.Fatal(err)
		}
		previous = next
	}
	if _, err := manifest.WriteString("]}"); err != nil {
		tb.Fatal(err)
	}
	if err := manifest.Close(); err != nil {
		tb.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		tb.Fatal(err)
	}
	return path
}

func TestInspectionStreamValidatesWholeDirectoryBeforeFirstVisit(t *testing.T) {
	for _, mode := range []string{"late-missing", "late-size", "late-generation", "late-version", "late-extension"} {
		t.Run(mode, func(t *testing.T) {
			path := inspectionStreamFixture(t, 3)
			root := filepath.Dir(path)
			switch mode {
			case "late-missing":
				if err := os.Remove(filepath.Join(root, "ledger-3.wal")); err != nil {
					t.Fatal(err)
				}
			case "late-size":
				if err := os.Truncate(filepath.Join(root, "ledger-3.wal"), 1); err != nil {
					t.Fatal(err)
				}
			default:
				raw, err := os.ReadFile(segmentManifestPath(root))
				if err != nil {
					t.Fatal(err)
				}
				if mode == "late-generation" {
					raw = bytes.Replace(raw, []byte(`"generation":3`), []byte(`"generation":4`), 1)
				}
				if mode == "late-version" {
					raw = bytes.Replace(raw, []byte(`"version":1`), []byte(`"version":999`), 1)
				}
				if mode == "late-extension" {
					raw = append(raw[:len(raw)-1], []byte(`,"extension":"`+string(bytes.Repeat([]byte{'x'}, replicationManifestValueLimit))+`"}`)...)
				}
				if err := os.WriteFile(segmentManifestPath(root), raw, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			visits := 0
			report, _, err := InspectReplayAuthenticated(path, testChainKey, func(Record) error { visits++; return nil })
			if err == nil || visits != 0 || report != (ChainReport{}) {
				t.Fatal("late directory failure exposed records", visits, report, err)
			}
		})
	}
}

func TestInspectionStreamPendingRenameSidesAndNonHourTimezone(t *testing.T) {
	for _, afterRename := range []bool{false, true} {
		t.Run(fmt.Sprintf("renamed=%v", afterRename), func(t *testing.T) {
			path := inspectionStreamFixture(t, 2)
			root := filepath.Dir(path)
			manifest, err := loadSegmentManifest(root)
			if err != nil {
				t.Fatal(err)
			}
			manifest.Segments[0].SealedAt = manifest.Segments[0].SealedAt.In(time.FixedZone("half-hour", 19800))
			last := manifest.Segments[1]
			previous, err := decodeChainHash(last.EndHash)
			if err != nil {
				t.Fatal(err)
			}
			event := validReservation("pending-event", "pending-attempt")
			payload, err := json.Marshal(event)
			if err != nil {
				t.Fatal(err)
			}
			frame, end := encodeChainFrame(testChainKey, 3, event.Kind, payload, previous)
			digest := sha256.Sum256(frame)
			pending := Segment{Generation: 3, File: "ledger-3.wal", FirstSequence: 3, LastSequence: 3, Length: int64(len(frame)), StoredLength: int64(len(frame)), StartHash: last.EndHash, EndHash: encodeChainHash(end), PlainChecksum: hex.EncodeToString(digest[:]), StoredChecksum: hex.EncodeToString(digest[:]), EndEpoch: 4, SealedAt: time.Now().In(time.FixedZone("pending-offset", 20700))}
			manifest.Pending = &pending
			if err := saveSegmentManifest(root, manifest); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, frame, 0o600); err != nil {
				t.Fatal(err)
			}
			if afterRename {
				if err := os.Rename(path, filepath.Join(root, pending.File)); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(segmentManifestPath(root))
			if err != nil {
				t.Fatal(err)
			}
			visits := 0
			report, partial, err := InspectReplayAuthenticated(path, testChainKey, func(record Record) error {
				visits++
				if record.Sequence != uint64(visits) {
					t.Error("global sequence changed", record.Sequence, visits)
				}
				return nil
			})
			expectedSealed := uint64(2)
			expectedActive := uint64(3)
			if afterRename {
				expectedSealed++
				expectedActive++
			}
			if err != nil || partial || visits != 3 || report.SealedGenerations != expectedSealed || report.Head.Generation != expectedActive || report.Head.Sequence != 3 || report.ChainHash != end {
				t.Fatal("pending/offset semantics changed", visits, report, err)
			}
			after, err := os.ReadFile(segmentManifestPath(root))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("inspection repaired pending manifest", err)
			}
		})
	}
}

func TestInspectionStreamRejectsManifestReplacementDuringVisit(t *testing.T) {
	for _, mode := range []string{"replace", "rewrite", "appear"} {
		t.Run(mode, func(t *testing.T) {
			path := inspectionStreamFixture(t, 2)
			root := filepath.Dir(path)
			manifestPath := segmentManifestPath(root)
			raw, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "appear" {
				// The callback in a manifest-free active-only directory creates it.
				if err := os.Remove(manifestPath); err != nil {
					t.Fatal(err)
				}
				first, err := os.ReadFile(filepath.Join(root, "ledger-1.wal"))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, first, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			visits := 0
			report, _, err := InspectReplayAuthenticated(path, testChainKey, func(Record) error {
				visits++
				if visits != 1 {
					return nil
				}
				if mode == "replace" {
					if err := os.Rename(manifestPath, manifestPath+".old"); err != nil {
						return err
					}
				}
				if mode == "rewrite" {
					raw = append(raw, ' ')
				}
				return os.WriteFile(manifestPath, raw, 0o600)
			})
			if !errors.Is(err, ErrCorrupt) || report != (ChainReport{}) {
				t.Fatal("mixed manifest inspection accepted", report, err)
			}
		})
	}
}

func TestInspectionManifestDigestDetectsSameSizeRewriteWithRestoredMtime(t *testing.T) {
	path := inspectionStreamFixture(t, 1)
	root := filepath.Dir(path)
	raw, err := os.ReadFile(segmentManifestPath(root))
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw[:len(raw)-1], []byte(`,"extension":"a"}`)...)
	if err := os.WriteFile(segmentManifestPath(root), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	view, err := openInspectionManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	defer view.close()
	_, first, err := view.walk(nil)
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.Replace(raw, []byte(`"extension":"a"`), []byte(`"extension":"b"`), 1)
	if err := os.WriteFile(segmentManifestPath(root), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(segmentManifestPath(root), view.info.ModTime(), view.info.ModTime()); err != nil {
		t.Fatal(err)
	}
	_, second, err := view.walk(nil)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("same-size/mtime rewrite escaped directory content digest")
	}
}

func TestInspectionStreamReplaysLargeGenerationCountWithoutHistorySlice(t *testing.T) {
	const count = 1000
	path := inspectionStreamFixture(t, count)
	visits := 0
	report, partial, err := InspectReplayAuthenticated(path, testChainKey, func(record Record) error {
		visits++
		if record.Sequence != uint64(visits) {
			t.Error(record.Sequence, visits)
		}
		return nil
	})
	if err != nil || partial || visits != count || report.Head.Generation != count+1 || report.Head.Sequence != count || report.SealedGenerations != count || report.SealedAuthenticated != count {
		t.Fatal("large history incomplete", visits, report, err)
	}
}

// Capacity comparison isolates directory residency. GC at the last descriptor
// observes retained structures rather than claiming cumulative B/op is live RAM.
func BenchmarkInspectionManifestRetainedDirectory(b *testing.B) {
	for _, count := range []int{100, 10000} {
		root := b.TempDir()
		file, err := os.Create(segmentManifestPath(root))
		if err != nil {
			b.Fatal(err)
		}
		if _, err := file.WriteString(`{"version":1,"segments":[`); err != nil {
			b.Fatal(err)
		}
		encoder := json.NewEncoder(file)
		for i := 1; i <= count; i++ {
			if i > 1 {
				if _, err := file.WriteString(","); err != nil {
					b.Fatal(err)
				}
			}
			if err := encoder.Encode(Segment{Generation: uint64(i), File: fmt.Sprintf("ledger-%d.wal", i)}); err != nil {
				b.Fatal(err)
			}
		}
		if _, err := file.WriteString("]}"); err != nil {
			b.Fatal(err)
		}
		if err := file.Close(); err != nil {
			b.Fatal(err)
		}
		for _, stream := range []bool{false, true} {
			name := "Collected"
			if stream {
				name = "Streaming"
			}
			b.Run(fmt.Sprintf("%s/%d", name, count), func(b *testing.B) {
				var live uint64
				b.ReportAllocs()
				for range b.N {
					runtime.GC()
					var before runtime.MemStats
					runtime.ReadMemStats(&before)
					sample := func() {
						runtime.GC()
						var after runtime.MemStats
						runtime.ReadMemStats(&after)
						if after.HeapAlloc > before.HeapAlloc {
							live += after.HeapAlloc - before.HeapAlloc
						}
					}
					if stream {
						visited := 0
						if _, err := walkReplicationManifest(root, func(Segment) error {
							visited++
							if visited == count {
								sample()
							}
							return nil
						}); err != nil || visited != count {
							b.Fatal(visited, err)
						}
					} else {
						segments, _, err := resolveSegments(root)
						if err != nil || len(segments) != count {
							b.Fatal(err)
						}
						sample()
						runtime.KeepAlive(segments)
					}
				}
				b.ReportMetric(float64(live)/float64(b.N), "retained-B/op")
			})
		}
	}
}
