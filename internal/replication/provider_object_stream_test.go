package replication

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"
)

type boundedObjectInput struct {
	reader             io.Reader
	maxRead, readBytes int
}

func (r *boundedObjectInput) Read(p []byte) (int, error) {
	if len(p) > r.maxRead {
		r.maxRead = len(p)
	}
	n, err := r.reader.Read(p)
	r.readBytes += n
	return n, err
}

// Deliberately implement the shortcut to prove the bounded copier hides it.
func (r *boundedObjectInput) WriteTo(io.Writer) (int64, error) {
	return 0, errors.New("unbounded input WriteTo used")
}

type boundedObjectOutput struct{ bytes.Buffer }

func (w *boundedObjectOutput) ReadFrom(io.Reader) (int64, error) {
	return 0, errors.New("unbounded output ReadFrom used")
}

func TestProviderObjectCopyBoundsReadSizeAndAuthenticatesBeforePublication(t *testing.T) {
	body := bytes.Repeat([]byte{0xa5}, 3*providerObjectCopyBytes+7)
	digest := sha256.Sum256(body)
	input := &boundedObjectInput{reader: bytes.NewReader(body)}
	output := &boundedObjectOutput{}
	if err := copyProviderObject(output, input, uint64(len(body)), digest); err != nil {
		t.Fatal(err)
	}
	if input.maxRead > providerObjectCopyBytes || !bytes.Equal(output.Bytes(), body) {
		t.Fatal("copy not bounded/exact", input.maxRead)
	}
	for _, mode := range []string{"short", "extra", "digest", "overflow", "read-error"} {
		t.Run(mode, func(t *testing.T) {
			n := uint64(len(body))
			hash := digest
			var reader io.Reader = bytes.NewReader(body)
			switch mode {
			case "short":
				n++
			case "extra":
				n--
			case "digest":
				hash[0] ^= 1
			case "overflow":
				n = math.MaxUint64
			case "read-error":
				reader = io.MultiReader(bytes.NewReader(body[:8]), objectErrorReader{})
			}
			tracked := &boundedObjectInput{reader: reader}
			if err := copyProviderObject(io.Discard, tracked, n, hash); err == nil {
				t.Fatal("invalid copy accepted")
			}
			if mode == "overflow" && tracked.readBytes != 0 {
				t.Fatal("overflow read input")
			}
			if mode == "extra" && tracked.readBytes != len(body) {
				t.Fatal("did not stop at declared length plus one")
			}
		})
	}
	root := t.TempDir()
	directory := filepath.Join(root, "source")
	digest[0] ^= 1
	if err := persistProviderObjectSource(directory, "bad.content", bytes.NewReader(body), uint64(len(body)), digest); err == nil {
		t.Fatal("bad object published")
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 0 {
		t.Fatal("bad copy retained final/temp file", entries, err)
	}
}

type objectErrorReader struct{}

func (objectErrorReader) Read([]byte) (int, error) { return 0, errors.New("controlled read failure") }

func TestProviderObjectStreamChecksFileSizeTypeAndCorruption(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "object.content")
	body := bytes.Repeat([]byte{0x3a}, 2*providerObjectCopyBytes+7)
	digest := sha256.Sum256(body)
	for _, mode := range []string{"valid", "short", "extra", "early-corruption", "late-corruption", "directory"} {
		t.Run(mode, func(t *testing.T) {
			target := path
			data := append([]byte(nil), body...)
			switch mode {
			case "short":
				data = data[:len(data)-1]
			case "extra":
				data = append(data, 0)
			case "early-corruption":
				data[0] ^= 1
			case "late-corruption":
				data[len(data)-2] ^= 1
			case "directory":
				target = root
			}
			if target == path {
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			file, err := os.Open(target)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			err = verifyProviderObjectFile(file, uint64(len(body)), digest)
			if (mode == "valid") != (err == nil) {
				t.Fatal(mode, err)
			}
		})
	}
}

func TestProviderObjectStreamRefusesReplacedNamedFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "object.content")
	body := []byte("sealed")
	digest := sha256.Sum256(body)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyProviderObjectFile(file, uint64(len(body)), digest); err == nil {
		t.Fatal("named replacement accepted despite matching digest")
	}
}

func TestProviderObjectFinalReadAndExistingReplicaRemainAuthenticated(t *testing.T) {
	root := t.TempDir()
	live := filepath.Join(root, "live")
	sourceDir := filepath.Join(root, "source")
	if err := os.MkdirAll(live, 0o700); err != nil {
		t.Fatal(err)
	}
	body := bytes.Repeat([]byte{0x65}, MaxPayloadBytes+3)
	digest := sha256.Sum256(body)
	name := "large.content"
	if err := os.WriteFile(filepath.Join(live, name), body, 0o600); err != nil {
		t.Fatal(err)
	}
	metadata := ProviderObjectMetadata{Name: name, Offset: uint64(len(body) - 3), ChunkLength: 3, TotalLength: uint64(len(body)), Digest: digest, Final: true}
	encoded, err := metadata.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	sink := &NativeSink{objectDir: live, objectSourceDir: sourceDir}
	frame := Frame{Kind: KindProviderObject, Metadata: encoded, Payload: body[len(body)-3:]}
	if err := sink.Persist(frame); err != nil {
		t.Fatal("existing live object could not restore absent spool", err)
	}
	source := &NativeSource{options: NativeSourceOptions{ProviderObjectDir: sourceDir}}
	got, err := source.ReadProviderObjectChunk(metadata)
	if err != nil || !bytes.Equal(got, body[len(body)-3:]) {
		t.Fatal("streaming final read failed", err)
	}
	// A wrong byte before the requested chunk must still fail a final full audit.
	file, err := os.OpenFile(filepath.Join(sourceDir, name), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte{0}, 0); err != nil {
		t.Fatal(err)
	}
	file.Close()
	if got, err := source.ReadProviderObjectChunk(metadata); err == nil || got != nil {
		t.Fatal("unauthenticated tail escaped", err)
	}
	if err := sink.Persist(frame); err == nil {
		t.Fatal("corrupt existing spool acknowledged")
	}
	// No publication occurs if final validation fails after chunk fsync.
	badDir := filepath.Join(root, "bad-spool")
	partial := metadata
	partial.Final = false
	partial.Offset = 0
	partial.ChunkLength = MaxPayloadBytes
	if err := PersistProviderObjectChunkSource(badDir, partial, body[:MaxPayloadBytes]); err != nil {
		t.Fatal(err)
	}
	file, err = os.OpenFile(filepath.Join(badDir, name), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte{0}, 0); err != nil {
		t.Fatal(err)
	}
	file.Close()
	badLive := filepath.Join(root, "bad-live")
	if err := os.MkdirAll(badLive, 0o700); err != nil {
		t.Fatal(err)
	}
	stagedPath := filepath.Join(badLive, ".replicating-"+fmt.Sprintf("%x", digest[:]))
	if err := os.WriteFile(stagedPath, body[:MaxPayloadBytes], 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (&NativeSink{objectDir: badLive, objectSourceDir: badDir}).Persist(frame); err == nil {
		t.Fatal("corrupt spool published")
	}
	if _, err := os.Stat(filepath.Join(badLive, name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed verification published object", err)
	}
	// Revalidate chunk-source final path itself, including exact final length.
	if err := PersistProviderObjectChunkSource(badDir, metadata, frame.Payload); err == nil {
		t.Fatal("final corrupt source accepted")
	}
}

// Build the fixture without a whole-object slice; input generation is outside
// measured operations so B/op reflects streaming operation temporaries only.
func writeRepeatedObject(tb testing.TB, path string, length int64) (digest [sha256.Size]byte) {
	tb.Helper()
	file, err := os.Create(path)
	if err != nil {
		tb.Fatal(err)
	}
	hash := sha256.New()
	block := bytes.Repeat([]byte{0xa5}, providerObjectCopyBytes)
	for left := length; left > 0; {
		n := int64(len(block))
		if n > left {
			n = left
		}
		if _, err := file.Write(block[:n]); err != nil {
			tb.Fatal(err)
		}
		hash.Write(block[:n])
		left -= n
	}
	if err := file.Close(); err != nil {
		tb.Fatal(err)
	}
	copy(digest[:], hash.Sum(nil))
	return digest
}
func BenchmarkProviderObjectBoundedFilePaths(b *testing.B) {
	for _, length := range []int64{1 << 20, 64 << 20} {
		b.Run(fmt.Sprintf("bytes=%d", length), func(b *testing.B) {
			root := b.TempDir()
			live := filepath.Join(root, "live")
			sourceDir := filepath.Join(root, "source")
			if err := os.MkdirAll(live, 0o700); err != nil {
				b.Fatal(err)
			}
			if err := os.MkdirAll(sourceDir, 0o700); err != nil {
				b.Fatal(err)
			}
			name := "benchmark.content"
			digest := writeRepeatedObject(b, filepath.Join(live, name), length)
			metadata := ProviderObjectMetadata{Name: name, Offset: uint64(length - 1), ChunkLength: 1, TotalLength: uint64(length), Digest: digest, Final: true}
			encoded, err := metadata.MarshalBinary()
			if err != nil {
				b.Fatal(err)
			}
			sink := &NativeSink{objectDir: live, objectSourceDir: sourceDir}
			frame := Frame{Kind: KindProviderObject, Metadata: encoded, Payload: []byte{0xa5}}
			if err := sink.Persist(frame); err != nil {
				b.Fatal(err)
			}
			source := &NativeSource{options: NativeSourceOptions{ProviderObjectDir: sourceDir}}
			b.Run("ReadFinalChunk", func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(length)
				for range b.N {
					if got, err := source.ReadProviderObjectChunk(metadata); err != nil || len(got) != 1 {
						b.Fatal(err)
					}
				}
			})
			b.Run("ExistingReplicaAndSpool", func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(2 * length)
				for range b.N {
					if err := sink.Persist(frame); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run("ExistingReplicaRestoresMissingSpool", func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(2 * length)
				for range b.N {
					if err := os.Remove(filepath.Join(sourceDir, name)); err != nil {
						b.Fatal(err)
					}
					if err := sink.Persist(frame); err != nil {
						b.Fatal(err)
					}
				}
			})
		})
	}
}

func TestProviderObjectChunkRetransmissionRepairsPartialSpoolWithoutDeletingPrefix(t *testing.T) {
	root := t.TempDir()
	live := filepath.Join(root, "live")
	spool := filepath.Join(root, "spool")
	if err := os.MkdirAll(live, 0o700); err != nil {
		t.Fatal(err)
	}
	body := []byte("abcdefghijkl")
	digest := sha256.Sum256(body)
	name := "partial.content"
	sink := &NativeSink{objectDir: live, objectSourceDir: spool}
	frameAt := func(offset uint64, final bool) Frame {
		metadata := ProviderObjectMetadata{Name: name, Offset: offset, ChunkLength: 4, TotalLength: 12, Digest: digest, Final: final}
		encoded, err := metadata.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		return Frame{Kind: KindProviderObject, Metadata: encoded, Payload: body[offset : offset+4]}
	}
	if err := sink.Persist(frameAt(0, false)); err != nil {
		t.Fatal(err)
	}
	// Native live staging has a full next chunk, source spool only two bytes;
	// ordering did not publish it. Replay must repair both crash suffixes.
	stage := filepath.Join(live, ".replicating-"+fmt.Sprintf("%x", digest[:]))
	if err := os.WriteFile(stage, body[:8], 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(spool, name), body[:6], 0o600); err != nil {
		t.Fatal(err)
	}
	if err := sink.Persist(frameAt(4, false)); err != nil {
		t.Fatal("partial spool could not replay", err)
	}
	got, err := os.ReadFile(filepath.Join(spool, name))
	if err != nil || !bytes.Equal(got, body[:8]) {
		t.Fatal("partial replay changed earlier prefix", got, err)
	}
	if err := sink.Persist(frameAt(8, true)); err != nil {
		t.Fatal(err)
	}
	// Old complete chunks must not truncate a later authenticated suffix.
	metadata := ProviderObjectMetadata{Name: name, Offset: 4, ChunkLength: 4, TotalLength: 12, Digest: digest}
	if err := PersistProviderObjectChunkSource(spool, metadata, body[4:8]); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(filepath.Join(spool, name))
	if err != nil || !bytes.Equal(got, body) {
		t.Fatal("old complete chunk deleted later bytes", err)
	}
}

func TestProviderObjectExistingRetryRepeatsLivePublicationBarrier(t *testing.T) {
	root := t.TempDir()
	live := filepath.Join(root, "live")
	spool := filepath.Join(root, "spool")
	if err := os.MkdirAll(live, 0o700); err != nil {
		t.Fatal(err)
	}
	body := []byte("sealed")
	digest := sha256.Sum256(body)
	name := "barrier.content"
	metadata := ProviderObjectMetadata{Name: name, ChunkLength: uint64(len(body)), TotalLength: uint64(len(body)), Digest: digest, Final: true}
	encoded, err := metadata.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	frame := Frame{Kind: KindProviderObject, Metadata: encoded, Payload: body}
	original := syncProviderObjectLiveDirectory
	t.Cleanup(func() { syncProviderObjectLiveDirectory = original })
	calls := 0
	syncProviderObjectLiveDirectory = func(path string) error {
		calls++
		if path != live {
			t.Fatal("wrong live barrier", path)
		}
		if calls <= 2 {
			return errors.New("controlled live directory sync failure")
		}
		return original(path)
	}
	sink := &NativeSink{objectDir: live, objectSourceDir: spool}
	if err := sink.Persist(frame); err == nil {
		t.Fatal("first failed barrier acknowledged")
	}
	if _, err := os.Stat(filepath.Join(live, name)); err != nil {
		t.Fatal("rename did not precede failure", err)
	}
	if err := sink.Persist(frame); err == nil {
		t.Fatal("existing file bypassed repeated barrier failure")
	}
	if err := sink.Persist(frame); err != nil || calls != 3 {
		t.Fatal("retry did not repair live name durability", calls, err)
	}
}
