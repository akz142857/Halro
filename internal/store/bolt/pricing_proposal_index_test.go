package bolt

import (
	"bytes"
	"context"
	"errors"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	"time"
)

func proposalCountForTest(t testing.TB, s *Store, dep string) uint64 {
	t.Helper()
	var count uint64
	if err := s.view(func(tx *Tx) error { var err error; count, err = proposalCountTx(tx, dep); return err }); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestProposalOrderTracksMovesReordersDeletionAndRollback(t *testing.T) {
	s := openJournalledStore(t, filepath.Join(t.TempDir(), "metadata.db"))
	for _, dep := range []string{"a", "b"} {
		seedPricingDeployment(t, s, dep, 0, 0, 0)
	}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := proposalFixture("proposal", "a", at)
	if err := s.update(func(tx *Tx) error { return putProposalFixture(tx, p) }); err != nil {
		t.Fatal(err)
	}
	old := proposalOrder(p)
	first, err := s.DeploymentPriceProposalsPage(context.Background(), "a", "", 1)
	if err != nil || len(first.Items) != 1 {
		t.Fatal(first, err)
	}
	p = proposalFixture("proposal", "b", at.Add(time.Second))
	if err := s.update(func(tx *Tx) error { return putProposalFixture(tx, p) }); err != nil {
		t.Fatal(err)
	}
	if proposalCountForTest(t, s, "a") != 0 || proposalCountForTest(t, s, "b") != 1 || proposalRevisionForTest(t, s, "a") != 2 || proposalRevisionForTest(t, s, "b") != 1 {
		t.Fatal("move lost owner/count/fence")
	}
	if err := s.view(func(tx *Tx) error {
		if tx.Bucket(bucketPricingProposalOrder).Bucket([]byte("a")).Get(old[:]) != nil {
			t.Fatal("old order survived")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if page, err := s.DeploymentPriceProposalsPage(context.Background(), "a", "", 1); err != nil || len(page.Items) != 0 {
		t.Fatal(page, err)
	}
	p = proposalFixture("proposal", "b", at.Add(2*time.Second))
	if err := s.update(func(tx *Tx) error { return putProposalFixture(tx, p) }); err != nil {
		t.Fatal(err)
	}
	page, err := s.DeploymentPriceProposalsPage(context.Background(), "b", "", 1)
	if err != nil || len(page.Items) != 1 || !page.Items[0].CreatedAt.Equal(p.CreatedAt) || proposalCountForTest(t, s, "b") != 1 {
		t.Fatal(page, err)
	}
	native, _ := s.MetadataJournalState()
	if err := s.update(func(tx *Tx) error {
		if err := tx.Bucket(bucketDeploymentPriceProposals).Delete([]byte(p.ID)); err != nil {
			return err
		}
		return errors.New("rollback")
	}); err == nil {
		t.Fatal("failed transaction accepted")
	}
	next, _ := s.MetadataJournalState()
	if next.Sequence != native.Sequence || proposalCountForTest(t, s, "b") != 1 {
		t.Fatal(native, next)
	}
	if err := s.update(func(tx *Tx) error { return tx.Bucket(bucketDeploymentPriceProposals).Delete([]byte(p.ID)) }); err != nil {
		t.Fatal(err)
	}
	if proposalCountForTest(t, s, "b") != 0 {
		t.Fatal("delete did not retire count")
	}
}

func TestProposalColdAuditRefusesMissingSpuriousAndWrongCount(t *testing.T) {
	for _, kind := range []string{"missing", "extra", "wrong_id", "wrong_count", "zero_count", "missing_fact", "missing_root"} {
		t.Run(kind, func(t *testing.T) {
			s := openJournalledStore(t, filepath.Join(t.TempDir(), "metadata.db"))
			seedPricingDeployment(t, s, "dep_proposals", 0, 0, 0)
			seedProposalPageFixture(t, s, "dep_proposals", 130)
			p := proposalFixture("prop_00000", "dep_proposals", time.Date(1969, 12, 31, 23, 59, 0, 0, time.UTC))
			pos := proposalOrder(p)
			if err := s.migrate(func(tx *Tx) error {
				root := tx.Bucket(bucketPricingProposalOrder)
				order := root.Bucket([]byte("dep_proposals"))
				switch kind {
				case "missing":
					return order.Delete(pos[:])
				case "extra":
					extra := proposalOrder(proposalFixture("extra", "dep_proposals", time.Now().UTC()))
					return order.Put(extra[:], []byte(p.ID))
				case "wrong_id":
					return order.Put(pos[:], []byte("prop_00001"))
				case "wrong_count":
					return tx.Bucket(bucketPricingProposalCount).Put([]byte("dep_proposals"), encodeUint64(129))
				case "zero_count":
					return tx.Bucket(bucketPricingProposalCount).Delete([]byte("dep_proposals"))
				case "missing_fact":
					return tx.Bucket(bucketDeploymentPriceProposals).Delete([]byte(p.ID))
				case "missing_root":
					return tx.DeleteBucket(bucketPricingProposalOrder)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			page, err := s.DeploymentPriceProposalsPage(context.Background(), "dep_proposals", "", 1)
			if err == nil || len(page.Items) != 0 || s.deploymentPricingState("dep_proposals").proposalLoaded {
				t.Fatal("cold audit accepted incomplete set", page, err)
			}
		})
	}
}

func TestProposalOrderAndCountOnlyOwnedByRecorder(t *testing.T) {
	s := openJournalledStore(t, filepath.Join(t.TempDir(), "metadata.db"))
	seedProposalPageFixture(t, s, "dep_proposals", 1)
	native, _ := s.MetadataJournalState()
	for _, fn := range []func(*Tx) error{
		func(tx *Tx) error { return tx.DeleteBucket(bucketPricingProposalOrder) },
		func(tx *Tx) error { return tx.Bucket(bucketPricingProposalOrder).DeleteBucket([]byte("dep_proposals")) },
		func(tx *Tx) error {
			return tx.Bucket(bucketPricingProposalCount).Put([]byte("dep_proposals"), encodeUint64(0))
		},
		func(tx *Tx) error {
			return tx.Bucket(bucketPricingProposalOrder).Bucket([]byte("dep_proposals")).Put(make([]byte, 44), []byte("fake"))
		},
	} {
		if err := s.update(fn); err == nil {
			t.Fatal("caller changed an owned index")
		}
	}
	for _, raw := range [][]byte{[]byte("bad"), encodeUint64(math.MaxUint64), encodeUint64(0)} {
		if err := s.migrate(func(tx *Tx) error { return tx.Bucket(bucketPricingProposalCount).Put([]byte("dep_proposals"), raw) }); err != nil {
			t.Fatal(err)
		}
		fn := func(tx *Tx) error {
			return putProposalFixture(tx, proposalFixture("another", "dep_proposals", time.Now().UTC()))
		}
		if bytes.Equal(raw, encodeUint64(0)) {
			fn = func(tx *Tx) error { return tx.Bucket(bucketDeploymentPriceProposals).Delete([]byte("prop_00000")) }
		}
		if err := s.update(fn); err == nil {
			t.Fatal("invalid count admitted mutation")
		}
	}
	after, _ := s.MetadataJournalState()
	if after.Sequence != native.Sequence {
		t.Fatal("failed mutation appended", native, after)
	}
	for _, b := range [][]byte{bucketPricingProposalOrder, bucketPricingProposalCount} {
		if class, err := classify([]string{string(b)}, []byte("dep_proposals")); err != nil || class != classAuthoritative {
			t.Fatal(class, err)
		}
	}
}

func TestMigration45ProposalOrderReservationIsAtomic(t *testing.T) {
	for _, point := range []string{"before_proposal_order", "after_proposal_order", "after_schema_version", "after_migration_history"} {
		t.Run(point, func(t *testing.T) {
			s := openJournalledStore(t, filepath.Join(t.TempDir(), "metadata.db"))
			seedPricingDeployment(t, s, "dep_proposals", 0, 0, 0)
			seedProposalPageFixture(t, s, "dep_proposals", 130)
			revision := proposalRevisionForTest(t, s, "dep_proposals")
			var before []byte
			if err := s.migrate(func(tx *Tx) error {
				before = bytes.Clone(tx.Bucket(bucketDeploymentPriceProposals).Get([]byte("prop_00129")))
				for _, name := range [][]byte{bucketPricingProposalOrder, bucketPricingProposalCount} {
					if err := tx.DeleteBucket(name); err != nil {
						return err
					}
				}
				if err := tx.Bucket(bucketMigrationHistory).Delete(encodeUint64(45)); err != nil {
					return err
				}
				return tx.Bucket(bucketMeta).Put(keySchemaVersion, encodeUint64(44))
			}); err != nil {
				t.Fatal(err)
			}
			path := s.path
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			failed, err := openWithMigrationStepHook(path, func(version uint64, step string) error {
				if version != 45 || step != point {
					return nil
				}
				return errors.New("injected")
			})
			if err == nil {
				failed.Close()
				t.Fatal("fault published partial index")
			}
			raw, err := OpenReplica(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := raw.view(func(tx *Tx) error {
				version, _ := decodeUint64(tx.Bucket(bucketMeta).Get(keySchemaVersion))
				if version != 44 || tx.Bucket(bucketPricingProposalOrder) != nil || tx.Bucket(bucketPricingProposalCount) != nil || tx.Bucket(bucketMigrationHistory).Get(encodeUint64(45)) != nil || !bytes.Equal(before, tx.Bucket(bucketDeploymentPriceProposals).Get([]byte("prop_00129"))) {
					t.Fatal("partial migration persisted", version)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}
			s = openJournalledStore(t, path)
			if proposalCountForTest(t, s, "dep_proposals") != 130 || proposalRevisionForTest(t, s, "dep_proposals") != revision {
				t.Fatal("migration changed facts/fence")
			}
			page, err := s.DeploymentPriceProposalsPage(context.Background(), "dep_proposals", "", 7)
			if err != nil || len(page.Items) != 7 || page.Revision != revision {
				t.Fatal(page, err)
			}
		})
	}
}

func TestProposalCreationCapacityUsesAtomicDeploymentCountAndReplay(t *testing.T) {
	s := openJournalledStore(t, filepath.Join(t.TempDir(), "metadata.db"))
	seedPricingDeployment(t, s, "dep_proposals", 0, 0, 0)
	seedProposalPageFixture(t, s, "dep_proposals", maxPriceProposalsPerDeployment-1)
	at := time.Now().UTC().Truncate(time.Second)
	p := proposalFixture("at_limit", "dep_proposals", at)
	p.ProviderID = "provider_dep_proposals"
	var err error
	p.Digest, err = p.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	intent := domain.PricingAuditIntent{EventID: "limit_event", OccurredAt: at, ActorID: "system", Action: "deployment_price.proposal_create", TargetType: "deployment_price_proposal", TargetID: p.ID, RequestSHA256: "sha256:" + strings.Repeat("b", 64)}
	intent.RecordSource(p.Source)
	key := "sha256:" + strings.Repeat("c", 64)
	if _, _, replayed, err := s.CreateDeploymentPriceProposal(context.Background(), p, intent, key); err != nil || replayed {
		t.Fatal(replayed, err)
	}
	if proposalCountForTest(t, s, p.DeploymentID) != maxPriceProposalsPerDeployment {
		t.Fatal("creation lost count")
	}
	native, _ := s.MetadataJournalState()
	if _, _, replayed, err := s.CreateDeploymentPriceProposal(context.Background(), p, intent, key); err != nil || !replayed {
		t.Fatal("replay blocked by capacity", replayed, err)
	}
	another := p
	another.ID = "over_limit"
	another.Digest, err = another.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	newIntent := intent
	newIntent.EventID, newIntent.TargetID = "over_limit_event", another.ID
	if _, _, _, err := s.CreateDeploymentPriceProposal(context.Background(), another, newIntent, "sha256:"+strings.Repeat("d", 64)); err == nil {
		t.Fatal("capacity exceeded")
	}
	after, _ := s.MetadataJournalState()
	if after.Sequence != native.Sequence {
		t.Fatal("replay/refusal appended", native, after)
	}
	if err := s.update(func(tx *Tx) error { return tx.Bucket(bucketDeploymentPriceProposals).Delete([]byte("prop_00000")) }); err != nil {
		t.Fatal(err)
	}
	if _, _, replayed, err := s.CreateDeploymentPriceProposal(context.Background(), another, newIntent, "sha256:"+strings.Repeat("d", 64)); err != nil || replayed {
		t.Fatal("deleted capacity not reusable", replayed, err)
	}
	if proposalCountForTest(t, s, p.DeploymentID) != maxPriceProposalsPerDeployment {
		t.Fatal("replacement lost count")
	}
}
