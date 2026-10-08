package metadatajournal

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/ownedfile"
)

func TestBoundReplicaHandoffKeepsActualDescriptorAndReceivesExactSuffix(t *testing.T) {
	ctx := context.Background()
	path, original, a := boundReplicationFixture(t)
	if _, err := original.Append([]Op{put("projects", "before", "original")}); err != nil {
		t.Fatal(err)
	}
	before := original.Head()
	file := original.file
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(t.TempDir(), "source.journal")
	if err := os.WriteFile(sourcePath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	source, err := OpenBoundContext(ctx, sourcePath, testKey(), a.Baseline, a.FileRevision, a.MaxFileBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if _, err := source.Append([]Op{put("projects", "after", "received")}); err != nil {
		t.Fatal(err)
	}
	if err := original.ConfigureBoundReplicaContext(ctx, a, before); err != nil {
		t.Fatal(err)
	}
	if original.file != file || original.Head() != before || !original.replica {
		t.Fatal("handoff reopened or changed the actual native prefix")
	}
	if _, err := original.Append([]Op{put("projects", "local", "forbidden")}); err == nil || original.Head() != before {
		t.Fatal("Replica mode allowed a local append", err)
	}
	r, err := NewBoundReplicationRangeReader(sourcePath, testKey(), 1<<20, a)
	if err != nil {
		t.Fatal(err)
	}
	frames, err := r.Read(a.Baseline.Epoch, 2, 2)
	if closeErr := r.Close(); err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	if err := original.AppendReplicatedContext(ctx, a.Baseline.Epoch, 2, 2, frames); err != nil {
		t.Fatal(err)
	}
	if original.file != file || original.Head() != source.Head() {
		t.Fatal("received suffix changed actual owner or prefix")
	}
	want, err := os.ReadFile(sourcePath)
	got, readErr := os.ReadFile(path)
	if err != nil || readErr != nil || !bytes.Equal(want, got) {
		t.Fatal("handoff changed native bytes", err, readErr)
	}
}

func TestBoundReplicaHandoffRejectsWrongHeadCancellationCorruptAndForeignSource(t *testing.T) {
	for _, fault := range []string{"wrong-head", "cancel", "corrupt", "replacement"} {
		t.Run(fault, func(t *testing.T) {
			ctx := context.Background()
			path, original, a := boundReplicationFixture(t)
			if _, err := original.Append([]Op{put("projects", "native", "complete")}); err != nil {
				t.Fatal(err)
			}
			head := original.Head()
			file := original.file
			switch fault {
			case "wrong-head":
				head.Sequence--
			case "cancel":
				c, cancel := context.WithCancel(ctx)
				cancel()
				ctx = c
			case "corrupt":
				var tail [1]byte
				if _, err := file.ReadAt(tail[:], head.Offset-1); err != nil {
					t.Fatal(err)
				}
				tail[0] ^= 1
				if _, err := file.WriteAt(tail[:], head.Offset-1); err != nil {
					t.Fatal(err)
				}
				if err := file.Sync(); err != nil {
					t.Fatal(err)
				}
			case "replacement":
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(path, path+".original"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			err = original.ConfigureBoundReplicaContext(ctx, a, head)
			if err == nil || original.replica || original.file != file {
				t.Fatal("invalid source installed or replaced the original owner", err)
			}
			if fault == "replacement" && !errors.Is(err, ownedfile.ErrReplaced) {
				t.Fatal("foreign source did not preserve ownership failure", err)
			}
			after, readErr := os.ReadFile(path)
			if readErr != nil || !bytes.Equal(before, after) {
				t.Fatal("refused handoff modified source", readErr)
			}
		})
	}
}
