package ledger

import (
	"bytes"
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

type selectedStartupFixture struct {
	native               *Log
	path                 string
	o                    SelectedSealOpenOptions
	creator              *AuthenticatedDirectory
	expectDebt           bool
	expectNativeJoinDebt bool
	ledgerOwner          *AuthenticatedLedgerProjection
	ledgerDirectory      string
	ledgerOptions        AuthenticatedLedgerOpenOptions
	publisherCredit      resourcebudget.Permit
}

func newSelectedStartupFixture(t *testing.T, sealed bool) *selectedStartupFixture {
	return newSelectedStartupCompressedFixture(t, sealed, false)
}

func newSelectedStartupCompressedFixture(t *testing.T, sealed, compressed bool) *selectedStartupFixture {
	return newSelectedStartupGenerationsFixture(t, sealed, compressed, 0)
}

func newSelectedStartupGenerationsFixture(t *testing.T, sealed, compressed bool, extra int, physicalBudget ...*resourcebudget.Manager) *selectedStartupFixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.wal")
	native, err := OpenWithOptions(path, NewStatus(), Options{ChainKey: testChainKey})
	if err != nil {
		t.Fatal(err)
	}
	for i, e := range append(projectionTestAttribution(), projectionTestLifecycle(1, true)...) {
		if _, err := native.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
		if sealed && i == 2 {
			if _, err := native.Roll(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if compressed {
		if _, err := native.Compact(1); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < extra; i++ {
		if _, err := native.Append(ctx, projectionTestLifecycle(i+2, true)[0]); err != nil {
			t.Fatal(err)
		}
		if _, err := native.Roll(); err != nil {
			t.Fatal(err)
		}
	}
	report, partial, err := InspectReplayAuthenticated(path, testChainKey, nil)
	if err != nil || partial {
		t.Fatal(partial, err)
	}
	opts := authenticatedLedgerTestOptions(t)
	if len(physicalBudget) == 1 {
		opts.Budget = physicalBudget[0]
	}
	ledgerDirectory := filepath.Join(t.TempDir(), "projection")
	owner, err := BuildAuthenticatedLedgerProjection(ctx, path, testChainKey, ledgerDirectory, report, opts)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := native.BeginMaintenance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	do := AuthenticatedDirectoryOptions{Identity: opts.Identity, Budget: opts.Budget, MaxDiskBytes: 16 << 20, MaxAppendBytes: 4 << 20}
	directory := filepath.Join(t.TempDir(), "directory")
	creator, err := BuildAuthenticatedDirectory(ctx, native, lease, owner, directory, do)
	if err != nil {
		t.Fatal(err)
	}
	dr, err := creator.SealReference(ctx)
	if err != nil {
		t.Fatal(err)
	}
	lr, err := owner.SealReference(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := creator.Close(); err != nil {
		t.Fatal(err)
	}
	credit, err := creator.DiskReservation()
	if err != nil {
		t.Fatal(err)
	}
	do.DiskReservation = credit
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	ledgerCredit, err := owner.DiskReservation()
	if err != nil {
		t.Fatal(err)
	}
	ledgerAnchor := lr.Anchor
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	guard, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	seal := authenticatedstate.CandidateSeal{SchemaVersion: 1, ApplyVersion: 1, IndexVersion: 1, KeyDerivation: 1, Instance: [32]byte{1}, Incarnation: [16]byte{1}, Generation: 1, Revision: 1, Role: authenticatedstate.SealStandalone, Coverage: authenticatedstate.SealCoverageAuthenticated, MaxPageBytes: 4 << 20, MaxValueBytes: 3 << 20, MaxTreeHeight: 32, PositionCount: 5, RootCount: 2}
	seal.Positions[0] = dr.Position
	seal.Positions[1] = authenticatedstate.SealPosition{Component: authenticatedstate.SealComponentMetadata, Epoch: 1, Revision: 1, MetadataBaseline: [32]byte{2}}
	seal.Positions[2].Component = authenticatedstate.SealComponentGovernance
	seal.Positions[3].Component = authenticatedstate.SealComponentAudit
	seal.Positions[4].Component = authenticatedstate.SealComponentUsage
	seal.Roots[0], seal.Roots[1] = lr.Root, dr.Root
	hard := authenticatedstate.SealHardLimits{MaxPageBytes: seal.MaxPageBytes, MaxValueBytes: seal.MaxValueBytes, MaxTreeHeight: 32, RequiredRoots: []authenticatedstate.SealRootSchema{lr.Schema, dr.Schema}}
	fee, err := opts.Budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: 32 << 10})
	if err != nil {
		t.Fatal(err)
	}
	pub, err := authenticatedstate.OpenSealCandidatePublisher(dir, guard, testChainKey, hard, opts.Budget, fee)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pub.Publish(ctx, seal); err != nil {
		t.Fatal(err)
	}
	authority := authenticatedstate.SealEligibility{SchemaVersion: 1, ApplyVersion: 1, IndexVersion: 1, KeyDerivation: 1, Instance: seal.Instance, Incarnation: seal.Incarnation, Role: seal.Role, Coverage: seal.Coverage}
	copy(authority.Positions[:], seal.Positions[:])
	f := &selectedStartupFixture{native: native, path: path, creator: creator, ledgerOwner: owner, ledgerDirectory: ledgerDirectory, ledgerOptions: AuthenticatedLedgerOpenOptions{AuthenticatedLedgerOptions: opts, SavedAnchor: ledgerAnchor, DiskReservation: ledgerCredit}, o: SelectedSealOpenOptions{Publisher: pub, Guard: guard, Authority: authority, Directory: directory, DirectoryOptions: do}}
	t.Cleanup(func() {
		if err := f.native.Close(); err != nil && !((f.expectDebt || f.expectNativeJoinDebt) && errors.Is(err, ownedfile.ErrRetirement)) {
			t.Error(err)
		}
		if err := owner.Discard(); err != nil {
			t.Error(err)
		}
		if err := ledgerCredit.Release(); err != nil {
			t.Error(err)
		}
		if err := creator.Discard(); err != nil {
			t.Error(err)
		}
		if err := credit.Release(); err != nil {
			t.Error(err)
		}
		if err := f.o.Publisher.Close(); err != nil {
			t.Error(err)
		}
		if err := f.o.Publisher.Retire(); err != nil {
			t.Error(err)
		}
		if f.publisherCredit != (resourcebudget.Permit{}) {
			if err := f.publisherCredit.Release(); err != nil {
				t.Error(err)
			}
		}
		if err := fee.Release(); err != nil {
			t.Error(err)
		}
		if err := f.o.Guard.Close(); err != nil {
			t.Error(err)
		}
		if got := opts.Budget.Snapshot().Used; (!f.expectDebt && got != (resourcebudget.Cost{})) || (f.expectDebt && got.RAM < AuthenticatedLedgerSourceRAMBytes) {
			t.Error("startup leaked resources", got)
		}
	})
	return f
}

func (f *selectedStartupFixture) reopen(t *testing.T) NativeStartupReceipt {
	t.Helper()
	if err := f.native.Close(); err != nil {
		t.Fatal(err)
	}
	native, receipt, err := OpenWithSelectedSeal(context.Background(), f.path, NewStatus(), Options{ChainKey: testChainKey}, f.o)
	if err != nil {
		t.Fatal(err)
	}
	f.native = native
	return receipt
}

func TestSelectedNativeStartupReadsOnlyDeltaAndAuditGatesWrites(t *testing.T) {
	f := newSelectedStartupFixture(t, true)
	if _, err := f.native.Append(context.Background(), projectionTestLifecycle(2, true)[0]); err != nil {
		t.Fatal(err)
	}
	r := f.reopen(t)
	if r.DecodedRecords != 1 || !r.HistoricalAuditPending || f.native.SelectedStartupReady() {
		t.Fatal(r)
	}
	if _, err := f.native.Append(context.Background(), projectionTestLifecycle(3, true)[0]); !errors.Is(err, ErrStartupAuditPending) {
		t.Fatal(err)
	}
	if _, err := f.native.Roll(); !errors.Is(err, ErrStartupAuditPending) {
		t.Fatal(err)
	}
	if err := f.native.AuditSelectedStartup(context.Background(), authenticatedLedgerTestOptions(t).Budget); !errors.Is(err, ErrProjectionSource) {
		t.Fatal("foreign budget", err)
	}
	if err := f.native.AuditSelectedStartup(context.Background(), f.o.DirectoryOptions.Budget); err != nil {
		t.Fatal(err)
	}
	if !f.native.SelectedStartupReady() {
		t.Fatal("audit did not clear only the startup gate")
	}
	if _, err := f.native.Append(context.Background(), projectionTestLifecycle(3, true)[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := f.native.Roll(); err != nil {
		t.Fatal(err)
	}
	if _, partial, err := InspectReplayAuthenticated(f.path, testChainKey, nil); err != nil || partial {
		t.Fatal(partial, err)
	}
}

func TestSelectedNativeStartupOldByteTamperKeepsReadinessClosed(t *testing.T) {
	f := newSelectedStartupFixture(t, false)
	if err := f.native.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	raw[frameHeaderSize+chainPreviousHashSize+chainMACSize] ^= 1
	if err := os.WriteFile(f.path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	native, receipt, err := OpenWithSelectedSeal(context.Background(), f.path, NewStatus(), Options{ChainKey: testChainKey}, f.o)
	if err != nil {
		t.Fatal("old bytes must be excluded from initialization and gated for audit", err)
	}
	f.native = native
	if receipt.DecodedRecords != 0 || native.SelectedStartupReady() {
		t.Fatal(receipt)
	}
	if err := native.AuditSelectedStartup(context.Background(), f.o.DirectoryOptions.Budget); err == nil {
		t.Fatal("tamper accepted")
	}
	if native.SelectedStartupReady() || native.Status().Load() != AccountingRecoveryRequired {
		t.Fatal("audit did not withdraw native source")
	}
}

func TestSelectedNativeStartupRejectsUnselectedAuthorityBeforeTailRepair(t *testing.T) {
	f := newSelectedStartupFixture(t, false)
	if err := f.native.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(f.path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("torn")); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	want, err := os.Stat(f.path)
	if err != nil {
		t.Fatal(err)
	}
	o := f.o
	o.Authority.Positions[0].Sequence++
	if n, _, err := OpenWithSelectedSeal(context.Background(), f.path, NewStatus(), Options{ChainKey: testChainKey}, o); n != nil || !errors.Is(err, authenticatedstate.ErrSealIneligible) {
		t.Fatal(n, err)
	}
	got, err := os.Stat(f.path)
	if err != nil || got.Size() != want.Size() {
		t.Fatal("unselected cut repaired native tail", got, err)
	}
}

func TestSelectedNativeStartupAuthenticatesPostSealRollSuffix(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		t.Run(fmt.Sprint(compressed), func(t *testing.T) {
			f := newSelectedStartupCompressedFixture(t, true, compressed)
			for i := 2; i < 5; i++ {
				if _, err := f.native.Append(context.Background(), projectionTestLifecycle(i, true)[0]); err != nil {
					t.Fatal(err)
				}
				if _, err := f.native.Roll(); err != nil {
					t.Fatal(err)
				}
			}
			if compressed {
				if _, err := f.native.Compact(1); err != nil {
					t.Fatal(err)
				}
			}
			r := f.reopen(t)
			if r.DecodedRecords != 3 || r.Head.Head.Generation != r.Saved.Head.Generation+3 || f.native.SelectedStartupReady() {
				t.Fatal(r)
			}
			if err := f.native.AuditSelectedStartup(context.Background(), f.o.DirectoryOptions.Budget); err != nil {
				t.Fatal(err)
			}
			if !f.native.SelectedStartupReady() {
				t.Fatal("completed Roll never became ready after actual audit")
			}
		})
	}
}

func TestSelectedNativeStartupRefusesChangedDirectoryAndMissingHistory(t *testing.T) {
	for _, mode := range []string{"compressed-descriptor", "missing-native-file"} {
		t.Run(mode, func(t *testing.T) {
			f := newSelectedStartupFixture(t, true)
			if mode == "compressed-descriptor" {
				if _, err := f.native.Compact(1); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.native.Close(); err != nil {
				t.Fatal(err)
			}
			if mode == "compressed-descriptor" {
				// Physical compression alone is now qualified by the signed
				// plaintext SHA. A changed logical sequence must still refuse.
				m, err := loadSegmentManifest(filepath.Dir(f.path))
				if err != nil {
					t.Fatal(err)
				}
				m.Segments[0].FirstSequence++
				if err := saveSegmentManifest(filepath.Dir(f.path), m); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "missing-native-file" {
				path := filepath.Join(filepath.Dir(f.path), "ledger-1.wal")
				if err := os.Rename(path, path+".saved"); err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := os.Rename(path+".saved", path); err != nil {
						t.Error(err)
					}
				}()
			}
			n, _, err := OpenWithSelectedSeal(context.Background(), f.path, NewStatus(), Options{ChainKey: testChainKey}, f.o)
			if n != nil || err == nil {
				t.Fatal("changed directory authorized startup", n, err)
			}
		})
	}
}

func TestSelectedNativeStartupAuthenticatesLaterFactsBeforeTailRepair(t *testing.T) {
	f := newSelectedStartupFixture(t, false)
	if _, err := f.native.Append(context.Background(), projectionTestLifecycle(2, true)[0]); err != nil {
		t.Fatal(err)
	}
	if err := f.native.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 1
	if err := os.WriteFile(f.path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	n, _, err := OpenWithSelectedSeal(context.Background(), f.path, NewStatus(), Options{ChainKey: testChainKey}, f.o)
	if n != nil || err == nil {
		t.Fatal("later corrupt native fact was accepted", n, err)
	}
	got, err := os.ReadFile(f.path)
	if err != nil || len(got) != len(raw) {
		t.Fatal("authenticated corruption was truncated", len(got), err)
	}
}

func TestSelectedNativeStartupCanceledAuditRetainsOnlyItsGate(t *testing.T) {
	f := newSelectedStartupFixture(t, false)
	f.reopen(t)
	before := f.o.DirectoryOptions.Budget.Snapshot().Used
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.native.AuditSelectedStartup(ctx, f.o.DirectoryOptions.Budget); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if f.native.SelectedStartupReady() || f.native.Status().Load() != AccountingHealthy {
		t.Fatal("caller cancellation changed native health or opened the startup gate")
	}
	if got := f.o.DirectoryOptions.Budget.Snapshot().Used; got != before {
		t.Fatal("canceled audit retained workspace", before, got)
	}
	if err := f.native.AuditSelectedStartup(context.Background(), f.o.DirectoryOptions.Budget); err != nil {
		t.Fatal(err)
	}
}

func TestSelectedNativeStartupUncertainActualCloseKeepsOwnerAndJobCharge(t *testing.T) {
	opts := authenticatedLedgerTestOptions(t)
	credit, err := opts.Budget.TryAcquire(resourcebudget.IO, resourcebudget.Cost{RAM: AuthenticatedLedgerSourceRAMBytes})
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.CreateTemp(t.TempDir(), "source")
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	selected := &selectedNativeStartup{}
	retireSelectedStartupFile(file, selected)
	if !errors.Is(selected.retirementErr, ownedfile.ErrRetirement) || selected.retainedFile != file {
		t.Fatal("actual Close failure lost physical owner", selected.retirementErr)
	}
	var result *Log
	retainSelectedStartupCleanup(&result, nil, nil, credit, selected.retirementErr)
	result.startupCleanup.nativeFile = selected.retainedFile
	if result.SelectedStartupReady() || !errors.Is(result.Close(), ownedfile.ErrRetirement) || !errors.Is(result.Close(), ownedfile.ErrRetirement) {
		t.Fatal("unknown actual exit was made ready or reclassified")
	}
	if result.startupCleanup.nativeFile != file || opts.Budget.Snapshot().Used.RAM < AuthenticatedLedgerSourceRAMBytes {
		t.Fatal("uncertain exit lost actual resource or whole-job charge")
	}
}

func TestSelectedNativeRolledSuffixRefusesDamageBeforeActiveTailRepair(t *testing.T) {
	f := newSelectedStartupFixture(t, true)
	if _, err := f.native.Append(context.Background(), projectionTestLifecycle(2, true)[0]); err != nil {
		t.Fatal(err)
	}
	rolled, err := f.native.Roll()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.native.Append(context.Background(), projectionTestLifecycle(3, true)[0]); err != nil {
		t.Fatal(err)
	}
	if err := f.native.Close(); err != nil {
		t.Fatal(err)
	}
	sealedPath := filepath.Join(filepath.Dir(f.path), rolled.Sealed.File)
	sealed, err := os.ReadFile(sealedPath)
	if err != nil {
		t.Fatal(err)
	}
	sealed[len(sealed)-1] ^= 1
	if err := os.WriteFile(sealedPath, sealed, 0600); err != nil {
		t.Fatal(err)
	}
	active, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	active = append(active, 1, 2, 3)
	if err := os.WriteFile(f.path, active, 0600); err != nil {
		t.Fatal(err)
	}
	n, _, err := OpenWithSelectedSeal(context.Background(), f.path, NewStatus(), Options{ChainKey: testChainKey}, f.o)
	if n != nil || err == nil {
		t.Fatal("damaged future generation acquired selected owner", n, err)
	}
	got, readErr := os.ReadFile(f.path)
	if readErr != nil || !bytes.Equal(got, active) {
		t.Fatal("unqualified suffix authorized native tail repair", readErr)
	}
	got, readErr = os.ReadFile(sealedPath)
	if readErr != nil || !bytes.Equal(got, sealed) {
		t.Fatal("damaged authority was modified", readErr)
	}
}

func TestSelectedDirectoryCompletedRollDescriptorsUseBoundedPages(t *testing.T) {
	assertSelectedDirectoryRolledPages(t, false)
}
func TestSelectedDirectoryCompletedRollResumesUnpublishedFirstPage(t *testing.T) {
	assertSelectedDirectoryRolledPages(t, true)
}
func assertSelectedDirectoryRolledPages(t *testing.T, interrupted bool) {
	t.Helper()
	ctx := context.Background()
	f := newSelectedStartupFixture(t, true)
	for i := 2; i < 72; i++ {
		if _, err := f.native.Append(ctx, projectionTestLifecycle(i, true)[0]); err != nil {
			t.Fatal(err)
		}
		if _, err := f.native.Roll(); err != nil {
			t.Fatal(err)
		}
	}
	receipt := f.reopen(t)
	if receipt.DecodedRecords != 70 {
		t.Fatal(receipt)
	}
	if err := f.native.AuditSelectedStartup(ctx, f.o.DirectoryOptions.Budget); err != nil {
		t.Fatal(err)
	}
	seal, found, err := f.o.Publisher.Current()
	if err != nil || !found {
		t.Fatal(found, err)
	}
	ref := AuthenticatedDirectoryReference{Schema: authenticatedDirectorySchema, Root: seal.Roots[1], Position: seal.Positions[0]}
	selected := SelectedLedgerProjectionOpenOptions{Publisher: f.o.Publisher, Guard: f.o.Guard, Authority: f.o.Authority}
	if interrupted {
		pagesPath := filepath.Join(f.o.Directory, "pages")
		before, err := os.ReadFile(pagesPath)
		if err != nil {
			t.Fatal(err)
		}
		err = f.native.WithSelectedStartupOwner(ctx, f.o.DirectoryOptions.Budget, selected, f.o.DirectoryOptions.Identity, func(ctx context.Context, source SelectedStartupSource) error {
			scope := authenticatedDirectoryScope(f.o.DirectoryOptions.Identity)
			key, err := hkdf.Key(sha256.New, testChainKey, scope[:], "halro:ledger:authenticated-directory:hmac:v1", 32)
			if err != nil {
				return err
			}
			defer clear(key)
			schema := authenticatedDirectorySchema
			store, err := authenticatedstate.OpenStore(ctx, f.o.Directory, authenticatedstate.StoreOptions{Scope: scope, ExpectedCut: authenticatedDirectoryCut(scope, source.SavedAnchor()), Key: key, Budget: f.o.DirectoryOptions.Budget, MaxDiskBytes: f.o.DirectoryOptions.MaxDiskBytes, MaxAppendBytes: f.o.DirectoryOptions.MaxAppendBytes, DiskReservation: f.o.DirectoryOptions.DiskReservation, ExpectedSealRoot: &ref.Root, ExpectedSealSchema: &schema})
			if err != nil {
				return err
			}
			_, err = store.UpdateNativeFrame(ctx, authenticatedDirectoryCut(scope, source.NativeHead()), func(tx *authenticatedstate.Transaction) error {
				for g := source.SavedAnchor().Head.Generation; g < source.SavedAnchor().Head.Generation+63; g++ {
					segment, found, err := f.native.lookupSegmentLocked(g)
					if err != nil || !found {
						return errors.Join(ErrSegmentMissing, err)
					}
					raw, err := json.Marshal(segment)
					if err != nil {
						return err
					}
					if err := tx.Put(authenticatedDirectoryKey(g), raw); err != nil {
						return err
					}
				}
				return nil
			}, func() error { return source.Check(ctx) })
			return errors.Join(err, store.Close())
		})
		if err != nil {
			t.Fatal("actual durable unpublished first page", err)
		}
		after, err := os.ReadFile(pagesPath)
		if err != nil || len(after) <= len(before) || !bytes.Equal(before, after[:len(before)]) {
			t.Fatal("unpublished private commit overwrote original selected pack", err)
		}
		current, found, err := f.o.Publisher.Current()
		if err != nil || !found || current != seal {
			t.Fatal("unpublished descriptor page changed common authority", err)
		}
	}
	directory, err := OpenSelectedAuthenticatedDirectory(ctx, f.native, ref, f.o.Directory, f.o.DirectoryOptions, selected)
	if err != nil {
		t.Fatal("70-roll selected directory", err)
	}
	defer func() {
		if err := directory.Close(); err != nil {
			t.Error(err)
		}
		if err := f.creator.AdoptClosedSuccessor(directory); err != nil {
			t.Error(err)
		}
	}()
	current, err := directory.SealReference(ctx)
	if err != nil || current.Root.Rows != 72 || current.Position.Generation != 72 || current.Root.State.Revision != ref.Root.State.Revision+3+map[bool]uint64{false: 0, true: 1}[interrupted] {
		t.Fatal("directory did not commit two finite descriptor pages and final state", current, err)
	}
}

func TestSelectedNativeRolledDescriptorRejectsUnqualifiedAttributionAndChecksum(t *testing.T) {
	for _, mode := range []string{"first-sequence", "start-hash", "plain-checksum", "stored-checksum", "gzip-trailer"} {
		t.Run(mode, func(t *testing.T) {
			f := newSelectedStartupCompressedFixture(t, true, true)
			if _, err := f.native.Append(context.Background(), projectionTestLifecycle(2, true)[0]); err != nil {
				t.Fatal(err)
			}
			rolled, err := f.native.Roll()
			if err != nil {
				t.Fatal(err)
			}
			if mode == "gzip-trailer" {
				if _, err := f.native.Compact(rolled.Sealed.Generation); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.native.Close(); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Dir(f.path)
			m, err := loadSegmentManifest(dir)
			if err != nil {
				t.Fatal(err)
			}
			segment := &m.Segments[len(m.Segments)-1]
			switch mode {
			case "first-sequence":
				segment.FirstSequence++
			case "start-hash":
				segment.StartHash = strings.Repeat("f", 64)
			case "plain-checksum":
				segment.PlainChecksum = strings.Repeat("f", 64)
			case "stored-checksum":
				segment.StoredChecksum = strings.Repeat("f", 64)
			case "gzip-trailer":
				path := filepath.Join(dir, segment.File)
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				data[len(data)-1] ^= 1
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				sum := sha256.Sum256(data)
				segment.StoredChecksum = hex.EncodeToString(sum[:]) // raw digest alone cannot credit a corrupt gzip trailer
			}
			if err := saveSegmentManifest(dir, m); err != nil {
				t.Fatal(err)
			}
			partial := []byte{1, 2, 3}
			if err := os.WriteFile(f.path, partial, 0600); err != nil {
				t.Fatal(err)
			}
			n, _, err := OpenWithSelectedSeal(context.Background(), f.path, NewStatus(), Options{ChainKey: testChainKey}, f.o)
			if n != nil || err == nil {
				t.Fatal("unqualified descriptor acquired a selected owner", n, err)
			}
			got, err := os.ReadFile(f.path)
			if err != nil || !bytes.Equal(got, partial) {
				t.Fatal("bad descriptor authorized tail repair", err)
			}
		})
	}
}
