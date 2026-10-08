package ledger

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/akz142857/Halro/internal/ownedfile"
)

type retainedNativeAdmission struct{ releases int }

func (a *retainedNativeAdmission) Release() error { a.releases++; return nil }

func TestLedgerNativeActualCloseDebtKeepsDescriptorKeyAndAdmission(t *testing.T) {
	l, err := OpenWithOptions(filepath.Join(t.TempDir(), "ledger.wal"), NewStatus(), Options{ChainKey: testChainKey})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Append(context.Background(), validReservation("actual", "actual")); err != nil {
		t.Fatal(err)
	}
	original, key := l.file, bytes.Clone(l.chainKey)
	admission := &retainedNativeAdmission{}
	l.uncertainAdmissions = []AppendAdmission{admission}
	if err := original.Close(); err != nil {
		t.Fatal(err)
	}
	first := l.Close()
	if !errors.Is(first, ownedfile.ErrRetirement) || !errors.Is(first, ErrSegmentRetirement) || !errors.Is(first, os.ErrClosed) {
		t.Fatal(first)
	}
	for i := 0; i < 2; i++ {
		if l.Close() != first || l.file != original || !bytes.Equal(l.chainKey, key) || admission.releases != 0 || len(l.uncertainAdmissions) != 1 {
			t.Fatal("repeat Close cleared actual unknown exit or refunded admission")
		}
	}
	if _, err := l.Replay(Watermark{}, nil); err == nil {
		t.Fatal("retained descriptor reopened legacy replay authority")
	}
	if _, err := l.ReplayAuthenticated(context.Background(), ReplayAnchor{}, nil); err == nil {
		t.Fatal("retained descriptor reopened authenticated replay authority")
	}
	if _, err := l.Append(context.Background(), validReservation("later", "later")); err == nil {
		t.Fatal("retained descriptor reopened native writes")
	}
}

func TestLedgerConcurrentKnownCloseJoinsOnceAndReleasesAdmission(t *testing.T) {
	l, err := OpenWithOptions(filepath.Join(t.TempDir(), "ledger.wal"), NewStatus(), Options{ChainKey: testChainKey})
	if err != nil {
		t.Fatal(err)
	}
	admission := &retainedNativeAdmission{}
	l.uncertainAdmissions = []AppendAdmission{admission}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := l.Close(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if admission.releases != 1 || l.file != nil || l.chainKey != nil || l.segmentDirectory != nil || l.uncertainAdmissions != nil {
		t.Fatal("known concurrent Close did not join and release once")
	}
}
