package replication

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/resourcebudget"
)

func TestOrderingLedgerReadQueuesDurableAppendAndCloseJoins(t *testing.T) {
	testOrderingStoreReadQueuesDurableAppendAndCloseJoins(t, StoreLedger)
}
func TestOrderingMetadataAndGovernanceReadQueueDurableAppendAndCloseJoin(t *testing.T) {
	for _, store := range []Store{StoreMetadata, StoreGovernance} {
		t.Run(fmt.Sprint(store), func(t *testing.T) { testOrderingStoreReadQueuesDurableAppendAndCloseJoins(t, store) })
	}
}
func testOrderingStoreReadQueuesDurableAppendAndCloseJoins(t *testing.T, store Store) {
	t.Helper()
	for _, closeDuringRead := range []bool{false, true} {
		name := "append"
		if closeDuringRead {
			name = "close"
		}
		t.Run(name, func(t *testing.T) {
			path, key, header, records := orderingInspectionFixture(t)
			j, err := OpenExistingOrderingJournal(path, key, header.ClusterID, header.Incarnation, uint64(len(records)), records[len(records)-1].MAC)
			if err != nil {
				t.Fatal(err)
			}
			defer j.Close()
			manager := frozenOrderingBudget(t)
			expected, err := j.StoreCursorsThrough(6)
			if err != nil {
				t.Fatal(err)
			}
			readPrefix := j.WithLedgerReadPrefix
			if store == StoreMetadata {
				readPrefix = j.WithMetadataReadPrefix
			}
			if store == StoreGovernance {
				readPrefix = j.WithGovernanceReadPrefix
			}
			entered, release, readDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			go func() {
				readDone <- readPrefix(context.Background(), manager, 6, 1, func(ctx context.Context, head FrozenOrderingHead, cursor StoreCursor, boundary OrderingRecord) error {
					if head.Index != 7 || cursor != expected[store-StoreLedger] || boundary.Index != 1 {
						return ErrOrderingSource
					}
					close(entered)
					if closeDuringRead {
						<-ctx.Done()
					}
					<-release
					return ctx.Err()
				})
			}()
			<-entered
			opDone := make(chan error, 1)
			if closeDuringRead {
				go func() { opDone <- j.Close() }()
			} else {
				go func() {
					_, err := j.Append(OrderingRecord{Index: 8, Term: 2, Kind: KindData, Store: StoreAudit, StoreGeneration: 1, StoreSequenceFirst: 2, StoreSequenceLast: 2, FrameDigest: sha256.Sum256([]byte("durable next native fact"))})
					opDone <- err
				}()
			}
			select {
			case err := <-opDone:
				t.Fatal("actual operation left before source callback joined", err)
			case <-time.After(20 * time.Millisecond):
			}
			close(release)
			if err := <-readDone; err != nil && (!closeDuringRead || !errors.Is(err, context.Canceled)) {
				t.Fatal(err)
			}
			if err := <-opDone; err != nil {
				t.Fatal("durable append was failed by ordinary read or Close failed", err)
			}
			if manager.Snapshot().Used != (resourcebudget.Cost{}) {
				t.Fatal("actual source fee survived known join", manager.Snapshot())
			}
		})
	}
}
