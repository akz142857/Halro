package metadatajournal

import (
	"bytes"
	"context"
	"testing"
)

func TestMetadataReplicationSourceReadsExactFramesAndTruncatesTail(t *testing.T) {
	path, log := newJournal(t)
	var frames [][]byte
	if err := log.SetAfterDurable(func(batch DurableBatch) (uint64, error) {
		frames = append(frames, append([]byte(nil), batch.Frames...))
		return uint64(len(frames)), nil
	}, func(context.Context, uint64) error { return nil }); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"one", "two"} {
		if _, err := log.Append([]Op{put("projects", id, id)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := ReadReplicationFrames(path, testKey(), 1, 2, 2)
	if err != nil || !bytes.Equal(got, frames[1]) {
		t.Fatalf("read exact frame equal=%t err=%v", bytes.Equal(got, frames[1]), err)
	}
	firstHead, err := ReplicationHeadAt(path, testKey(), 1, 1)
	if err != nil || firstHead == ([32]byte{}) {
		t.Fatalf("first head=%x err=%v", firstHead, err)
	}
	if err := TruncateReplicationTail(path, testKey(), 1, 1); err != nil {
		t.Fatal(err)
	}
	truncatedHead, err := ReplicationHeadAt(path, testKey(), 1, 1)
	if err != nil || truncatedHead != firstHead {
		t.Fatalf("truncated head=%x want=%x err=%v", truncatedHead, firstHead, err)
	}
	epoch, sequence, err := ReplicationCursor(path, testKey())
	if err != nil || epoch != 1 || sequence != 1 {
		t.Fatalf("cursor=%d/%d err=%v", epoch, sequence, err)
	}
}

func TestMetadataReplicationHeadAtBindsTheEmptyEpoch(t *testing.T) {
	path, log := newJournal(t)
	before := log.Head()
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	head, err := ReplicationHeadAt(path, testKey(), before.Epoch, 0)
	if err != nil {
		t.Fatal(err)
	}
	if head != before.Hash || head == ([32]byte{}) {
		t.Fatalf("empty epoch head=%x, want=%x", head, before.Hash)
	}
}
