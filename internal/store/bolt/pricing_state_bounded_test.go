package bolt

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/ownedfile"
)

func TestPricingClockDiskPreservesAllKeysAndMonotonicPairs(t *testing.T) {
	x, err := newPricingClockIndex(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer x.close()
	base := time.Now().UTC()
	for i := 0; i < 1000; i++ {
		candidate := pricingClockObservation{SelectedAt: base.Add(time.Duration(i) * time.Second), ObservedAt: x.origin.Add(time.Duration(i) * time.Millisecond)}
		if err := x.mergeClockAnchor(fmt.Sprint("dep_", i), candidate); err != nil {
			t.Fatal(i, err)
		}
	}
	for i := 999; i >= 0; i-- {
		id := fmt.Sprint("dep_", i)
		want := base.Add(time.Duration(i) * time.Second)
		if got, ok, err := x.clockAnchor(id); err != nil || !ok || !got.SelectedAt.Equal(want) || got.ObservedAt.Sub(x.origin) != time.Duration(i)*time.Millisecond {
			t.Fatal(i, got, ok, err)
		}
		if err := x.mergeClockAnchor(id, pricingClockObservation{SelectedAt: want.Add(time.Second), ObservedAt: x.origin.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
		if err := x.mergeClockAnchor(id, pricingClockObservation{SelectedAt: want, ObservedAt: x.origin.Add(2 * time.Hour)}); err != nil {
			t.Fatal(err)
		}
		if got, ok, err := x.clockAnchor(id); err != nil || !ok || !got.SelectedAt.Equal(want.Add(time.Second)) || got.ObservedAt.Sub(x.origin) != time.Hour {
			t.Fatal(i, got, ok, err)
		}
	}
	if _, ok, err := x.clockAnchor("absent"); err != nil || ok {
		t.Fatal(ok, err)
	}
}

func TestPricingClockMerkleRejectsOldValidLeafAndBranchRollback(t *testing.T) {
	for _, fault := range []string{"leaf", "branch", "truncated"} {
		t.Run(fault, func(t *testing.T) {
			x, err := newPricingClockIndex(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer x.close()
			base := time.Now().UTC()
			put := func(id string, delta time.Duration) {
				t.Helper()
				if err := x.mergeClockAnchor(id, pricingClockObservation{SelectedAt: base.Add(delta), ObservedAt: x.origin.Add(delta)}); err != nil {
					t.Fatal(err)
				}
			}
			put("dep_a", 0)
			put("dep_b", 0)
			at := x.root
			if fault == "leaf" {
				var err error
				at, _, _, _, err = x.find(sha256.Sum256([]byte("dep_a")), nil)
				if err != nil {
					t.Fatal(err)
				}
			}
			var saved [pricingClockNodeBytes]byte
			if _, err := x.file.ReadAt(saved[:], int64(at)); err != nil {
				t.Fatal(err)
			}
			put("dep_a", time.Second)
			if fault == "truncated" {
				err = x.file.Truncate(pricingClockNodeBytes)
			} else {
				_, err = x.file.WriteAt(saved[:], int64(at))
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := x.clockAnchor("dep_a"); err == nil {
				t.Fatal("accepted disk rollback/deletion", fault)
			}
			if _, _, err := x.clockAnchor("never-seen"); err == nil {
				t.Fatal("poisoned owner supplied absence")
			}
		})
	}
}

func TestPricingClockAllowanceKeepsExistingObservationsAndOwnedReplacement(t *testing.T) {
	x, err := newPricingClockIndex(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC()
	obs := pricingClockObservation{SelectedAt: base, ObservedAt: x.origin}
	x.max = 2 * pricingClockNodeBytes
	if err := x.mergeClockAnchor("dep_a", obs); err != nil {
		t.Fatal(err)
	}
	size := x.size
	if err := x.mergeClockAnchor("dep_b", obs); !errors.Is(err, ErrPricingClockLimit) || x.size != size {
		t.Fatal(err, x.size, size)
	}
	obs.SelectedAt = base.Add(time.Second)
	if err := x.mergeClockAnchor("dep_a", obs); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(x.dir, "observations")
	if err := os.Rename(path, path+".held"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := x.close(); !errors.Is(err, ownedfile.ErrReplaced) {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(path); err != nil || string(raw) != "replacement" {
		t.Fatal(string(raw), err)
	}
}

func TestPricingFixedSlotsCollisionCannotReuseOtherDeploymentAudit(t *testing.T) {
	s, base := streamPriceFixture(t, 1, 0)
	other := ""
	for i := 0; i < 100000; i++ {
		id := fmt.Sprint("dep_collision_", i)
		if s.deploymentPricingState(id) == s.deploymentPricingState("dep_stream") {
			other = id
			break
		}
	}
	if other == "" {
		t.Fatal("fixture did not find a slot collision")
	}
	seedPricingDeployment(t, s, other, 0, 0, 0)
	if err := s.update(func(tx *Tx) error {
		for _, id := range []string{"other_price_a", "other_price_b"} {
			p := newStoredPrice(id, other, base)
			p.Version, p.Revision = 1, 1
			if id == "other_price_b" {
				p.EffectiveFrom = base.Add(time.Second)
			}
			if err := putDeploymentPriceVersionTx(tx, p); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SelectDeploymentPriceVersion(context.Background(), "dep_stream", base); err != nil {
		t.Fatal(err)
	}
	// Both timelines were written in one transaction, hence have revision 1.
	if _, err := s.SelectDeploymentPriceVersion(context.Background(), other, base); !errors.Is(err, domain.ErrPriceTimelineConflict) {
		t.Fatal("slot marker skipped the other deployment's audit", err)
	}
	for i := 0; i < 100000; i++ {
		s.deploymentPricingState(fmt.Sprint("unused_", i))
	}
	if len(s.pricingStates) != pricingStateSlots {
		t.Fatal("slot table grew")
	}
}

func TestPricingStoreCloseJoinsSharedGateAndDiscardsClockOwner(t *testing.T) {
	s, base := streamPriceFixture(t, 1, 0)
	if _, _, _, err := s.PrepareDeploymentPricePin(context.Background(), "dep_stream", "att_clock", base, time.Second, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	dir := s.pricingClock.dir
	release := s.LockDeploymentPricingShared("dep_stream")
	done := make(chan error, 1)
	go func() { done <- s.Close() }()
	select {
	case err := <-done:
		t.Fatal("closed while shared lease still live", err)
	case <-time.After(20 * time.Millisecond):
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("clock owner removed before real join", err)
	}
	release()
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("close did not finish")
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if s.pricingStates != nil {
		t.Fatal("closed Store retained its slot table")
	}
}

func TestPricingSlotReplacementKeepsDeploymentForwardJumpEvidence(t *testing.T) {
	s, base := streamPriceFixture(t, 1, 0)
	other := ""
	for i := 0; i < 100000; i++ {
		id := fmt.Sprint("dep_other_", i)
		if s.deploymentPricingState(id) == s.deploymentPricingState("dep_stream") {
			other = id
			break
		}
	}
	if other == "" {
		t.Fatal("no collision fixture")
	}
	seedPricingDeployment(t, s, other, 0, 0, 0)
	price := newStoredPrice("other_clock_price", other, base)
	price.Version, price.Revision = 1, 1
	if err := s.update(func(tx *Tx) error { return putDeploymentPriceVersionTx(tx, price) }); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, _, _, err := s.PrepareDeploymentPricePin(ctx, "dep_stream", "att_first", base, time.Second, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SelectDeploymentPriceVersion(ctx, other, base); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.PrepareDeploymentPricePin(ctx, other, "att_other", base.Add(time.Hour), time.Second, 30*time.Second); err != nil {
		t.Fatal("clock leaked across a slot collision", err)
	}
	if _, _, _, err := s.PrepareDeploymentPricePin(ctx, "dep_stream", "att_forward", base.Add(10*time.Minute), time.Second, 30*time.Second); !errors.Is(err, ErrPricingQuarantined) {
		t.Fatal("lost forward-jump evidence", err)
	}
	if yes, reason, err := s.DeploymentPricingQuarantine(ctx, "dep_stream"); err != nil || !yes || reason != "wall_clock_forward_jump" {
		t.Fatal(yes, reason, err)
	}
}

func TestPricingDamagedClockRefusesPinBeforeNativeCommit(t *testing.T) {
	s, base := streamPriceFixture(t, 1, 0)
	ctx := context.Background()
	if _, _, _, err := s.PrepareDeploymentPricePin(ctx, "dep_stream", "att_first", base, time.Second, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	before, err := s.MetadataJournalState()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.pricingClock.file.Truncate(pricingClockNodeBytes); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.PrepareDeploymentPricePin(ctx, "dep_stream", "att_damaged", base, time.Second, 30*time.Second); err == nil {
		t.Fatal("damaged clock supplied a first observation")
	}
	after, err := s.MetadataJournalState()
	if err != nil || before.Sequence != after.Sequence {
		t.Fatal(before, after, err)
	}
	if err := s.view(func(tx *Tx) error {
		if tx.Bucket(bucketDeploymentPricePins).Get([]byte("att_damaged")) != nil {
			t.Fatal("failed clock read created a pin")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPricingClockClosePreservesReplacedEmptyDirectory(t *testing.T) {
	x, err := newPricingClockIndex(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(x.dir, x.dir+".held"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(x.dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := x.close(); !errors.Is(err, ownedfile.ErrReplaced) {
		t.Fatal(err)
	}
	if info, err := os.Stat(x.dir); err != nil || !info.IsDir() {
		t.Fatal("replacement directory was removed", info, err)
	}
}

func TestPricingClockPostCommitFailureReturnsNoUsablePinAndRecoversOrphan(t *testing.T) {
	s, base := streamPriceFixture(t, 1, 0)
	s.batches.mu.Lock()
	s.batches.delay = time.Hour
	s.batches.mu.Unlock()
	prepareDone, siblingDone := make(chan error, 1), make(chan error, 1)
	go func() {
		price, snapshot, pin, err := s.PrepareDeploymentPricePin(context.Background(), "dep_stream", "att_io_failure", base, time.Second, 30*time.Second)
		if err == nil || price.ID != "" || pin.AttemptID != "" || snapshot.PriceVersionID != "" {
			prepareDone <- errors.New("post-commit clock failure returned a usable price")
			return
		}
		prepareDone <- nil
	}()
	waitPending := func(want int) {
		t.Helper()
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			s.batches.mu.Lock()
			n := len(s.batches.pending)
			s.batches.mu.Unlock()
			if n == want {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatal("batch was not queued", want)
	}
	waitPending(1)
	go func() {
		siblingDone <- s.batch(func(tx *Tx) error {
			if tx.Bucket(bucketDeploymentPricePins).Get([]byte("att_io_failure")) == nil {
				return errors.New("fixture closed clock before pin callback")
			}
			return s.pricingClock.file.Close()
		})
	}()
	waitPending(2)
	s.flushBatches()
	for _, done := range []chan error{prepareDone, siblingDone} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("batch did not exit")
		}
	}
	if err := s.view(func(tx *Tx) error {
		if tx.Bucket(bucketDeploymentPricePins).Get([]byte("att_io_failure")) == nil {
			t.Fatal("committed prepared fact was silently discarded")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// The existing authoritative-reference recovery removes prepared facts that
	// never acquired a Ledger lease. This empty fixture makes no production claim.
	if err := s.RecoverDeploymentPricePins(context.Background(), ledger.NewState()); err != nil {
		t.Fatal(err)
	}
	if err := s.view(func(tx *Tx) error {
		if tx.Bucket(bucketDeploymentPricePins).Get([]byte("att_io_failure")) != nil {
			t.Fatal("orphan prepared fact survived recovery")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Reopen the same original inode only for test cleanup; the live owner stays
	// poisoned, so this cannot restore pricing admission.
	file, err := os.OpenFile(filepath.Join(s.pricingClock.dir, "observations"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	s.pricingClock.file = file
	if _, _, err := s.pricingClock.clockAnchor("dep_stream"); err == nil {
		t.Fatal("poisoned clock was reused")
	}
}
