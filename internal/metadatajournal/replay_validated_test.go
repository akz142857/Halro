package metadatajournal

import (
	"errors"
	"os"
	"testing"
)

func TestValidatedReplayAuthenticatesExcludedTailBeforeVisiting(t *testing.T) {
	path, log := newJournal(t)
	for range 3 {
		if _, err := log.Append([]Op{put("projects", "p", "value")}); err != nil {
			t.Fatal(err)
		}
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		t.Fatal(err)
	}
	var b [1]byte
	_, err = f.ReadAt(b[:], info.Size()-1)
	if err == nil {
		b[0] ^= 1
		_, err = f.WriteAt(b[:], info.Size()-1)
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		t.Fatal(err)
	}
	visited := false
	if err := log.ReplayRangeValidated(0, 2, func(Record) error { visited = true; return nil }); err == nil || visited {
		t.Fatal("visitor ran before excluded suffix authentication", visited, err)
	}
}
