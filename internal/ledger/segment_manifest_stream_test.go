package ledger

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/durable"
)

type manifestProbeWriter struct {
	maxWrite, total int
	limit           int
}

func (w *manifestProbeWriter) Write(p []byte) (int, error) {
	if len(p) > w.maxWrite {
		w.maxWrite = len(p)
	}
	w.total += len(p)
	if w.limit > 0 && len(p) > w.limit {
		return 0, errors.New("oversized IO request")
	}
	return len(p), nil
}

type manifestProbeReader struct {
	source  io.Reader
	maxRead int
}

func (r *manifestProbeReader) Read(p []byte) (int, error) {
	if len(p) > r.maxRead {
		r.maxRead = len(p)
	}
	return r.source.Read(p)
}

func TestSegmentManifestStreamingIOAndLegacyRoundTrip(t *testing.T) {
	for _, count := range []int{0, 100, 10_000} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			manifest := segmentManifest{Version: segmentManifestVersion, Segments: make([]Segment, count)}
			for i := range manifest.Segments {
				manifest.Segments[i] = Segment{Generation: uint64(i + 1), File: fmt.Sprintf("ledger-%d.wal", i+1), Length: 123, StoredLength: 123}
			}
			manifest.Pending = &Segment{Generation: uint64(count + 1), File: "pending.wal"}
			var encoded bytes.Buffer
			if err := writeSegmentManifest(&encoded, manifest); err != nil {
				t.Fatal(err)
			}
			var legacy segmentManifest
			if err := json.Unmarshal(encoded.Bytes(), &legacy); err != nil || !reflect.DeepEqual(legacy, manifest) {
				t.Fatalf("legacy=%+v err=%v", legacy, err)
			}
			probe := &manifestProbeWriter{limit: replicationManifestValueLimit}
			if err := writeSegmentManifest(probe, manifest); err != nil {
				t.Fatal(err)
			}
			if probe.total != encoded.Len() || probe.maxWrite > replicationManifestValueLimit {
				t.Fatal("writer used whole-history IO", probe)
			}
			reader := &manifestProbeReader{source: bytes.NewReader(encoded.Bytes())}
			var visited uint64
			parsed, err := parseSegmentManifest(reader, func(segment Segment) error {
				visited++
				if segment.Generation != visited {
					return errors.New("unordered descriptor")
				}
				return nil
			})
			if err != nil || parsed.count != uint64(count) || visited != uint64(count) || reader.maxRead > replicationManifestValueLimit {
				t.Fatalf("parse=%+v visits=%d maxRead=%d err=%v", parsed, visited, reader.maxRead, err)
			}
		})
	}
	// nil and non-nil empty arrays preserve the old marshaled shape.
	for _, segments := range [][]Segment{nil, {}} {
		dir := t.TempDir()
		original := segmentManifest{Version: segmentManifestVersion, Segments: segments}
		if err := saveSegmentManifest(dir, original); err != nil {
			t.Fatal(err)
		}
		loaded, err := loadSegmentManifest(dir)
		if err != nil || !reflect.DeepEqual(loaded, original) {
			t.Fatalf("loaded=%+v original=%+v err=%v", loaded, original, err)
		}
	}
}

func TestSegmentManifestStreamingRejectsInvalidTailAndNeverReturnsPartial(t *testing.T) {
	for _, raw := range []string{
		`{"version":1,"segments":[{"generation":1},{"generation":3}]}`,
		`{"version":1,"segments":[{"generation":1}],"pending":{"generation":3}}`,
		`{"version":1,"segments":[{"generation":1}],"version":1}`,
		`{"version":1,"segments":[{"generation":1}],"extension":"` + strings.Repeat("x", replicationManifestValueLimit) + `"}`,
		`{"version":1,"segments":[{"generation":1}]} {}`,
		`{"version":1,"segments":[{"generation":1},]}`,
	} {
		dir := t.TempDir()
		if err := os.WriteFile(segmentManifestPath(dir), []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := loadSegmentManifest(dir)
		if err == nil || !reflect.DeepEqual(got, segmentManifest{}) {
			t.Fatalf("invalid tail returned partial manifest=%+v err=%v", got, err)
		}
	}
}

func TestSegmentManifestStreamingEncodingFailurePreservesPublishedFile(t *testing.T) {
	dir := t.TempDir()
	original := segmentManifest{Segments: []Segment{{Generation: 1, File: "ledger-1.wal"}}}
	if err := saveSegmentManifest(dir, original); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(segmentManifestPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	for _, oversized := range []string{strings.Repeat("x", replicationManifestValueLimit+1), strings.Repeat("\x00", replicationManifestValueLimit/2)} {
		manifest := segmentManifest{Segments: []Segment{{Generation: 1, File: oversized}}}
		renamed, err := saveSegmentManifestWithSync(dir, manifest, durable.SyncDirectory)
		if renamed || !errors.Is(err, ErrCorrupt) {
			t.Fatalf("renamed=%t err=%v", renamed, err)
		}
		after, err := os.ReadFile(segmentManifestPath(dir))
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("encoding failure replaced manifest", err)
		}
		if _, err := os.Stat(filepath.Join(dir, segmentManifestName+segmentTempSuffix)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("failed encoder left temporary file", err)
		}
	}
}

func BenchmarkSegmentManifestStreamingPublish(b *testing.B) {
	for _, count := range []int{100, 10_000} {
		manifest := segmentManifest{Segments: make([]Segment, count)}
		for i := range manifest.Segments {
			manifest.Segments[i] = Segment{Generation: uint64(i + 1), File: "ledger.wal", Length: 100, StoredLength: 100}
		}
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if err := writeSegmentManifest(io.Discard, manifest); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
