package metadatajournal

import (
	"bytes"
	"context"
	"os"
	"testing"
)

func TestBoundReplicationPrefixHeadReturnsActualFrameEndsAndRejectsExcludedTail(t *testing.T) {
	path, log, permission := boundReplicationFixture(t)
	heads := []Head{log.Head()}
	for i := 0; i < 2; i++ {
		if _, err := log.Append([]Op{put("projects", "p", "value")}); err != nil {
			t.Fatal(err)
		}
		heads = append(heads, log.Head())
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	for _, expected := range heads {
		actual, err := BoundReplicationPrefixHead(context.Background(), path, testKey(), permission, expected.Epoch, expected.Sequence)
		if err != nil || actual != expected {
			t.Fatal("physical native frame-end receipt changed", actual, expected, err)
		}
	}
	foreign := permission
	foreign.Baseline.Scope[0] ^= 1
	if actual, err := BoundReplicationPrefixHead(context.Background(), path, testKey(), foreign, heads[0].Epoch, 0); err == nil || actual != (Head{}) {
		t.Fatal("foreign independent baseline granted prefix", actual, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 1
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if actual, err := BoundReplicationPrefixHead(context.Background(), path, testKey(), permission, heads[0].Epoch, 0); err == nil || actual != (Head{}) {
		t.Fatal("excluded corrupted suffix granted prefix", actual, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, raw) {
		t.Fatal("prefix inspection repaired unqualified bytes", err)
	}
}
