package bolt

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/metadatajournal"
)

func priceRevisionForTest(t *testing.T, s *Store, dep string) uint64 {
	t.Helper()
	var revision uint64
	if err := s.view(func(tx *Tx) (err error) {
		revision, err = deploymentPriceRevisionTx(tx, dep)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return revision
}

func TestPricingRevisionUnrelatedConcurrentWritesKeepAuditUsable(t *testing.T) {
	for _, otherPrice := range []bool{false, true} {
		t.Run(map[bool]string{false: "runtime", true: "other-deployment"}[otherPrice], func(t *testing.T) {
			s, base := streamPriceFixture(t, 130, 0)
			before := priceRevisionForTest(t, s, "dep_stream")
			ctx := &priceAuditChangeContext{Context: context.Background(), store: s, mutate: func(tx *Tx) error {
				if !otherPrice {
					return tx.Bucket(bucketMeta).Put(keyRuntimeSettings, []byte("during-audit"))
				}
				p := newStoredPrice("other_price", "dep_other", base)
				p.Version, p.Revision = 1, 1
				return putDeploymentPriceVersionTx(tx, p)
			}}
			if revision, err := s.auditDeploymentTimeline(ctx, "dep_stream"); err != nil || revision != before || !ctx.changed {
				t.Fatal(revision, before, err, ctx.changed)
			}
		})
	}
}

func TestPricingRevisionGenericWritesInvalidateWarmAuditAndBumpOnce(t *testing.T) {
	s, base := streamPriceFixture(t, 130, 0)
	ctx := context.Background()
	if _, err := s.SelectDeploymentPriceVersion(ctx, "dep_stream", base.Add(129*time.Second)); err != nil {
		t.Fatal(err)
	}
	before := priceRevisionForTest(t, s, "dep_stream")
	if err := s.update(func(tx *Tx) error {
		prices := tx.Bucket(bucketDeploymentPriceVersions)
		for _, id := range []string{"price_stream_128", "price_stream_129"} {
			var price domain.DeploymentPriceVersion
			if err := json.Unmarshal(prices.Get([]byte(id)), &price); err != nil {
				return err
			}
			price.Version = 130 // creates duplicates outside the selected row
			raw, err := json.Marshal(price)
			if err != nil {
				return err
			}
			if err := prices.Put([]byte(id), raw); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := priceRevisionForTest(t, s, "dep_stream"); got != before+1 {
		t.Fatal(got, before)
	}
	if price, err := s.SelectDeploymentPriceVersion(ctx, "dep_stream", base); !errors.Is(err, domain.ErrPriceTimelineConflict) || price.ID != "" {
		t.Fatal("warm marker bypassed changed history", price.ID, err)
	}
	if s.deploymentPricingState("dep_stream").timelineLoaded {
		t.Fatal("failed audit stayed usable")
	}
}

func TestPricingRevisionMergedCallbacksShareOneFenceAndNativeFrame(t *testing.T) {
	s, _ := streamPriceFixture(t, 2, 0)
	before := priceRevisionForTest(t, s, "dep_stream")
	callbacks := []func(*Tx) error{}
	for _, id := range []string{"price_stream_0", "price_stream_1"} {
		callbacks = append(callbacks, func(tx *Tx) error {
			prices := tx.Bucket(bucketDeploymentPriceVersions)
			var price domain.DeploymentPriceVersion
			if err := json.Unmarshal(prices.Get([]byte(id)), &price); err != nil {
				return err
			}
			price.InputMicrosPerMillion++
			raw, err := json.Marshal(price)
			if err != nil {
				return err
			}
			return prices.Put([]byte(id), raw)
		})
	}
	leave, err := s.enter()
	if err != nil {
		t.Fatal(err)
	}
	defer leave()
	s.journalMu.RLock()
	receipt, _, err := s.updateAll(callbacks)
	s.journalMu.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	if got := priceRevisionForTest(t, s, "dep_stream"); got != before+1 {
		t.Fatal(got, before)
	}
	// The receipt itself binds both fact operations and their single fence.
	if len(receipt.Record.Ops) != 3 {
		t.Fatal("fence was not in the same merged frame", receipt.Record)
	}
}

func TestPricingRevisionFailureOverflowAndMalformedFenceRollback(t *testing.T) {
	for _, fault := range []string{"callback", "overflow", "malformed", "caller-reset"} {
		t.Run(fault, func(t *testing.T) {
			s, _ := streamPriceFixture(t, 1, 0)
			if fault == "overflow" || fault == "malformed" {
				// Model damaged/exhausted persisted state without granting ordinary
				// recorded callers permission to rewrite the transaction-owned fence.
				if err := s.migrate(func(tx *Tx) error {
					raw := encodeUint64(math.MaxUint64)
					if fault == "malformed" {
						raw = []byte("bad")
					}
					return tx.Bucket(bucketDeploymentPriceRevision).Put([]byte("dep_stream"), raw)
				}); err != nil {
					t.Fatal(err)
				}
			}
			journalBefore, err := s.MetadataJournalState()
			if err != nil {
				t.Fatal(err)
			}
			var before uint64
			if err := s.view(func(tx *Tx) error {
				before, _ = decodeUint64(tx.Bucket(bucketMeta).Get(keyAppliedJournalSequence))
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			err = s.update(func(tx *Tx) error {
				if fault == "caller-reset" {
					return tx.Bucket(bucketDeploymentPriceRevision).Put([]byte("dep_stream"), encodeUint64(0))
				}
				if err := tx.Bucket(bucketDeploymentPriceVersions).Delete([]byte("price_stream_0")); err != nil {
					return err
				}
				if fault == "callback" {
					return errors.New("injected failure")
				}
				return nil
			})
			if err == nil {
				t.Fatal("mutation accepted", fault)
			}
			journalAfter, journalErr := s.MetadataJournalState()
			if journalErr != nil || journalAfter.Sequence != journalBefore.Sequence {
				t.Fatal("failed mutation advanced native journal", journalBefore, journalAfter, journalErr)
			}
			if err := s.view(func(tx *Tx) error {
				after, _ := decodeUint64(tx.Bucket(bucketMeta).Get(keyAppliedJournalSequence))
				if after != before || tx.Bucket(bucketDeploymentPriceVersions).Get([]byte("price_stream_0")) == nil {
					t.Fatal("failed mutation advanced or deleted facts", before, after)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMigration42EmptyFencePreservesPricingAndRetriesAtomically(t *testing.T) {
	for _, point := range []string{"before_deployment_price_revision", "after_deployment_price_revision", "after_schema_version", "after_migration_history"} {
		t.Run(point, func(t *testing.T) {
			s, base := streamPriceFixture(t, 130, 0)
			before, err := s.PricingBackupState()
			if err != nil {
				t.Fatal(err)
			}
			path := s.path
			if err := s.migrate(func(tx *Tx) error {
				if err := tx.DeleteBucket(bucketDeploymentPriceRevision); err != nil {
					return err
				}
				if err := tx.Bucket(bucketMigrationHistory).Delete(encodeUint64(42)); err != nil {
					return err
				}
				return tx.Bucket(bucketMeta).Put(keySchemaVersion, encodeUint64(41))
			}); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if old, err := OpenPrimary(path); err == nil {
				old.Close()
				t.Fatal("old schema accepted without migration")
			}
			failed, err := openWithMigrationStepHook(path, func(version uint64, step string) error {
				if version == 42 && step == point {
					return errors.New("injected migration failure")
				}
				return nil
			})
			if err == nil {
				failed.Close()
				t.Fatal("injected migration committed")
			}
			raw, err := OpenReplica(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := raw.view(func(tx *Tx) error {
				version, _ := decodeUint64(tx.Bucket(bucketMeta).Get(keySchemaVersion))
				if version != 41 || tx.Bucket(bucketDeploymentPriceRevision) != nil {
					t.Fatal("partial migration published", version)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			raw.Close()
			migrated, err := OpenForWholeFilePublish(path)
			if err != nil {
				t.Fatal(err)
			}
			defer migrated.Close()
			after, err := migrated.PricingBackupState()
			if err != nil || before != after {
				t.Fatal("logical pricing hash changed", before, after, err)
			}
			if revision := priceRevisionForTest(t, migrated, "dep_stream"); revision != 0 {
				t.Fatal(revision)
			}
			if price, err := migrated.SelectDeploymentPriceVersion(context.Background(), "dep_stream", base.Add(129*time.Second)); err != nil || price.ID != "price_stream_129" {
				t.Fatal(price.ID, err)
			}
		})
	}
}

func TestPricingRevisionReplicaConfirmedApplyInvalidatesWarmAudit(t *testing.T) {
	primary, base := streamPriceFixture(t, 2, 0)
	path := primary.path
	primaryDir, replicaDir := filepath.Dir(path), t.TempDir()
	if err := primary.Close(); err != nil {
		t.Fatal(err)
	}
	replicaPath := filepath.Join(replicaDir, "metadata.db")
	copyTestFile(t, path, replicaPath)
	copyTestFile(t, filepath.Join(primaryDir, MetadataJournalFileName), filepath.Join(replicaDir, MetadataJournalFileName))
	primary, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer primary.Close()
	if _, err := primary.AttachMetadataJournal(testJournalKey(), "test"); err != nil {
		t.Fatal(err)
	}
	if err := primary.update(func(tx *Tx) error {
		prices := tx.Bucket(bucketDeploymentPriceVersions)
		var price domain.DeploymentPriceVersion
		if err := json.Unmarshal(prices.Get([]byte("price_stream_1")), &price); err != nil {
			return err
		}
		price.InputMicrosPerMillion++
		raw, err := json.Marshal(price)
		if err != nil {
			return err
		}
		return prices.Put([]byte(price.ID), raw)
	}); err != nil {
		t.Fatal(err)
	}
	var epoch, sequence uint64
	if err := primary.view(func(tx *Tx) error {
		epoch, _ = decodeUint64(tx.Bucket(bucketMeta).Get(keyMetadataJournalEpoch))
		sequence, _ = decodeUint64(tx.Bucket(bucketMeta).Get(keyAppliedJournalSequence))
		return nil
	}); err != nil {
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
	ctx := context.Background()
	old, err := replica.SelectDeploymentPriceVersion(ctx, "dep_stream", base.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	oldRevision := priceRevisionForTest(t, replica, "dep_stream")
	if _, err := replica.ApplyReplicaMetadataThrough(ctx, epoch, sequence-1); err != nil {
		t.Fatal(err)
	}
	if got := priceRevisionForTest(t, replica, "dep_stream"); got != oldRevision {
		t.Fatal("unconfirmed revision applied", got, oldRevision)
	}
	if _, err := replica.ApplyReplicaMetadataThrough(ctx, epoch, sequence); err != nil {
		t.Fatal(err)
	}
	updated, err := replica.SelectDeploymentPriceVersion(ctx, "dep_stream", base.Add(time.Second))
	if err != nil || updated.InputMicrosPerMillion != old.InputMicrosPerMillion+1 || priceRevisionForTest(t, replica, "dep_stream") != oldRevision+1 {
		t.Fatal(old, updated, err)
	}
}
