package ledger

import (
	"errors"
	"os"
	"testing"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/replicationrange"
)

func TestLedgerRangeActualStreamCloseDebtSurvivesResetAndClose(t *testing.T) {
	path, _ := ledgerRangeHistory(t, [][]byte{{4, 4}}, false)
	r, err := NewReplicationRangeReader(path, testChainKey, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Read(1, 1, 1); err != nil {
		t.Fatal(err)
	}
	file := r.stream.(*os.File)
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.Reset(); !errors.Is(err, ErrSegmentRetirement) || !errors.Is(err, ownedfile.ErrRetirement) {
		t.Fatal(err)
	}
	if r.stream != file {
		t.Fatal("unknown actual stream was forgotten")
	}
	first := r.Close()
	if !errors.Is(first, ownedfile.ErrRetirement) || r.Close() != first {
		t.Fatal(first)
	}
	if frames, err := r.Read(1, 2, 2); frames != nil || !errors.Is(err, replicationrange.ErrClosed) || !errors.Is(err, ownedfile.ErrRetirement) {
		t.Fatal(frames, err)
	}
}

func TestSegmentActualCloseFailureCannotReopenOrEraseDebt(t *testing.T) {
	path, _ := ledgerRangeHistory(t, [][]byte{{4}}, false)
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	r := &segmentReader{reader: file}
	first := r.Close()
	if !errors.Is(first, ErrSegmentRetirement) || r.Close() != first || r.reader != file {
		t.Fatal(first)
	}
	if _, err := r.Seek(0, 0); !errors.Is(err, os.ErrClosed) || !errors.Is(err, ErrSegmentRetirement) {
		t.Fatal(err)
	}
	if _, err := r.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
		t.Fatal(err)
	}
}
