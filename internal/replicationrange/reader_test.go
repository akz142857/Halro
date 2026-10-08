package replicationrange

import (
	"bytes"
	"errors"
	"github.com/akz142857/Halro/internal/ownedfile"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReaderCloseJoinsActiveReadAndClearsOnlyItsOwnKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(path, []byte{1}, 0o600); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{7}, 32)
	entered, release := make(chan struct{}), make(chan struct{})
	calls := 0
	reader, err := New(path, key, 1, func(input io.ReadSeeker, scanKey []byte, initial Position, first, last uint64) (Range, error) {
		calls++
		if calls == 1 {
			close(entered)
			<-release
		}
		if !bytes.Equal(scanKey, key) {
			t.Error("Close cleared the key during an active scan")
		}
		var value [1]byte
		if _, err := io.ReadFull(input, value[:]); err != nil {
			return Range{}, err
		}
		return Range{After: Position{Sequence: 1, Offset: 1, Hash: [32]byte{1}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	readResult := make(chan error, 1)
	go func() { _, err := reader.Read(1, 1); readResult <- err }()
	<-entered
	closed := make(chan struct{})
	go func() { reader.Close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("Close returned before active read completed")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-readResult; err != nil {
		t.Fatal(err)
	}
	<-closed
	if len(reader.key) != 0 || reader.position != (Position{}) || reader.identity != nil {
		t.Fatal("Close retained key or prefix authority")
	}
	if !bytes.Equal(key, bytes.Repeat([]byte{7}, 32)) {
		t.Fatal("Close modified caller's key")
	}
	if _, err := reader.Read(1, 1); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}

func TestActualRangeDescriptorCloseFailureWithdrawsCursorAndSurvivesReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(path, []byte{1}, 0600); err != nil {
		t.Fatal(err)
	}
	r, err := New(path, make([]byte, 32), 1, func(input io.ReadSeeker, _ []byte, _ Position, _, _ uint64) (Range, error) {
		var value [1]byte
		if _, err := io.ReadFull(input, value[:]); err != nil {
			return Range{}, err
		}
		return Range{After: Position{Sequence: 1, Offset: 1, Hash: [32]byte{1}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	r.closeFile = func(file *os.File) error {
		if err := file.Close(); err != nil {
			return err
		}
		return file.Close()
	}
	if frames, err := r.Read(1, 1); !errors.Is(err, ownedfile.ErrRetirement) || frames != nil {
		t.Fatal(frames, err)
	}
	first := r.Close()
	r.Reset()
	if !errors.Is(first, ownedfile.ErrRetirement) || r.Close() != first || r.retiredFile == nil {
		t.Fatal(first, r.retiredFile)
	}
	if _, err := r.Read(1, 1); !errors.Is(err, ownedfile.ErrRetirement) || !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}
