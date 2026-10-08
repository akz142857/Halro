package bolt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/resourcebudget"
	bbolt "go.etcd.io/bbolt"
)

func proposalFixture(id, dep string, at time.Time) domain.DeploymentPriceProposal {
	p := domain.DeploymentPriceProposal{ID: id, DeploymentID: dep, ProviderID: "provider", ProviderModel: "gpt-test", BillingMode: domain.BillingModeMetered, Currency: "USD", FormulaVersion: domain.PriceFormulaUSDTokensV1, InputMicrosPerMillion: 1, Match: domain.PriceProposalMatchExact, Status: domain.PriceProposalPending, CreatedBy: "system", CreatedAt: at, FetchedAt: at, ExpiresAt: at.Add(24 * time.Hour), Revision: 1,
		Source: domain.PriceSource{Type: domain.PriceSourceOfficialURL, Assurance: domain.PriceAssuranceAsserted, ReceivedAt: at, RetrievedAt: &at, URI: "https://example.test/prices", Reference: "public", ContentSHA256: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}
	var err error
	p.Digest, err = p.ComputeDigest()
	if err != nil {
		panic(err)
	}
	return p
}
func putProposalFixture(tx *Tx, p domain.DeploymentPriceProposal) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return tx.Bucket(bucketDeploymentPriceProposals).Put([]byte(p.ID), raw)
}
func seedProposalPageFixture(t testing.TB, s *Store, dep string, n int) {
	t.Helper()
	for start := 0; start < n; start += 64 {
		if err := s.update(func(tx *Tx) error {
			for i := start; i < n && i < start+64; i++ {
				at := time.Date(1969, 12, 31, 23, 59, 0, 0, time.UTC).Add(time.Duration(i/3) * time.Second)
				if err := putProposalFixture(tx, proposalFixture(fmt.Sprintf("prop_%05d", i), dep, at)); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}
func proposalRevisionForTest(t testing.TB, s *Store, dep string) uint64 {
	t.Helper()
	var v uint64
	if err := s.view(func(tx *Tx) error { var err error; v, err = proposalRevisionTx(tx, dep); return err }); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestProposalPagesPreserveCreatedOrderAndTiesAcrossAllPages(t *testing.T) {
	s := openJournalledStore(t, filepath.Join(t.TempDir(), "metadata.db"))
	seedPricingDeployment(t, s, "dep_proposals", 0, 0, 0)
	seedPricingDeployment(t, s, "dep_other", 0, 0, 0)
	seedProposalPageFixture(t, s, "dep_proposals", 137)
	if err := s.update(func(tx *Tx) error {
		return putProposalFixture(tx, proposalFixture("other", "dep_other", time.Now().UTC()))
	}); err != nil {
		t.Fatal(err)
	}
	expected, err := s.ListDeploymentPriceProposals(context.Background(), "dep_proposals")
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(expected, func(i, j int) bool {
		a, b := proposalOrder(expected[i]), proposalOrder(expected[j])
		return bytes.Compare(a[:], b[:]) > 0
	})
	seen := 0
	cursor := ""
	for {
		page, err := s.DeploymentPriceProposalsPage(context.Background(), "dep_proposals", cursor, 7)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) == 0 || len(page.Items) > 7 {
			t.Fatal(page)
		}
		for _, p := range page.Items {
			if seen >= len(expected) || p.ID != expected[seen].ID {
				t.Fatal(p.ID, seen)
			}
			seen++
		}
		if page.NextCursor == "" {
			break
		}
		if page.NextCursor == cursor || len(page.NextCursor) > 160 {
			t.Fatal(page.NextCursor)
		}
		cursor = page.NextCursor
	}
	if seen != 137 {
		t.Fatal(seen)
	}
}

func TestProposalPageCursorScopesRevisionAndProcessAndRejectsPriceCursor(t *testing.T) {
	s := openJournalledStore(t, filepath.Join(t.TempDir(), "metadata.db"))
	seedPricingDeployment(t, s, "dep_proposals", 0, 0, 0)
	seedPricingDeployment(t, s, "dep_other", 0, 0, 0)
	seedProposalPageFixture(t, s, "dep_proposals", 3)
	first, err := s.DeploymentPriceProposalsPage(context.Background(), "dep_proposals", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, cursor := range []string{"bad", first.NextCursor[:30] + "!" + first.NextCursor[31:], s.encodePricingPageCursor("dep_proposals", 0, make([]byte, 12)), first.NextCursor + "x"} {
		if p, err := s.DeploymentPriceProposalsPage(context.Background(), "dep_proposals", cursor, 1); !errors.Is(err, ErrPricingPageCursor) || len(p.Items) != 0 {
			t.Fatal(p, err)
		}
	}
	if _, err := s.DeploymentPriceProposalsPage(context.Background(), "dep_other", first.NextCursor, 1); !errors.Is(err, ErrPricingPageCursor) {
		t.Fatal(err)
	}
	// Unrelated deployment writes do not invalidate the current deployment.
	if err := s.update(func(tx *Tx) error {
		return putProposalFixture(tx, proposalFixture("other", "dep_other", time.Now().UTC()))
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeploymentPriceProposalsPage(context.Background(), "dep_proposals", first.NextCursor, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.update(func(tx *Tx) error { return tx.Bucket(bucketDeploymentPriceProposals).Delete([]byte("prop_00000")) }); err != nil {
		t.Fatal(err)
	}
	if p, err := s.DeploymentPriceProposalsPage(context.Background(), "dep_proposals", first.NextCursor, 1); !errors.Is(err, ErrPricingPageChanged) || len(p.Items) != 0 {
		t.Fatal(p, err)
	}
	newest, err := s.DeploymentPriceProposalsPage(context.Background(), "dep_proposals", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	path := s.path
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openJournalledStore(t, path)
	if _, err := s.DeploymentPriceProposalsPage(context.Background(), "dep_proposals", newest.NextCursor, 1); !errors.Is(err, ErrPricingPageCursor) {
		t.Fatal(err)
	}
}

func TestProposalPagesByteLimitWideRowsAndMalformedOffPageRefuse(t *testing.T) {
	s := openJournalledStore(t, filepath.Join(t.TempDir(), "metadata.db"))
	seedPricingDeployment(t, s, "dep_proposals", 0, 0, 0)
	seedProposalPageFixture(t, s, "dep_proposals", 3)
	for i := 0; i < 3; i++ {
		if err := s.update(func(tx *Tx) error {
			b := tx.Bucket(bucketDeploymentPriceProposals)
			key := []byte(fmt.Sprintf("prop_%05d", i))
			raw := b.Get(key)
			raw = append(bytes.Clone(raw[:len(raw)-1]), []byte(`,"unknown":"`)...)
			raw = append(raw, bytes.Repeat([]byte("x"), 2<<20)...)
			raw = append(raw, []byte(`"}`)...)
			return b.Put(key, raw)
		}); err != nil {
			t.Fatal(err)
		}
	}
	seen := 0
	cursor := ""
	for {
		page, err := s.DeploymentPriceProposalsPage(context.Background(), "dep_proposals", cursor, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 1 {
			t.Fatal(page)
		}
		seen++
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if seen != 3 {
		t.Fatal(seen)
	}
	if err := s.update(func(tx *Tx) error {
		p := proposalFixture("prop_00000", "dep_proposals", time.Date(1969, 1, 1, 0, 0, 0, 0, time.UTC))
		p.Digest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		return putProposalFixture(tx, p)
	}); err != nil {
		t.Fatal(err)
	}
	if p, err := s.DeploymentPriceProposalsPage(context.Background(), "dep_proposals", "", 1); err == nil || len(p.Items) != 0 {
		t.Fatal("late malformed proposal hidden by page", p, err)
	}
}

func TestProposalRevisionMergedCallbacksRollbackAndCounterOwner(t *testing.T) {
	s := openJournalledStore(t, filepath.Join(t.TempDir(), "metadata.db"))
	seedProposalPageFixture(t, s, "dep_proposals", 2)
	before := proposalRevisionForTest(t, s, "dep_proposals")
	callbacks := []func(*Tx) error{func(tx *Tx) error { return tx.Bucket(bucketDeploymentPriceProposals).Delete([]byte("prop_00000")) }, func(tx *Tx) error { return tx.Bucket(bucketDeploymentPriceProposals).Delete([]byte("prop_00001")) }}
	s.journalMu.RLock()
	receipt, _, err := s.updateAll(callbacks)
	s.journalMu.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	if proposalRevisionForTest(t, s, "dep_proposals") != before+1 || len(receipt.Record.Ops) != 7 {
		t.Fatal(receipt.Record)
	}
	native, _ := s.MetadataJournalState()
	for _, f := range []func(*Tx) error{func(tx *Tx) error {
		return tx.Bucket(bucketPricingProposalRevision).Put([]byte("dep_proposals"), encodeUint64(0))
	}, func(tx *Tx) error { return tx.DeleteBucket(bucketDeploymentPriceProposals) }, func(tx *Tx) error {
		if err := putProposalFixture(tx, proposalFixture("rollback", "dep_proposals", time.Now().UTC())); err != nil {
			return err
		}
		return errors.New("rollback")
	}} {
		if err := s.update(f); err == nil {
			t.Fatal("unowned/failed write accepted")
		}
	}
	after, _ := s.MetadataJournalState()
	if native.Sequence != after.Sequence {
		t.Fatal(native, after)
	}
	for _, raw := range [][]byte{encodeUint64(math.MaxUint64), []byte("bad")} {
		if err := s.migrate(func(tx *Tx) error { return tx.Bucket(bucketPricingProposalRevision).Put([]byte("dep_proposals"), raw) }); err != nil {
			t.Fatal(err)
		}
		if err := s.update(func(tx *Tx) error {
			return putProposalFixture(tx, proposalFixture("refuse", "dep_proposals", time.Now().UTC()))
		}); err == nil {
			t.Fatal("invalid revision appended")
		}
	}
	after, _ = s.MetadataJournalState()
	if native.Sequence != after.Sequence {
		t.Fatal(native, after)
	}
	// Control has its own authoritative class, not a derived or node-local counter.
	if class, err := classify([]string{string(bucketPricingProposalRevision)}, []byte("dep_proposals")); err != nil || class != classAuthoritative {
		t.Fatal(class, err)
	}
}

func TestProposalPageCancellationAndCloseRetainActualIO(t *testing.T) {
	s, m, _ := pricingBudgetFixture(t)
	if _, err := s.DeploymentPriceProposalsPage(context.Background(), "dep_budget", "", 1); !errors.Is(err, resourcebudget.ErrPressure) {
		t.Fatal(err)
	}
	if err := m.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	entered, release, initDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() { s.pricingPageKeyOnce.Do(func() { close(entered); <-release }); close(initDone) }()
	<-entered
	done := make(chan error, 1)
	go func() {
		_, err := s.DeploymentPriceProposalsPage(context.Background(), "dep_budget", "", 1)
		done <- err
	}()
	deadline := time.Now().Add(time.Second)
	for m.Snapshot().NormalClaims != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if m.Snapshot().NormalClaims != 1 {
		close(release)
		t.Fatal(m.Snapshot())
	}
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case err := <-closed:
		close(release)
		t.Fatal("close returned before IO join", err)
	case <-time.After(20 * time.Millisecond):
	}
	if m.Snapshot().NormalClaims != 1 {
		close(release)
		t.Fatal("close refunded actual IO", m.Snapshot())
	}
	close(release)
	<-initDone
	if err := <-done; !errors.Is(err, context.Canceled) && !errors.Is(err, bbolt.ErrDatabaseNotOpen) {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if m.Snapshot().Used != (resourcebudget.Cost{}) {
		t.Fatal(m.Snapshot())
	}
}

func TestMigration44ProposalFenceRollbackAndBaselinePreserveSource(t *testing.T) {
	for _, point := range []string{"before_proposal_revision", "after_proposal_revision", "after_schema_version", "after_migration_history"} {
		t.Run(point, func(t *testing.T) {
			s := openJournalledStore(t, filepath.Join(t.TempDir(), "metadata.db"))
			seedPricingDeployment(t, s, "dep_proposals", 0, 0, 0)
			seedProposalPageFixture(t, s, "dep_proposals", 130)
			var before []byte
			if err := s.view(func(tx *Tx) error {
				before = bytes.Clone(tx.Bucket(bucketDeploymentPriceProposals).Get([]byte("prop_00129")))
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := s.migrate(func(tx *Tx) error {
				if err := tx.DeleteBucket(bucketPricingProposalRevision); err != nil {
					return err
				}
				if err := tx.Bucket(bucketMigrationHistory).Delete(encodeUint64(44)); err != nil {
					return err
				}
				return tx.Bucket(bucketMeta).Put(keySchemaVersion, encodeUint64(43))
			}); err != nil {
				t.Fatal(err)
			}
			path := s.path
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			bad, err := openWithMigrationStepHook(path, func(v uint64, step string) error {
				if v == 44 && step == point {
					return errors.New("injected")
				}
				return nil
			})
			if err == nil {
				bad.Close()
				t.Fatal("failed migration published")
			}
			raw, err := OpenReplica(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := raw.view(func(tx *Tx) error {
				v, _ := decodeUint64(tx.Bucket(bucketMeta).Get(keySchemaVersion))
				if v != 43 || tx.Bucket(bucketPricingProposalRevision) != nil || tx.Bucket(bucketMigrationHistory).Get(encodeUint64(44)) != nil || !bytes.Equal(before, tx.Bucket(bucketDeploymentPriceProposals).Get([]byte("prop_00129"))) {
					t.Fatal("partial migration/source mutation", v)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			raw.Close()
			s = openJournalledStore(t, path)
			if proposalRevisionForTest(t, s, "dep_proposals") != 0 {
				t.Fatal("migration invented revision")
			}
			page, err := s.DeploymentPriceProposalsPage(context.Background(), "dep_proposals", "", 7)
			if err != nil || len(page.Items) != 7 {
				t.Fatal(page, err)
			}
		})
	}
}

func TestProposalRevisionReplicaConfirmedApplyInvalidatesCursor(t *testing.T) {
	primary := openJournalledStore(t, filepath.Join(t.TempDir(), "metadata.db"))
	seedPricingDeployment(t, primary, "dep_proposals", 0, 0, 0)
	seedProposalPageFixture(t, primary, "dep_proposals", 3)
	path := primary.path
	dir := filepath.Dir(path)
	if err := primary.Close(); err != nil {
		t.Fatal(err)
	}
	replicaDir := t.TempDir()
	replicaPath := filepath.Join(replicaDir, "metadata.db")
	copyTestFile(t, path, replicaPath)
	copyTestFile(t, filepath.Join(dir, MetadataJournalFileName), filepath.Join(replicaDir, MetadataJournalFileName))
	primary = openJournalledStore(t, path)
	if err := primary.update(func(tx *Tx) error {
		p := proposalFixture("prop_00000", "dep_proposals", time.Date(1969, 12, 31, 23, 59, 0, 0, time.UTC))
		now := time.Now().UTC()
		p.Status, p.ReviewedBy, p.ReviewedAt, p.Revision = domain.PriceProposalRejected, "system", &now, 2
		return putProposalFixture(tx, p)
	}); err != nil {
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
	first, err := replica.DeploymentPriceProposalsPage(context.Background(), "dep_proposals", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := replica.ApplyReplicaMetadataThrough(context.Background(), state.Epoch, state.Sequence-1); err != nil {
		t.Fatal(err)
	}
	if _, err := replica.DeploymentPriceProposalsPage(context.Background(), "dep_proposals", first.NextCursor, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := replica.ApplyReplicaMetadataThrough(context.Background(), state.Epoch, state.Sequence); err != nil {
		t.Fatal(err)
	}
	if _, err := replica.DeploymentPriceProposalsPage(context.Background(), "dep_proposals", first.NextCursor, 1); !errors.Is(err, ErrPricingPageChanged) {
		t.Fatal(err)
	}
	after, err := replica.DeploymentPriceProposalsPage(context.Background(), "dep_proposals", "", 10)
	if err != nil || after.Revision != first.Revision+1 {
		t.Fatal(after, err)
	}
	p, err := replica.GetDeploymentPriceProposal(context.Background(), "dep_proposals", "prop_00000")
	if err != nil || p.Status != domain.PriceProposalRejected {
		t.Fatal(p, err)
	}
}

func TestProposalScanFreezesPhysicalUpperIncludingEmptyHistory(t *testing.T) {
	s := openJournalledStore(t, filepath.Join(t.TempDir(), "metadata.db"))
	seedProposalPageFixture(t, s, "dep_proposals", 3)
	var upper []byte
	if err := s.view(func(tx *Tx) error {
		key, _ := tx.Bucket(bucketDeploymentPriceProposals).Cursor().Last()
		upper = bytes.Clone(key)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// A future unrelated suffix must neither extend this query nor be decoded.
	if err := s.migrate(func(tx *Tx) error {
		return tx.Bucket(bucketDeploymentPriceProposals).Put([]byte("zzz_future"), []byte("malformed"))
	}); err != nil {
		t.Fatal(err)
	}
	bounded, err := s.readPricingRecoveryPageChecked(context.Background(), bucketDeploymentPriceProposals, nil, upper, nil)
	if err != nil || !bounded.done || len(bounded.rows) != 3 {
		t.Fatal(bounded, err)
	}
	empty, err := s.readPricingRecoveryPageChecked(context.Background(), bucketDeploymentPriceProposals, nil, []byte{}, nil)
	if err != nil || !empty.done || len(empty.rows) != 0 {
		t.Fatal(empty, err)
	}
	// Existing trusted recovery callers pass nil: they still scan the full source.
	whole, err := s.readPricingRecoveryPage(context.Background(), bucketDeploymentPriceProposals, nil, 0)
	if err != nil || len(whole.rows) != 4 {
		t.Fatal(whole, err)
	}
}
