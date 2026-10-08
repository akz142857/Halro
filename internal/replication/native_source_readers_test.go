package replication

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/replicationrange"
)

func acquiredNativeReader(t *testing.T, source *NativeSource) (*nativeSourceRangeReader, func()) {
	t.Helper()
	reader, release, err := source.acquireRangeReader()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	return reader.(*nativeSourceRangeReader), release
}

func nativeReaderCount(source *NativeSource) int {
	source.mu.RLock()
	defer source.mu.RUnlock()
	count := 0
	for _, reader := range source.rangeReaders {
		if reader != nil {
			count++
		}
	}
	return count
}

func TestNativeReaderPoolLimitsReleasesAndClosesEveryCursor(t *testing.T) {
	source, _, _, _ := nativeRangeFixture(t)
	first, releaseFirst := acquiredNativeReader(t, source)
	second, _ := acquiredNativeReader(t, source)
	if _, _, err := source.acquireRangeReader(); err == nil || nativeReaderCount(source) != 2 {
		t.Fatal("reader pool grew beyond two Replica slots", err)
	}
	for store := StoreLedger; store <= StoreMetadata; store++ {
		want, err := source.Read(store, 1, 1, 1)
		if err != nil {
			t.Fatal(err)
		}
		for _, reader := range []*nativeSourceRangeReader{first, second} {
			got, err := reader.Read(store, 1, 1, 1)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatal("independent reader changed native bytes", store, err)
			}
		}
	}
	firstLedgerReader := first.readers.ledgerReader
	releaseFirst()
	replacement, _ := acquiredNativeReader(t, source)
	releaseFirst() // A stale release cannot close a newly allocated slot.
	if nativeReaderCount(source) != 2 {
		t.Fatal("stale release removed replacement")
	}
	if _, err := replacement.Read(StoreLedger, 1, 2, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Read(StoreAudit, 1, 1, 1); !errors.Is(err, replicationrange.ErrClosed) {
		t.Fatal("released reader retained authority", err)
	}
	if _, err := firstLedgerReader.Read(1, 1, 1); !errors.Is(err, replicationrange.ErrClosed) {
		t.Fatal("released owned Ledger stream was not closed", err)
	}
	source.Close()
	for _, reader := range []FrameSource{source, second, replacement} {
		for store := StoreLedger; store <= StoreMetadata; store++ {
			if _, err := reader.Read(store, 1, 1, 1); !errors.Is(err, replicationrange.ErrClosed) {
				t.Fatal("source close left reader live", store, err)
			}
		}
		if _, err := reader.(providerObjectSource).ReadProviderObjectChunk(ProviderObjectMetadata{}); !errors.Is(err, replicationrange.ErrClosed) {
			t.Fatal("closed provider source left live", err)
		}
	}
	if _, _, err := source.acquireRangeReader(); !errors.Is(err, replicationrange.ErrClosed) || nativeReaderCount(source) != 0 {
		t.Fatal(err)
	}
	if _, err := source.Cursor(StoreAudit); !errors.Is(err, replicationrange.ErrClosed) {
		t.Fatal(err)
	}
	if _, err := source.HeadAt(StoreAudit, StoreCursor{Generation: 1, Sequence: 1}); !errors.Is(err, replicationrange.ErrClosed) {
		t.Fatal(err)
	}
}

func TestNativePeerReadersAndPaginationHaveIndependentPrefixAuthority(t *testing.T) {
	source, _, frames, _ := nativeRangeFixture(t)
	first, releaseFirst := acquiredNativeReader(t, source)
	second, _ := acquiredNativeReader(t, source)
	for _, reader := range []FrameSource{first, second, source} {
		if _, err := reader.Read(StoreAudit, 1, 1, 1); err != nil {
			t.Fatal(err)
		}
	}
	// A fresh root audit must fail after this corruption, while each already
	// authenticated prefix retains the documented process-local trust. If
	// peer 2 shares peer 1's cursor, its repeated read of frame 2 would reset
	// to the corrupt root and fail. Pagination must affect neither peer.
	bad := bytes.Join(frames, nil)
	bad[len(frames[0])-1] ^= 1
	if err := os.WriteFile(source.options.AuditPath, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Read(StoreAudit, 1, 1, 1); err == nil {
		t.Fatal("fresh pagination audit trusted corrupt root")
	}
	for _, reader := range []FrameSource{first, second} {
		if got, err := reader.Read(StoreAudit, 1, 2, 2); err != nil || !bytes.Equal(got, frames[1]) {
			t.Fatal("other reader revoked this peer's own prefix", err)
		}
	}
	releaseFirst()
	replacement, _ := acquiredNativeReader(t, source)
	if _, err := replacement.Read(StoreAudit, 1, 2, 2); err == nil {
		t.Fatal("replacement inherited old generation's prefix authority")
	}
}

func TestNativeSourceMutationsRevokeAllReaderPrefixes(t *testing.T) {
	for _, store := range []Store{StoreLedger, StoreAudit, StoreGovernance, StoreMetadata} {
		for _, stage := range []string{"truncate", "repair", "failed-truncate", "failed-repair", "repair-roll"} {
			if stage == "repair-roll" && store != StoreLedger {
				continue
			}
			t.Run(fmt.Sprintf("store-%d/%s", store, stage), func(t *testing.T) {
				source, _, _, _ := nativeRangeFixture(t)
				first, _ := acquiredNativeReader(t, source)
				second, _ := acquiredNativeReader(t, source)
				for _, reader := range []FrameSource{source, first, second} {
					if _, err := reader.Read(store, 1, 1, 1); err != nil {
						t.Fatal(err)
					}
				}
				oldRevision, err := first.sourceRevision()
				if err != nil {
					t.Fatal(err)
				}
				last, err := source.Read(store, 1, 2, 2)
				if err != nil {
					t.Fatal(err)
				}
				path := map[Store]string{StoreLedger: source.options.LedgerPath, StoreAudit: source.options.AuditPath, StoreGovernance: source.options.GovernancePath, StoreMetadata: source.options.MetadataPath}[store]
				bad, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				bad[len(bad)-len(last)-1] ^= 1
				failed := stage == "failed-truncate" || stage == "failed-repair"
				if failed {
					if err := os.WriteFile(path, bad, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				switch stage {
				case "repair", "failed-repair":
					err = source.RepairReplicaTail(store, StoreCursor{Generation: 1, Sequence: 2})
				case "repair-roll":
					err = source.RepairReplicaLedgerRoll(StoreCursor{Generation: 1, Sequence: 2})
				default:
					err = source.Truncate(store, StoreCursor{Generation: 1, Sequence: 1})
				}
				if (err != nil) != failed {
					t.Fatal(stage, err)
				}
				sent := false
				if err := first.deliverAtRevision(oldRevision, func() error { sent = true; return nil }); !errors.Is(err, errSourceRevisionChanged) || sent {
					t.Fatal("explicit mutation retained candidate delivery authority", err, sent)
				}
				newRevision, err := first.sourceRevision()
				if err != nil || newRevision == oldRevision {
					t.Fatal("mutation failed to rotate revision", err)
				}
				// Regrow to the prior length: file size alone cannot revoke a
				// cached prefix. Explicit mutation must reset every reader.
				if err := os.WriteFile(path, bad, 0o600); err != nil {
					t.Fatal(err)
				}
				for _, reader := range []FrameSource{source, first, second} {
					if got, err := reader.Read(store, 1, 2, 2); err == nil || len(got) != 0 {
						t.Fatal("source mutation left old prefix authority", err)
					}
				}
			})
		}
	}
}

func TestNativeSourceCloseWaitsForAdmittedRangeAndClearsKeysAfterward(t *testing.T) {
	source, _, _, _ := nativeRangeFixture(t)
	reader, _ := acquiredNativeReader(t, source)
	entered, release, readDone, closeDone := make(chan struct{}), make(chan struct{}), make(chan error, 1), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	controlled := errors.New("controlled scanner stop")
	reader.readers.auditReader.Close()
	var err error
	reader.readers.auditReader, err = replicationrange.New(source.options.AuditPath, source.options.AuditKey, MaxPayloadBytes,
		func(_ io.ReadSeeker, key []byte, _ replicationrange.Position, _, _ uint64) (replicationrange.Range, error) {
			close(entered)
			<-release
			if !bytes.Equal(key, bytes.Repeat([]byte{0x31}, 32)) {
				return replicationrange.Range{}, errors.New("source close cleared a borrowed scanner key")
			}
			return replicationrange.Range{}, controlled
		})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, err := reader.Read(StoreAudit, 1, 1, 1); readDone <- err }()
	<-entered
	go func() { source.Close(); close(closeDone) }()
	deadline := time.Now().Add(time.Second)
	for source.mu.TryRLock() {
		source.mu.RUnlock()
		if time.Now().After(deadline) {
			t.Fatal("close did not reach its write-lock drain")
		}
		runtime.Gosched()
	}
	select {
	case <-closeDone:
		t.Fatal("close finished before admitted scanner returned")
	default:
	}
	once.Do(func() { close(release) })
	if err := <-readDone; !errors.Is(err, controlled) {
		t.Fatal(err)
	}
	<-closeDone
	if _, err := reader.Read(StoreAudit, 1, 1, 1); !errors.Is(err, replicationrange.ErrClosed) {
		t.Fatal(err)
	}
}

func TestPrimaryNativeWindowsOwnBoundedReadersAcrossReconnectForgetAndStop(t *testing.T) {
	source, journal, _, _ := nativeRangeFixture(t)
	c := windowPrimary(t, journal, source, 0, DefaultPrimaryWindowLimits(), nil)
	first, second := &windowSender{}, &windowSender{}
	if err := c.PreparePeer("halro-1", 0, 0, first.send); err != nil {
		t.Fatal(err)
	}
	if err := c.PreparePeer("halro-2", 0, 0, second.send); err != nil || nativeReaderCount(source) != 2 {
		t.Fatal(err)
	}
	oldReader := c.windowFor("halro-1").reader
	for i := 0; i < 10; i++ {
		if err := c.PreparePeer("halro-1", 0, 0, first.send); err != nil || nativeReaderCount(source) != 2 {
			t.Fatal("reconnect leaked a reader", err)
		}
		if _, err := c.PendingFrom(0); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := oldReader.Read(StoreAudit, 1, 1, 1); !errors.Is(err, replicationrange.ErrClosed) {
		t.Fatal(err)
	}
	first.err = ErrPeerSessionClosed
	if _, err := c.RecordSchemaBoundary(1, 2); err != nil || nativeReaderCount(source) != 1 || c.windowFor("halro-1").reader != nil {
		t.Fatal("failed send retained its reader", err)
	}
	c.ForgetPeer("halro-2")
	if nativeReaderCount(source) != 0 || c.windowFor("halro-2").bytes != 0 {
		t.Fatal("forget retained window ownership")
	}
	if err := c.PreparePeer("halro-2", 0, 0, second.send); err != nil {
		t.Fatal(err)
	}
	c.Stop()
	if nativeReaderCount(source) != 0 {
		t.Fatal("coordinator Stop retained source leases")
	}
	if _, err := source.Read(StoreAudit, 1, 1, 1); err != nil {
		t.Fatal("coordinator Stop closed caller-owned source", err)
	}
}

func TestNativeCandidateDeliveryJoinsMutationAndLeaseClose(t *testing.T) {
	for _, stage := range []string{"truncate", "source-close", "lease-close"} {
		t.Run(stage, func(t *testing.T) {
			source, _, _, _ := nativeRangeFixture(t)
			reader, releaseReader := acquiredNativeReader(t, source)
			revision, err := reader.sourceRevision()
			if err != nil {
				t.Fatal(err)
			}
			entered, release, delivered, mutated := make(chan struct{}), make(chan struct{}), make(chan error, 1), make(chan error, 1)
			var once sync.Once
			t.Cleanup(func() { once.Do(func() { close(release) }) })
			go func() {
				delivered <- reader.deliverAtRevision(revision, func() error { close(entered); <-release; return nil })
			}()
			<-entered
			go func() {
				switch stage {
				case "truncate":
					mutated <- source.Truncate(StoreAudit, StoreCursor{Generation: 1, Sequence: 1})
				case "source-close":
					source.Close()
					mutated <- nil
				default:
					releaseReader()
					mutated <- nil
				}
			}()
			deadline := time.Now().Add(time.Second)
			for source.mu.TryRLock() {
				source.mu.RUnlock()
				if time.Now().After(deadline) {
					t.Fatal("mutation did not join admitted queue copy")
				}
				runtime.Gosched()
			}
			select {
			case err := <-mutated:
				t.Fatal("mutation finished before synchronous queue copy", err)
			default:
			}
			once.Do(func() { close(release) })
			if err := <-delivered; err != nil {
				t.Fatal(err)
			}
			if err := <-mutated; err != nil {
				t.Fatal(err)
			}
			sent := false
			if err := reader.deliverAtRevision(revision, func() error { sent = true; return nil }); !errors.Is(err, errSourceRevisionChanged) || sent {
				t.Fatal("stale candidate sent after revocation", err, sent)
			}
		})
	}
}
