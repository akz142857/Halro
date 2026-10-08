//go:build linux

package metadatastate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/store/lock"
)

func requireMetadataPhysicalExt4(t *testing.T) {
	t.Helper()
	var fs syscall.Statfs_t
	if err := syscall.Statfs(os.TempDir(), &fs); err != nil {
		t.Fatal("physical fixture filesystem", err)
	}
	if fs.Type != 0xef53 {
		t.Skip("physical allocation fixture requires ext4")
	}
}

func TestMetadataPhysicalActualExt4PricePinCommitAtENOSPC(t *testing.T) {
	metadataPhysicalActualExt4PricePinTerminalAtENOSPC(t, false)
}

func TestMetadataPhysicalActualExt4PreparedDeleteAtENOSPC(t *testing.T) {
	metadataPhysicalActualExt4PricePinTerminalAtENOSPC(t, true)
}

func metadataPhysicalActualExt4PricePinTerminalAtENOSPC(t *testing.T, cleanup bool) {
	root := os.Getenv("HALRO_TERMINAL_ENOSPC_ROOT")
	if root == "" {
		t.Skip("requires owned isolated full ext4 fixture")
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(root, &fs); err != nil || fs.Type != 0xef53 {
		t.Fatal("actual ext4 prerequisite", err, fs.Type)
	}
	f := selectedMetadataNativeCountRetirement(t, 0, true)
	ctx := context.Background()
	*f.rootReplaced = true
	if err := f.baseline.Close(); err != nil {
		t.Fatal(err)
	}
	rootFee, oldJournal, err := f.baseline.DiskReservations()
	if err != nil {
		t.Fatal(err)
	}
	defer rootFee.Release()
	defer oldJournal.Release()
	journalFee, err := f.options.Budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer journalFee.Release()
	o := SelectedOpenOptions{BaselineOptions: baselineOptionsForSelectedFixture(f), Evidence: f.baseline.evidence, Binding: f.options.Binding, Publisher: f.options.Publisher, Guard: f.options.Guard, Authority: f.options.Authority, RootDiskReservation: rootFee, JournalDiskReservation: journalFee, MaxJournalBytes: 64 << 20, PhysicalTerminalReserve: true}
	b, _, err := OpenSelectedFromRoot(ctx, bytes.Repeat([]byte{51}, 32), f.baseline.output, f.path, o)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := b.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := b.AuditSelectedRootStartup(ctx); err != nil {
		t.Fatal(err)
	}
	p := physicalPin(t)
	raw, _ := json.Marshal(p)
	if err := b.EnableQualifiedPhysicalTerminals(ctx, f.options.Publisher, f.options.Guard, f.options.Authority); err != nil {
		t.Fatal("enable actual source targets", err)
	}
	ref, err := b.SealReference(ctx)
	if err != nil {
		t.Fatal(err)
	}
	backend, err := b.StandaloneFacade(ctx, b.budget, b.evidence.Identity, b.evidence.InstanceID, b.evidence.KeyGeneration, ref)
	if err != nil {
		t.Fatal(err)
	}
	store, err := boltstore.NewFacadeStore(ctx, backend)
	if err != nil {
		t.Fatal(err)
	}
	w, err := b.ReservePhysicalTerminals(ctx, 1)
	if err != nil {
		t.Fatal("prepay actual terminal", err)
	}
	if _, err := b.Update(w.Bind(ctx), func(tx *Transaction) error {
		return tx.Put([]string{"deployment_price_pin_intents"}, []byte(p.AttemptID), raw)
	}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := w.Close(); err != nil {
			t.Error(err)
		}
	}()
	nativeBefore := b.journal.Head()
	filler, err := os.OpenFile(filepath.Join(t.TempDir(), "full-ext4-owned"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer filler.Close()
	var filled int64
	for chunk := int64(16 << 20); chunk >= 4096; chunk /= 2 {
		for {
			err = resourcebudget.AllocatePhysicalRange(filler, filled, chunk)
			if errors.Is(err, syscall.ENOSPC) {
				break
			}
			if err != nil {
				t.Fatal("fill actual FS", err)
			}
			filled += chunk
		}
	}
	t.Log("actual filesystem full", filled)
	// Ordinary Metadata must allocate above the remaining terminal native/COW debt.
	if _, err := b.Update(ctx, func(tx *Transaction) error {
		return tx.Put([]string{"projects"}, []byte("ordinary-above-terminal"), []byte("must refuse"))
	}); !errors.Is(err, syscall.ENOSPC) {
		t.Fatal("ordinary update did not refuse actual ENOSPC", err)
	}
	if b.journal.Head() != nativeBefore || b.physicalDebt != 1 {
		t.Fatal("pre-native refusal changed native or debt")
	}
	if err := b.budget.SetPressure(resourcebudget.Pressured); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Update(ctx, func(boltstore.FacadeTransaction) error { t.Fatal("ordinary pressured callback entered"); return nil }); !errors.Is(err, resourcebudget.ErrPressure) {
		t.Fatal("ordinary pressure refusal", err)
	}
	// Known pre-native refusal preserves this original accepted window.
	// Append currently readies false on refusal; a failed physical prepare occurs
	// before that ready transition and must not revoke the original capability.
	if cleanup {
		// The original canceled request may clean up through its preserved values.
		canceled, cancel := context.WithCancel(w.Bind(ctx))
		cancel()
		if err := store.DeletePreparedDeploymentPricePin(context.WithoutCancel(canceled), p.AttemptID); err != nil {
			t.Fatal("actual prepaid unchanged Prepared deletion failed", err)
		}
	} else {
		if _, err := store.CommitDeploymentPricePin(w.Bind(ctx), p.AttemptID, p.SnapshotSHA256, 7, time.Now().UTC()); err != nil {
			t.Fatal("actual prepaid PricePin terminal failed", err)
		}
	}
	if b.journal.Head().Sequence != nativeBefore.Sequence+1 || b.physicalDebt != 0 || w.remaining != 0 {
		t.Fatal("terminal did not consume exact one debt")
	}
	if w.hasPreparedPins() {
		t.Fatal("known terminal retained completed obligation")
	}
	terminalCtx := backend.(interface {
		PricePinTerminalContext(context.Context) context.Context
	}).PricePinTerminalContext(w.Bind(ctx))
	if err := backend.View(terminalCtx, func(tx boltstore.FacadeTransaction) error {
		raw, found, err := tx.Get([]string{"deployment_price_pin_intents"}, []byte(p.AttemptID))
		if err != nil {
			return err
		}
		if cleanup {
			if found {
				return ErrSource
			}
			return nil
		}
		var actual domain.PricePinIntent
		if !found || json.Unmarshal(raw, &actual) != nil || actual.State != domain.PricePinCommitted || actual.LedgerSequence != 7 {
			return ErrSource
		}
		return nil
	}); err != nil {
		t.Fatal("actual authenticated terminal row", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := filler.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filler.Name()); err != nil {
		t.Fatal(err)
	}
	if err := b.budget.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	publishMetadataPhysicalActual(t, b, f.options.Publisher)
	if err := b.RetirePhysicalPreparation(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestMetadataPhysicalPostCommonCloseUsesOriginalCallerWorkspace(t *testing.T) {
	requireMetadataPhysicalExt4(t)
	f := selectedMetadataNativeCountRetirement(t, 0, true)
	ctx := context.Background()
	if err := f.baseline.Close(); err != nil {
		t.Fatal(err)
	}
	rc, jc, err := f.baseline.DiskReservations()
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Release()
	defer jc.Release()
	o := SelectedOpenOptions{PhysicalTerminalReserve: true, BaselineOptions: baselineOptionsForSelectedFixture(f), Evidence: f.baseline.evidence, Binding: f.options.Binding, Publisher: f.options.Publisher, Guard: f.options.Guard, Authority: f.options.Authority, RootDiskReservation: rc, JournalDiskReservation: jc, MaxJournalBytes: f.options.MaxBytes}
	b, _, err := OpenSelectedFromRoot(ctx, metadataTestKey(), f.baseline.output, f.path, o)
	if err != nil {
		t.Fatal(err)
	}
	*f.rootReplaced = true
	if err := b.AuditSelectedRootStartup(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.EnableQualifiedPhysicalTerminals(ctx, f.options.Publisher, f.options.Guard, f.options.Authority); err != nil {
		t.Fatal(err)
	}
	if err := b.RetirePhysicalPreparationAfterPinRecovery(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.BindPhysicalCommonSelection(ctx); err != nil {
		t.Fatal(err)
	}
	// This is an original paid maintenance caller, acquired before pressure.
	// No synthetic Healthy is injected after the pressure transition.
	caller, err := b.acquirePhysicalCompletion()
	if err != nil {
		t.Fatal(err)
	}
	if err := b.budget.SetPressure(resourcebudget.Pressured); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.options.Publisher.Current(); !errors.Is(err, resourcebudget.ErrPressure) {
		t.Fatal("unpaid final read should refuse", err)
	}
	if err := resourcebudget.WithCompletionWorkspace(caller.Bind(ctx), func(ctx context.Context) error { return b.CloseContext(ctx) }); err != nil {
		t.Fatal("same original completion Close", err)
	}
	if err := b.Close(); err != nil {
		t.Fatal("stable second actual Close", err)
	}
	if err := errors.Join(caller.Close(), caller.Active.Release()); err != nil {
		t.Fatal(err)
	}
}

// The independent test authority is supplied by the parent harness, not read
// from a candidate header. Child/root/native/publication are real persistent IO.
type metadataPhysicalCrashInput struct {
	Root, Journal string
	Evidence      BaselineEvidence
	Binding       metadatajournal.BaselineBinding
	Seal          authenticatedstate.CandidateSeal
	Authority     authenticatedstate.SealEligibility
	PublisherKey  []byte
}

func openMetadataPhysicalCrash(t *testing.T, input metadataPhysicalCrashInput) (*Baseline, *authenticatedstate.SealCandidatePublisher, func()) {
	t.Helper()
	ctx := context.Background()
	manager := metadataPhysicalBudget(t)
	guard, err := lock.Acquire(filepath.Dir(input.Journal))
	if err != nil {
		t.Fatal(err)
	}
	fee, err := manager.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: 32 << 10})
	if err != nil {
		t.Fatal(err)
	}
	schema := authenticatedstate.SealRootSchema{Kind: authenticatedstate.SealRootState, Component: authenticatedstate.SealComponentMetadata, IndexID: AuthenticatedMetadataSealIndexID, Codec: AuthenticatedMetadataSealCodec}
	hard := authenticatedstate.SealHardLimits{MaxPageBytes: input.Seal.MaxPageBytes, MaxValueBytes: input.Seal.MaxValueBytes, MaxTreeHeight: input.Seal.MaxTreeHeight, RequiredRoots: []authenticatedstate.SealRootSchema{schema}}
	publisher, err := authenticatedstate.OpenSealCandidatePublisher(filepath.Dir(input.Journal), guard, input.PublisherKey, hard, manager, fee)
	if err != nil {
		t.Fatal(err)
	}
	rootFee, err := manager.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	journalFee, err := manager.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	o := SelectedOpenOptions{PhysicalTerminalReserve: true, BaselineOptions: BaselineOptions{Identity: input.Evidence.Identity, InstanceID: input.Evidence.InstanceID, KeyGeneration: input.Evidence.KeyGeneration, Budget: manager, MaxSourceBytes: 64 << 20, MaxDiskBytes: 64 << 20, MaxAppendBytes: 8 << 20}, Evidence: input.Evidence, Binding: input.Binding, Publisher: publisher, Guard: guard, Authority: input.Authority, RootDiskReservation: rootFee, JournalDiskReservation: journalFee, MaxJournalBytes: 64 << 20}
	b, _, err := OpenSelectedFromRoot(ctx, bytes.Repeat([]byte{51}, 32), input.Root, input.Journal, o)
	if err != nil {
		t.Fatal("real physical reopen", err)
	}
	if err := b.AuditSelectedRootStartup(ctx); err != nil {
		t.Fatal(err)
	}
	var cleanup sync.Once
	return b, publisher, func() {
		cleanup.Do(func() {
			if err := b.Close(); err != nil && !(b.physicalRecovery != nil && errors.Is(err, authenticatedstate.ErrRecovery)) {
				t.Error(err)
			}
			if err := publisher.Close(); err != nil {
				t.Error(err)
			}
			if err := guard.Close(); err != nil {
				t.Error(err)
			}
			if err := rootFee.Release(); err != nil {
				t.Error(err)
			}
			if err := journalFee.Release(); err != nil {
				t.Error(err)
			}
			if err := fee.Release(); err != nil {
				t.Error(err)
			}
		})
	}
}
func TestMetadataPhysicalSIGKILLResumesPreNativeAndDurableCOWDebt(t *testing.T) {
	if cfgPath := os.Getenv("HALRO_METADATA_PHYSICAL_CRASH_CHILD"); cfgPath != "" {
		raw, err := os.ReadFile(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		var input metadataPhysicalCrashInput
		if err = json.Unmarshal(raw, &input); err != nil {
			t.Fatal(err)
		}
		b, _, closeOwner := openMetadataPhysicalCrash(t, input)
		defer closeOwner()
		ctx := context.Background()
		p := physicalPin(t)
		value, _ := json.Marshal(p)
		if err = b.EnableQualifiedPhysicalTerminals(ctx, b.selectedPublisher, b.selectedGuard, input.Authority); err != nil {
			t.Fatal(err)
		}
		w, err := b.ReservePhysicalTerminals(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()
		// Match the real request: prepaid terminal debt exists before the
		// ordinary prepared pin allocates above it. A later terminal must keep
		// the already allocated larger native extent in its durable receipt.
		if _, err = b.Update(w.Bind(ctx), func(tx *Transaction) error {
			return tx.Put([]string{"deployment_price_pin_intents"}, []byte(p.AttemptID), value)
		}); err != nil {
			t.Fatal(err)
		}
		at := time.Now().UTC()
		p.State = domain.PricePinCommitted
		p.LedgerSequence = 7
		p.CommittedAt = &at
		value, _ = json.Marshal(p)
		callCtx := context.WithValue(w.Bind(ctx), pricePinTerminalKey{}, true)
		_, err = b.appendFrame(callCtx, []metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"deployment_price_pin_intents"}, Key: []byte(p.AttemptID), Value: value}}, func(phase string) {
			if phase == os.Getenv("HALRO_METADATA_PHYSICAL_CRASH_PHASE") {
				fmt.Println("actual-metadata-phase", phase)
				_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
				select {}
			}
		})
		t.Fatal("SIGKILL phase did not terminate child", err)
		return
	}
	if os.Getenv("HALRO_EXT4_ROOT") == "" {
		t.Skip("requires actual isolated ext4 process fixture")
	}
	for _, phase := range []string{"candidate-prepared", "native-durable"} {
		t.Run(phase, func(t *testing.T) {
			f := selectedMetadataNativeCountRetirement(t, 0, true)
			*f.rootReplaced = true
			seal, found, err := f.options.Publisher.Current()
			if err != nil || !found {
				t.Fatal(err)
			}
			input := metadataPhysicalCrashInput{Root: f.baseline.output, Journal: f.path, Evidence: f.baseline.evidence, Binding: f.options.Binding, Seal: seal, Authority: f.options.Authority, PublisherKey: bytes.Clone(f.key)}
			if err = f.baseline.Close(); err != nil {
				t.Fatal(err)
			}
			if err = f.options.Publisher.Close(); err != nil {
				t.Fatal(err)
			}
			if err = f.options.Guard.Close(); err != nil {
				t.Fatal(err)
			}
			cfgPath := filepath.Join(t.TempDir(), "independent-authority.json")
			raw, _ := json.Marshal(input)
			if err = os.WriteFile(cfgPath, raw, 0600); err != nil {
				t.Fatal(err)
			}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(executable, "-test.run", "^TestMetadataPhysicalSIGKILLResumesPreNativeAndDurableCOWDebt$", "-test.count=1", "-test.v")
			cmd.Env = append(os.Environ(), "HALRO_METADATA_PHYSICAL_CRASH_CHILD="+cfgPath, "HALRO_METADATA_PHYSICAL_CRASH_PHASE="+phase)
			output, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL || !bytes.Contains(output, []byte("actual-metadata-phase "+phase)) {
				t.Fatal("child did not reach real kill", err, string(output))
			}
			b, publisher, closeOwner := openMetadataPhysicalCrash(t, input)
			defer closeOwner()
			if err = b.View(context.Background(), func(tx *Transaction) error {
				raw, found, err := tx.Get([]string{"deployment_price_pin_intents"}, []byte("att_terminal"))
				if err != nil || !found {
					return errors.Join(ErrSource, err)
				}
				var p domain.PricePinIntent
				if err = json.Unmarshal(raw, &p); err != nil {
					return err
				}
				expected := domain.PricePinPrepared
				if phase == "native-durable" {
					expected = domain.PricePinCommitted
				}
				if p.State != expected {
					t.Fatalf("source fact changed %s != %s", p.State, expected)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if phase == "native-durable" {
				if b.physicalDebt != 0 || b.position.Sequence != 2 {
					t.Fatal("durable native fact did not consume original target", b.physicalDebt, b.position)
				}
				closeOwner() // joins actual FDs, retains the zero-count receipt
				again, _, closeAgain := openMetadataPhysicalCrash(t, input)
				defer closeAgain()
				if again.physicalRecovery == nil || again.physicalRecovery.remaining != 0 || again.position.Sequence != 2 {
					t.Fatal("zero-debt unpublished root lost authority on second reopen")
				}

			} else {
				if b.physicalDebt != 1 || b.physicalRecovery == nil || b.position.Sequence != 1 {
					t.Fatal("pre-native outstanding terminal was guessed away", b.physicalDebt, b.position)
				}
				// The actual empty authenticated native Ledger proves this
				// prepared pin never acquired a lease; recovery can remove it.
				nativePath := filepath.Join(t.TempDir(), "real-ledger.wal")
				key := bytes.Repeat([]byte{71}, 32)
				native, err := ledger.OpenWithOptions(nativePath, ledger.NewStatus(), ledger.Options{ChainKey: key})
				if err != nil {
					t.Fatal(err)
				}
				defer native.Close()
				report, partial, err := ledger.InspectReplayAuthenticated(nativePath, key, nil)
				if err != nil || partial {
					t.Fatal(err, partial)
				}
				projection, err := ledger.BuildLedgerProjection(context.Background(), nativePath, key, filepath.Join(t.TempDir(), "real-projection.db"), report)
				if err != nil {
					t.Fatal(err)
				}
				defer projection.Close()
				ref, err := b.SealReference(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				backend, err := b.StandaloneFacade(context.Background(), b.budget, b.evidence.Identity, b.evidence.InstanceID, b.evidence.KeyGeneration, ref)
				if err != nil {
					t.Fatal(err)
				}
				dao, err := boltstore.NewFacadeStore(context.Background(), backend)
				if err != nil {
					t.Fatal(err)
				}
				if err = dao.RecoverDeploymentPricePins(context.Background(), projection); err != nil {
					t.Fatal("real source pin reconciliation", err)
				}
				if !b.physicalPinRecoveryVerified || b.physicalDebt != 0 {
					t.Fatal("recovery not joined")
				}
				publishMetadataPhysicalActual(t, b, publisher)
				if err = b.RetirePhysicalPreparationAfterPinRecovery(context.Background()); err != nil {
					t.Fatal(err)
				}
				if err = b.BindPhysicalCommonSelection(context.Background()); err != nil {
					t.Fatal(err)
				}
				// Authorize the next open from the actual independently MAC-
				// authenticated journal head, never from the new root header.
				input.Authority.Positions[1] = metadataSealPosition(b.position)
				closeOwner()
				reopened, _, closeReopened := openMetadataPhysicalCrash(t, input)
				defer closeReopened()
				if err = reopened.View(context.Background(), func(tx *Transaction) error {
					_, found, e := tx.Get([]string{"deployment_price_pin_intents"}, []byte("att_terminal"))
					if found {
						return ErrSource
					}
					return e
				}); err != nil {
					t.Fatal("post-common pin recovery reopen", err)
				}

			}
		})
	}
}

func publishMetadataPhysicalActual(t *testing.T, b *Baseline, p *authenticatedstate.SealCandidatePublisher) {
	t.Helper()
	ctx := context.Background()
	ref, err := b.SealReference(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seal, found, err := p.Current()
	if err != nil || !found {
		t.Fatal(err)
	}
	seal.Revision++
	for i := 0; i < int(seal.RootCount); i++ {
		if seal.Roots[i].Component == authenticatedstate.SealComponentMetadata {
			seal.Roots[i] = ref.Root
		}
	}
	for i := 0; i < int(seal.PositionCount); i++ {
		if seal.Positions[i].Component == authenticatedstate.SealComponentMetadata {
			seal.Positions[i] = ref.Position
		}
	}
	if _, err = p.Publish(ctx, seal); err != nil {
		t.Fatal("actual current Metadata publication", err)
	}
}

func TestMetadataPhysicalCompanionDebtRetainsOriginalTargetsAndFees(t *testing.T) {
	if os.Getenv("HALRO_EXT4_ROOT") == "" {
		t.Skip("actual ext4 required")
	}
	f := selectedMetadataNativeCountRetirement(t, 0, true)
	*f.rootReplaced = true
	if err := f.baseline.Close(); err != nil {
		t.Fatal(err)
	}
	rootFee, journalFee, err := f.baseline.DiskReservations()
	if err != nil {
		t.Fatal(err)
	}
	defer rootFee.Release()
	defer journalFee.Release()
	o := SelectedOpenOptions{PhysicalTerminalReserve: true, BaselineOptions: baselineOptionsForSelectedFixture(f), Evidence: f.baseline.evidence, Binding: f.options.Binding, Publisher: f.options.Publisher, Guard: f.options.Guard, Authority: f.options.Authority, RootDiskReservation: rootFee, JournalDiskReservation: journalFee, MaxJournalBytes: 16 << 20}
	b, _, err := OpenSelectedFromRoot(context.Background(), bytes.Repeat([]byte{51}, 32), f.baseline.output, f.path, o)
	if err != nil {
		t.Fatal(err)
	}
	if err = b.AuditSelectedRootStartup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = b.EnableQualifiedPhysicalTerminals(context.Background(), f.options.Publisher, f.options.Guard, f.options.Authority); err != nil {
		t.Fatal(err)
	}
	w, err := b.ReservePhysicalTerminals(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	before := b.budget.Snapshot().Used
	err = w.RetainForRecovery(authenticatedstate.ErrRecovery)
	var debt *PhysicalRetirementFailure
	if !errors.As(err, &debt) || debt.Owner != b || w.remaining != 1 || b.physicalDebt != 1 || b.ready.Load() {
		t.Fatal("companion debt lost", err)
	}
	if b.budget.Snapshot().Used != before {
		t.Fatal("hold refunded fees")
	}
	if !errors.Is(w.Close(), ownedfile.ErrRetirement) || b.budget.Snapshot().Used != before {
		t.Fatal("window Close deleted targets or refunded fees")
	}
	if _, err = b.Update(context.Background(), func(*Transaction) error { t.Fatal("retained owner callback entered"); return nil }); !errors.Is(err, authenticatedstate.ErrClosed) {
		t.Fatal(err)
	}
	err = b.Close()
	if !errors.As(err, &debt) || debt.Owner != b || !errors.Is(err, ownedfile.ErrRetirement) {
		t.Fatal("joined close lost owner", err)
	}
	if b.Close() != err || b.budget.Snapshot().Used.RAM < before.RAM {
		t.Fatal("repeat Close erased debt")
	}
	if _, err = os.Stat(f.path); err != nil {
		t.Fatal("native removed", err)
	}
}
