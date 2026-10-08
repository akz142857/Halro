package metadatajournal

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
)

func nativeTrimState(t *testing.T, l *Log, through uint64) TrimStateBinding {
	t.Helper()
	head := l.Head()
	state := TrimStateBinding{Version: 1, Sequence: through, SourceRevision: head.FileRevision, Root: RootCommitment{Scope: head.Baseline.Scope, Cut: head.Baseline.Cut, Digest: head.Baseline.RootDigest, ID: head.Baseline.RootID, Rows: head.Baseline.Rows, Revision: 3}}
	var previous Record
	_, err := l.InspectBoundContext(context.Background(), func(r Record) error {
		if previous.Sequence == through && previous.Kind == KindOperations {
			state.SourceEndOffset = r.Offset
		}
		if r.Sequence == through && r.Kind == KindOperations {
			state.SourceFrameOffset = r.Offset
			state.SourceHash = r.Hash
		}
		previous = r
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if through == head.Sequence {
		state.SourceEndOffset = head.Offset
	}
	return state
}
func TestBoundTrimStateOwnsReplacementAndPreservesActualCutProvenance(t *testing.T) {
	path, l, _ := newBoundJournal(t)
	for range 3 {
		if _, err := l.Append([]Op{put("projects", "p", "v")}); err != nil {
			t.Fatal(err)
		}
	}
	source := l.Head()
	state := nativeTrimState(t, l, 2)
	head, err := l.TrimBoundContext(context.Background(), source, state)
	if err != nil || head.FileRevision != 2 || head.TrimmedThrough != 2 || head.Sequence != 3 || head.Hash != source.Hash {
		t.Fatal(head, err)
	}
	seen := false
	_, err = l.InspectBoundContext(context.Background(), func(r Record) error {
		if r.Kind == KindTrim {
			seen = r.Trim.State != nil && *r.Trim.State == state
		}
		return nil
	})
	if err != nil || !seen {
		t.Fatal(seen, err)
	}
	if _, err := l.Append([]Op{put("projects", "after", "v")}); err != nil {
		t.Fatal(err)
	}
	if err := l.Discard(); err != nil {
		t.Fatal("replacement creation receipt lost", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}
func TestBoundTrimStatePreflightMismatchPreservesSourceAndDiskLimit(t *testing.T) {
	for _, shape := range []string{"source head", "source offset", "source end", "source hash", "root scope", "root id", "revision", "disk"} {
		t.Run(shape, func(t *testing.T) {
			path, l, _ := newBoundJournal(t)
			for range 2 {
				if _, err := l.Append([]Op{put("projects", "p", "v")}); err != nil {
					t.Fatal(err)
				}
			}
			head := l.Head()
			state := nativeTrimState(t, l, 1)
			switch shape {
			case "source head":
				head.Hash[0] ^= 1
			case "source offset":
				state.SourceFrameOffset++
			case "source end":
				state.SourceEndOffset++
			case "source hash":
				state.SourceHash[0] ^= 1
			case "root scope":
				state.Root.Scope[0] ^= 1
			case "root id":
				state.Root.ID[0] ^= 1
			case "revision":
				state.SourceRevision++
			case "disk":
				l.maxBytes = head.Offset
			}
			before, _ := os.ReadFile(path)
			if h, err := l.TrimBoundContext(context.Background(), head, state); err == nil || h != (Head{}) {
				t.Fatal(h, err)
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) {
				t.Fatal("refusal replaced source")
			}
		})
	}
}
func TestBoundTrimStatePostRenameCancellationOrPanicRetainsCreatedReceipt(t *testing.T) {
	for _, shape := range []string{"cancel", "panic"} {
		t.Run(shape, func(t *testing.T) {
			path, l, _ := newBoundJournal(t)
			for range 2 {
				if _, err := l.Append([]Op{put("projects", "p", "v")}); err != nil {
					t.Fatal(err)
				}
			}
			state := nativeTrimState(t, l, 2)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var err error
			panicked := false
			func() {
				defer func() {
					if recover() != nil {
						panicked = true
					}
				}()
				_, err = l.trimBoundContext(ctx, l.Head(), state, func(phase string) {
					if phase == "journal-renamed" {
						if shape == "panic" {
							panic("publication uncertain")
						}
						cancel()
					}
				})
			}()
			if shape == "panic" && !panicked || shape == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal(panicked, err)
			}
			if _, err := l.Append([]Op{put("projects", "forbidden", "v")}); err == nil {
				t.Fatal("uncertain rewrite admitted append")
			}
			if err := l.Discard(); err != nil {
				t.Fatal("post-rename owner lost", err)
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
		})
	}
}
