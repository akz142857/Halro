package metadatajournal

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
)

func TestBoundExactOpenNeverRepairsUnqualifiedHeadOrPartialTail(t *testing.T) {
	path, owner, _ := newBoundJournal(t)
	if _, err := owner.Append([]Op{put("projects", "p", "v")}); err != nil {
		t.Fatal(err)
	}
	head := owner.Head()
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, shape := range []string{"hash", "offset", "sequence", "partial", "symlink"} {
		t.Run(shape, func(t *testing.T) {
			wanted := head
			switch shape {
			case "hash":
				wanted.Hash[0] ^= 1
			case "offset":
				wanted.Offset--
			case "sequence":
				wanted.Sequence--
			case "partial":
				if err := os.WriteFile(path, append(bytes.Clone(original), []byte("HM")...), 0600); err != nil {
					t.Fatal(err)
				}
				defer os.WriteFile(path, original, 0600)
			case "symlink":
				if err := os.Rename(path, path+".real"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".real", path); err != nil {
					t.Fatal(err)
				}
				defer func() { os.Remove(path); os.Rename(path+".real", path) }()
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			next, err := OpenBoundExactContext(context.Background(), path, testKey(), wanted, 64<<20)
			if err == nil || next != nil {
				t.Fatal(next, err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("unqualified open changed file", err)
			}
		})
	}
	next, err := OpenBoundExactContext(context.Background(), path, testKey(), head, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	var kinds []Kind
	actual, err := next.InspectBoundContext(context.Background(), func(r Record) error { kinds = append(kinds, r.Kind); return nil })
	if err != nil || actual != head || len(kinds) != 2 || kinds[0] != KindEpoch || kinds[1] != KindOperations {
		t.Fatal(actual, kinds, err)
	}
}

func TestBoundInspectionAuthenticatesExcludedTailBeforeVisitorAndJoinsCancellation(t *testing.T) {
	path, l, _ := newBoundJournal(t)
	for range 3 {
		if _, err := l.Append([]Op{put("projects", "p", "v")}); err != nil {
			t.Fatal(err)
		}
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	bad := bytes.Clone(original)
	bad[len(bad)-1] ^= 1
	if err := os.WriteFile(path, bad, 0600); err != nil {
		t.Fatal(err)
	}
	defer os.WriteFile(path, original, 0600)
	visited := 0
	if actual, err := l.InspectBoundContext(context.Background(), func(Record) error { visited++; return nil }); !errors.Is(err, ErrCorrupt) || actual != (Head{}) || visited != 0 {
		t.Fatal(actual, visited, err)
	}
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	actual, err := l.InspectBoundContext(ctx, func(Record) error { visited++; cancel(); return ctx.Err() })
	if !errors.Is(err, context.Canceled) || actual != (Head{}) || visited != 1 {
		t.Fatal(actual, visited, err)
	}
}
