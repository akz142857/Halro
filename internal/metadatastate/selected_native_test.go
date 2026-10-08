package metadatastate

import (
	"bytes"
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

type selectedMetadataNativeFixture struct {
	baseline     *Baseline
	path         string
	key          []byte
	options      metadatajournal.SelectedSealOpenOptions
	head         metadatajournal.Head
	rootReplaced *bool
}

func (f selectedMetadataNativeFixture) reopenRoot(t *testing.T) (*Baseline, ReopenReceipt) {
	t.Helper()
	root, journal, err := f.baseline.DiskReservations()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := root.Release(); err != nil {
			t.Error(err)
		}
		if err := journal.Release(); err != nil {
			t.Error(err)
		}
	})
	o := SelectedOpenOptions{BaselineOptions: BaselineOptions{Identity: f.baseline.evidence.Identity, InstanceID: f.baseline.evidence.InstanceID, KeyGeneration: f.baseline.evidence.KeyGeneration, Budget: f.options.Budget, MaxSourceBytes: 64 << 20, MaxDiskBytes: 64 << 20, MaxAppendBytes: 8 << 20}, Evidence: f.baseline.evidence, Binding: f.options.Binding, Publisher: f.options.Publisher, Guard: f.options.Guard, Authority: f.options.Authority, RootDiskReservation: root, JournalDiskReservation: journal, MaxJournalBytes: f.options.MaxBytes}
	b, receipt, err := OpenSelectedFromRoot(context.Background(), bytes.Repeat([]byte{51}, 32), f.baseline.output, f.path, o)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.AppliedFrames > 0 {
		*f.rootReplaced = true
	}
	t.Cleanup(func() {
		if err := b.Close(); err != nil {
			t.Error(err)
		}
	})
	return b, receipt
}

func TestMetadataSelectedRootFactoryReducesOnlyDeltaAndBlocksDAOUntilAudit(t *testing.T) {
	f := selectedMetadataNative(t)
	for range 2 {
		if _, err := f.baseline.AppendFrame(context.Background(), []metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte("tail"), Value: []byte("durable")}}); err != nil {
			t.Fatal(err)
		}
	}
	want := f.baseline.journal.Head()
	if err := f.baseline.Close(); err != nil {
		t.Fatal(err)
	}
	b, receipt := f.reopenRoot(t)
	if receipt.From != f.head || receipt.To != want || receipt.SourceFrames != 2 || receipt.AppliedFrames != 2 || b.ready.Load() {
		t.Fatal("selected factory traversed history or readied pending root", receipt)
	}
	if err := b.View(context.Background(), func(*Transaction) error { t.Fatal("pending DAO callback entered"); return nil }); !errors.Is(err, authenticatedstate.ErrClosed) {
		t.Fatal("pending DAO admitted", err)
	}
	if _, err := b.SealReference(context.Background()); !errors.Is(err, authenticatedstate.ErrClosed) {
		t.Fatal("pending seal admitted", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.AuditSelectedRootStartup(ctx); !errors.Is(err, context.Canceled) || b.ready.Load() {
		t.Fatal("canceled audit readied root", err)
	}
	if err := b.AuditSelectedRootStartup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !b.ready.Load() {
		t.Fatal("completed audit did not ready root")
	}
	if err := b.View(context.Background(), func(tx *Transaction) error {
		v, found, err := tx.Get([]string{"projects"}, []byte("tail"))
		if err != nil || !found || string(v) != "durable" {
			t.Fatal(v, found, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	selected, err := b.SealReference(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	backend, err := b.StandaloneFacade(context.Background(), b.budget, b.evidence.Identity, b.evidence.InstanceID, b.evidence.KeyGeneration, selected)
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.HealthError(); err != nil {
		t.Fatal(err)
	}
}

func TestMetadataSelectedRootFactoryOldTamperStaysPendingAfterActualDelta(t *testing.T) {
	f := selectedMetadataNative(t)
	if err := f.baseline.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-33] ^= 1
	if err := os.WriteFile(f.path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	b, receipt := f.reopenRoot(t)
	if receipt.SourceFrames != 0 || receipt.AppliedFrames != 0 || b.ready.Load() {
		t.Fatal(receipt)
	}
	if err := b.AuditSelectedRootStartup(context.Background()); !errors.Is(err, metadatajournal.ErrCorrupt) || b.ready.Load() {
		t.Fatal("old native tamper accepted", err)
	}
	if _, err := b.SealReference(context.Background()); !errors.Is(err, authenticatedstate.ErrClosed) {
		t.Fatal(err)
	}
}

func selectedMetadataNative(t *testing.T) selectedMetadataNativeFixture {
	return selectedMetadataNativeCount(t, 3)
}

func selectedMetadataNativeCount(t *testing.T, count int) selectedMetadataNativeFixture {
	return selectedMetadataNativeCountRetirement(t, count, false)
}
func selectedMetadataNativeCountRetirement(t *testing.T, count int, changedPhysicalRoot bool) selectedMetadataNativeFixture {
	return selectedMetadataNativeCountRetirementPolicy(t, count, changedPhysicalRoot, false)
}

func selectedMetadataNativeCountRetirementPolicy(t *testing.T, count int, changedPhysicalRoot, retainedBorrower bool) selectedMetadataNativeFixture {
	return selectedMetadataNativeCountRole(t, count, changedPhysicalRoot, 16<<20, authenticatedstate.SealStandalone, retainedBorrower)
}
func selectedMetadataNativeCountLimits(t *testing.T, count int, changedPhysicalRoot bool, journalBytes int64) selectedMetadataNativeFixture {
	return selectedMetadataNativeCountRole(t, count, changedPhysicalRoot, journalBytes, authenticatedstate.SealStandalone)
}
func selectedMetadataNativeCountRole(t *testing.T, count int, changedPhysicalRoot bool, journalBytes int64, role authenticatedstate.SealRole, retainedBorrower ...bool) selectedMetadataNativeFixture {
	t.Helper()
	ctx := context.Background()
	archive, _, _ := baselineArchive(t, true, nil, nil)
	o := baselineOptions(t)
	if changedPhysicalRoot {
		o.Budget = metadataPhysicalBudget(t)
	}
	key := bytes.Repeat([]byte{51}, 32)
	b, err := BuildFromBackup(ctx, archive, key, privateSource(t), filepath.Join(t.TempDir(), "root"), o)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(filepath.Dir(privateSource(t)), "metadata.journal")
	if _, err := b.StartNewEpoch(ctx, key, path, 8, journalBytes); err != nil {
		t.Fatal(err)
	}
	rootReplaced := false
	t.Cleanup(func() {
		// A successfully reopened borrower may replace the root inode or update
		// its authenticated bytes in place. The original creator then refuses
		// removal by inode or digest; the temp-directory harness,
		// not the product's old ownership receipt, cleans up that test output.
		if err := b.Discard(); err != nil && !(rootReplaced && (errors.Is(err, ownedfile.ErrReplaced) || errors.Is(err, authenticatedstate.ErrCorrupt) || len(retainedBorrower) == 1 && retainedBorrower[0] && errors.Is(err, lock.ErrAlreadyLocked))) {
			t.Error(err)
		}
	})
	for i := range count {
		if _, err := b.AppendFrame(ctx, []metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte{byte(i)}, Value: []byte("accepted")}}); err != nil {
			t.Fatal(err)
		}
	}
	ref, err := b.SealReference(ctx)
	if err != nil {
		t.Fatal(err)
	}
	head := b.journal.Head()
	key, err = hkdf.Key(sha256.New, bytes.Repeat([]byte{51}, 32), head.Baseline.Scope[:], "halro:metadata:bound-journal:hmac:v1", 32)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(path)
	guard, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	seal := authenticatedstate.CandidateSeal{SchemaVersion: 1, ApplyVersion: 1, IndexVersion: 1, KeyDerivation: 1, Instance: [32]byte{1}, Incarnation: [16]byte{1}, Generation: 1, Revision: 1, Role: authenticatedstate.SealStandalone, Coverage: authenticatedstate.SealCoverageAuthenticated, MaxPageBytes: 4 << 20, MaxValueBytes: 3 << 20, MaxTreeHeight: 32, PositionCount: 5, RootCount: 1}
	seal.Positions[0] = authenticatedstate.SealPosition{Component: authenticatedstate.SealComponentLedger, Epoch: 1, Revision: 1}
	seal.Positions[1] = ref.Position
	seal.Positions[2].Component = authenticatedstate.SealComponentGovernance
	seal.Positions[3].Component = authenticatedstate.SealComponentAudit
	seal.Positions[4].Component = authenticatedstate.SealComponentUsage
	seal.Roots[0] = ref.Root
	if role != authenticatedstate.SealStandalone {
		seal.Instance = b.evidence.Identity
		seal.Role = role
		seal.Cluster = [32]byte{2}
		seal.Member = [32]byte{3}
		seal.Term = 1
		seal.OrderingTerm = 1
		seal.OrderingIndex = 1
		seal.OrderingOffset = 256
		seal.OrderingMAC = [32]byte{4}
	}
	hard := authenticatedstate.SealHardLimits{MaxPageBytes: seal.MaxPageBytes, MaxValueBytes: seal.MaxValueBytes, MaxTreeHeight: seal.MaxTreeHeight, RequiredRoots: []authenticatedstate.SealRootSchema{ref.Schema}}
	fee, err := o.Budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: 32 << 10})
	if err != nil {
		t.Fatal(err)
	}
	pub, err := authenticatedstate.OpenSealCandidatePublisher(dir, guard, key, hard, o.Budget, fee)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pub.Publish(ctx, seal); err != nil {
		t.Fatal(err)
	}
	authority := authenticatedstate.SealEligibility{SchemaVersion: 1, ApplyVersion: 1, IndexVersion: 1, KeyDerivation: 1, Instance: seal.Instance, Incarnation: seal.Incarnation, Role: seal.Role, Coverage: seal.Coverage}
	authority.Cluster = seal.Cluster
	authority.Member = seal.Member
	authority.Term = seal.Term
	authority.OrderingIndex = seal.OrderingIndex
	authority.OrderingOffset = seal.OrderingOffset
	authority.OrderingMAC = seal.OrderingMAC
	copy(authority.Positions[:], seal.Positions[:])
	t.Cleanup(func() {
		if err := pub.Close(); err != nil {
			t.Error(err)
		}
		if err := pub.Retire(); err != nil && !(changedPhysicalRoot && rootReplaced && errors.Is(err, ownedfile.ErrReplaced)) {
			t.Error(err)
		}
		if err := fee.Release(); err != nil {
			t.Error(err)
		}
		if err := guard.Close(); err != nil {
			t.Error(err)
		}
		clear(key)
	})
	return selectedMetadataNativeFixture{b, path, key, metadatajournal.SelectedSealOpenOptions{Publisher: pub, Guard: guard, Authority: authority, Budget: o.Budget, Binding: head.Baseline, MaxBytes: journalBytes}, head, &rootReplaced}
}

func TestMetadataSelectedNativeOnlyDecodesSuffixAndAuditsBeforeAdmission(t *testing.T) {
	f := selectedMetadataNative(t)
	for range 2 {
		if _, err := f.baseline.AppendFrame(context.Background(), []metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte("tail"), Value: []byte("durable")}}); err != nil {
			t.Fatal(err)
		}
	}
	want := f.baseline.journal.Head()
	if err := f.baseline.Close(); err != nil {
		t.Fatal(err)
	}
	before := f.options.Budget.Snapshot().Used
	l, receipt, err := metadatajournal.OpenBoundWithSelectedSeal(context.Background(), f.path, f.key, f.options)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if receipt.Saved != f.head || receipt.Head != want || receipt.DecodedRecords != 2 || !receipt.HistoricalAuditPending || l.SelectedStartupReady() || !errors.Is(l.HealthError(), metadatajournal.ErrStartupAuditPending) {
		t.Fatal(receipt, l.HealthError())
	}
	if _, err := l.Append([]metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte("refused"), Value: []byte("v")}}); !errors.Is(err, metadatajournal.ErrStartupAuditPending) {
		t.Fatal("pending native admitted append", err)
	}
	prepared := false
	if _, err := l.AppendPreparedContext(context.Background(), []metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte("refused"), Value: []byte("v")}}, func(metadatajournal.Record, func() error) error { prepared = true; return nil }); !errors.Is(err, metadatajournal.ErrStartupAuditPending) || prepared {
		t.Fatal("pending native admitted preparation", prepared, err)
	}
	if err := l.AuditSelectedStartup(context.Background(), f.options.Budget); err != nil {
		t.Fatal(err)
	}
	if !l.SelectedStartupReady() || l.HealthError() != nil || l.Head() != want || f.options.Budget.Snapshot().Used != before {
		t.Fatal("audit readiness/head/fee", l.HealthError(), f.options.Budget.Snapshot())
	}
	if _, err := l.Append([]metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte("next"), Value: []byte("v")}}); err != nil {
		t.Fatal(err)
	}
}

func TestMetadataSelectedNativeDefersOldTamperButNeverReadiesIt(t *testing.T) {
	f := selectedMetadataNative(t)
	if err := f.baseline.Close(); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	// Damage an old operations payload while preserving header and selected
	// end. No delta exists: initialization must not scan these historical bytes.
	damaged := bytes.Clone(original)
	damaged[len(damaged)-33] ^= 1
	if err := os.WriteFile(f.path, damaged, 0600); err != nil {
		t.Fatal(err)
	}
	l, receipt, err := metadatajournal.OpenBoundWithSelectedSeal(context.Background(), f.path, f.key, f.options)
	if err != nil {
		t.Fatal("old bytes were traversed during initialization", err)
	}
	defer l.Close()
	if receipt.DecodedRecords != 0 || l.SelectedStartupReady() {
		t.Fatal(receipt)
	}
	if err := l.AuditSelectedStartup(context.Background(), f.options.Budget); !errors.Is(err, metadatajournal.ErrCorrupt) {
		t.Fatal("old tamper passed actual audit", err)
	}
	if l.SelectedStartupReady() || !errors.Is(l.HealthError(), metadatajournal.ErrCorrupt) {
		t.Fatal("tamper became healthy", l.HealthError())
	}
	after, err := os.ReadFile(f.path)
	if err != nil || !bytes.Equal(after, damaged) {
		t.Fatal("audit repaired old bytes", err)
	}
}

func TestMetadataSelectedNativeRejectsForeignAuthorityAndPartialTailWithoutRepair(t *testing.T) {
	for _, shape := range []string{"authority", "binding", "manager", "closed guard", "partial tail", "tail tamper", "canceled"} {
		t.Run(shape, func(t *testing.T) {
			f := selectedMetadataNative(t)
			if shape == "tail tamper" {
				if _, err := f.baseline.AppendFrame(context.Background(), []metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte("tail"), Value: []byte("v")}}); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.baseline.Close(); err != nil {
				t.Fatal(err)
			}
			o := f.options
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch shape {
			case "authority":
				o.Authority.Positions[1].Sequence++
			case "binding":
				o.Binding.RootDigest[0] ^= 1
			case "manager":
				o.Budget = baselineBudget(t)
			case "closed guard":
				closedGuard, acquireErr := lock.Acquire(t.TempDir())
				if acquireErr != nil {
					t.Fatal(acquireErr)
				}
				o.Guard = closedGuard
				if err := o.Guard.Close(); err != nil {
					t.Fatal(err)
				}
			case "partial tail":
				file, err := os.OpenFile(f.path, os.O_WRONLY|os.O_APPEND, 0600)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := file.Write([]byte("HM")); err != nil {
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
			case "tail tamper":
				raw, err := os.ReadFile(f.path)
				if err != nil {
					t.Fatal(err)
				}
				raw[len(raw)-1] ^= 1
				if err := os.WriteFile(f.path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "canceled":
				cancel()
			}
			before, err := os.ReadFile(f.path)
			if err != nil {
				t.Fatal(err)
			}
			fee := o.Budget.Snapshot().Used
			l, _, err := metadatajournal.OpenBoundWithSelectedSeal(ctx, f.path, f.key, o)
			if err == nil || l != nil {
				t.Fatal("invalid selected source admitted", l, err)
			}
			after, err := os.ReadFile(f.path)
			if err != nil || !bytes.Equal(before, after) || o.Budget.Snapshot().Used != fee {
				t.Fatal("refusal repaired source or fees", err, o.Budget.Snapshot())
			}
		})
	}
}
