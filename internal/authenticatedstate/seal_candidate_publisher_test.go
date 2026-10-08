package authenticatedstate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

func sealPublisherReservation(t *testing.T, budget *resourcebudget.Manager) resourcebudget.Permit {
	t.Helper()
	p, err := budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: sealCandidateDiskCeiling})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSealCandidatePublisherDurableSuccessorAndReopen(t *testing.T) {
	dir := t.TempDir()
	key := make([]byte, 32)
	key[0] = 7
	budget := testBudget(t)
	reservation := sealPublisherReservation(t, budget)
	hard := sealHardFor(sealCandidateFixture())
	guard, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	p, err := OpenSealCandidatePublisher(dir, guard, key, hard, budget, reservation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSealCandidatePublisher(dir, guard, key, hard, budget, reservation); !errors.Is(err, ErrBusy) {
		t.Fatalf("second live publisher was accepted: %v", err)
	}
	want := sealCandidateFixture()
	invalid := want
	invalid.Revision = 3
	if _, err := p.Publish(context.Background(), invalid); !errors.Is(err, ErrSealSequence) {
		t.Fatalf("revision gap was accepted: %v", err)
	}
	receipt, err := p.Publish(context.Background(), want)
	if err != nil || receipt.Generation != 1 || receipt.Revision != 1 {
		t.Fatalf("initial publish: %#v %v", receipt, err)
	}
	want.Revision = 2
	receipt, err = p.Publish(context.Background(), want)
	if err != nil || receipt.Revision != 2 {
		t.Fatalf("successor publish: %#v %v", receipt, err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	persistent, err := p.DiskReservation()
	if err != nil {
		t.Fatal(err)
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}

	guard, err = lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	p, err = OpenSealCandidatePublisher(dir, guard, key, hard, budget, persistent)
	if err != nil {
		t.Fatal(err)
	}
	got, found, err := p.Current()
	if err != nil || !found || got != want {
		t.Fatalf("reopened seal = %#v, found=%t, err=%v", got, found, err)
	}
	selected, found, err := p.Select(sealEligibilityFor(want))
	if err != nil || !found || selected != want {
		t.Fatalf("eligible seal selection = %#v, found=%t, err=%v", selected, found, err)
	}
	wrongCut := sealEligibilityFor(want)
	wrongCut.Positions[0].Sequence++
	if selected, found, err := p.Select(wrongCut); !errors.Is(err, ErrSealIneligible) || found || selected != (CandidateSeal{}) {
		t.Fatalf("seal with stale Ledger cut selected: %#v, found=%t, err=%v", selected, found, err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.Retire(); err != nil {
		t.Fatal(err)
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "active.seal")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("active seal remains after retire: %v", err)
	}
	if err := persistent.Release(); err != nil {
		t.Fatal(err)
	}
	if err := reservation.Release(); err != nil {
		t.Fatal(err)
	}
	if used := budget.Snapshot().Used.Disk; used != 0 {
		t.Fatalf("disk charge remained after retirement: %d", used)
	}
}

func TestSealCandidatePublisherRetireKeepsChargeForPendingFile(t *testing.T) {
	dir := t.TempDir()
	key := make([]byte, 32)
	key[0] = 10
	budget := testBudget(t)
	reservation := sealPublisherReservation(t, budget)
	hard := sealHardFor(sealCandidateFixture())
	guard, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	p, err := OpenSealCandidatePublisher(dir, guard, key, hard, budget, reservation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Publish(context.Background(), sealCandidateFixture()); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "active.seal.pending"), []byte("foreign"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p.Retire(); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("retire with pending foreign file = %v", err)
	}
	if used := budget.Snapshot().Used.Disk; used < sealCandidateDiskCeiling {
		t.Fatalf("retire released disk charge with pending file: %d", used)
	}
	if err := os.Remove(filepath.Join(dir, "active.seal.pending")); err != nil {
		t.Fatal(err)
	}
	if err := p.Retire(); err != nil {
		t.Fatal(err)
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	if err := reservation.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestSealCandidatePublisherChargesReadWorkspaceBeforeOpen(t *testing.T) {
	limits := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: 64 << 20, Disk: 64 << 20}, MaxClaims: 8, MaxOwners: 4, MaxPressureAge: time.Hour}
	for i := range limits.Classes {
		limits.Classes[i] = limits.Total
	}
	limits.Classes[resourcebudget.Recovery] = resourcebudget.Cost{}
	limits.Classes[resourcebudget.Control] = resourcebudget.Cost{RAM: int64(limits.MaxClaims) * resourcebudget.ClaimRAMBytes}
	limits.Classes[resourcebudget.IO] = resourcebudget.Cost{}
	budget, err := resourcebudget.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	if err := budget.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	reservation := sealPublisherReservation(t, budget)
	dir := t.TempDir()
	guard, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	key[0] = 11
	_, err = OpenSealCandidatePublisher(dir, guard, key, sealHardFor(sealCandidateFixture()), budget, reservation)
	if err == nil {
		t.Fatal("open succeeded without budget for bounded seal decode")
	}
	if used := budget.Snapshot().Used.Disk; used != sealCandidateDiskCeiling {
		t.Fatalf("failed open leaked/released caller disk ownership: %d", used)
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	if err := reservation.Release(); err != nil {
		t.Fatal(err)
	}
	if used := budget.Snapshot().Used.Disk; used != 0 {
		t.Fatalf("disk charge remained after caller release: %d", used)
	}
}

func TestSealCandidatePublisherAdmissionPrecedesActiveRead(t *testing.T) {
	dir := t.TempDir()
	key := make([]byte, 32)
	key[0] = 12
	budget := testBudget(t)
	reservation := sealPublisherReservation(t, budget)
	defer reservation.Release()
	guard, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	p, err := OpenSealCandidatePublisher(dir, guard, key, sealHardFor(sealCandidateFixture()), budget, reservation)
	if err != nil {
		t.Fatal(err)
	}
	want := sealCandidateFixture()
	if _, err := p.Publish(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	// Corrupt active while IO admission is closed. Publish must refuse the
	// workspace first, without reading or poisoning the old seal owner.
	raw, err := os.ReadFile(p.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.path, []byte("bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := budget.SetPressure(resourcebudget.Pressured); err != nil {
		t.Fatal(err)
	}
	want.Revision++
	if _, err := p.Publish(context.Background(), want); !errors.Is(err, resourcebudget.ErrPressure) {
		t.Fatalf("publish read active before workspace admission: %v", err)
	}
	if p.fault != nil {
		t.Fatalf("budget refusal poisoned publisher: %v", p.fault)
	}
	if err := os.WriteFile(p.path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := budget.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Publish(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.Retire(); err != nil {
		t.Fatal(err)
	}
}

func TestSealCandidatePublisherPendingRecoveryRequiresAuthenticatedSuccessor(t *testing.T) {
	for _, kind := range []string{"valid", "stale", "tampered", "partial"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			key := make([]byte, 32)
			key[0] = 13
			budget := testBudget(t)
			reservation := sealPublisherReservation(t, budget)
			defer reservation.Release()
			guard, err := lock.Acquire(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer guard.Close()
			want := sealCandidateFixture()
			hard := sealHardFor(want)
			p, err := OpenSealCandidatePublisher(dir, guard, key, hard, budget, reservation)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := p.Publish(context.Background(), want); err != nil {
				t.Fatal(err)
			}
			if err := p.Close(); err != nil {
				t.Fatal(err)
			}
			credit, err := p.DiskReservation()
			if err != nil {
				t.Fatal(err)
			}
			defer credit.Release()
			next := want
			next.Revision++
			if kind == "stale" {
				next = want
			}
			raw, err := EncodeSealCandidate(next, key, hard)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "tampered" {
				raw[len(raw)-1] ^= 1
			}
			if kind == "partial" {
				raw = raw[:len(raw)/2]
			}
			path := filepath.Join(dir, "active.seal.pending")
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenSealCandidatePublisher(dir, guard, key, hard, budget, credit)
			if kind != "valid" {
				if err == nil {
					t.Fatal("unproven pending file was accepted")
				}
				got, readErr := os.ReadFile(path)
				if readErr != nil || string(got) != string(raw) {
					t.Fatal("rejection changed pending", readErr)
				}
				if used := budget.Snapshot().Used.Disk; used != sealCandidateDiskCeiling {
					t.Fatalf("persistent fee changed: %d", used)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, found, err := reopened.Current()
			if err != nil || !found || got != want {
				t.Fatalf("pending promoted or active lost: %v %t %v", got, found, err)
			}
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("authenticated orphan remains: %v", err)
			}
			if err := reopened.Close(); err != nil {
				t.Fatal(err)
			}
			if err := reopened.Retire(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSealCandidatePublisherFailureStagesAndForeignReplacement(t *testing.T) {
	for _, stage := range []string{"seal-file-synced", "seal-renamed"} {
		t.Run(stage, func(t *testing.T) {
			dir := t.TempDir()
			key := make([]byte, 32)
			key[0] = 8
			budget := testBudget(t)
			reservation := sealPublisherReservation(t, budget)
			hard := sealHardFor(sealCandidateFixture())
			guard, err := lock.Acquire(dir)
			if err != nil {
				t.Fatal(err)
			}
			injected := errors.New("injected publication interruption")
			p, err := openSealCandidatePublisher(dir, guard, key, hard, budget, reservation, func(got string) error {
				if got == stage {
					return injected
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = p.Publish(context.Background(), sealCandidateFixture())
			if !errors.Is(err, injected) {
				t.Fatalf("publish error = %v", err)
			}
			if stage == "seal-file-synced" {
				if _, statErr := os.Lstat(filepath.Join(dir, "active.seal")); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("pre-rename failure published seal: %v", statErr)
				}
			} else if _, statErr := os.Stat(filepath.Join(dir, "active.seal")); statErr != nil {
				t.Fatalf("post-rename failure lost visible seal: %v", statErr)
			}
			_ = p.Close()
			persistent, transferErr := p.DiskReservation()
			if transferErr != nil {
				t.Fatal(transferErr)
			}
			if err := guard.Close(); err != nil {
				t.Fatal(err)
			}
			guard, err = lock.Acquire(dir)
			if err != nil {
				t.Fatal(err)
			}
			p, err = OpenSealCandidatePublisher(dir, guard, key, hard, budget, persistent)
			if err != nil {
				t.Fatal(err)
			}
			got, found, err := p.Current()
			wantFound := stage == "seal-renamed"
			if err != nil || found != wantFound || found && got != sealCandidateFixture() {
				t.Fatalf("reopen stage=%s current=%#v found=%t err=%v", stage, got, found, err)
			}
			if err := p.Close(); err != nil {
				t.Fatal(err)
			}
			if wantFound {
				if err := p.Retire(); err != nil {
					t.Fatal(err)
				}
			}
			if err := guard.Close(); err != nil {
				t.Fatal(err)
			}
			if err := persistent.Release(); err != nil {
				t.Fatal(err)
			}
			if err := reservation.Release(); err != nil {
				t.Fatal(err)
			}
		})
	}

	dir := t.TempDir()
	key := make([]byte, 32)
	key[0] = 9
	budget := testBudget(t)
	reservation := sealPublisherReservation(t, budget)
	hard := sealHardFor(sealCandidateFixture())
	guard, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	p, err := OpenSealCandidatePublisher(dir, guard, key, hard, budget, reservation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Publish(context.Background(), sealCandidateFixture()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "active.seal")
	if err := os.Rename(path, path+".owned"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("foreign"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.Current(); !errors.Is(err, ownedfile.ErrReplaced) && !errors.Is(err, ErrCorrupt) {
		t.Fatalf("foreign replacement error = %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".owned", path); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err == nil {
		t.Fatal("integrity fault was forgotten on close")
	}
	if err := p.Retire(); err != nil {
		t.Fatal(err)
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	if err := reservation.Release(); err != nil {
		t.Fatal(err)
	}
}
