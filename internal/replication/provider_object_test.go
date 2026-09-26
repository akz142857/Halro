package replication

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
)

func TestProviderObjectMetadataRejectsTraversalAndRangeForgery(t *testing.T) {
	digest := sha256.Sum256([]byte("sealed"))
	for _, metadata := range []ProviderObjectMetadata{
		{Name: "../secret", TotalLength: 6, ChunkLength: 6, Digest: digest, Final: true},
		{Name: "safe.content", Offset: 5, ChunkLength: 2, TotalLength: 6, Digest: digest, Final: true},
		{Name: "safe.content", ChunkLength: 6, TotalLength: 6, Digest: digest, Final: false},
	} {
		encoded, err := metadata.MarshalBinary()
		if err == nil {
			t.Fatalf("unsafe metadata encoded as %x", encoded)
		}
	}
}

func TestNativeSinkPublishesProviderObjectOnlyAfterFinalVerifiedChunk(t *testing.T) {
	objectDir := filepath.Join(t.TempDir(), "objects")
	if err := os.MkdirAll(objectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sink := &NativeSink{objectDir: objectDir}
	contents := []byte("sealed-provider-object")
	digest := sha256.Sum256(contents)
	first := ProviderObjectMetadata{Name: "file_1.content", ChunkLength: 8, TotalLength: uint64(len(contents)), Digest: digest}
	firstBytes, err := first.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Persist(Frame{Kind: KindProviderObject, Metadata: firstBytes, Payload: contents[:8]}); err != nil {
		t.Fatal(err)
	}
	finalPath := filepath.Join(sink.objectDir, first.Name)
	if _, err := os.Stat(finalPath); !os.IsNotExist(err) {
		t.Fatalf("partial object became visible: %v", err)
	}
	last := ProviderObjectMetadata{
		Name: first.Name, Offset: 8, ChunkLength: uint64(len(contents) - 8), TotalLength: uint64(len(contents)),
		Digest: digest, Final: true,
	}
	lastBytes, err := last.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Persist(Frame{Kind: KindProviderObject, Metadata: lastBytes, Payload: contents[8:]}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(finalPath)
	if err != nil || string(got) != string(contents) {
		t.Fatalf("published object=%q err=%v", got, err)
	}
	if err := sink.Persist(Frame{Kind: KindProviderObject, Metadata: lastBytes, Payload: contents[8:]}); err != nil {
		t.Fatalf("exact final retransmission was not idempotent: %v", err)
	}
}

func TestPersistProviderObjectSourceAcceptsObjectsLargerThanOneFrame(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "sources")
	contents := make([]byte, MaxPayloadBytes+1)
	contents[len(contents)-1] = 1
	if err := PersistProviderObjectSource(directory, "large.content", contents); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(directory, "large.content"))
	if err != nil || len(got) != len(contents) || got[len(got)-1] != 1 {
		t.Fatalf("persisted large object length=%d err=%v", len(got), err)
	}
}
