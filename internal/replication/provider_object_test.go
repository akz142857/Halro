package replication

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

func TestProviderObjectSourceRefusesSymlinkDirectory(t *testing.T) {
	root := t.TempDir()
	realDirectory := filepath.Join(root, "real")
	if err := os.Mkdir(realDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	linkedDirectory := filepath.Join(root, "linked")
	if err := os.Symlink(realDirectory, linkedDirectory); err != nil {
		t.Fatal(err)
	}
	if err := PersistProviderObjectSource(linkedDirectory, "object.content", []byte("sealed")); err == nil {
		t.Fatal("provider-object source accepted a symlink directory")
	}
}

func TestProviderObjectSourceRetriesParentDurabilityAfterDirectoryCreation(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "sources")
	originalSync := syncProviderObjectDirectory
	t.Cleanup(func() { syncProviderObjectDirectory = originalSync })

	failedParentBarrier := false
	syncProviderObjectDirectory = func(path string) error {
		if path == root && !failedParentBarrier {
			failedParentBarrier = true
			return errors.New("injected parent fsync failure")
		}
		return originalSync(path)
	}
	if err := PersistProviderObjectSource(directory, "object.content", []byte("sealed")); err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("first persist error=%v", err)
	}
	if info, err := os.Stat(directory); err != nil || !info.IsDir() {
		t.Fatalf("failed attempt did not leave the created directory for retry: info=%v err=%v", info, err)
	}

	retriedParentBarrier := false
	syncProviderObjectDirectory = func(path string) error {
		if path == root {
			retriedParentBarrier = true
		}
		return originalSync(path)
	}
	if err := PersistProviderObjectSource(directory, "object.content", []byte("sealed")); err != nil {
		t.Fatal(err)
	}
	if !retriedParentBarrier {
		t.Fatal("retry trusted an existing source directory without repeating its parent durability barrier")
	}
}

func TestProviderObjectChunkReplayRepairsNativeTailAndKeepsPartialReconstructionSource(t *testing.T) {
	root := t.TempDir()
	objectDir := filepath.Join(root, "objects")
	sourceDir := filepath.Join(root, "sources")
	contents := []byte("abcdefghijkl")
	digest := sha256.Sum256(contents)
	sink := &NativeSink{objectDir: objectDir, objectSourceDir: sourceDir}
	if err := os.MkdirAll(objectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	chunk := func(offset uint64, final bool) (ProviderObjectMetadata, Frame) {
		metadata := ProviderObjectMetadata{
			Name: "file_2.content", Offset: offset, ChunkLength: 4, TotalLength: uint64(len(contents)),
			Digest: digest, Final: final,
		}
		encoded, err := metadata.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		return metadata, Frame{Kind: KindProviderObject, Metadata: encoded, Payload: contents[offset : offset+4]}
	}
	_, first := chunk(0, false)
	if err := sink.Persist(first); err != nil {
		t.Fatal(err)
	}
	secondMetadata, second := chunk(4, false)
	if err := sink.Persist(second); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after the chunk fsync but before its ordering append:
	// a fresh sink receives the same frame again.
	sink = &NativeSink{objectDir: objectDir, objectSourceDir: sourceDir}
	if err := sink.Persist(second); err != nil {
		t.Fatalf("replayed middle chunk: %v", err)
	}
	source := &NativeSource{options: NativeSourceOptions{ProviderObjectDir: sourceDir}}
	got, err := source.ReadProviderObjectChunk(secondMetadata)
	if err != nil || string(got) != "efgh" {
		t.Fatalf("partial reconstruction source=%q err=%v", got, err)
	}
	_, final := chunk(8, true)
	if err := sink.Persist(final); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(filepath.Join(objectDir, "file_2.content"))
	if err != nil || string(got) != string(contents) {
		t.Fatalf("final object=%q err=%v", got, err)
	}
}

func TestProviderObjectChunkRejectsStagingPrefixShorterThanOrderingOffset(t *testing.T) {
	objectDir := filepath.Join(t.TempDir(), "objects")
	if err := os.MkdirAll(objectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	contents := []byte("abcdefghijkl")
	digest := sha256.Sum256(contents)
	stagingPath := filepath.Join(objectDir, ".replicating-"+fmt.Sprintf("%x", digest[:]))
	if err := os.WriteFile(stagingPath, contents[:3], 0o600); err != nil {
		t.Fatal(err)
	}
	metadata := ProviderObjectMetadata{
		Name: "file_3.content", Offset: 4, ChunkLength: 4, TotalLength: uint64(len(contents)), Digest: digest,
	}
	encoded, err := metadata.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	err = (&NativeSink{objectDir: objectDir}).Persist(Frame{Kind: KindProviderObject, Metadata: encoded, Payload: contents[4:8]})
	if err == nil || !strings.Contains(err.Error(), "does not continue") {
		t.Fatalf("short staging prefix error=%v", err)
	}
	got, readErr := os.ReadFile(stagingPath)
	if readErr != nil || string(got) != string(contents[:3]) {
		t.Fatalf("short staging prefix was modified: %q err=%v", got, readErr)
	}
}
