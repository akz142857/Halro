package statecut

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/metadatastate"
	"github.com/akz142857/Halro/internal/usage"
)

func TestCommonStandaloneSelectedMetadataDAOAdvancesAndRepublishesNativeCut(t *testing.T) {
	f := newCommonFixture(t)
	o := prepareCommonReopen(t, f)
	ctx := context.Background()
	r, err := ReopenStandalone(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	wrong := f.authority
	wrong.Positions[1].Sequence++
	fees := r.Owners.Budget.Snapshot().Used
	if s, err := r.OpenMetadataStore(ctx, wrong); s != nil || !errors.Is(err, authenticatedstate.ErrSealIneligible) {
		t.Fatal("unselected common authority entered Metadata business factory", err)
	}
	if r.Owners.Budget.Snapshot().Used != fees {
		t.Fatal("unselected authority changed ownership fees")
	}
	s, err := r.OpenMetadataStore(ctx, f.authority)
	if err != nil {
		t.Fatal("selected Metadata DAO", err)
	}
	if duplicate, err := r.OpenMetadataStore(ctx, f.authority); duplicate != nil || !errors.Is(err, authenticatedstate.ErrSealIneligible) {
		t.Fatal("duplicate DAO owner", err)
	}
	now := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	project := domain.Project{ID: "prj_common_facade", Name: "Common selected Metadata", Enabled: true, CreatedAt: now, UpdatedAt: now}
	written, err := s.PutProject(ctx, project, 0, nil)
	if err != nil {
		t.Fatal("actual business DAO write", err)
	}
	if read, err := s.GetProject(ctx, project.ID); err != nil || read.ID != written.ID || read.Name != written.Name || read.Revision != written.Revision {
		t.Fatal("actual business DAO read", read, written, err)
	}
	position, err := s.ReadMetadataAppliedPosition(ctx)
	if err != nil || position.Sequence != r.Seal.Positions[1].Sequence+1 {
		t.Fatal("business transaction did not advance real native journal", position, err)
	}
	metadataRef, err := r.Owners.Metadata.SealReference(ctx)
	if err != nil || metadataRef.Position.Sequence != position.Sequence {
		t.Fatal("native and authenticated Metadata cuts diverged", err)
	}
	authority := f.authority
	authority.Positions[1] = metadataRef.Position
	base := f.base
	base.Revision++
	publication, err := PublishStandalone(ctx, r.Owners, base, authority)
	if err != nil || publication.Seal.Roots[1] == r.Seal.Roots[1] || publication.Seal.Positions[1] != metadataRef.Position {
		t.Fatal("new Metadata cut was not durably published", err)
	}
	for i := range publication.Seal.Roots {
		if i != 1 && publication.Seal.Roots[i] != r.Seal.Roots[i] {
			t.Fatal("Metadata DAO write changed unrelated root", i)
		}
	}
	if selected, found, err := r.Owners.Publisher.Select(authority); err != nil || !found || selected != publication.Seal {
		t.Fatal("durable common selection", found, err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.RecoveryError(); err == nil {
		t.Fatal("common owner Close left business DAO admitted")
	}
	if reopened, err := r.OpenMetadataStore(ctx, authority); reopened != nil || !errors.Is(err, authenticatedstate.ErrSealIneligible) {
		t.Fatal("closed common owner opened another business DAO", err)
	}
	for _, adopt := range []func() error{
		func() error { return f.owners.Ledger.AdoptClosedSuccessor(r.Owners.Ledger) },
		func() error { return f.owners.Usage.AdoptClosedTypedSuccessor(r.Owners.Usage) },
		func() error { return f.owners.Governance.AdoptClosedTypedSuccessor(r.Owners.Governance) },
		func() error { return f.owners.Metadata.AdoptClosedSuccessor(r.Owners.Metadata) },
		func() error { return f.owners.Directory.AdoptClosedSuccessor(r.Owners.Directory) },
	} {
		if err := adopt(); err != nil {
			t.Fatal("actual closed successor cleanup receipt", err)
		}
	}
}

func prepareCommonReopen(t *testing.T, f *commonFixture) ReopenOptions {
	t.Helper()
	if _, err := PublishStandalone(context.Background(), f.owners, f.base, f.authority); err != nil {
		t.Fatal(err)
	}
	head, err := f.owners.Metadata.AppliedHead()
	if err != nil {
		t.Fatal(err)
	}
	evidence := f.owners.Metadata.Evidence()
	for _, close := range []func() error{f.owners.Usage.Close, f.owners.Governance.Close, f.owners.Metadata.Close, f.owners.Directory.Close, f.owners.Ledger.Close} {
		if err := close(); err != nil {
			t.Fatal(err)
		}
	}
	lc, err := f.owners.Ledger.DiskReservation()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lc.Release(); err != nil {
			t.Error(err)
		}
	})
	uc, err := f.owners.Usage.TypedDiskReservation()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := uc.Release(); err != nil {
			t.Error(err)
		}
	})
	gc, err := f.owners.Governance.TypedDiskReservation()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := gc.Release(); err != nil {
			t.Error(err)
		}
	})
	dc, err := f.owners.Directory.DiskReservation()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := dc.Release(); err != nil {
			t.Error(err)
		}
	})
	mc, jc, err := f.owners.Metadata.DiskReservations()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := mc.Release(); err != nil {
			t.Error(err)
		}
		if err := jc.Release(); err != nil {
			t.Error(err)
		}
	})
	sources := f.owners
	sources.Ledger, sources.Usage, sources.Governance, sources.Metadata, sources.Directory = nil, nil, nil, nil, nil
	o := ReopenOptions{Sources: sources, Authority: f.authority, LedgerDirectory: filepath.Join(f.dir, "ledger-root"), LedgerOptions: ledger.AuthenticatedLedgerOptions{Identity: f.authority.Instance, Budget: sources.Budget, MaxDiskBytes: 64 << 20, MaxAppendBytes: 16 << 20}, LedgerDisk: lc, DirectoryPath: filepath.Join(f.dir, "directory-root"), DirectoryOptions: ledger.AuthenticatedDirectoryOptions{Identity: f.authority.Instance, Budget: sources.Budget, MaxDiskBytes: 16 << 20, MaxAppendBytes: 4 << 20, DiskReservation: dc}, UsageOptions: usage.AuthenticatedUsageOptions{Identity: f.authority.Instance, UsageKey: f.key, Budget: sources.Budget, RootDirectory: filepath.Join(f.dir, "usage-root"), RootMaxDiskBytes: 64 << 20, RootMaxAppendBytes: 16 << 20}, UsageDisk: uc, GovernanceOptions: governance.AuthenticatedGovernanceOptions{Identity: f.authority.Instance, RootKey: f.key, Budget: sources.Budget, RootDirectory: filepath.Join(f.dir, "governance-root"), RootMaxDiskBytes: 64 << 20, RootMaxAppendBytes: 8 << 20}, GovernanceDisk: gc, MetadataDirectory: filepath.Join(f.dir, "metadata-root"), MetadataJournalPath: filepath.Join(f.dir, "metadata.bound.journal"), MetadataKey: f.key, MetadataOptions: metadatastate.OpenOptions{BaselineOptions: metadatastate.BaselineOptions{Identity: f.authority.Instance, InstanceID: sources.InstanceID, KeyGeneration: sources.KeyGeneration, Budget: sources.Budget, MaxSourceBytes: 64 << 20, MaxDiskBytes: 64 << 20, MaxAppendBytes: 8 << 20}, Evidence: evidence, SavedHead: head, NativeHead: head, PermittedHead: head, RootDiskReservation: mc, JournalDiskReservation: jc, MaxJournalBytes: 16 << 20}}
	return o
}

func TestCommonStandaloneReopensAllSelectedRootsAndRepublishesSameCut(t *testing.T) {
	f := newCommonFixture(t)
	o := prepareCommonReopen(t, f)
	r, err := ReopenStandalone(context.Background(), o)
	if err != nil || r == nil {
		t.Fatal("common reopen", err)
	}
	defer r.Close()
	if r.LedgerReceipt.AppliedRecords != 0 || r.MetadataReceipt.AppliedFrames != 0 {
		t.Fatal("selected prefix was reduced again", r.LedgerReceipt, r.MetadataReceipt)
	}
	base := f.base
	base.Revision++
	publication, err := PublishStandalone(context.Background(), r.Owners, base, f.authority)
	if err != nil {
		t.Fatal("reopened common owners cannot republish", err)
	}
	for i := 0; i < 7; i++ {
		if publication.Seal.Roots[i] != r.Seal.Roots[i] {
			t.Fatal("selected root changed during reopen", i)
		}
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	for _, adopt := range []func() error{
		func() error { return f.owners.Ledger.AdoptClosedSuccessor(r.Owners.Ledger) },
		func() error { return f.owners.Usage.AdoptClosedTypedSuccessor(r.Owners.Usage) },
		func() error { return f.owners.Governance.AdoptClosedTypedSuccessor(r.Owners.Governance) },
		func() error { return f.owners.Metadata.AdoptClosedSuccessor(r.Owners.Metadata) },
		func() error { return f.owners.Directory.AdoptClosedSuccessor(r.Owners.Directory) },
	} {
		if err := adopt(); err != nil {
			t.Fatal("closed successor cleanup receipt", err)
		}
	}
}

func TestCommonStandaloneReopenRefusesUnselectedCutBeforeOrphanRepair(t *testing.T) {
	f := newCommonFixture(t)
	o := prepareCommonReopen(t, f)
	path := filepath.Join(o.LedgerDirectory, "pages")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	orphan := append(bytes.Clone(before), []byte("unselected-orphan")...)
	if err := os.WriteFile(path, orphan, 0600); err != nil {
		t.Fatal(err)
	}
	defer os.WriteFile(path, before, 0600)
	o.Authority.Positions[0].Sequence++
	fees := o.Sources.Budget.Snapshot().Used
	if r, err := ReopenStandalone(context.Background(), o); !errors.Is(err, authenticatedstate.ErrSealIneligible) || r != nil {
		t.Fatal("unselected cut entered component factory", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, orphan) || o.Sources.Budget.Snapshot().Used != fees {
		t.Fatal("unselected root repaired bytes or changed fees", err)
	}
	if _, err := os.Stat(filepath.Join(o.LedgerDirectory, "active.root")); err != nil {
		t.Fatal("selected namespace removed", err)
	}
}

func TestCommonStandaloneSavedRootsApplyOnlyLaterLedgerUsageFacts(t *testing.T) {
	f := newCommonFixture(t)
	o := prepareCommonReopen(t, f)
	ctx := context.Background()
	for i, kind := range []ledger.EventKind{ledger.EventRequestAccepted, ledger.EventReservationCreated, ledger.EventAttemptStarted, ledger.EventAttemptSettled, ledger.EventRequestFinalized} {
		e := ledger.Event{EventID: fmt.Sprintf("evt_after_seal_%d", i), Kind: kind, RequestID: "req_after_seal", ProjectID: "prj_common", PeriodID: "2026-10-06", PeriodTimezone: "UTC", PeriodTimezoneVersion: 1, OccurredAt: time.Date(2026, 10, 6, 12, 1, 0, 0, time.UTC)}
		if i >= 1 && i <= 3 {
			e.AttemptID = "att_after_seal"
		}
		if i == 1 {
			e.ReservationMicrosUSD = ledger.MicrosUSD(30)
		}
		if i == 3 {
			e.CommittedMicrosUSD = ledger.MicrosUSD(7)
			e.Outcome = "success"
		}
		if _, _, err := f.owners.Lease.AppendApply(ctx, e, func(context.Context, ledger.Record) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	r, err := ReopenStandalone(ctx, o)
	if err != nil {
		t.Fatal("saved roots with durable tail", err)
	}
	defer r.Close()
	head, hash, _ := r.Owners.NativeLedger.ChainHead()
	advance, err := r.Owners.Ledger.AdvanceFromNative(ctx, r.Owners.NativeLedger, ledger.ProjectionPrefix{Head: head, Hash: hash})
	if err != nil || advance.AppliedRecords != 5 {
		t.Fatal("Ledger delta", advance, err)
	}
	usageAdvance, err := r.Derivative.CatchUpAuthenticatedFromNative(ctx, r.Owners.NativeLedger, r.Owners.Lease, r.Owners.Budget)
	if err != nil || usageAdvance.AppliedRecords != 5 {
		t.Fatal("Usage delta", usageAdvance, err)
	}
	oracle := ledger.NewState()
	if _, err := r.Owners.NativeLedger.ReplayAuthenticated(ctx, ledger.ReplayAnchor{}, oracle.Apply); err != nil {
		t.Fatal(err)
	}
	balance, err := r.Owners.Ledger.ReadBalance(ctx, ledger.BalanceKey{ProjectID: "prj_common", PeriodID: "2026-10-06", TimezoneVersion: 1})
	if err != nil || balance != oracle.Balance("prj_common", "2026-10-06", 1) || balance.CommittedMicrosUSD != 19 || balance.ReservedMicrosUSD != 0 {
		t.Fatal("saved+delta balance differs from complete native oracle", balance, err)
	}
	ref, err := r.Owners.Usage.TypedSealReference(ctx)
	if err != nil || ref.Projection.Position.Sequence != 10 {
		t.Fatal("Usage did not reach selected Ledger cut", ref, err)
	}
	retry, err := r.Derivative.CatchUpAuthenticatedFromNative(ctx, r.Owners.NativeLedger, r.Owners.Lease, r.Owners.Budget)
	if err != nil || retry.AppliedRecords != 0 {
		t.Fatal("delta retry duplicated facts", retry, err)
	}
	after, err := r.Owners.Usage.TypedSealReference(ctx)
	if err != nil || after != ref {
		t.Fatal("empty retry changed root", err)
	}
	authority := f.authority
	authority.Positions[0] = authenticatedstate.SealPosition{Component: authenticatedstate.SealComponentLedger, Generation: head.Generation, Sequence: head.Sequence, Offset: uint64(head.Offset), Hash: hash, Epoch: uint64(ref.Ledger.Anchor.Epoch)}
	authority.Positions[4] = ref.Projection.Position
	base := f.base
	base.Revision++
	if publication, err := PublishStandalone(ctx, r.Owners, base, authority); err == nil || publication.Seal != (authenticatedstate.CandidateSeal{}) {
		t.Fatal("old directory paired with new Ledger cut", err)
	}
	if changed, err := r.Owners.Directory.AdvanceFromNative(ctx, r.Owners.NativeLedger, r.Owners.Lease, r.Owners.Ledger); err != nil || changed != 0 {
		t.Fatal("active-cut directory refresh", changed, err)
	}
	publication, err := PublishStandalone(ctx, r.Owners, base, authority)
	if err != nil || publication.Seal.Positions[0] != authority.Positions[0] || publication.Seal.Roots[6] == r.Seal.Roots[6] {
		t.Fatal("saved plus delta complete common publication", err)
	}
	stable, err := r.Owners.Directory.SealReference(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := r.Owners.Directory.AdvanceFromNative(ctx, r.Owners.NativeLedger, r.Owners.Lease, r.Owners.Ledger); err != nil || changed != 0 {
		t.Fatal("directory retry", changed, err)
	}
	if again, err := r.Owners.Directory.SealReference(ctx); err != nil || again != stable {
		t.Fatal("directory retry changed root", err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	for _, adopt := range []func() error{
		func() error { return f.owners.Ledger.AdoptClosedSuccessor(r.Owners.Ledger) },
		func() error { return f.owners.Usage.AdoptClosedTypedSuccessor(r.Owners.Usage) },
		func() error { return f.owners.Governance.AdoptClosedTypedSuccessor(r.Owners.Governance) },
		func() error { return f.owners.Metadata.AdoptClosedSuccessor(r.Owners.Metadata) },
		func() error { return f.owners.Directory.AdoptClosedSuccessor(r.Owners.Directory) },
	} {
		if err := adopt(); err != nil {
			t.Fatal("delta successor cleanup receipt", err)
		}
	}
}
