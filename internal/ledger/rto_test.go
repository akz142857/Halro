package ledger

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestTenGiBWALRecoveryProfile(t *testing.T) {
	if os.Getenv("HALRO_10GIB_WAL_RTO") != "1" {
		t.Skip("set HALRO_10GIB_WAL_RTO=1 for the destructive-size recovery profile")
	}
	const targetBytes int64 = 10 << 30
	root := t.TempDir()
	path := filepath.Join(root, "ledger-10gib.wal")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	buffer := bufio.NewWriterSize(file, 4<<20)
	filler := strings.Repeat("x", maxPayloadSize-768)
	now := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	var bytesWritten int64
	var records uint64
	for bytesWritten < targetBytes {
		records++
		event := Event{
			EventID: fmt.Sprintf("rto_evt_%016x", records), Kind: EventReservationCreated,
			RequestID: fmt.Sprintf("rto_req_%016x", records),
			AttemptID: fmt.Sprintf("rto_attempt_%016x", records),
			ProjectID: "rto_project", PeriodID: "rto_project:2026-07-31:UTC",
			ProviderModel: filler, OccurredAt: now, ReservationMicrosUSD: MicrosUSD(1),
		}
		payload, err := json.Marshal(event)
		if err != nil {
			file.Close()
			t.Fatal(err)
		}
		if len(payload) > maxPayloadSize {
			file.Close()
			t.Fatalf("profile payload=%d exceeds limit=%d", len(payload), maxPayloadSize)
		}
		frame := encodeFrame(records, event.Kind, payload)
		if _, err := buffer.Write(frame); err != nil {
			file.Close()
			t.Fatal(err)
		}
		bytesWritten += int64(len(frame))
	}
	if err := buffer.Flush(); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	runtime.GC()
	started := time.Now()
	log, err := Open(path, NewStatus())
	openDuration := time.Since(started)
	if err != nil {
		t.Fatal(err)
	}
	state := NewState()
	replayStarted := time.Now()
	watermark, replayErr := log.Replay(Watermark{}, state.Apply)
	replayDuration := time.Since(replayStarted)
	closeErr := log.Close()
	if replayErr != nil {
		t.Fatal(replayErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if watermark.Sequence != records || watermark.Offset != bytesWritten ||
		state.Watermark() != watermark || state.PendingReservations() != int(records) {
		t.Fatalf("watermark=%#v records=%d bytes=%d pending=%d", watermark, records, bytesWritten, state.PendingReservations())
	}
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	total := openDuration + replayDuration
	throughput := float64(bytesWritten*2) / total.Seconds() / (1 << 20)
	t.Logf("wal_bytes=%d records=%d payload_bytes≈%d open_verify=%s state_replay=%s total=%s effective_scan_mib_s=%.2f heap_alloc=%d heap_sys=%d",
		bytesWritten, records, maxPayloadSize-768, openDuration, replayDuration, total, throughput, memory.HeapAlloc, memory.HeapSys)
	if total > 10*time.Minute {
		t.Fatalf("10 GiB recovery exceeded the publishable 10 minute safety bound: %s", total)
	}
}
