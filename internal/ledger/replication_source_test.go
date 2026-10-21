package ledger

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
)

func TestLedgerReplicationSourceReadsExactFramesAndTruncatesTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.wal")
	var frames [][]byte
	log, err := OpenWithOptions(path, NewStatus(), Options{
		MaxBatch: 1, ChainKey: testChainKey,
		AfterDurable: func(batch DurableBatch) (uint64, error) {
			frames = append(frames, append([]byte(nil), batch.Frames...))
			return uint64(len(frames)), nil
		},
		WaitConfirmed: func(context.Context, uint64) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"one", "two"} {
		if _, err := log.Append(context.Background(), validReservation("evt_"+id, "att_"+id)); err != nil {
			t.Fatal(err)
		}
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := ReadReplicationFrames(path, testChainKey, 1, 2, 2)
	if err != nil || !bytes.Equal(got, frames[1]) {
		t.Fatalf("read exact frame equal=%t err=%v", bytes.Equal(got, frames[1]), err)
	}
	firstHead, err := ReplicationHeadAt(path, testChainKey, 1, 1)
	if err != nil || firstHead == ([32]byte{}) {
		t.Fatalf("first head=%x err=%v", firstHead, err)
	}
	if err := TruncateReplicationTail(path, testChainKey, 1, 1); err != nil {
		t.Fatal(err)
	}
	truncatedHead, err := ReplicationHeadAt(path, testChainKey, 1, 1)
	if err != nil || truncatedHead != firstHead {
		t.Fatalf("truncated head=%x want=%x err=%v", truncatedHead, firstHead, err)
	}
	generation, sequence, err := ReplicationCursor(path, testChainKey)
	if err != nil || generation != 1 || sequence != 1 {
		t.Fatalf("cursor=%d/%d err=%v", generation, sequence, err)
	}
}
