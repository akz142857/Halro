package replication

import (
	"crypto/sha256"
	"errors"
	"github.com/akz142857/Halro/internal/replicationlimits"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/replicationrange"
)

func TestNativeSourceActualProviderReaderCloseDebtKeepsOriginalFeeAndAllSetsWithdrawn(t *testing.T) {
	options, budget := nativeBudgetOptions(t)
	options.ProviderObjectDir = t.TempDir()
	data := []byte("sealed-provider-object")
	meta := ProviderObjectMetadata{Name: "file_retirement.content", TotalLength: uint64(len(data)), ChunkLength: uint64(len(data)), Digest: sha256.Sum256(data), Final: true}
	if err := os.WriteFile(filepath.Join(options.ProviderObjectDir, meta.Name), data, 0600); err != nil {
		t.Fatal(err)
	}
	source, err := NewNativeSource(options)
	if err != nil {
		t.Fatal(err)
	}
	peer, _ := acquiredNativeReader(t, source)
	source.closeProviderFile = func(file *os.File) error {
		if err := file.Close(); err != nil {
			return err
		}
		return file.Close()
	}
	before := budget.Snapshot().Used
	if payload, err := source.ReadProviderObjectChunk(meta); payload != nil || !errors.Is(err, ownedfile.ErrRetirement) {
		t.Fatal(payload, err)
	}
	if _, err := peer.Read(StoreLedger, 1, 1, 1); !errors.Is(err, replicationrange.ErrClosed) {
		t.Fatal("another set bypassed owner retirement", err)
	}
	if _, _, err := source.acquireRangeReader(); !errors.Is(err, replicationrange.ErrClosed) {
		t.Fatal("reader slot replaced after actual debt", err)
	}
	first := source.Close()
	if !errors.Is(first, ownedfile.ErrRetirement) || source.Close() != first {
		t.Fatal(first)
	}
	if got := budget.Snapshot().Used; got != before {
		t.Fatal("unknown actual provider descriptor repaid source fee", before, got)
	}
	retained := 0
	for _, file := range source.retiredProviderFiles {
		if file != nil {
			retained++
		}
	}
	if retained != 1 {
		t.Fatal("actual unknown descriptor lost", retained)
	}
}

func TestNativeProviderFixedSlotsRefuseFourthReaderAndJoinActualClose(t *testing.T) {
	options, budget := nativeBudgetOptions(t)
	options.ProviderObjectDir = t.TempDir()
	data := []byte("sealed-provider-object")
	meta := ProviderObjectMetadata{Name: "file_slots.content", TotalLength: uint64(len(data)), ChunkLength: uint64(len(data)), Digest: sha256.Sum256(data), Final: true}
	if err := os.WriteFile(filepath.Join(options.ProviderObjectDir, meta.Name), data, 0600); err != nil {
		t.Fatal(err)
	}
	source, err := NewNativeSource(options)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{}, replicationlimits.NativeReaderSets)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(func() {
		unblock()
		if err := source.Close(); err != nil {
			t.Error(err)
		}
	})
	source.closeProviderFile = func(file *os.File) error { entered <- struct{}{}; <-release; return file.Close() }
	results := make(chan error, replicationlimits.NativeReaderSets)
	for range replicationlimits.NativeReaderSets {
		go func() { _, err := source.ReadProviderObjectChunk(meta); results <- err }()
	}
	for range replicationlimits.NativeReaderSets {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("actual reader did not enter Close")
		}
	}
	fourth := make(chan error, 1)
	go func() {
		data, err := source.ReadProviderObjectChunk(meta)
		if data != nil {
			fourth <- errors.New("fourth read returned data")
		} else {
			fourth <- err
		}
	}()
	select {
	case err := <-fourth:
		if err == nil {
			t.Fatal("fourth reader acquired an uncharged FD")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fourth reader queued behind actual Close")
	}
	before := budget.Snapshot().Used
	closed := make(chan error, 1)
	go func() { closed <- source.Close() }()
	select {
	case err := <-closed:
		t.Fatal("Close escaped three actual descriptors", err)
	case <-time.After(20 * time.Millisecond):
	}
	if got := budget.Snapshot().Used; got != before {
		t.Fatal("active actual Close refunded original source", before, got)
	}
	unblock()
	for range replicationlimits.NativeReaderSets {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	assertNativeReaderFee(t, budget, 0)
}

func TestOrderingActualCloseAndConstructorDebtRemainObservable(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	header := OrderingHeader{ClusterID: "close-debt", Incarnation: "inc_01"}
	journal, err := OpenOrderingJournal(filepath.Join(t.TempDir(), "ordering"), key, header, 0, [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.file.Close(); err != nil {
		t.Fatal(err)
	}
	first := journal.Close()
	if !errors.Is(first, ownedfile.ErrRetirement) || journal.Close() != first || journal.file == nil {
		t.Fatal(first)
	}
	if _, err := journal.Append(testLeadershipRecord(1, 1, "one")); err == nil {
		t.Fatal("closed ordering admitted append")
	}
	journal, err = OpenOrderingJournalWithOptions(filepath.Join(t.TempDir(), "constructor"), key, header, 0, [32]byte{}, OrderingJournalOptions{WrapDurability: func(file *os.File) OrderingDurability {
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		return nil
	}})
	if journal != nil || !errors.Is(err, ownedfile.ErrRetirement) {
		t.Fatal(journal, err)
	}
}
