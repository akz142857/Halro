package ledger

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func writeDirectoryIndexFixture(t testing.TB, count int) (string, string) {
	t.Helper()
	dir, scratch := t.TempDir(), t.TempDir()
	file, err := os.Create(segmentManifestPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.WriteString("{ \n\"version\": 1, \"segments\": [\n"); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= count; i++ {
		if i > 1 {
			if _, err = file.WriteString(", \n"); err != nil {
				t.Fatal(err)
			}
		}
		if _, err = fmt.Fprintf(file, ` { "generation": %d, "file": "ledger-%d.wal", "first_sequence": %d, "last_sequence": %d, "length": 100, "stored_length": 50 } `, i, i, i, i); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = file.WriteString("\n]}\n"); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	return dir, scratch
}

func TestSegmentDirectoryIndexOrdinalLookupAndOwnedCleanup(t *testing.T) {
	for _, count := range []int{0, 1, 10_000} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			dir, scratch := writeDirectoryIndexFixture(t, count)
			d, err := buildSegmentDirectoryIndex(dir, scratch)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			if d.summary != (SegmentSummary{Generations: uint64(count), PlainBytes: int64(count) * 100, StoredBytes: int64(count) * 50}) {
				t.Fatal(d.summary)
			}
			info, err := d.index.Stat()
			if err != nil || info.Size() != int64(count)*segmentDirectorySlotBytes {
				t.Fatal(info, err)
			}
			for _, generation := range []uint64{0, 1, uint64(count), uint64(count + 1)} {
				segment, found, err := d.Get(generation)
				want := generation > 0 && generation <= uint64(count)
				if err != nil || found != want || found && (segment.Generation != generation || segment.File != fmt.Sprintf("ledger-%d.wal", generation)) {
					t.Fatalf("generation=%d segment=%+v found=%t err=%v", generation, segment, found, err)
				}
			}
			if err := d.Close(); err != nil {
				t.Fatal(err)
			}
			if _, _, err := d.Get(1); err == nil {
				t.Fatal("closed directory returned data")
			}
			entries, err := os.ReadDir(scratch)
			if err != nil || len(entries) != 0 {
				t.Fatal("owned index leaked", entries, err)
			}
			if _, err := os.Stat(segmentManifestPath(dir)); err != nil {
				t.Fatal("cleanup removed native manifest", err)
			}
		})
	}
}

func TestSegmentDirectoryIndexFindsFirstCompactableFromAuthenticatedHints(t *testing.T) {
	dir, scratch := t.TempDir(), t.TempDir()
	manifest := segmentManifest{Segments: []Segment{
		{Generation: 1, File: "ledger-1.wal", FirstSequence: 1, LastSequence: 5, Compressed: true},
		{Generation: 2, File: "ledger-2.wal", FirstSequence: 6, LastSequence: 10},
		{Generation: 3, File: "ledger-3.wal", FirstSequence: 11, LastSequence: 20},
	}}
	if err := saveSegmentManifest(dir, manifest); err != nil {
		t.Fatal(err)
	}
	d, err := buildSegmentDirectoryIndex(dir, scratch)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if generation, found, err := d.FindFirstUncompressedThrough(9); err != nil || found {
		t.Fatalf("below first sequence bound generation=%d found=%t err=%v", generation, found, err)
	}
	generation, found, err := d.FindFirstUncompressedThrough(10)
	if err != nil || !found || generation != 2 {
		t.Fatalf("candidate generation=%d found=%t err=%v", generation, found, err)
	}
	segment, found, err := d.Get(generation)
	if err != nil || !found || segment.Generation != generation || segment.Compressed || segment.LastSequence != 10 {
		t.Fatalf("candidate descriptor=%+v found=%t err=%v", segment, found, err)
	}

	if _, err := d.index.WriteAt([]byte{0xff}, segmentDirectorySlotBytes+48); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.FindFirstUncompressedThrough(10); !errors.Is(err, ErrTampered) {
		t.Fatalf("changed sequence hint error=%v", err)
	}
}

func TestSegmentDirectoryIndexFailureNeverPublishesPrefix(t *testing.T) {
	for _, raw := range []string{
		`{"version":1,"segments":[{"generation":1},{"generation":3}]}`,
		`{"segments":[{"generation":1}],"version":2}`,
		`{"version":1,"segments":[{"generation":1}],"extension":"` + strings.Repeat("x", replicationManifestValueLimit) + `"}`,
	} {
		dir, scratch := t.TempDir(), t.TempDir()
		if err := os.WriteFile(segmentManifestPath(dir), []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		d, err := buildSegmentDirectoryIndex(dir, scratch)
		if err == nil || d != nil {
			t.Fatal("invalid tail published candidate", d, err)
		}
		entries, err := os.ReadDir(scratch)
		if err != nil || len(entries) != 0 {
			t.Fatal("failed build leaked index", entries, err)
		}
	}
}

func TestSegmentDirectoryIndexRejectsChangedSlotsAndSource(t *testing.T) {
	for _, mode := range []string{"slot", "truncated index", "descriptor with restored timestamp", "replaced manifest"} {
		t.Run(mode, func(t *testing.T) {
			dir, scratch := writeDirectoryIndexFixture(t, 2)
			d, err := buildSegmentDirectoryIndex(dir, scratch)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			switch mode {
			case "slot":
				if _, err := d.index.WriteAt([]byte{0xff}, 0); err != nil {
					t.Fatal(err)
				}
			case "truncated index":
				if err := d.index.Truncate(0); err != nil {
					t.Fatal(err)
				}
			case "descriptor with restored timestamp":
				path := segmentManifestPath(dir)
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				raw = bytes.Replace(raw, []byte("ledger-1.wal"), []byte("edited-1.wal"), 1)
				if err := os.WriteFile(path, raw, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(path, d.manifest.info.ModTime(), d.manifest.info.ModTime()); err != nil {
					t.Fatal(err)
				}
			case "replaced manifest":
				path := segmentManifestPath(dir)
				if err := os.Rename(path, path+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(`{"version":1,"segments":[]}`), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if segment, found, err := d.Get(1); err == nil || found || segment != (Segment{}) {
				t.Fatalf("damaged directory returned data=%+v found=%t err=%v", segment, found, err)
			}
		})
	}
}

func TestSegmentDirectoryIndexCleanupPreservesReplacedPath(t *testing.T) {
	dir, scratch := writeDirectoryIndexFixture(t, 1)
	d, err := buildSegmentDirectoryIndex(dir, scratch)
	if err != nil {
		t.Fatal(err)
	}
	name := d.index.Name()
	if err := os.Rename(name, name+".owned"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte("unrelated replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err == nil {
		t.Fatal("replacement cleanup was not reported")
	}
	raw, err := os.ReadFile(name)
	if err != nil || string(raw) != "unrelated replacement" {
		t.Fatal("unrelated replacement removed", err)
	}
}

func TestSegmentDirectoryIndexConcurrentReadAndClose(t *testing.T) {
	dir, scratch := writeDirectoryIndexFixture(t, 100)
	d, err := buildSegmentDirectoryIndex(dir, scratch)
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for range 4 {
		group.Add(1)
		go func() {
			defer group.Done()
			for i := uint64(1); i <= 100; i++ {
				segment, found, err := d.Get(i)
				if err != nil {
					if errors.Is(err, errSegmentDirectoryBusy) {
						continue
					}
					if strings.Contains(err.Error(), "closed") {
						return
					}
					t.Error(err)
					return
				}
				if !found || segment.Generation != i {
					t.Errorf("wrong generation: %+v %t", segment, found)
					return
				}
			}
		}()
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	group.Wait()
}

func TestSegmentDirectoryIndexRetainsPendingWithoutResolvingIt(t *testing.T) {
	dir, scratch := t.TempDir(), t.TempDir()
	if err := os.WriteFile(segmentManifestPath(dir), []byte(`{"version":1,"segments":[],"pending":{"generation":1,"file":"missing.wal"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := buildSegmentDirectoryIndex(dir, scratch)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if d.parsed.pending == nil || d.parsed.pending.Generation != 1 || d.parsed.count != 0 {
		t.Fatal("pending promoted or dropped")
	}
	if _, found, err := d.Get(1); err != nil || found {
		t.Fatal("pending treated as committed", found, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "missing.wal")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("index resolved native filesystem", err)
	}
}

var segmentDirectoryBenchmarkSink Segment

func BenchmarkSegmentDirectoryIndexLookup(b *testing.B) {
	for _, count := range []int{100, 10_000} {
		dir, scratch := writeDirectoryIndexFixture(b, count)
		d, err := buildSegmentDirectoryIndex(dir, scratch)
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() {
			if err := d.Close(); err != nil {
				b.Error(err)
			}
		})
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				segment, found, err := d.Get(uint64(count))
				if err != nil || !found {
					b.Fatal(found, err)
				}
				segmentDirectoryBenchmarkSink = segment
			}
		})
	}
}

func TestSegmentDirectoryIndexWorkspaceAndCloseTrackActualReadExit(t *testing.T) {
	dir, scratch := writeDirectoryIndexFixture(t, 1)
	d, err := buildSegmentDirectoryIndex(dir, scratch)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	entered, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(resume) }) }
	defer release()
	done := make(chan error, 1)
	go func() {
		_, _, err := d.getWithRead(1, func(p []byte, offset int64) (int, error) {
			close(entered)
			<-resume
			return d.manifest.file.ReadAt(p, offset)
		})
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("read did not enter native source")
	}
	if _, _, err := d.Get(1); !errors.Is(err, errSegmentDirectoryBusy) {
		t.Fatalf("second read accepted workspace: %v", err)
	}
	closed := make(chan error, 1)
	go func() { closed <- d.Close() }()
	deadline := time.Now().Add(10 * time.Second)
	for !d.closed.Load() {
		if time.Now().After(deadline) {
			t.Fatal("Close did not withdraw reads")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-closed:
		t.Fatalf("Close returned before actual read exit: %v", err)
	default:
	}
	if _, _, err := d.Get(1); !errors.Is(err, errSegmentDirectoryClosed) {
		t.Fatal("Close did not reject new reads", err)
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("source read did not exit")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not join read")
	}
}
