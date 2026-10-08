package bolt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/pricinglimits"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func pendingIntentFixture(id string, at time.Time) domain.PricingAuditIntent {
	return domain.PricingAuditIntent{EventID: id, OccurredAt: at, ActorID: "system", Action: "deployment_price.migrate", TargetType: "deployment_pricing", TargetID: "dep_one", RequestSHA256: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
}

func seedPendingIntents(t *testing.T, s *Store, count int) {
	t.Helper()
	for start := 0; start < count; start += 64 {
		if err := s.update(func(tx *Tx) error {
			for i := start; i < count && i < start+64; i++ {
				// Receipt order deliberately differs from the event timestamps.
				if err := putPricingAuditIntentTx(tx, pendingIntentFixture(fmt.Sprintf("event_%05d", i), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(count-i)*time.Second))); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func pendingCountForTest(t *testing.T, s *Store, want uint64) {
	t.Helper()
	got, err := s.PendingPricingAuditIntentCount(context.Background())
	if err != nil || got != want {
		t.Fatal(got, want, err)
	}
}

func TestPricingPendingCountAtomicMutationsAndRollback(t *testing.T) {
	s := openJournalledStore(t, filepath.Join(t.TempDir(), "metadata.db"))
	seedPendingIntents(t, s, 130)
	pendingCountForTest(t, s, 130)
	if err := s.MarkPricingAuditIntentDelivered(context.Background(), "event_00001"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkPricingAuditIntentDelivered(context.Background(), "event_00001"); err != nil {
		t.Fatal(err)
	}
	pendingCountForTest(t, s, 129)
	if err := s.update(func(tx *Tx) error {
		if err := tx.Bucket(bucketPricingAuditIntents).Delete([]byte("event_00002")); err != nil {
			return err
		}
		return tx.Bucket(bucketPricingAuditIntents).Delete([]byte("event_00001"))
	}); err != nil {
		t.Fatal(err)
	}
	pendingCountForTest(t, s, 128)
	before, _ := s.MetadataJournalState()
	if err := s.update(func(tx *Tx) error {
		if err := putPricingAuditIntentTx(tx, pendingIntentFixture("rollback", time.Now().UTC())); err != nil {
			return err
		}
		return errors.New("rollback")
	}); err == nil {
		t.Fatal("expected rollback")
	}
	for _, change := range []func(*Tx) error{
		func(tx *Tx) error {
			return tx.Bucket(bucketPricingAuditControl).Put(pricingPendingCountKey, encodeUint64(0))
		},
		func(tx *Tx) error { return tx.DeleteBucket(bucketPricingAuditIntents) },
	} {
		if err := s.update(change); err == nil {
			t.Fatal("caller bypassed counter owner")
		}
	}
	after, _ := s.MetadataJournalState()
	if before.Sequence != after.Sequence {
		t.Fatal(before, after)
	}
	pendingCountForTest(t, s, 128)
}

func TestPricingPendingCountOverflowUnderflowAndMalformedRefuseBeforeNativeAppend(t *testing.T) {
	for _, fault := range []string{"overflow", "underflow", "malformed"} {
		t.Run(fault, func(t *testing.T) {
			s := openJournalledStore(t, filepath.Join(t.TempDir(), "metadata.db"))
			seedPendingIntents(t, s, 1)
			if err := s.migrate(func(tx *Tx) error {
				value := encodeUint64(0)
				if fault == "overflow" {
					value = encodeUint64(math.MaxUint64)
				}
				if fault == "malformed" {
					value = []byte{1}
				}
				return tx.Bucket(bucketPricingAuditControl).Put(pricingPendingCountKey, value)
			}); err != nil {
				t.Fatal(err)
			}
			before, _ := s.MetadataJournalState()
			var err error
			if fault == "overflow" {
				err = s.update(func(tx *Tx) error {
					return putPricingAuditIntentTx(tx, pendingIntentFixture("overflow", time.Now().UTC()))
				})
			} else {
				err = s.MarkPricingAuditIntentDelivered(context.Background(), "event_00000")
			}
			if err == nil {
				t.Fatal("counter fault was accepted")
			}
			after, _ := s.MetadataJournalState()
			if before.Sequence != after.Sequence {
				t.Fatal(before, after)
			}
		})
	}
}

func TestPricingPendingRecoveryFinitePagesAndStableReceiptsKeepOriginalTime(t *testing.T) {
	s := openJournalledStore(t, filepath.Join(t.TempDir(), "metadata.db"))
	seedPendingIntents(t, s, 130)
	count := 0
	err := s.RecoverPendingPricingAuditIntents(context.Background(), func(ctx context.Context, intent domain.PricingAuditIntent) error {
		if intent.EventID != fmt.Sprintf("event_%05d", count) || !intent.OccurredAt.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(130-count)*time.Second)) {
			t.Fatal(intent, count)
		}
		count++
		return s.MarkPricingAuditIntentDelivered(ctx, intent.EventID)
	}, nil)
	if err != nil || count != 130 {
		t.Fatal(count, err)
	}
	pendingCountForTest(t, s, 0)
}

func TestPricingPendingRecoveryPrevalidatesLateMalformedAndRejectsForgedZeroCount(t *testing.T) {
	for _, fault := range []string{"late-invalid", "zero-count"} {
		t.Run(fault, func(t *testing.T) {
			s := openJournalledStore(t, filepath.Join(t.TempDir(), "metadata.db"))
			seedPendingIntents(t, s, 130)
			if err := s.migrate(func(tx *Tx) error {
				if fault == "zero-count" {
					return tx.Bucket(bucketPricingAuditControl).Put(pricingPendingCountKey, encodeUint64(0))
				}
				intent := pendingIntentFixture("event_00129", time.Now().UTC())
				intent.ActorID = ""
				raw, err := json.Marshal(intent)
				if err != nil {
					return err
				}
				return tx.Bucket(bucketPricingAuditIntents).Put([]byte(intent.EventID), raw)
			}); err != nil {
				t.Fatal(err)
			}
			calls := 0
			if err := s.RecoverPendingPricingAuditIntents(context.Background(), func(context.Context, domain.PricingAuditIntent) error { calls++; return nil }, nil); err == nil || calls != 0 {
				t.Fatal("incomplete validation delivered records", calls, err)
			}
		})
	}
}

func TestPricingPendingRecoveryNewRecordBehindCursorPreventsReady(t *testing.T) {
	s := openJournalledStore(t, filepath.Join(t.TempDir(), "metadata.db"))
	seedPendingIntents(t, s, 65)
	added := false
	err := s.RecoverPendingPricingAuditIntents(context.Background(), func(ctx context.Context, intent domain.PricingAuditIntent) error {
		if err := s.MarkPricingAuditIntentDelivered(ctx, intent.EventID); err != nil {
			return err
		}
		if !added {
			added = true
			return s.update(func(tx *Tx) error {
				return putPricingAuditIntentTx(tx, pendingIntentFixture("earlier", time.Now().UTC()))
			})
		}
		return nil
	}, nil)
	if !errors.Is(err, ErrPricingRecoveryChanged) {
		t.Fatal(err)
	}
	pendingCountForTest(t, s, 1)
	if err := s.RecoverPendingPricingAuditIntents(context.Background(), func(ctx context.Context, i domain.PricingAuditIntent) error {
		return s.MarkPricingAuditIntentDelivered(ctx, i.EventID)
	}, nil); err != nil {
		t.Fatal(err)
	}
	pendingCountForTest(t, s, 0)
}

func TestMigration43PendingCountBackfillRetriesAtomically(t *testing.T) {
	for _, point := range []string{"before_pricing_audit_count", "after_pricing_audit_count", "after_schema_version", "after_migration_history"} {
		t.Run(point, func(t *testing.T) {
			s := openJournalledStore(t, filepath.Join(t.TempDir(), "metadata.db"))
			seedPendingIntents(t, s, 3)
			before, _ := s.PricingBackupState()
			path := s.path
			if err := s.migrate(func(tx *Tx) error {
				if err := tx.DeleteBucket(bucketPricingAuditControl); err != nil {
					return err
				}
				if err := tx.Bucket(bucketMigrationHistory).Delete(encodeUint64(43)); err != nil {
					return err
				}
				return tx.Bucket(bucketMeta).Put(keySchemaVersion, encodeUint64(42))
			}); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			bad, err := openWithMigrationStepHook(path, func(version uint64, step string) error {
				if version == 43 && step == point {
					return errors.New("injected")
				}
				return nil
			})
			if err == nil {
				bad.Close()
				t.Fatal("migration committed")
			}
			raw, err := OpenReplica(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := raw.view(func(tx *Tx) error {
				version, _ := decodeUint64(tx.Bucket(bucketMeta).Get(keySchemaVersion))
				if version != 42 || tx.Bucket(bucketPricingAuditControl) != nil || tx.Bucket(bucketMigrationHistory).Get(encodeUint64(43)) != nil {
					t.Fatal("partial migration published", version)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}
			s = openJournalledStore(t, path)
			pendingCountForTest(t, s, 3)
			after, _ := s.PricingBackupState()
			if before != after {
				t.Fatal(before, after)
			}
		})
	}
}

func TestPricingPendingRecoveryInterruptedDeliveryResumesRemaining(t *testing.T) {
	s := openJournalledStore(t, filepath.Join(t.TempDir(), "metadata.db"))
	seedPendingIntents(t, s, 130)
	stop := errors.New("delivery interrupted")
	calls := 0
	err := s.RecoverPendingPricingAuditIntents(context.Background(), func(ctx context.Context, i domain.PricingAuditIntent) error {
		if calls == 70 {
			return stop
		}
		calls++
		return s.MarkPricingAuditIntentDelivered(ctx, i.EventID)
	}, nil)
	if !errors.Is(err, stop) {
		t.Fatal(err)
	}
	pendingCountForTest(t, s, 60)
	calls = 70
	if err := s.RecoverPendingPricingAuditIntents(context.Background(), func(ctx context.Context, i domain.PricingAuditIntent) error {
		if i.EventID != fmt.Sprintf("event_%05d", calls) {
			t.Fatal(i.EventID, calls)
		}
		calls++
		return s.MarkPricingAuditIntentDelivered(ctx, i.EventID)
	}, nil); err != nil || calls != 130 {
		t.Fatal(calls, err)
	}
	pendingCountForTest(t, s, 0)
}

func TestPricingPendingRecoveryCloseJoinsCallbackAndKeepsRecoveryCharge(t *testing.T) {
	s := openJournalledStore(t, filepath.Join(t.TempDir(), "metadata.db"))
	l := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: 128 << 20}, RecoveryReserve: resourcebudget.Cost{RAM: 40 << 20}, MaxClaims: 4, RecoveryClaims: 2, MaxOwners: 2, MaxPressureAge: time.Hour}
	l.Classes[resourcebudget.Recovery] = resourcebudget.Cost{RAM: 35 << 20}
	l.Classes[resourcebudget.Control] = resourcebudget.Cost{RAM: 4 * resourcebudget.ClaimRAMBytes}
	m, err := resourcebudget.New(l)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ConfigurePricingResources(m, pricinglimits.MinLifetimeRAMBytes, pricinglimits.MinIORAMBytes); err != nil {
		t.Fatal(err)
	}
	seedPendingIntents(t, s, 1)
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- s.RecoverPendingPricingAuditIntents(context.Background(), func(ctx context.Context, i domain.PricingAuditIntent) error {
			close(entered)
			<-release
			return ctx.Err()
		}, nil)
	}()
	<-entered
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	if stats := m.Snapshot(); stats.Pressure != resourcebudget.Unknown || stats.RecoveryClaims != 2 || stats.Classes[resourcebudget.Recovery].RAM != pricinglimits.MinLifetimeRAMBytes+pricinglimits.MinIORAMBytes {
		t.Fatal(stats)
	}
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case err := <-closed:
		t.Fatal("Close returned while callback is live", err)
	case <-time.After(20 * time.Millisecond):
	}
	if stats := m.Snapshot(); stats.RecoveryClaims != 2 {
		t.Fatal("close refunded running job", stats)
	}
	close(release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if stats := m.Snapshot(); stats.Used != (resourcebudget.Cost{}) {
		t.Fatal(stats)
	}
}

func TestPricingPendingMergedFrameAndReplicaApplyOwnCounter(t *testing.T) {
	primary := openJournalledStore(t, filepath.Join(t.TempDir(), "metadata.db"))
	path := primary.path
	if err := primary.Close(); err != nil {
		t.Fatal(err)
	}
	replicaDir := t.TempDir()
	replicaPath := filepath.Join(replicaDir, "metadata.db")
	copyTestFile(t, path, replicaPath)
	copyTestFile(t, filepath.Join(filepath.Dir(path), MetadataJournalFileName), filepath.Join(replicaDir, MetadataJournalFileName))
	primary = openJournalledStore(t, path)
	callbacks := []func(*Tx) error{}
	for _, id := range []string{"first", "second"} {
		callbacks = append(callbacks, func(tx *Tx) error { return putPricingAuditIntentTx(tx, pendingIntentFixture(id, time.Now().UTC())) })
	}
	primary.journalMu.RLock()
	receipt, _, err := primary.updateAll(callbacks)
	primary.journalMu.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	countOps := 0
	for _, op := range receipt.Record.Ops {
		if len(op.Path) > 0 && op.Path[0] == string(bucketPricingAuditControl) {
			countOps++
		}
	}
	if countOps != 1 || len(receipt.Record.Ops) != 3 {
		t.Fatal(receipt.Record)
	}
	pendingCountForTest(t, primary, 2)
	if err := primary.MarkPricingAuditIntentDelivered(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	state, err := primary.MetadataJournalState()
	if err != nil {
		t.Fatal(err)
	}
	copyTestFile(t, primary.MetadataJournalPath(), filepath.Join(replicaDir, MetadataJournalFileName))
	replica, err := OpenReplica(replicaPath)
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	journal, err := metadatajournal.OpenReplica(filepath.Join(replicaDir, MetadataJournalFileName), testJournalKey())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := replica.AttachReplicaMetadataJournal(journal, testJournalKey()); err != nil {
		t.Fatal(err)
	}
	pendingCountForTest(t, replica, 0)
	if _, err := replica.ApplyReplicaMetadataThrough(context.Background(), state.Epoch, state.Sequence-1); err != nil {
		t.Fatal(err)
	}
	pendingCountForTest(t, replica, 2)
	if _, err := replica.ApplyReplicaMetadataThrough(context.Background(), state.Epoch, state.Sequence); err != nil {
		t.Fatal(err)
	}
	pendingCountForTest(t, replica, 1)
	if err := replica.MarkPricingAuditIntentDelivered(context.Background(), "second"); err == nil {
		t.Fatal("replica wrote local authority")
	}
	pendingCountForTest(t, replica, 1)
}

func TestPricingPendingPhysicalPageBoundsIncludeDeliveredPrefixesAndWideRow(t *testing.T) {
	s := openJournalledStore(t, filepath.Join(t.TempDir(), "metadata.db"))
	// Historical delivered rows need not satisfy today's source-assurance contract.
	if err := s.migrate(func(tx *Tx) error {
		b := tx.Bucket(bucketPricingAuditIntents)
		for i := 0; i < 130; i++ {
			if err := b.Put([]byte(fmt.Sprintf("done_%05d", i)), []byte(`{"delivered":true}`)); err != nil {
				return err
			}
		}
		wide := append([]byte(`{"delivered":true,"unknown":"`), bytes.Repeat([]byte("x"), 2<<20)...)
		wide = append(wide, []byte(`"}`)...)
		return b.Put([]byte("wide"), wide)
	}); err != nil {
		t.Fatal(err)
	}
	pages := 0
	total := 0
	var after []byte
	for {
		rows, next, err := s.pricingIntentPage(context.Background(), after, []byte("zz"))
		if err != nil {
			t.Fatal(err)
		}
		used := 0
		for _, row := range rows {
			used += len(row.id) + len(row.raw)
		}
		if len(rows) > 64 || used > priceAuditPageBytes && len(rows) != 1 {
			t.Fatal(len(rows), used)
		}
		total += len(rows)
		pages++
		if next == nil {
			break
		}
		after = next
	}
	if total != 131 || pages != 4 {
		t.Fatal(total, pages)
	}
	if err := s.RecoverPendingPricingAuditIntents(context.Background(), func(context.Context, domain.PricingAuditIntent) error {
		t.Fatal("delivered history replayed")
		return nil
	}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestPricingPendingRecoveryRetriesExistingAuditReceiptBeforeEvictionWindow(t *testing.T) {
	s := openJournalledStore(t, filepath.Join(t.TempDir(), "metadata.db"))
	const n = 4098
	seedPendingIntents(t, s, n)
	path := filepath.Join(t.TempDir(), "audit.log")
	key := bytes.Repeat([]byte{0x3a}, 32)
	log, err := audit.Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	toEvent := func(i domain.PricingAuditIntent) audit.Event {
		return audit.Event{EventID: i.EventID, OccurredAt: i.OccurredAt, ActorType: "admin_user", ActorID: i.ActorID, Action: i.Action, TargetType: i.TargetType, TargetID: i.TargetID, Outcome: "success"}
	}
	existing := pendingIntentFixture(fmt.Sprintf("event_%05d", n-1), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Second))
	if _, err := log.Append(context.Background(), toEvent(existing)); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	log, err = audit.Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	calls := 0
	if err := s.RecoverPendingPricingAuditIntents(context.Background(), func(ctx context.Context, i domain.PricingAuditIntent) error {
		if calls == 0 && i.EventID != existing.EventID {
			t.Fatal("interrupted receipt would be evicted", i.EventID)
		}
		calls++
		if _, err := log.Append(ctx, toEvent(i)); err != nil {
			return err
		}
		return s.MarkPricingAuditIntentDelivered(ctx, i.EventID)
	}, log.HasRetainedEventID); err != nil {
		t.Fatal(err)
	}
	if calls != n || log.Summary().Records != n {
		t.Fatal(calls, log.Summary())
	}
	seen := 0
	_, err = log.Replay(func(r audit.Record) error {
		if r.Event.EventID == existing.EventID {
			seen++
		}
		return nil
	})
	if err != nil || seen != 1 {
		t.Fatal(seen, err)
	}
	pendingCountForTest(t, s, 0)
}
