package ledger

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/durable"
)

func TestSegmentDirectoryDiskRefusalPrecedesTemporaryAndPreservesOldCache(t *testing.T) {
	dir, _ := writeDirectoryIndexFixture(t, 3)
	path := filepath.Join(dir, segmentDirectoryIndexName)
	original := []byte("stale captured sidecar")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	reject := errors.New("no shared temporary sidecar credit")
	index, err := buildRuntimeSegmentDirectoryIndexWithDisk(dir, 4096, testChainKey, func(bytes int64) (NativeFileDiskReservation, error) {
		if bytes != 64<<10 {
			t.Fatal("missing complete slot/header envelope", bytes)
		}
		return nil, reject
	})
	if index != nil || !errors.Is(err, reject) {
		t.Fatal("unfunded cache rebuilt", err)
	}
	if actual, err := os.ReadFile(path); err != nil || !bytes.Equal(actual, original) {
		t.Fatal("unfunded rebuild changed old named inode", err)
	}
	partials, err := filepath.Glob(path + "-*.partial")
	if err != nil || len(partials) != 0 {
		t.Fatal("quota refusal created temporary inode", partials, err)
	}
}

func TestSegmentDirectoryDiskPublishedReplacementsAndOldOpenInodeKeepFees(t *testing.T) {
	dir, _ := writeDirectoryIndexFixture(t, 1)
	first := &nativeDiskTestToken{}
	index, err := buildRuntimeSegmentDirectoryIndexWithDisk(dir, 4096, testChainKey, func(int64) (NativeFileDiskReservation, error) { return first, nil })
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, segmentDirectoryIndexName)
	old, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	appendEntry := Segment{Generation: 2, File: "ledger-2.wal", FirstSequence: 2, LastSequence: 2, Length: 100, StoredLength: 50}
	if _, err := saveSegmentManifestSourceWithSync(dir, index.source(&appendEntry, nil, nil), durable.SyncDirectory); err != nil {
		t.Fatal(err)
	}
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}
	second := &nativeDiskTestToken{}
	next, err := buildRuntimeSegmentDirectoryIndexWithDisk(dir, 4096, testChainKey, func(int64) (NativeFileDiskReservation, error) { return second, nil })
	if err != nil || next.summary.Generations != 2 {
		t.Fatal("funded replacement did not publish complete index", err)
	}
	if err := next.Close(); err != nil {
		t.Fatal(err)
	}
	actual := make([]byte, len(before))
	if _, err := old.ReadAt(actual, 0); err != nil || !bytes.Equal(actual, before) {
		t.Fatal("actual deleted-old-inode bytes disappeared", err)
	}
	if first.retired != 0 || second.retired != 0 {
		t.Fatal("canonical replacement refunded live old/published fee")
	}
	// Matching reuse needs no additional producer reservation.
	reused, err := buildRuntimeSegmentDirectoryIndexWithDisk(dir, 4096, testChainKey, func(int64) (NativeFileDiskReservation, error) {
		t.Fatal("valid cache rebuild charged another inode")
		return nil, nil
	})
	if err != nil || reused == nil {
		t.Fatal("matching cache not reused", err)
	}
	if err := reused.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSegmentDirectoryDiskEphemeralRefundRequiresActualCloseAndReceipt(t *testing.T) {
	for _, fault := range []string{"known", "unknown-close", "missing-name"} {
		t.Run(fault, func(t *testing.T) {
			dir, scratch := writeDirectoryIndexFixture(t, 2)
			token := &nativeDiskTestToken{}
			index, err := buildSegmentDirectoryIndexInternalWithDisk(dir, scratch, 4096, nil, false, func(int64) (NativeFileDiskReservation, error) { return token, nil })
			if err != nil {
				t.Fatal(err)
			}
			name := index.index.Name()
			if fault == "unknown-close" {
				if err := index.index.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if fault == "missing-name" {
				if err := os.Rename(name, name+".retained"); err != nil {
					t.Fatal(err)
				}
			}
			err = index.Close()
			if fault == "known" {
				if err != nil || token.retired != 1 {
					t.Fatal("actual retired temporary credit not reusable", err, token.retired)
				}
				if _, err := os.Stat(name); !os.IsNotExist(err) {
					t.Fatal("refund preceded actual temporary unlink", err)
				}
				if err := durable.SyncDirectory(scratch); err != nil {
					t.Fatal(err)
				}
			} else {
				var debt *nativeFileDiskDebt
				if !errors.Is(err, ErrSegmentRetirement) || !errors.As(err, &debt) || debt.reservation != token || token.retired != 0 {
					t.Fatal("uncertain exit/name discarded actual credit", err, token.retired)
				}
				if err := index.Close(); !errors.Is(err, ErrSegmentRetirement) || token.retired != 0 {
					t.Fatal("repeated Close erased uncertainty", err)
				}
			}
		})
	}
}
