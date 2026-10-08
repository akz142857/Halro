package audit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/akz142857/Halro/internal/replicationrange"
)

func rangeFixture(t testing.TB, count int) (string, []byte, [][]byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "native.log")
	key := bytes.Repeat([]byte{0x31}, 32)
	var frames [][]byte
	log, err := OpenWithOptions(path, key, Options{AfterDurable: func(batch DurableBatch) error {
		frames = append(frames, append([]byte(nil), batch.Frames...))
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= count; i++ {
		if _, err := log.Append(context.Background(), validEvent(i, "range")); err != nil {
			t.Fatal(err)
		}
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	return path, key, frames
}

func mustRangeReader(t testing.TB, path string, key []byte, limit int64) *replicationrange.Reader {
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

func checkRange(t *testing.T, reader *replicationrange.Reader, frames [][]byte, first, last uint64) {
	t.Helper()
	got, err := reader.Read(first, last)
	want := bytes.Join(frames[first-1:last], nil)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("range %d-%d exact=%t err=%v", first, last, bytes.Equal(got, want), err)
	}
}

func TestAuditRangeCursorAuthenticatesForwardSkippedRepeatedAndOverlappingFrames(t *testing.T) {
	path, key, frames := rangeFixture(t, 5)
	reader := mustRangeReader(t, path, key, 16<<20)
	for _, span := range [][2]uint64{{1, 1}, {3, 3}, {4, 5}, {3, 4}, {5, 5}, {1, 2}} {
		checkRange(t, reader, frames, span[0], span[1])
	}
}

func TestAuditRangeCursorExcludesLaterPartialOrCorruptTail(t *testing.T) {
	for _, kind := range []string{"partial", "corrupt"} {
		t.Run(kind, func(t *testing.T) {
			path, key, frames := rangeFixture(t, 3)
			physical := bytes.Join(frames[:2], nil)
			if kind == "partial" {
				physical = append(physical, frames[2][:len(frames[2])/2]...)
			} else {
				tail := append([]byte(nil), frames[2]...)
				tail[len(tail)-1] ^= 1
				physical = append(physical, tail...)
			}
			if err := os.WriteFile(path, physical, 0600); err != nil {
				t.Fatal(err)
			}
			reader := mustRangeReader(t, path, key, 16<<20)
			checkRange(t, reader, frames, 1, 2)
			if _, err := ReadReplicationFrames(path, key, 1, 2); err == nil {
				t.Fatal("whole-file audit accepted bad physical tail")
			}
			if _, err := reader.Read(3, 3); err == nil {
				t.Fatal("requested bad frame accepted")
			}
			if err := os.WriteFile(path, bytes.Join(frames, nil), 0600); err != nil {
				t.Fatal(err)
			}
			checkRange(t, reader, frames, 3, 3)
		})
	}
}

func TestAuditRangeCursorTrustsPriorPrefixUntilResetOrBackwardRead(t *testing.T) {
	for _, kind := range []string{"in-place", "truncate-and-regrow"} {
		t.Run(kind, func(t *testing.T) {
			path, key, frames := rangeFixture(t, 3)
			reader := mustRangeReader(t, path, key, 16<<20)
			checkRange(t, reader, frames, 1, 1)
			tampered := bytes.Join(frames, nil)
			tampered[len(frames[0])-1] ^= 1
			if kind == "truncate-and-regrow" {
				if err := os.Truncate(path, 0); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(path, tampered, 0600); err != nil {
				t.Fatal(err)
			}
			// This intentionally documents the process-local immutable-prefix trust
			// boundary. It is not a current whole-file integrity audit.
			checkRange(t, reader, frames, 2, 2)
			if _, err := ReadReplicationFrames(path, key, 2, 2); err == nil {
				t.Fatal("full audit missed history tamper")
			}
			if _, err := reader.Read(1, 1); err == nil {
				t.Fatal("backward read missed history tamper")
			}
			if _, err := reader.Read(3, 3); err == nil {
				t.Fatal("failed read retained suspect cursor")
			}
			if err := os.WriteFile(path, bytes.Join(frames, nil), 0600); err != nil {
				t.Fatal(err)
			}
			checkRange(t, reader, frames, 1, 1)
			if err := os.WriteFile(path, tampered, 0600); err != nil {
				t.Fatal(err)
			}
			reader.Reset()
			if _, err := reader.Read(2, 2); err == nil {
				t.Fatal("reset reused old prefix")
			}
		})
	}
}

func TestAuditRangeCursorResetsForReplacementAndShrink(t *testing.T) {
	for _, kind := range []string{"replacement", "shrink"} {
		t.Run(kind, func(t *testing.T) {
			path, key, frames := rangeFixture(t, 3)
			reader := mustRangeReader(t, path, key, 16<<20)
			checkRange(t, reader, frames, 1, 2)
			bad := append([]byte(nil), frames[0]...)
			bad[len(bad)-1] ^= 1
			if kind == "replacement" {
				replacement := path + ".new"
				if err := os.WriteFile(replacement, append(bad, bytes.Join(frames[1:], nil)...), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(replacement, path); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(path, bad, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := reader.Read(3, 3); err == nil {
				t.Fatal("changed source reused prefix authority")
			}
		})
	}
}

func TestAuditRangeCursorChecksReturnedCopyAndChangedPath(t *testing.T) {
	for _, kind := range []string{"changed-copy", "changed-path"} {
		t.Run(kind, func(t *testing.T) {
			path, key, frames := rangeFixture(t, 2)
			mutated := false
			reader, err := replicationrange.New(path, key, 16<<20, func(input io.ReadSeeker, key []byte, initial replicationrange.Position, first, last uint64) (replicationrange.Range, error) {
				span, err := scanReplicationRange(input, key, initial, first, last)
				if err == nil && !mutated {
					mutated = true
					if kind == "changed-copy" {
						contents := bytes.Join(frames, nil)
						contents[len(frames[0])-1] ^= 1
						if err := os.WriteFile(path, contents, 0600); err != nil {
							t.Fatal(err)
						}
					} else {
						replacement := path + ".new"
						if err := os.WriteFile(replacement, bytes.Join(frames, nil), 0600); err != nil {
							t.Fatal(err)
						}
						if err := os.Rename(replacement, path); err != nil {
							t.Fatal(err)
						}
					}
				}
				return span, err
			})
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			if _, err := reader.Read(1, 1); err == nil {
				t.Fatal("source changed between authentication and copy was accepted")
			}
		})
	}
}

func TestAuditRangeCursorFreezesLengthBeforeConcurrentAppend(t *testing.T) {
	path, key, frames := rangeFixture(t, 3)
	if err := os.WriteFile(path, bytes.Join(frames[:2], nil), 0600); err != nil {
		t.Fatal(err)
	}
	appended := false
	reader, err := replicationrange.New(path, key, 16<<20, func(input io.ReadSeeker, key []byte, initial replicationrange.Position, first, last uint64) (replicationrange.Range, error) {
		if !appended {
			appended = true
			file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.Write(frames[2]); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
		}
		return scanReplicationRange(input, key, initial, first, last)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, err := reader.Read(3, 3); err == nil {
		t.Fatal("range read escaped frozen file length")
	}
	checkRange(t, reader, frames, 3, 3)
}

func TestAuditRangeCursorBoundsOutputAndSerializesReadersWithClose(t *testing.T) {
	path, key, frames := rangeFixture(t, 5)
	small := mustRangeReader(t, path, key, int64(len(frames[0])))
	if _, err := small.Read(1, 2); err == nil {
		t.Fatal("range byte limit ignored")
	}
	checkRange(t, small, frames, 1, 1)
	reader := mustRangeReader(t, path, key, 16<<20)
	var group sync.WaitGroup
	for peer := 0; peer < 4; peer++ {
		group.Add(1)
		go func(peer int) {
			defer group.Done()
			for i := 0; i < 20; i++ {
				index := uint64((i+peer)%5 + 1)
				got, err := reader.Read(index, index)
				if err != nil && !errors.Is(err, replicationrange.ErrClosed) {
					t.Error(err)
				}
				if err == nil && !bytes.Equal(got, frames[index-1]) {
					t.Error("concurrent reader returned different bytes")
				}
			}
		}(peer)
	}
	group.Add(1)
	go func() { defer group.Done(); reader.Close() }()
	group.Wait()
	if _, err := reader.Read(1, 1); err == nil {
		t.Fatal("closed reader accepted work")
	}
	if _, err := small.Read(0, 1); err == nil {
		t.Fatal("invalid range accepted")
	}
}

type measuredRangeInput struct {
	io.ReadSeeker
	count *int64
}

func (r measuredRangeInput) Read(b []byte) (int, error) {
	n, err := r.ReadSeeker.Read(b)
	*r.count += int64(n)
	return n, err
}

func TestAuditRangeCursorReadsForwardHistoryOnlyOnceFromDisk(t *testing.T) {
	path, key, frames := rangeFixture(t, 100)
	var diskRead int64
	reader, err := replicationrange.New(path, key, 16<<20, func(input io.ReadSeeker, key []byte, initial replicationrange.Position, first, last uint64) (replicationrange.Range, error) {
		if _, ok := input.(*io.SectionReader); ok {
			input = measuredRangeInput{ReadSeeker: input, count: &diskRead}
		}
		return scanReplicationRange(input, key, initial, first, last)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	for i := 1; i <= len(frames); i++ {
		checkRange(t, reader, frames, uint64(i), uint64(i))
	}
	var want int64
	for _, frame := range frames {
		want += int64(len(frame))
	}
	if diskRead != want {
		t.Fatalf("native disk scan read %d bytes, want one history %d", diskRead, want)
	}
}

func BenchmarkAuditReplicationRangeRecovery(b *testing.B) {
	for _, count := range []int{100, 1000} {
		path, key, frames := rangeFixture(b, count)
		b.Run(fmt.Sprintf("FullScan/%d", count), func(b *testing.B) {
			b.ReportAllocs()
			for n := 0; n < b.N; n++ {
				for i := 1; i <= count; i++ {
					got, err := ReadReplicationFrames(path, key, uint64(i), uint64(i))
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
					got, err := reader.Read(uint64(i), uint64(i))
					if err != nil || !bytes.Equal(got, frames[i-1]) {
						b.Fatal(err)
					}
				}
				reader.Close()
			}
		})
	}
}
