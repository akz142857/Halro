package replication

import (
	"bytes"
	"errors"
	"io"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/replicationlimits"
	"github.com/akz142857/Halro/internal/replicationrange"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func nativeBudgetOptions(t *testing.T) (NativeSourceOptions, *resourcebudget.Manager) {
	t.Helper()
	original, _, _, _ := nativeRangeFixture(t)
	options := original.options
	key := bytes.Repeat([]byte{0x31}, 32)
	options.LedgerKey, options.AuditKey, options.GovernanceKey, options.MetadataKey = key, key, key, key
	original.Close()
	m, _ := queueBudgetFixture(t, 1, replicationlimits.MinNativeReaderRAMBytes)
	options.ReaderBudget = m
	options.ReaderRAMBytes = replicationlimits.MinNativeReaderRAMBytes
	return options, m
}
func assertNativeReaderFee(t *testing.T, m *resourcebudget.Manager, count int) {
	t.Helper()
	s := m.Snapshot()
	if s.RecoveryClaims != count || s.Classes[resourcebudget.Recovery].RAM != int64(count)*replicationlimits.MinNativeReaderRAMBytes || s.Used.RAM != int64(count)*(replicationlimits.MinNativeReaderRAMBytes+resourcebudget.ClaimRAMBytes) {
		t.Fatal("reader lifetime fee mismatch", s, count)
	}
}
func TestNativeReaderBudgetAdmissionBoundsAllSetsAndReconnections(t *testing.T) {
	options, m := nativeBudgetOptions(t)
	source, err := NewNativeSource(options)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	assertNativeReaderFee(t, m, 1)
	first, releaseFirst := acquiredNativeReader(t, source)
	second, _ := acquiredNativeReader(t, source)
	for _, reader := range []FrameSource{source, first, second} {
		for store := StoreLedger; store <= StoreMetadata; store++ {
			if _, err := reader.Read(store, 1, 1, 1); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, _, err := source.acquireRangeReader(); err == nil {
		t.Fatal("third extra reader bypassed reservation")
	}
	assertNativeReaderFee(t, m, 1)
	oldLedger := first.readers.ledgerReader
	releaseFirst()
	if first.readers.ledgerReader != nil {
		t.Fatal("closed peer retained cursor references")
	}
	if _, err := oldLedger.Read(1, 1, 1); !errors.Is(err, replicationrange.ErrClosed) {
		t.Fatal("owned stream left live", err)
	}
	for range 100 {
		replacement, release := acquiredNativeReader(t, source)
		if _, err := replacement.Read(StoreLedger, 1, 1, 1); err != nil {
			t.Fatal(err)
		}
		releaseFirst() // stale release cannot revoke replacement or its reservation
		assertNativeReaderFee(t, m, 1)
		release()
	}
	source.Close()
	source.Close()
	assertNativeReaderFee(t, m, 0)
	if source.ledgerReader != nil || source.rangeReaders[0] != nil || source.rangeReaders[1] != nil || source.options.ReaderBudget != nil || source.options.LedgerKey != nil {
		t.Fatal("source retained released reader ownership")
	}
	if !bytes.Equal(options.LedgerKey, bytes.Repeat([]byte{0x31}, 32)) {
		t.Fatal("source cleared caller's borrowed key")
	}
}
func TestNativeReaderBudgetRejectsBeforeOwnedCopiesAndRollsBackAdmission(t *testing.T) {
	options, m := nativeBudgetOptions(t)
	for _, ram := range []int64{-1, 0, replicationlimits.MinNativeReaderRAMBytes - 1} {
		invalid := options
		invalid.ReaderRAMBytes = ram
		if source, err := NewNativeSource(invalid); err == nil || source != nil {
			t.Fatal("undersized reader envelope accepted", ram)
		}
		assertNativeReaderFee(t, m, 0)
	}
	invalid := options
	invalid.ReaderBudget = nil
	if source, err := NewNativeSource(invalid); err == nil || source != nil {
		t.Fatal("RAM configured without owner")
	}
	invalid = options
	invalid.AuditKey = []byte{1}
	if source, err := NewNativeSource(invalid); err == nil || source != nil {
		t.Fatal("invalid key accepted")
	}
	if m.Snapshot().Acquired[resourcebudget.Recovery] != 0 {
		t.Fatal("invalid constructor consumed recovery")
	}
	held, err := m.TryAcquireRecovery(resourcebudget.Cost{RAM: options.ReaderRAMBytes})
	if err != nil {
		t.Fatal(err)
	}
	if source, err := NewNativeSource(options); source != nil || !errors.Is(err, resourcebudget.ErrExhausted) {
		t.Fatal("reader bypassed shared manager exhaustion", err)
	}
	_ = held.Release()
	assertNativeReaderFee(t, m, 0)
	source, err := NewNativeSource(options)
	if err != nil {
		t.Fatal("rejected constructor retained ownership", err)
	}
	source.Close()
	assertNativeReaderFee(t, m, 0)
}
func TestNativeReaderBudgetCloseHoldsFeeUntilAdmittedScannerExits(t *testing.T) {
	options, m := nativeBudgetOptions(t)
	source, err := NewNativeSource(options)
	if err != nil {
		t.Fatal(err)
	}
	reader, _ := acquiredNativeReader(t, source)
	capturedKey := source.options.AuditKey
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }); source.Close() })
	reader.readers.auditReader.Close()
	reader.readers.auditReader, err = replicationrange.New(options.AuditPath, options.AuditKey, MaxPayloadBytes,
		func(_ io.ReadSeeker, key []byte, _ replicationrange.Position, _, _ uint64) (replicationrange.Range, error) {
			close(entered)
			<-release
			assertNativeReaderFee(t, m, 1)
			if !bytes.Equal(key, bytes.Repeat([]byte{0x31}, 32)) {
				t.Error("key cleared before borrowed read returned")
			}
			return replicationrange.Range{}, errors.New("controlled scan stop")
		})
	if err != nil {
		t.Fatal(err)
	}
	readDone, closeDone := make(chan error, 1), make(chan struct{})
	go func() { _, err := reader.Read(StoreAudit, 1, 1, 1); readDone <- err }()
	<-entered
	go func() { source.Close(); close(closeDone) }()
	deadline := time.Now().Add(time.Second)
	for source.mu.TryRLock() {
		source.mu.RUnlock()
		if time.Now().After(deadline) {
			t.Fatal("Close did not join read")
		}
		runtime.Gosched()
	}
	assertNativeReaderFee(t, m, 1)
	select {
	case <-closeDone:
		t.Fatal("Close refunded a live scanner")
	default:
	}
	once.Do(func() { close(release) })
	if err := <-readDone; err == nil {
		t.Fatal("scan failure swallowed")
	}
	<-closeDone
	assertNativeReaderFee(t, m, 0)
	if !bytes.Equal(capturedKey, make([]byte, 32)) {
		t.Fatal("source key was not erased")
	}
}

func TestNativeReaderBudgetOwnsAndClosesThreeCompressedLedgerCursors(t *testing.T) {
	options, m := nativeBudgetOptions(t)
	log, err := ledger.OpenWithOptions(options.LedgerPath, ledger.NewStatus(), ledger.Options{ChainKey: options.LedgerKey})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Roll(); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Compact(1); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	source, err := NewNativeSource(options)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	first, _ := acquiredNativeReader(t, source)
	second, _ := acquiredNativeReader(t, source)
	cursors := []*ledger.ReplicationRangeReader{source.ledgerReader, first.readers.ledgerReader, second.readers.ledgerReader}
	for _, cursor := range cursors {
		// Stops before sealed LastSequence so each keeps its owned gzip stream.
		if _, err := cursor.Read(1, 1, 1); err != nil {
			t.Fatal(err)
		}
	}
	assertNativeReaderFee(t, m, 1)
	source.Close()
	assertNativeReaderFee(t, m, 0)
	for _, cursor := range cursors {
		if _, err := cursor.Read(1, 2, 2); !errors.Is(err, replicationrange.ErrClosed) {
			t.Fatal("gzip reader survived source fee refund", err)
		}
	}
}
