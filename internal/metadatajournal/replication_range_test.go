package metadatajournal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/akz142857/Halro/internal/replicationrange"
)

func metadataRangeFixture(t testing.TB, count int, epoch uint64) (string, []byte, [][]byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "metadata.journal")
	key := testKey()
	log, err := StartEpoch(path, key, EpochHeader{Epoch: epoch, Reason: "range-test"})
	if err != nil {
		t.Fatal(err)
	}
	var frames [][]byte
	if err := log.SetAfterDurable(func(batch DurableBatch) (uint64, error) {
		frames = append(frames, append([]byte(nil), batch.Frames...))
		return uint64(len(frames)), nil
	}, func(context.Context, uint64) error { return nil }); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= count; i++ {
		if _, err := log.Append([]Op{put("projects", fmt.Sprintf("p_%d", i), "value")}); err != nil {
			t.Fatal(err)
		}
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	return path, key, frames
}

func metadataRangeReader(t testing.TB, path string, key []byte, limit int64) *ReplicationRangeReader {
	t.Helper()
	reader, err := NewReplicationRangeReader(path, key, limit)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reader.Close(); err != nil {
			t.Error(err)
		}
	})
	return reader
}

func assertMetadataRange(t *testing.T, reader *ReplicationRangeReader, frames [][]byte, epoch, first, last uint64) {
	t.Helper()
	got, err := reader.Read(epoch, first, last)
	want := bytes.Join(frames[first-1:last], nil)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("range %d/%d-%d exact=%t err=%v", epoch, first, last, bytes.Equal(got, want), err)
	}
}

func TestMetadataRangeCursorReadsExactOperationsWithEpochAndRealOffsets(t *testing.T) {
	path, key, frames := metadataRangeFixture(t, 5, 7)
	reader := metadataRangeReader(t, path, key, 16<<20)
	for _, span := range [][2]uint64{{1, 1}, {3, 3}, {4, 5}, {2, 4}, {5, 5}, {1, 2}} {
		assertMetadataRange(t, reader, frames, 7, span[0], span[1])
	}
	// Different legal JSON whitespace must change neither range boundaries nor
	// bytes returned. Re-encoding an Op is not a source-frame size calculation.
	headerPayload, err := encodePayload(KindEpoch, nil, EpochHeader{Epoch: 9, Reason: "noncanonical"}, TrimAnchor{})
	if err != nil {
		t.Fatal(err)
	}
	header, head := encodeFrame(key, KindEpoch, 9, 0, [32]byte{}, headerPayload)
	payload, err := encodePayload(KindOperations, []Op{put("projects", "p", "value")}, EpochHeader{}, TrimAnchor{})
	if err != nil {
		t.Fatal(err)
	}
	var spaced bytes.Buffer
	if err := json.Indent(&spaced, payload, "", "  "); err != nil {
		t.Fatal(err)
	}
	firstFrame, head := encodeFrame(key, KindOperations, 9, 1, head, spaced.Bytes())
	secondFrame, _ := encodeFrame(key, KindOperations, 9, 2, head, payload)
	if err := publish(path, bytes.Join([][]byte{header, firstFrame, secondFrame}, nil)); err != nil {
		t.Fatal(err)
	}
	for _, span := range [][2]uint64{{1, 1}, {2, 2}, {1, 2}} {
		assertMetadataRange(t, reader, [][]byte{firstFrame, secondFrame}, 9, span[0], span[1])
	}
}

func TestMetadataRangeCursorFollowsTrimCutHeadAndRejectsRemovedOperations(t *testing.T) {
	path, key, frames := metadataRangeFixture(t, 5, 1)
	reader := metadataRangeReader(t, path, key, 16<<20)
	assertMetadataRange(t, reader, frames, 1, 1, 2)
	if _, err := Trim(path, key, 3); err != nil {
		t.Fatal(err)
	}
	assertMetadataRange(t, reader, frames, 1, 4, 4)
	if _, err := reader.Read(1, 3, 4); err == nil {
		t.Fatal("removed range silently skipped")
	}
	assertMetadataRange(t, reader, frames, 1, 4, 5)
	if _, err := Trim(path, key, 5); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Read(1, 5, 5); err == nil {
		t.Fatal("all-trimmed file returned an operations frame")
	}
	log, err := Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	var sixth []byte
	if err := log.SetAfterDurable(func(batch DurableBatch) (uint64, error) { sixth = append([]byte(nil), batch.Frames...); return 1, nil }, func(context.Context, uint64) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append([]Op{put("projects", "six", "six")}); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	frames = append(frames, sixth)
	assertMetadataRange(t, reader, frames, 1, 6, 6)
}

func TestMetadataRangeCursorBindsEpochAcrossReplacementAndWrongRequests(t *testing.T) {
	path, key, frames := metadataRangeFixture(t, 2, 1)
	reader := metadataRangeReader(t, path, key, 16<<20)
	assertMetadataRange(t, reader, frames, 1, 1, 1)
	log, err := StartEpoch(path, key, EpochHeader{Epoch: 2, PreviousEpoch: 1, Reason: "replace"})
	if err != nil {
		t.Fatal(err)
	}
	var frame []byte
	if err := log.SetAfterDurable(func(batch DurableBatch) (uint64, error) { frame = append([]byte(nil), batch.Frames...); return 1, nil }, func(context.Context, uint64) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append([]Op{put("projects", "same-sequence", "new-epoch")}); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	for _, epoch := range []uint64{1, 0, 3} {
		if _, err := reader.Read(epoch, 1, 1); err == nil {
			t.Fatal("wrong epoch accepted", epoch)
		}
		assertMetadataRange(t, reader, [][]byte{frame}, 2, 1, 1)
	}
}

func TestMetadataRangeCursorRefusesAuthenticatedButInvalidPlacementAndOperations(t *testing.T) {
	for _, stage := range []string{"headerless", "mid-header", "mid-trim", "wrong-epoch", "gap", "invalid-op", "broken-trim-chain"} {
		t.Run(stage, func(t *testing.T) {
			path, key, frames := metadataRangeFixture(t, 2, 1)
			contents, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			header := contents[:len(contents)-len(frames[0])-len(frames[1])]
			var prefix []byte
			payload, err := encodePayload(KindOperations, []Op{put("projects", "different", "value")}, EpochHeader{}, TrimAnchor{})
			if err != nil {
				t.Fatal(err)
			}
			head, err := ReplicationHeadAt(path, key, 1, 1)
			if err != nil {
				t.Fatal(err)
			}
			switch stage {
			case "headerless":
				contents = bytes.Join(frames, nil)
			case "mid-header":
				contents = bytes.Join([][]byte{header, frames[0], header, frames[1]}, nil)
			case "mid-trim", "broken-trim-chain":
				trimPayload, err := encodePayload(KindTrim, nil, EpochHeader{}, TrimAnchor{Epoch: 1, TrimmedThrough: 1, ChainHeadAtTrim: bytes.Repeat([]byte{7}, 32), TrimmedFrameCount: 1, TrimmedByteCount: int64(len(header) + len(frames[0])), RetainedFromSeqMin: 2})
				if err != nil {
					t.Fatal(err)
				}
				anchor, _ := encodeFrame(key, KindTrim, 1, 1, [32]byte{}, trimPayload)
				if stage == "mid-trim" {
					contents = bytes.Join([][]byte{header, frames[0], anchor, frames[1]}, nil)
				} else {
					contents = append(anchor, frames[1]...)
				}
			case "wrong-epoch":
				prefix, _ = encodeFrame(key, KindOperations, 2, 2, head, payload)
			case "gap":
				prefix, _ = encodeFrame(key, KindOperations, 1, 3, head, payload)
			case "invalid-op":
				prefix, _ = encodeFrame(key, KindOperations, 1, 2, head, []byte(`[{"kind":255}]`))
			}
			if prefix != nil {
				contents = bytes.Join([][]byte{header, frames[0], prefix}, nil)
			}
			if err := os.WriteFile(path, contents, 0o600); err != nil {
				t.Fatal(err)
			}
			reader := metadataRangeReader(t, path, key, 16<<20)
			if _, err := reader.Read(1, 2, 2); err == nil {
				t.Fatal("invalid native semantics accepted")
			}
		})
	}
}

func TestMetadataRangeCursorExcludesLaterPartialAndCorruptTail(t *testing.T) {
	for _, stage := range []string{"partial", "corrupt"} {
		t.Run(stage, func(t *testing.T) {
			path, key, frames := metadataRangeFixture(t, 3, 1)
			contents, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			bad := append([]byte(nil), contents...)
			if stage == "partial" {
				bad = bad[:len(bad)-len(frames[2])/2]
			} else {
				bad[len(bad)-1] ^= 1
			}
			if err := os.WriteFile(path, bad, 0o600); err != nil {
				t.Fatal(err)
			}
			reader := metadataRangeReader(t, path, key, 16<<20)
			assertMetadataRange(t, reader, frames, 1, 1, 2)
			if _, err := ReadReplicationFrames(path, key, 1, 1, 2); err == nil {
				t.Fatal("full audit accepted bad physical tail")
			}
			if _, err := reader.Read(1, 3, 3); err == nil {
				t.Fatal("bad requested frame accepted")
			}
			if err := os.WriteFile(path, contents, 0o600); err != nil {
				t.Fatal(err)
			}
			assertMetadataRange(t, reader, frames, 1, 3, 3)
		})
	}
}

func TestMetadataRangeCursorChecksReturnedCopyAndPathChanges(t *testing.T) {
	for _, stage := range []string{"copy", "path"} {
		t.Run(stage, func(t *testing.T) {
			path, key, frames := metadataRangeFixture(t, 2, 1)
			reader := &ReplicationRangeReader{epoch: 1}
			changed := false
			inner, err := replicationrange.New(path, key, 16<<20, func(input io.ReadSeeker, key []byte, initial replicationrange.Position, first, last uint64) (replicationrange.Range, error) {
				span, err := reader.scan(input, key, initial, first, last)
				if err == nil && !changed {
					changed = true
					contents, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					if stage == "copy" {
						contents[len(contents)-len(frames[1])-1] ^= 1
						if err := os.WriteFile(path, contents, 0o600); err != nil {
							t.Fatal(err)
						}
					} else {
						if err := publish(path, contents); err != nil {
							t.Fatal(err)
						}
					}
				}
				return span, err
			})
			if err != nil {
				t.Fatal(err)
			}
			reader.reader = inner
			defer reader.Close()
			if _, err := reader.Read(1, 1, 1); err == nil {
				t.Fatal("source changed between locate and copy was accepted")
			}
		})
	}
}

func TestMetadataRangeCursorRechecksChangedPrefixesAfterBackwardResetOrShrink(t *testing.T) {
	for _, stage := range []string{"hot-prefix", "shrink"} {
		t.Run(stage, func(t *testing.T) {
			path, key, frames := metadataRangeFixture(t, 3, 1)
			contents, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			firstEnd := len(contents) - len(frames[1]) - len(frames[2])
			bad := append([]byte(nil), contents...)
			bad[firstEnd-1] ^= 1
			reader := metadataRangeReader(t, path, key, 16<<20)
			if stage == "shrink" {
				assertMetadataRange(t, reader, frames, 1, 1, 2)
				if err := os.WriteFile(path, bad[:firstEnd], 0o600); err != nil {
					t.Fatal(err)
				}
				if _, err := reader.Read(1, 3, 3); err == nil {
					t.Fatal("shrunk file retained prefix authority")
				}
				return
			}
			assertMetadataRange(t, reader, frames, 1, 1, 1)
			if err := os.WriteFile(path, bad, 0o600); err != nil {
				t.Fatal(err)
			}
			// Forward reads trust the already authenticated immutable prefix;
			// this success is not a whole-file integrity claim.
			assertMetadataRange(t, reader, frames, 1, 2, 2)
			if _, err := ReadReplicationFrames(path, key, 1, 2, 2); err == nil {
				t.Fatal("full audit missed old prefix tamper")
			}
			if _, err := reader.Read(1, 1, 1); err == nil {
				t.Fatal("backward read skipped old prefix")
			}
			if _, err := reader.Read(1, 3, 3); err == nil {
				t.Fatal("failed read retained old prefix")
			}
			if err := os.WriteFile(path, contents, 0o600); err != nil {
				t.Fatal(err)
			}
			assertMetadataRange(t, reader, frames, 1, 1, 1)
			if err := os.WriteFile(path, bad, 0o600); err != nil {
				t.Fatal(err)
			}
			reader.Reset()
			if _, err := reader.Read(1, 2, 2); err == nil {
				t.Fatal("explicit Reset retained old prefix")
			}
		})
	}
}

func TestMetadataRangeCursorBoundsOutputAndSerializesEpochsWithClose(t *testing.T) {
	path, key, frames := metadataRangeFixture(t, 3, 1)
	small := metadataRangeReader(t, path, key, int64(len(frames[0])))
	if _, err := small.Read(1, 1, 2); err == nil {
		t.Fatal("output byte limit ignored")
	}
	assertMetadataRange(t, small, frames, 1, 1, 1)
	reader := metadataRangeReader(t, path, key, 16<<20)
	var workers sync.WaitGroup
	for peer := 0; peer < 4; peer++ {
		workers.Add(1)
		go func(peer int) {
			defer workers.Done()
			for i := 0; i < 20; i++ {
				epoch := uint64(peer%2 + 1)
				index := uint64(i%3 + 1)
				got, err := reader.Read(epoch, index, index)
				if epoch == 2 && err == nil {
					t.Error("wrong epoch accepted concurrently")
				}
				if epoch == 1 && err != nil && !errors.Is(err, replicationrange.ErrClosed) {
					t.Error(err)
				}
				if err == nil && !bytes.Equal(got, frames[index-1]) {
					t.Error("different returned frame")
				}
			}
		}(peer)
	}
	workers.Add(1)
	go func() { defer workers.Done(); reader.Close() }()
	workers.Wait()
	if _, err := reader.Read(1, 1, 1); !errors.Is(err, replicationrange.ErrClosed) {
		t.Fatal(err)
	}
}

type metadataMeasuredInput struct {
	io.ReadSeeker
	count *int64
}

func (r metadataMeasuredInput) Read(b []byte) (int, error) {
	n, err := r.ReadSeeker.Read(b)
	*r.count += int64(n)
	return n, err
}

func TestMetadataRangeCursorScansForwardHistoryOnceFromDisk(t *testing.T) {
	path, key, frames := metadataRangeFixture(t, 100, 1)
	reader := &ReplicationRangeReader{epoch: 1}
	var scanned int64
	inner, err := replicationrange.New(path, key, 16<<20, func(input io.ReadSeeker, key []byte, initial replicationrange.Position, first, last uint64) (replicationrange.Range, error) {
		if _, ok := input.(*io.SectionReader); ok {
			input = metadataMeasuredInput{ReadSeeker: input, count: &scanned}
		}
		return reader.scan(input, key, initial, first, last)
	})
	if err != nil {
		t.Fatal(err)
	}
	reader.reader = inner
	defer reader.Close()
	for i := uint64(1); i <= 100; i++ {
		assertMetadataRange(t, reader, frames, 1, i, i)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if scanned != info.Size() {
		t.Fatalf("disk authentication scanned %d bytes, want %d", scanned, info.Size())
	}
}

func BenchmarkMetadataReplicationRangeRecovery(b *testing.B) {
	for _, count := range []int{100, 1000} {
		path, key, frames := metadataRangeFixture(b, count, 1)
		b.Run(fmt.Sprintf("FullScan/%d", count), func(b *testing.B) {
			b.ReportAllocs()
			for n := 0; n < b.N; n++ {
				for i := 1; i <= count; i++ {
					got, err := ReadReplicationFrames(path, key, 1, uint64(i), uint64(i))
					if err != nil || !bytes.Equal(got, frames[i-1]) {
						b.Fatal(err)
					}
				}
			}
		})
		b.Run(fmt.Sprintf("PrefixCursor/%d", count), func(b *testing.B) {
			b.ReportAllocs()
			for n := 0; n < b.N; n++ {
				reader, err := NewReplicationRangeReader(path, key, 16<<20)
				if err != nil {
					b.Fatal(err)
				}
				for i := 1; i <= count; i++ {
					got, err := reader.Read(1, uint64(i), uint64(i))
					if err != nil || !bytes.Equal(got, frames[i-1]) {
						b.Fatal(err)
					}
				}
				reader.Close()
			}
		})
	}
}
