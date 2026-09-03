package governance

// This package is an S0-only executable design probe. The framing below is not
// a proposed production format. It measures the lower-bound cost of an
// independently authenticated Outcome stream and its current-head read model
// before any production schema or Ledger epoch changes.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
)

const (
	s0JournalVersion = uint32(1)
	s0PayloadSize    = 33
	s0DigestSize     = sha256.Size
	s0FrameSize      = s0PayloadSize + s0DigestSize
)

var (
	s0JournalMagic = [8]byte{'H', 'L', 'R', 'G', 'O', 'V', 'S', '0'}
	errS0Tampered  = errors.New("S0 governance journal authentication failed")
)

type s0OutcomeKey struct {
	workUnit   uint64
	definition uint32
}

type s0OutcomeHead struct {
	revision uint32
	value    byte
}

type s0ReplayResult struct {
	records uint64
	heads   map[s0OutcomeKey]s0OutcomeHead
	bytes   int64
}

type s0CohortRow struct {
	costMicrosUSD int64
	value         byte
	matured       bool
}

var s0SummarySink struct {
	cost      int64
	successes int64
	matured   int64
}

func s0JournalKey() []byte {
	return bytes.Repeat([]byte{0x47}, 32)
}

func writeS0Journal(path string, records int, key []byte) (int64, time.Duration, error) {
	started := time.Now()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, 0, err
	}
	buffer := bufio.NewWriterSize(file, 1<<20)
	header := make([]byte, 12)
	copy(header, s0JournalMagic[:])
	binary.LittleEndian.PutUint32(header[8:], s0JournalVersion)
	if _, err := buffer.Write(header); err != nil {
		file.Close()
		return 0, 0, err
	}
	previous := make([]byte, sha256.Size)
	unique := records - records/10
	for index := 0; index < records; index++ {
		sequence := uint64(index + 1)
		workUnit := sequence
		revision := uint32(1)
		if index >= unique {
			workUnit = uint64(index-unique) + 1
			revision = 2
		}
		definition := uint32((workUnit-1)%8 + 1)
		payload := make([]byte, s0PayloadSize)
		binary.LittleEndian.PutUint64(payload[0:8], sequence)
		binary.LittleEndian.PutUint64(payload[8:16], workUnit)
		binary.LittleEndian.PutUint32(payload[16:20], definition)
		binary.LittleEndian.PutUint32(payload[20:24], revision)
		payload[24] = byte(index % 3)
		binary.LittleEndian.PutUint64(payload[25:33], uint64(index%1024+1))
		mac := hmac.New(sha256.New, key)
		mac.Write(previous)
		mac.Write(payload)
		digest := mac.Sum(nil)
		if _, err := buffer.Write(payload); err != nil {
			file.Close()
			return 0, 0, err
		}
		if _, err := buffer.Write(digest); err != nil {
			file.Close()
			return 0, 0, err
		}
		previous = digest
	}
	if err := buffer.Flush(); err != nil {
		file.Close()
		return 0, 0, err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return 0, 0, err
	}
	if err := file.Close(); err != nil {
		return 0, 0, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0, 0, err
	}
	return info.Size(), time.Since(started), nil
}

func replayS0Journal(path string, key []byte) (s0ReplayResult, error) {
	file, err := os.Open(path)
	if err != nil {
		return s0ReplayResult{}, err
	}
	defer file.Close()
	header := make([]byte, 12)
	if _, err := io.ReadFull(file, header); err != nil {
		return s0ReplayResult{}, err
	}
	if !bytes.Equal(header[:8], s0JournalMagic[:]) || binary.LittleEndian.Uint32(header[8:]) != s0JournalVersion {
		return s0ReplayResult{}, errors.New("S0 governance journal header is invalid")
	}
	result := s0ReplayResult{heads: make(map[s0OutcomeKey]s0OutcomeHead)}
	previous := make([]byte, sha256.Size)
	frame := make([]byte, s0FrameSize)
	for {
		_, err := io.ReadFull(file, frame)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return s0ReplayResult{}, fmt.Errorf("read S0 frame after %d records: %w", result.records, err)
		}
		payload := frame[:s0PayloadSize]
		digest := frame[s0PayloadSize:]
		mac := hmac.New(sha256.New, key)
		mac.Write(previous)
		mac.Write(payload)
		if !hmac.Equal(digest, mac.Sum(nil)) {
			return s0ReplayResult{}, fmt.Errorf("%w at record %d", errS0Tampered, result.records+1)
		}
		sequence := binary.LittleEndian.Uint64(payload[0:8])
		if sequence != result.records+1 {
			return s0ReplayResult{}, fmt.Errorf("S0 sequence=%d want=%d", sequence, result.records+1)
		}
		outcomeKey := s0OutcomeKey{
			workUnit:   binary.LittleEndian.Uint64(payload[8:16]),
			definition: binary.LittleEndian.Uint32(payload[16:20]),
		}
		revision := binary.LittleEndian.Uint32(payload[20:24])
		current, exists := result.heads[outcomeKey]
		if !exists && revision != 1 || exists && revision != current.revision+1 {
			return s0ReplayResult{}, fmt.Errorf("S0 revision=%d has no valid predecessor for %#v", revision, outcomeKey)
		}
		result.heads[outcomeKey] = s0OutcomeHead{revision: revision, value: payload[24]}
		result.records++
		previous = append(previous[:0], digest...)
	}
	info, err := file.Stat()
	if err != nil {
		return s0ReplayResult{}, err
	}
	result.bytes = info.Size()
	return result, nil
}

func TestS0GovernanceJournalRejectsTampering(t *testing.T) {
	path := filepath.Join(t.TempDir(), "governance.s0")
	if _, _, err := writeS0Journal(path, 100, s0JournalKey()); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte{0xff}, 12+s0FrameSize*50+4); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := replayS0Journal(path, s0JournalKey()); !errors.Is(err, errS0Tampered) {
		t.Fatalf("tampered journal replay error=%v", err)
	}
}

func TestS0GovernanceFailureDoesNotPoisonAccountingLog(t *testing.T) {
	governancePath := filepath.Join(t.TempDir(), "governance.s0")
	if _, _, err := writeS0Journal(governancePath, 10, s0JournalKey()); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(governancePath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte{0xff}, 12+s0PayloadSize); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := replayS0Journal(governancePath, s0JournalKey()); !errors.Is(err, errS0Tampered) {
		t.Fatalf("governance replay error=%v", err)
	}

	accountingPath := filepath.Join(t.TempDir(), "accounting.wal")
	accounting, err := ledger.OpenWithOptions(accountingPath, ledger.NewStatus(), ledger.Options{ChainKey: bytes.Repeat([]byte{0x24}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	event := ledger.Event{
		EventID: "evt_s0_accounting", Kind: ledger.EventRequestAccepted,
		RequestID: "req_s0_accounting", ProjectID: "project_s0", PeriodID: "2026-09-04",
		OccurredAt: time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC),
	}
	if _, err := accounting.Append(context.Background(), event); err != nil {
		accounting.Close()
		t.Fatalf("independent accounting append failed after governance corruption: %v", err)
	}
	if err := accounting.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestS0GovernanceRecoveryProfile is opt-in because the 1m case writes about
// 62 MiB and intentionally measures a cold full replay. Run with:
//
//	HALRO_RUN_GOVERNANCE_S0_PROFILE=1 go test -count=1 ./internal/governance/ -run RecoveryProfile -v
func TestS0GovernanceRecoveryProfile(t *testing.T) {
	if os.Getenv("HALRO_RUN_GOVERNANCE_S0_PROFILE") != "1" {
		t.Skip("set HALRO_RUN_GOVERNANCE_S0_PROFILE=1 for the S0 10k/100k/1m profile")
	}
	for _, records := range []int{10_000, 100_000, 1_000_000} {
		t.Run(fmt.Sprintf("records=%d", records), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "governance.s0")
			bytesWritten, writeDuration, err := writeS0Journal(path, records, s0JournalKey())
			if err != nil {
				t.Fatal(err)
			}
			runtime.GC()
			var before runtime.MemStats
			runtime.ReadMemStats(&before)
			started := time.Now()
			result, err := replayS0Journal(path, s0JournalKey())
			replayDuration := time.Since(started)
			if err != nil {
				t.Fatal(err)
			}
			var after runtime.MemStats
			runtime.ReadMemStats(&after)
			if result.records != uint64(records) {
				t.Fatalf("replayed=%d want=%d", result.records, records)
			}
			wantHeads := records - records/10
			if len(result.heads) != wantHeads {
				t.Fatalf("heads=%d want=%d", len(result.heads), wantHeads)
			}
			t.Logf("records=%d heads=%d bytes=%d write=%s replay=%s replay_records_s=%.2f heap_alloc_delta=%d total_alloc_delta=%d",
				records, len(result.heads), bytesWritten, writeDuration, replayDuration,
				float64(records)/replayDuration.Seconds(), after.HeapAlloc-before.HeapAlloc, after.TotalAlloc-before.TotalAlloc)
		})
	}
}

// BenchmarkS0CohortSummary measures an intentionally pessimistic in-memory
// scan of the proposed maximum built-in cohort. Production uses time indexes
// and low-cardinality rollups, so it must beat this lower-complexity ceiling
// rather than cite it as its own result.
func BenchmarkS0CohortSummary(b *testing.B) {
	for _, rows := range []int{10_000, 100_000} {
		fixture := make([]s0CohortRow, rows)
		for index := range fixture {
			fixture[index] = s0CohortRow{
				costMicrosUSD: int64(index%10_000 + 1),
				value:         byte(index % 3),
				matured:       index%10 != 0,
			}
		}
		b.Run(fmt.Sprintf("work_units=%d", rows), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(rows))
			for iteration := 0; iteration < b.N; iteration++ {
				var result struct {
					cost      int64
					successes int64
					matured   int64
				}
				for _, row := range fixture {
					if !row.matured {
						continue
					}
					result.matured++
					result.cost += row.costMicrosUSD
					if row.value == 0 {
						result.successes++
					}
				}
				s0SummarySink = result
			}
		})
	}
}
