package audit

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
)

func TestAuditReplicationSourceReadsExactFramesAndTruncatesTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	key := randomKey(t)
	var frames [][]byte
	log, err := OpenWithOptions(path, key, Options{AfterDurable: func(batch DurableBatch) error {
		frames = append(frames, append([]byte(nil), batch.Frames...))
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	for index := 1; index <= 2; index++ {
		if _, err := log.Append(context.Background(), validEvent(index, "test")); err != nil {
			t.Fatal(err)
		}
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := ReadReplicationFrames(path, key, 2, 2)
	if err != nil || !bytes.Equal(got, frames[1]) {
		t.Fatalf("read exact frame equal=%t err=%v", bytes.Equal(got, frames[1]), err)
	}
	firstHead, err := ReplicationHeadAt(path, key, 1, 1)
	if err != nil || firstHead == ([32]byte{}) {
		t.Fatalf("first head=%x err=%v", firstHead, err)
	}
	if err := TruncateReplicationTail(path, key, 1); err != nil {
		t.Fatal(err)
	}
	truncatedHead, err := ReplicationHeadAt(path, key, 1, 1)
	if err != nil || truncatedHead != firstHead {
		t.Fatalf("truncated head=%x want=%x err=%v", truncatedHead, firstHead, err)
	}
	generation, sequence, err := ReplicationCursor(path, key)
	if err != nil || generation != 1 || sequence != 1 {
		t.Fatalf("cursor=%d/%d err=%v", generation, sequence, err)
	}
}
