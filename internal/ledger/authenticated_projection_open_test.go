package ledger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

type authenticatedReopenFixture struct {
	owner     *AuthenticatedLedgerProjection
	native    *Log
	directory string
	options   AuthenticatedLedgerOpenOptions
	lease     *MaintenanceLease
	records   []Record
}

func newAuthenticatedReopenFixture(t *testing.T) *authenticatedReopenFixture {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger.wal")
	native := openChained(t, path, nil)
	var records []Record
	for _, event := range append(projectionTestAttribution(), projectionTestLifecycle(1, true)[0]) {
		r, _, err := native.AppendRecordWithReceipt(ctx, event)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, r)
	}
	report, partial, err := InspectReplayAuthenticated(path, testChainKey, nil)
	if err != nil || partial {
		t.Fatal(partial, err)
	}
	o := authenticatedLedgerTestOptions(t)
	directory := filepath.Join(t.TempDir(), "auth")
	p, err := BuildAuthenticatedLedgerProjection(ctx, path, testChainKey, directory, report, o)
	if err != nil {
		t.Fatal(err)
	}
	anchor, err := p.ReadReplayAnchor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	credit, err := p.DiskReservation()
	if err != nil {
		t.Fatal(err)
	}
	cut := ProjectionPrefix{Head: anchor.Head, Hash: anchor.Hash}
	o.Prefix = &cut
	f := &authenticatedReopenFixture{owner: p, native: native, directory: directory, records: records, options: AuthenticatedLedgerOpenOptions{AuthenticatedLedgerOptions: o, SavedAnchor: anchor, DiskReservation: credit}}
	t.Cleanup(func() {
		if f.lease != nil {
			f.lease.Close()
		}
		if err := native.Close(); err != nil {
			t.Error(err)
		}
		if err := p.Discard(); err != nil {
			t.Error(err)
		}
		if err := credit.Release(); err != nil {
			t.Error(err)
		}
		if got := o.Budget.Snapshot().Used; got != (resourcebudget.Cost{}) {
			t.Error("persistent owner/IO leaked", got)
		}
	})
	return f
}
func (f *authenticatedReopenFixture) maintain(t *testing.T) {
	t.Helper()
	lease, err := f.native.BeginMaintenance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.lease = lease
}

func TestAuthenticatedLedgerSealReferenceBindsRealRootAndNativePosition(t *testing.T) {
	f := newAuthenticatedReopenFixture(t)
	f.maintain(t)
	p, _, err := OpenAuthenticatedLedgerProjection(context.Background(), f.native, f.directory, f.options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Error(err)
		}
	})
	reference, err := p.SealReference(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	anchor, err := p.ReadReplayAnchor(context.Background())
	if err != nil || reference.Anchor != anchor {
		t.Fatalf("Seal anchor differs from live projection: %#v, %#v, %v", reference.Anchor, anchor, err)
	}
	wantSchema := authenticatedstate.SealRootSchema{Kind: authenticatedstate.SealRootState, Component: authenticatedstate.SealComponentLedger, IndexID: AuthenticatedLedgerSealIndexID, Codec: AuthenticatedLedgerSealCodec}
	if reference.Schema != wantSchema || reference.Scope != p.scope || reference.Root.Component != wantSchema.Component || reference.Root.IndexID != wantSchema.IndexID || reference.Root.Codec != wantSchema.Codec || reference.Root.Rows == 0 || reference.Root.Empty || reference.Root.FileID == ([16]byte{}) || reference.Root.Length == 0 || reference.Root.Digest == ([32]byte{}) {
		t.Fatalf("invalid complete Ledger Seal reference: %#v", reference)
	}
	seal := authenticatedstate.CandidateSeal{
		SchemaVersion: 1, ApplyVersion: 1, IndexVersion: 1, KeyDerivation: 1,
		Instance: [32]byte{1}, Incarnation: [16]byte{1}, Generation: 1, Revision: 1,
		Role: authenticatedstate.SealStandalone, Coverage: authenticatedstate.SealCoverageAuthenticated,
		MaxPageBytes: 512 << 10, MaxValueBytes: 3 << 20, MaxTreeHeight: 32,
		PositionCount: 5, RootCount: 1,
	}
	seal.Positions[0] = authenticatedstate.SealPosition{Component: authenticatedstate.SealComponentLedger, Epoch: uint64(anchor.Epoch), Generation: anchor.Head.Generation, Sequence: anchor.Head.Sequence, Offset: uint64(anchor.Head.Offset), Hash: anchor.Hash}
	seal.Positions[1] = authenticatedstate.SealPosition{Component: authenticatedstate.SealComponentMetadata, Epoch: 1, Revision: 1, MetadataBaseline: [32]byte{2}}
	seal.Positions[2].Component = authenticatedstate.SealComponentGovernance
	seal.Positions[3].Component = authenticatedstate.SealComponentAudit
	seal.Positions[4].Component = authenticatedstate.SealComponentUsage
	seal.Roots[0] = reference.Root
	hard := authenticatedstate.SealHardLimits{MaxPageBytes: seal.MaxPageBytes, MaxValueBytes: seal.MaxValueBytes, MaxTreeHeight: seal.MaxTreeHeight, RequiredRoots: []authenticatedstate.SealRootSchema{reference.Schema}}
	raw, err := authenticatedstate.EncodeSealCandidate(seal, bytes.Repeat([]byte{0x61}, 32), hard)
	if err != nil {
		t.Fatalf("actual Ledger root could not enter candidate Seal envelope: %v", err)
	}
	decoded, err := authenticatedstate.DecodeSealCandidate(raw, bytes.Repeat([]byte{0x61}, 32), hard)
	if err != nil || decoded.Roots[0] != reference.Root || decoded.Positions[0].Sequence != anchor.Head.Sequence || decoded.Positions[0].Hash != anchor.Hash {
		t.Fatalf("candidate did not preserve actual Ledger root/cut: %#v, %v", decoded, err)
	}
}

func TestAuthenticatedLedgerReopenRequiresSelectedSealRootBeforeOrphanRepair(t *testing.T) {
	f := newAuthenticatedReopenFixture(t)
	f.maintain(t)
	probe, _, err := OpenAuthenticatedLedgerProjection(context.Background(), f.native, f.directory, f.options)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := probe.SealReference(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(f.directory, "pages")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	orphan := append(bytes.Clone(original), []byte("unpublished-orphan-pages")...)
	if err := os.WriteFile(path, orphan, 0600); err != nil {
		t.Fatal(err)
	}
	defer os.WriteFile(path, original, 0600)
	baseline := f.options.Budget.Snapshot().Used

	wrong := selected.Root
	wrong.Digest[0] ^= 1
	f.options.SelectedSealRoot = &wrong
	if reopened, _, err := OpenAuthenticatedLedgerProjection(context.Background(), f.native, f.directory, f.options); !errors.Is(err, authenticatedstate.ErrRecovery) || reopened != nil {
		t.Fatalf("mismatched selected root was accepted: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, orphan) || f.options.Budget.Snapshot().Used != baseline {
		t.Fatalf("selected-root refusal repaired/refunded: err=%v usage=%+v", err, f.options.Budget.Snapshot().Used)
	}

	f.options.SelectedSealRoot = &selected.Root
	reopened, receipt, err := OpenAuthenticatedLedgerProjection(context.Background(), f.native, f.directory, f.options)
	if err != nil || receipt.AppliedRecords != 0 {
		t.Fatalf("matching selected root failed reopen: receipt=%+v err=%v", receipt, err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.owner.AdoptClosedSuccessor(reopened); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(path)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatalf("matching selected root did not allow qualified orphan repair: %v", err)
	}
}

func TestAuthenticatedLedgerReopenUsesActualSavedRowsAndOnlyPermittedDelta(t *testing.T) {
	f := newAuthenticatedReopenFixture(t)
	f.records = appendAuthenticatedTail(t, f.native, f.records)
	roll, err := f.native.Roll()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.native.Compact(roll.Sealed.Generation); err != nil {
		t.Fatal(err)
	}
	for _, event := range projectionTestLifecycle(2, true) {
		r, _, err := f.native.AppendRecordWithReceipt(context.Background(), event)
		if err != nil {
			t.Fatal(err)
		}
		f.records = append(f.records, r)
	}
	if _, err := f.native.Roll(); err != nil {
		t.Fatal(err)
	}
	f.maintain(t)
	cuts := []ProjectionPrefix{nativeRecordPrefix(f.records[3]), {Head: Watermark{Generation: 2, Sequence: 7}, Hash: f.records[6].Hash}, {Head: Watermark{Generation: 3, Sequence: 12}, Hash: f.records[11].Hash}}
	baseline := f.options.Budget.Snapshot().Used
	for _, cut := range cuts {
		f.options.Prefix = &cut
		p, receipt, err := OpenAuthenticatedLedgerProjection(context.Background(), f.native, f.directory, f.options)
		if err != nil {
			t.Fatal(err)
		}
		if receipt.From != f.options.SavedAnchor || receipt.To.Head != cut.Head || receipt.To.Hash != cut.Hash || receipt.NativeHead.Head != cuts[2].Head || receipt.SourceVerified != 12 || receipt.DeltaVerified != 12-receipt.From.Head.Sequence || receipt.AppliedRecords != cut.Head.Sequence-receipt.From.Head.Sequence {
			t.Fatal(receipt)
		}
		compareAdvancedCandidate(t, p, f.native, cut)
		if err := p.Discard(); !errors.Is(err, authenticatedstate.ErrLimit) {
			t.Fatal("borrowed reopen could delete publisher", err)
		}
		if err := p.Close(); err != nil {
			t.Fatal(err)
		}
		if err := f.owner.AdoptClosedSuccessor(p); err != nil {
			t.Fatal(err)
		}
		if got := f.options.Budget.Snapshot().Used; got != baseline {
			t.Fatal("closed reopen retained RAM/refunded disk", got, baseline)
		}
		f.options.SavedAnchor = receipt.To
		t.Logf("source decoded=%d delta decoded=%d applied=%d", receipt.SourceVerified, receipt.DeltaVerified, receipt.AppliedRecords)
	}
	p, receipt, err := OpenAuthenticatedLedgerProjection(context.Background(), f.native, f.directory, f.options)
	if err != nil || receipt.AppliedRecords != 0 || receipt.DeltaVerified != 0 || receipt.SourceVerified != 12 {
		t.Fatal(receipt, err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.owner.AdoptClosedSuccessor(p); err != nil {
		t.Fatal(err)
	}
}

func TestAuthenticatedLedgerReopenRefusesUnprovenCutsAndOwnersWithoutChangingFiles(t *testing.T) {
	for _, shape := range []string{"missing_permission", "saved_hash", "saved_epoch", "saved_offset", "permission_behind_saved", "future_permission", "permission_hash", "missing_disk", "foreign_identity", "foreign_budget", "missing_maintenance", "busy_lease", "canceled"} {
		t.Run(shape, func(t *testing.T) {
			f := newAuthenticatedReopenFixture(t)
			f.maintain(t)
			o := f.options
			cut := *o.Prefix
			o.Prefix = &cut
			ctx := context.Background()
			switch shape {
			case "missing_permission":
				o.Prefix = nil
			case "saved_hash":
				o.SavedAnchor.Hash[0] ^= 1
			case "saved_epoch":
				o.SavedAnchor.Epoch = 4
			case "saved_offset":
				o.SavedAnchor.Head.Offset--
			case "permission_behind_saved":
				cut = nativeRecordPrefix(f.records[1])
				o.Prefix = &cut
			case "future_permission":
				cut.Head.Offset++
				cut.Head.Sequence++
			case "permission_hash":
				cut.Hash[0] ^= 1
			case "missing_disk":
				o.DiskReservation = resourcebudget.Permit{}
			case "foreign_identity":
				o.Identity[0] ^= 1
			case "foreign_budget":
				o.Budget = authenticatedLedgerTestOptions(t).Budget
			case "missing_maintenance":
				f.lease.Close()
			case "busy_lease":
				f.lease.gate <- struct{}{}
				defer func() { <-f.lease.gate }()
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			root, _ := os.ReadFile(filepath.Join(f.directory, "active.root"))
			pack, _ := os.ReadFile(filepath.Join(f.directory, "pages"))
			baseline := f.options.Budget.Snapshot().Used
			p, receipt, err := OpenAuthenticatedLedgerProjection(ctx, f.native, f.directory, o)
			if err == nil || p != nil || receipt != (AuthenticatedLedgerReopen{}) {
				t.Fatal("unproven opener admitted", p, receipt, err)
			}
			afterRoot, _ := os.ReadFile(filepath.Join(f.directory, "active.root"))
			afterPack, _ := os.ReadFile(filepath.Join(f.directory, "pages"))
			if !bytes.Equal(root, afterRoot) || !bytes.Equal(pack, afterPack) || f.options.Budget.Snapshot().Used != baseline || f.native.status.Load() != AccountingHealthy {
				t.Fatal("refusal changed persistent bytes, fees or native health", err)
			}
		})
	}
}

func TestAuthenticatedLedgerReopenRejectsSavedRootAndPositionCorruption(t *testing.T) {
	for _, shape := range []string{"root_mac", "position_blob", "foreign_root"} {
		t.Run(shape, func(t *testing.T) {
			f := newAuthenticatedReopenFixture(t)
			f.maintain(t)
			file := "active.root"
			if shape == "position_blob" {
				file = "pages"
			}
			path := filepath.Join(f.directory, file)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Error(err)
				}
			}()
			changed := bytes.Clone(raw)
			index := len(changed) - 1
			if shape == "position_blob" {
				a := f.options.SavedAnchor
				position, _ := json.Marshal(projectionPosition{a.Head, a.Hash, a.Epoch})
				index = bytes.LastIndex(changed, position)
				if index < 0 {
					t.Fatal("actual position blob not located")
				}
			}
			if shape == "foreign_root" {
				report, partial, err := InspectReplayAuthenticated(f.native.path, testChainKey, nil)
				if err != nil || partial {
					t.Fatal(partial, err)
				}
				dir := filepath.Join(t.TempDir(), "foreign")
				foreign, err := BuildAuthenticatedLedgerProjection(context.Background(), f.native.path, testChainKey, dir, report, f.options.AuthenticatedLedgerOptions)
				if err != nil {
					t.Fatal(err)
				}
				defer foreign.Discard()
				if err := foreign.Close(); err != nil {
					t.Fatal(err)
				}
				changed, err = os.ReadFile(filepath.Join(dir, "active.root"))
				if err != nil {
					t.Fatal(err)
				}
			} else {
				changed[index] ^= 1
			}
			if err := os.WriteFile(path, changed, 0600); err != nil {
				t.Fatal(err)
			}
			baseline := f.options.Budget.Snapshot().Used
			p, receipt, err := OpenAuthenticatedLedgerProjection(context.Background(), f.native, f.directory, f.options)
			if p != nil || receipt != (AuthenticatedLedgerReopen{}) || !errors.Is(err, authenticatedstate.ErrCorrupt) {
				t.Fatal(p, receipt, err)
			}
			if f.options.Budget.Snapshot().Used != baseline || f.native.status.Load() != AccountingHealthy {
				t.Fatal("corrupt component altered native health or retired disk")
			}
		})
	}
}

func TestAuthenticatedLedgerReopenLeaseCloseJoinsBlockedFactoryAndSourceFees(t *testing.T) {
	f := newAuthenticatedReopenFixture(t)
	f.maintain(t)
	baseline := f.options.Budget.Snapshot().Used
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	done := make(chan error, 1)
	go func() {
		p, receipt, err := openAuthenticatedLedgerProjection(context.Background(), f.native, f.directory, f.options, func(phase string) {
			if phase == "component-open" {
				close(entered)
				<-release
			}
		})
		if p != nil || receipt != (AuthenticatedLedgerReopen{}) {
			t.Error("canceled factory published", p, receipt)
		}
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("component open not reached")
	}
	closed := make(chan error, 1)
	go func() { closed <- f.lease.Close() }()
	select {
	case <-f.lease.ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("lease did not cancel")
	}
	select {
	case err := <-closed:
		t.Fatal("lease Close retired blocked factory", err)
	default:
	}
	if got := f.options.Budget.Snapshot().Used; got.RAM < baseline.RAM+AuthenticatedLedgerSourceRAMBytes+4096 || got.Disk != baseline.Disk {
		t.Fatal("blocked factory refunded owned IO", got, baseline)
	}
	unblock()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("factory did not join")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("lease did not join factory")
	}
	if got := f.options.Budget.Snapshot().Used; got != baseline {
		t.Fatal("factory resources leaked", got, baseline)
	}
}

func TestAuthenticatedLedgerReopenOldAnchorCannotBlessNewRoot(t *testing.T) {
	f := newAuthenticatedReopenFixture(t)
	f.records = appendAuthenticatedTail(t, f.native, f.records)
	f.maintain(t)
	cut := nativeRecordPrefix(f.records[len(f.records)-1])
	f.options.Prefix = &cut
	p, _, err := OpenAuthenticatedLedgerProjection(context.Background(), f.native, f.directory, f.options)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.owner.AdoptClosedSuccessor(p); err != nil {
		t.Fatal(err)
	}
	rootPath := filepath.Join(f.directory, "active.root")
	raw, _ := os.ReadFile(rootPath)
	baseline := f.options.Budget.Snapshot().Used
	p, receipt, err := OpenAuthenticatedLedgerProjection(context.Background(), f.native, f.directory, f.options)
	if p != nil || receipt != (AuthenticatedLedgerReopen{}) || !errors.Is(err, authenticatedstate.ErrCorrupt) {
		t.Fatal("old anchor accepted different active cut", p, receipt, err)
	}
	after, _ := os.ReadFile(rootPath)
	if !bytes.Equal(raw, after) || f.options.Budget.Snapshot().Used != baseline {
		t.Fatal("old-cut refusal modified root or refunded persistent disk")
	}
}

func TestAuthenticatedLedgerReopenAuthenticatesExcludedNativeSuffixBeforeMutation(t *testing.T) {
	f := newAuthenticatedReopenFixture(t)
	f.records = appendAuthenticatedTail(t, f.native, f.records)
	f.maintain(t)
	raw, err := os.ReadFile(f.native.path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.WriteFile(f.native.path, raw, 0600); err != nil {
			t.Error(err)
		}
	}()
	changed := bytes.Clone(raw)
	changed[len(changed)-1] ^= 1
	if err := os.WriteFile(f.native.path, changed, 0600); err != nil {
		t.Fatal(err)
	}
	beforeRoot, _ := os.ReadFile(filepath.Join(f.directory, "active.root"))
	beforePack, _ := os.ReadFile(filepath.Join(f.directory, "pages"))
	baseline := f.options.Budget.Snapshot().Used
	// The selected role cut remains the original snapshot; corrupt later facts
	// may not be hidden by choosing that earlier, otherwise valid prefix.
	p, receipt, err := OpenAuthenticatedLedgerProjection(context.Background(), f.native, f.directory, f.options)
	if p != nil || receipt != (AuthenticatedLedgerReopen{}) || !errors.Is(err, ErrProjectionSource) || f.native.status.Load() != AccountingRecoveryRequired {
		t.Fatal(p, receipt, err)
	}
	afterRoot, _ := os.ReadFile(filepath.Join(f.directory, "active.root"))
	afterPack, _ := os.ReadFile(filepath.Join(f.directory, "pages"))
	if !bytes.Equal(beforeRoot, afterRoot) || !bytes.Equal(beforePack, afterPack) || f.options.Budget.Snapshot().Used != baseline {
		t.Fatal("source failure modified saved component or retained source fees")
	}
}

func TestAuthenticatedLedgerReopenProcessHelper(t *testing.T) {
	path := os.Getenv("HALRO_AUTH_REOPEN_PROCESS_SOURCE")
	if path == "" {
		t.Skip("subprocess-only persistent publisher")
	}
	native := openChained(t, path, nil)
	for _, event := range append(projectionTestAttribution(), projectionTestLifecycle(1, true)[0]) {
		if _, _, err := native.AppendRecordWithReceipt(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
	report, partial, err := InspectReplayAuthenticated(path, testChainKey, nil)
	if err != nil || partial {
		t.Fatal(partial, err)
	}
	p, err := BuildAuthenticatedLedgerProjection(context.Background(), path, testChainKey, os.Getenv("HALRO_AUTH_REOPEN_PROCESS_DIRECTORY"), report, authenticatedLedgerTestOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := native.Close(); err != nil {
		t.Fatal(err)
	}
	// Persistent bytes deliberately survive this process. The parent establishes
	// a NEW resource owner rather than attempting to resurrect this Go Permit.
}

func TestAuthenticatedLedgerReopenSurvivesPublisherProcessAndNewManager(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.wal")
	directory := filepath.Join(t.TempDir(), "auth")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestAuthenticatedLedgerReopenProcessHelper$", "-test.count=1")
	cmd.Env = append(os.Environ(), "HALRO_AUTH_REOPEN_PROCESS_SOURCE="+path, "HALRO_AUTH_REOPEN_PROCESS_DIRECTORY="+directory)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("publisher process failed: %v %s", err, output)
	}
	var saved ReplayAnchor
	report, partial, err := InspectReplayAuthenticated(path, testChainKey, func(r Record) error {
		saved = ReplayAnchor{Head: Watermark{r.Generation, r.Offset, r.Sequence}, Hash: r.Hash, Epoch: r.Epoch}
		return nil
	})
	if err != nil || partial || saved.Head != report.Head || saved.Hash != report.ChainHash || saved.Head.Sequence != 3 {
		t.Fatal("independent source cut", saved, report, partial, err)
	}
	o := authenticatedLedgerTestOptions(t)
	// The test owns this fresh private namespace and explicitly reconciles its
	// finite ceiling. This is not production-wide Disk startup reconciliation.
	credit, err := o.Budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: o.MaxDiskBytes})
	if err != nil {
		t.Fatal(err)
	}
	var p *AuthenticatedLedgerProjection
	t.Cleanup(func() {
		if p != nil {
			if err := p.Close(); err != nil {
				t.Error(err)
				return
			}
		}
		for _, name := range []string{"pages", "active.root", ".halro.lock"} {
			if err := os.Remove(filepath.Join(directory, name)); err != nil {
				t.Error(err)
				return
			}
		}
		if err := os.Remove(directory); err != nil {
			t.Error(err)
			return
		}
		dir, err := os.Open(filepath.Dir(directory))
		if err != nil {
			t.Error(err)
			return
		}
		if err := errors.Join(dir.Sync(), dir.Close()); err != nil {
			t.Error(err)
			return
		}
		if err := credit.Release(); err != nil {
			t.Error(err)
		}
		if got := o.Budget.Snapshot().Used; got != (resourcebudget.Cost{}) {
			t.Error("actual namespace retirement leaked credit", got)
		}
	})
	native := openChained(t, path, nil)
	records := appendAuthenticatedTail(t, native, nil)
	cut := nativeRecordPrefix(records[len(records)-1])
	o.Prefix = &cut
	lease, err := native.BeginMaintenance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	var receipt AuthenticatedLedgerReopen
	p, receipt, err = OpenAuthenticatedLedgerProjection(context.Background(), native, directory, AuthenticatedLedgerOpenOptions{AuthenticatedLedgerOptions: o, SavedAnchor: saved, DiskReservation: credit})
	if err != nil || receipt.From != saved || receipt.AppliedRecords != 4 || receipt.SourceVerified != 7 || receipt.DeltaVerified != 4 {
		t.Fatal(receipt, err)
	}
	compareAdvancedCandidate(t, p, native, cut)
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if got := o.Budget.Snapshot().Used; got.RAM != resourcebudget.ClaimRAMBytes || got.Disk != o.MaxDiskBytes {
		t.Fatal("borrowed Close refunded persistent bytes", got)
	}
}

func TestAuthenticatedLedgerReopenCanonicalGenesisAndEmptyRoll(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.wal")
	native := openChained(t, path, nil)
	dir := filepath.Join(t.TempDir(), "auth")
	report, partial, err := InspectReplayAuthenticated(path, testChainKey, nil)
	if err != nil || partial {
		t.Fatal(partial, err)
	}
	o := authenticatedLedgerTestOptions(t)
	owner, err := BuildAuthenticatedLedgerProjection(context.Background(), path, testChainKey, dir, report, o)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Discard()
	saved, err := owner.ReadReplayAnchor(context.Background())
	if err != nil || saved.Head != (Watermark{Generation: 1}) || saved.Hash != ([32]byte{}) || saved.Epoch != 0 {
		t.Fatal(saved, err)
	}
	owner.Close()
	credit, err := owner.DiskReservation()
	if err != nil {
		t.Fatal(err)
	}
	defer credit.Release()
	for index := 0; index < 2; index++ {
		// Roll on an empty Ledger is intentionally a no-op. The second case seals
		// one genuine fact and therefore creates a real empty active generation.
		if index == 1 {
			if _, _, err := native.AppendRecordWithReceipt(context.Background(), projectionTestAttribution()[0]); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := native.Roll(); err != nil {
			t.Fatal(err)
		}
		head, hash, _ := native.ChainHead()
		cut := ProjectionPrefix{Head: head, Hash: hash}
		o.Prefix = &cut
		lease, err := native.BeginMaintenance(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		p, receipt, err := OpenAuthenticatedLedgerProjection(context.Background(), native, dir, AuthenticatedLedgerOpenOptions{AuthenticatedLedgerOptions: o, SavedAnchor: saved, DiskReservation: credit})
		if err != nil || receipt.SourceVerified != uint64(index) || receipt.DeltaVerified != uint64(index) || receipt.AppliedRecords != uint64(index) || receipt.From != saved || receipt.To.Head != head || head.Generation != uint64(index+1) {
			lease.Close()
			t.Fatal(receipt, err)
		}
		compareAdvancedCandidate(t, p, native, cut)
		if err := p.Close(); err != nil {
			t.Fatal(err)
		}
		if err := owner.AdoptClosedSuccessor(p); err != nil {
			t.Fatal(err)
		}
		if err := lease.Close(); err != nil {
			t.Fatal(err)
		}
		saved = receipt.To
	}
}

func TestAuthenticatedLedgerReopenReplicaUsesOnlyExplicitPermittedCut(t *testing.T) {
	f := newAuthenticatedReopenFixture(t)
	f.records = appendAuthenticatedTail(t, f.native, f.records)
	if err := f.native.Close(); err != nil {
		t.Fatal(err)
	}
	replica, err := OpenWithOptions(f.native.path, NewStatus(), Options{ChainKey: testChainKey, Replica: true})
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	// Actual durable head is seven; the independent applied permission supplied
	// by this fixture remains three. Native presence alone grants no extra cut.
	p, receipt, err := OpenAuthenticatedLedgerProjection(context.Background(), replica, f.directory, f.options)
	if err != nil || receipt.To != f.options.SavedAnchor || receipt.AppliedRecords != 0 || receipt.SourceVerified != 7 || receipt.NativeHead.Head.Sequence != 7 {
		t.Fatal(receipt, err)
	}
	actual, err := p.ReadReplayAnchor(context.Background())
	if err != nil || actual.Head.Sequence != 3 {
		t.Fatal(actual, err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.owner.AdoptClosedSuccessor(p); err != nil {
		t.Fatal(err)
	}
}

func TestAuthenticatedLedgerReopenOldCommonSealAfterAheadComponentPublication(t *testing.T) {
	f := newAuthenticatedReopenFixture(t)
	ctx := context.Background()
	// Capture the independently selected root before any component gets ahead.
	f.maintain(t)
	initial, _, err := OpenAuthenticatedLedgerProjection(ctx, f.native, f.directory, f.options)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := initial.SealReference(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := initial.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.lease.Close(); err != nil {
		t.Fatal(err)
	}
	f.lease = nil
	f.records = appendAuthenticatedTail(t, f.native, f.records)
	f.maintain(t)
	permitted := nativeRecordPrefix(f.records[len(f.records)-1])
	f.options.Prefix = &permitted
	f.options.SelectedSealRoot = &saved.Root
	ahead, receipt, err := OpenAuthenticatedLedgerProjection(ctx, f.native, f.directory, f.options)
	if err != nil || receipt.AppliedRecords == 0 {
		t.Fatal(receipt, err)
	}
	aheadRef, err := ahead.SealReference(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := ahead.Close(); err != nil {
		t.Fatal(err)
	}
	pages := filepath.Join(f.directory, "pages")
	before, err := os.ReadFile(pages)
	if err != nil {
		t.Fatal(err)
	}
	// Model a failed common seal publication: native/component are ahead, the
	// admitted global seal still names saved. Reopen must authenticate and replay
	// the saved logical root, without truncating the ahead component's pages.
	next, again, err := OpenAuthenticatedLedgerProjection(ctx, f.native, f.directory, f.options)
	if err != nil || again.From != saved.Anchor || again.AppliedRecords != receipt.AppliedRecords {
		t.Fatal(again, err)
	}
	compareAdvancedCandidate(t, next, f.native, permitted)
	after, err := os.ReadFile(pages)
	if err != nil || len(after) <= len(before) || !bytes.Equal(before, after[:len(before)]) {
		t.Fatal("old seal replay overwrote committed ahead pages", err)
	}
	branch, err := next.SealReference(ctx)
	if err != nil || branch.Root.State.Revision <= aheadRef.Root.State.Revision {
		t.Fatal(branch, err)
	}
	if err := next.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.owner.AdoptClosedSuccessor(next); err != nil {
		t.Fatal(err)
	}
}
