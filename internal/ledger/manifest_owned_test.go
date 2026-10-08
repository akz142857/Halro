package ledger

import (
	"errors"
	"os"
	"testing"

	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/ownedfile"
)

func TestManifestPreparationPreservesForeignPartialAndLateReplacements(t *testing.T) {
	directory := t.TempDir()
	path := segmentManifestPath(directory)
	partial := path + segmentTempSuffix
	source := segmentManifestSource{nilSegments: true}
	if renamed, err := saveSegmentManifestSourceWithSync(directory, source, durable.SyncDirectory); err != nil || !renamed {
		t.Fatal(renamed, err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(partial, []byte("foreign partial"), 0600); err != nil {
		t.Fatal(err)
	}
	if renamed, err := saveSegmentManifestSourceWithSync(directory, source, durable.SyncDirectory); !errors.Is(err, os.ErrExist) || renamed {
		t.Fatal("truncated someone else's partial", renamed, err)
	}
	if raw, err := os.ReadFile(partial); err != nil || string(raw) != "foreign partial" {
		t.Fatal(err)
	}
	if err := os.Remove(partial); err != nil {
		t.Fatal(err)
	}
	source = segmentManifestSource{each: func(emit func(Segment) error) error {
		if err := os.Rename(partial, partial+".owned"); err != nil {
			return err
		}
		if err := os.WriteFile(partial, []byte("late foreign"), 0600); err != nil {
			return err
		}
		return emit(Segment{Generation: 1})
	}}
	if renamed, err := saveSegmentManifestSourceWithSync(directory, source, durable.SyncDirectory); !errors.Is(err, ownedfile.ErrReplaced) || renamed {
		t.Fatal("published a replaced temporary", renamed, err)
	}
	for _, name := range []string{partial, partial + ".owned"} {
		if _, err := os.Stat(name); err != nil {
			t.Fatal("deleted unretired inode", name, err)
		}
	}
	if raw, err := os.ReadFile(path); err != nil || string(raw) != string(before) {
		t.Fatal("failed preparation changed authority", err)
	}
	if err := os.Remove(partial); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(partial + ".owned"); err != nil {
		t.Fatal(err)
	}
	source = segmentManifestSource{each: func(func(Segment) error) error {
		if err := os.Rename(path, path+".original"); err != nil {
			return err
		}
		return os.WriteFile(path, []byte("foreign authority name"), 0600)
	}}
	if renamed, err := saveSegmentManifestSourceWithSync(directory, source, durable.SyncDirectory); !errors.Is(err, ownedfile.ErrReplaced) || renamed {
		t.Fatal("overwrote changed destination", renamed, err)
	}
	if raw, err := os.ReadFile(path); err != nil || string(raw) != "foreign authority name" {
		t.Fatal(err)
	}
	if _, err := os.Stat(partial); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("definitely owned uncommitted temp was not retired", err)
	}
}
