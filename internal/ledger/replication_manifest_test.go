package ledger

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestReplicationManifestLookupMatchesExistingLayoutResolution(t *testing.T) {
	directory := t.TempDir()
	segments := make([]Segment, 12)
	for i := range segments {
		segments[i] = Segment{Generation: uint64(i + 1), File: fmt.Sprintf("ledger-%d.wal", i+1), FirstSequence: uint64(i*3 + 1), LastSequence: uint64(i*3 + 3), Length: 1234, StoredLength: 1234, EndEpoch: 4, PlainChecksum: strings.Repeat("a", 64), StoredChecksum: strings.Repeat("a", 64)}
	}
	if err := saveSegmentManifest(directory, segmentManifest{Segments: segments}); err != nil {
		t.Fatal(err)
	}
	old, _, err := resolveSegments(directory)
	if err != nil {
		t.Fatal(err)
	}
	for gen := uint64(1); gen <= 13; gen++ {
		result, err := findReplicationGeneration(directory, gen)
		if err != nil || result.active != activeGeneration(old) || result.pending {
			t.Fatal(result, err)
		}
		if gen < 13 {
			if !result.sealed || !reflect.DeepEqual(result.segment, old[gen-1]) {
				t.Fatal("descriptor differs", gen, result)
			}
		} else if result.sealed {
			t.Fatal("active generation became sealed")
		}
	}
	if _, err := findReplicationGeneration(directory, 14); err == nil {
		t.Fatal("absent generation accepted")
	}
}

func TestReplicationManifestLookupPreservesPendingRenameVerdicts(t *testing.T) {
	for _, stage := range []string{"before", "after", "bad-length", "bad-checksum", "bad-generation"} {
		t.Run(stage, func(t *testing.T) {
			directory := t.TempDir()
			contents := []byte("durable-pending-ledger")
			digest := sha256.Sum256(contents)
			pending := Segment{Generation: 1, File: "ledger-1.wal", Length: int64(len(contents)), StoredLength: int64(len(contents)), PlainChecksum: hex.EncodeToString(digest[:])}
			if stage == "bad-generation" {
				pending.Generation = 2
			}
			if stage != "before" {
				if stage == "bad-length" {
					contents = contents[:len(contents)-1]
				}
				if stage == "bad-checksum" {
					contents[0] ^= 1
				}
				if err := os.WriteFile(filepath.Join(directory, pending.File), contents, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := saveSegmentManifest(directory, segmentManifest{Pending: &pending}); err != nil {
				t.Fatal(err)
			}
			old, oldPending, oldErr := resolveSegments(directory)
			result, err := findReplicationGeneration(directory, 1)
			if (err != nil) != (oldErr != nil) {
				t.Fatal("pending error verdict changed", err, oldErr)
			}
			if err == nil && (result.active != activeGeneration(old) || result.pending != oldPending || result.sealed != (len(old) == 1)) {
				t.Fatal(result, old)
			}
		})
	}
}

func TestReplicationManifestLookupValidatesPastTargetAndBoundsEveryValue(t *testing.T) {
	for _, raw := range []string{
		`{"version":1,"segments":[{"generation":1},{"generation":3}]}`,
		`{"segments":[{"generation":1}],"version":2}`,
		`{"version":1,"segments":[{"generation":1}],"pending":{"generation":9}}`,
		`{"version":1,"segments":[{"generation":1}],"extension":"` + strings.Repeat("x", replicationManifestValueLimit) + `"}`,
		`{"version":1,"segments":[{"generation":1,"file":"` + strings.Repeat("x", replicationManifestValueLimit) + `"}]}`,
		`{"version":1,"segments":[{"generation":1},]}`,
		`{"version":1,"segments":[{"generation":1}],}`,
		`{"version":1,"segments":[{"generation":1}]} {}`,
		`{"version":1,"segments":[{"generation":1}],"version":1}`,
		`{"version":1,"segments":[{"generation":1}],"segments":[]}`,
		`{"version":1,"segments":[{"generation":1}],"pending":null,"pending":null}`,
		`{null:0,"version":1,"segments":[{"generation":1}]}`,
		`{"version":1,"segments":[{"generation":1}],"extension":[}`,
		`{"version":1,"segments":[{"generation":1}],"extension":`,
	} {
		t.Run(fmt.Sprintf("case-%x", sha256.Sum256([]byte(raw))), func(t *testing.T) {
			directory := t.TempDir()
			if err := os.WriteFile(segmentManifestPath(directory), []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := findReplicationGeneration(directory, 1); err == nil {
				t.Fatal("target hit hid invalid or oversized later data")
			}
		})
	}
}

func TestReplicationManifestLookupAcceptsSupportedEmptyAndEscapedExtensions(t *testing.T) {
	for _, raw := range []string{
		`{"version":1}`,
		`{"version":1,"segments":null,"pending":null}`,
		`{"version":1,"segments":[]}`,
		`{"extension":{"array":["\\\"} ]",{"x":true}],"nested":{"a":null}},"version":1,"segments":[]}`,
	} {
		directory := t.TempDir()
		if !json.Valid([]byte(raw)) {
			t.Fatal("invalid fixture", raw)
		}
		if err := os.WriteFile(segmentManifestPath(directory), []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		result, err := findReplicationGeneration(directory, 1)
		if err != nil || result.active != 1 || result.sealed {
			t.Fatal(result, err)
		}
	}
	directory := t.TempDir()
	if result, err := findReplicationGeneration(directory, 1); err != nil || result.active != 1 {
		t.Fatal(result, err)
	}
}

func BenchmarkReplicationManifestLookupGrowth(b *testing.B) {
	for _, count := range []int{100, 10000} {
		directory := b.TempDir()
		file, err := os.Create(segmentManifestPath(directory))
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
		b.Run(fmt.Sprintf("Collecting/%d", count), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				segments, _, err := resolveSegments(directory)
				if err != nil || len(segments) != count {
					b.Fatal(err)
				}
			}
		})
		b.Run(fmt.Sprintf("Streaming/%d", count), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				result, err := findReplicationGeneration(directory, uint64(count))
				if err != nil || !result.sealed || result.segment.Generation != uint64(count) {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestManifestFramingPreservesRawJSONWithinFixedLimit(t *testing.T) {
	for _, value := range []string{`""`, `"a\\\"b"`, `null`, `123`, `-2`, `true`, `[]`, `{}`, `{"x":[{},"}",false]}`} {
		input := bufio.NewReader(strings.NewReader(value + ","))
		got, err := manifestValue(input)
		if err != nil || !bytes.Equal(got, []byte(value)) {
			t.Fatal(string(got), err)
		}
		if c, err := input.ReadByte(); err != nil || c != ',' {
			t.Fatal("value consumed separator", c, err)
		}
	}
}
