package statecut

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/auditlimits"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/governancelimits"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/startupaudit"
)

func prepareCommonSelectedReopen(t *testing.T, f *commonFixture) SelectedReopenOptions {
	t.Helper()
	return prepareCommonSelectedReopenWithSuffix(t, f, nil)
}

func prepareCommonSelectedReopenWithSuffix(t *testing.T, f *commonFixture, appendSuffix func(*ledger.Log, *ledger.MaintenanceLease)) SelectedReopenOptions {
	t.Helper()
	persistedCommonStartupCaches(t, f)
	o := prepareCommonReopen(t, f)
	if appendSuffix != nil {
		appendSuffix(f.owners.NativeLedger, f.owners.Lease)
	}
	for _, close := range []func() error{f.owners.Lease.Close, f.owners.NativeLedger.Close, f.owners.NativeGovernance.Close, f.owners.Audit.Close} {
		if err := close(); err != nil {
			t.Fatal(err)
		}
	}
	return openCommonSelectedSources(t, f, o, filepath.Join(f.dir, "audit-candidate.cache"), filepath.Join(f.dir, "governance-candidate.cache"))
}

func openCommonSelectedSources(t *testing.T, f *commonFixture, o ReopenOptions, auditCachePath, governanceCachePath string) SelectedReopenOptions {
	t.Helper()
	ctx := context.Background()
	readAudit := func() *audit.StartupCache {
		file, err := os.Open(auditCachePath)
		if err != nil {
			t.Fatal(err)
		}
		info, err := file.Stat()
		if err != nil {
			t.Fatal(err)
		}
		cache, err := audit.ReadStartupCache(ctx, file, info.Size(), f.key, f.authority.Instance, o.Sources.Budget)
		if err := errors.Join(err, ownedfile.CloseFile(file)); err != nil {
			t.Fatal(err)
		}
		return cache
	}
	readGovernance := func() *governance.StartupCache {
		file, err := os.Open(governanceCachePath)
		if err != nil {
			t.Fatal(err)
		}
		info, err := file.Stat()
		if err != nil {
			t.Fatal(err)
		}
		cache, err := governance.ReadStartupCache(ctx, file, info.Size(), f.key, f.authority.Instance, o.Sources.Budget)
		if err := errors.Join(err, ownedfile.CloseFile(file)); err != nil {
			t.Fatal(err)
		}
		return cache
	}
	ac, gc := readAudit(), readGovernance()
	defer ac.Close()
	defer gc.Close()
	var err error
	o.Sources.NativeLedger, _, err = ledger.OpenWithSelectedSeal(ctx, filepath.Join(f.dir, "ledger.wal"), ledger.NewStatus(), ledger.Options{ChainKey: f.key}, ledger.SelectedSealOpenOptions{Publisher: o.Sources.Publisher, Guard: o.Sources.Guard, Authority: o.Authority, Directory: o.DirectoryPath, DirectoryOptions: o.DirectoryOptions})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := o.Sources.NativeLedger.Close(); err != nil {
			t.Error(err)
		}
	})
	o.Sources.Lease, err = o.Sources.NativeLedger.BeginMaintenance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := o.Sources.Lease.Close(); err != nil {
			t.Error(err)
		}
	})
	o.Sources.Audit, _, err = audit.OpenWithSelectedSeal(ctx, filepath.Join(f.dir, "audit.log"), f.key, audit.Options{CacheBudget: o.Sources.Budget, CacheRAMBytes: auditlimits.MinCacheRAMBytes}, audit.SelectedSealOpenOptions{Publisher: o.Sources.Publisher, Guard: o.Sources.Guard, Authority: o.Authority, Cache: ac, Identity: o.Authority.Instance})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := o.Sources.Audit.Close(); err != nil {
			t.Error(err)
		}
	})
	o.Sources.NativeGovernance, _, err = governance.OpenWithSelectedSeal(ctx, filepath.Join(f.dir, "governance.log"), f.key, governance.Options{CacheBudget: o.Sources.Budget, CacheRAMBytes: governancelimits.MinCacheRAMBytes}, governance.SelectedSealOpenOptions{Publisher: o.Sources.Publisher, Guard: o.Sources.Guard, Authority: o.Authority, Cache: gc, Identity: o.Authority.Instance})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := o.Sources.NativeGovernance.Close(); err != nil {
			t.Error(err)
		}
	})
	return SelectedReopenOptions{ReopenOptions: o, MetadataBinding: o.MetadataOptions.SavedHead.Baseline, AuditOptions: startupaudit.Options{Deadline: 5 * time.Second, ProgressTimeout: 2 * time.Second}}
}

func TestCommonSelectedIncrementalReopenAllRootsMetadataDAOAndRepublish(t *testing.T) {
	f := newCommonFixture(t)
	o := prepareCommonSelectedReopen(t, f)
	ctx := context.Background()
	r, err := ReopenSelectedStandalone(ctx, o)
	if err != nil {
		t.Fatal("whole selected incremental factory", err)
	}
	defer r.Close()
	if err := r.WaitStartupAudit(ctx); err != nil {
		t.Fatal("four actual historical readers", err)
	}
	if r.LedgerReceipt.AppliedRecords != 0 || r.LedgerReceipt.SourceVerified != 0 || r.MetadataReceipt.AppliedFrames != 0 {
		t.Fatal("saved history decoded by initializer", r.LedgerReceipt, r.MetadataReceipt)
	}
	store, err := r.OpenMetadataStore(ctx, o.Authority)
	if err != nil {
		t.Fatal("selected actual DAO", err)
	}
	project := domain.Project{ID: "prj_selected_factory", Name: "incremental common factory", Enabled: true}
	if _, err := store.PutProject(ctx, project, 0, nil); err != nil {
		t.Fatal(err)
	}
	if read, err := store.GetProject(ctx, project.ID); err != nil || read.Name != project.Name {
		t.Fatal(read, err)
	}
	metadata, err := r.Owners.Metadata.SealReference(ctx)
	if err != nil {
		t.Fatal(err)
	}
	authority := o.Authority
	authority.Positions[1] = metadata.Position
	base := f.base
	base.Revision++
	publication, err := PublishStandalone(ctx, r.Owners, base, authority)
	if err != nil {
		t.Fatal("incremental generation cannot publish complete common cut", err)
	}
	if publication.Seal.Roots[1] == r.Seal.Roots[1] {
		t.Fatal("DAO did not update authenticated Metadata root")
	}
	for i := 0; i < 7; i++ {
		if i != 1 && publication.Seal.Roots[i] != r.Seal.Roots[i] {
			t.Fatal("unchanged component root changed", i)
		}
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.RecoveryError(); err == nil {
		t.Fatal("whole generation Close retained DAO")
	}
	for _, adopt := range []func() error{
		func() error { return f.owners.Ledger.AdoptClosedSuccessor(r.Owners.Ledger) },
		func() error { return f.owners.Usage.AdoptClosedTypedSuccessor(r.Owners.Usage) },
		func() error { return f.owners.Governance.AdoptClosedTypedSuccessor(r.Owners.Governance) },
		func() error { return f.owners.Metadata.AdoptClosedSuccessor(r.Owners.Metadata) },
		func() error { return f.owners.Directory.AdoptClosedSuccessor(r.Owners.Directory) },
	} {
		if err := adopt(); err != nil {
			t.Fatal("actual generation retirement receipt", err)
		}
	}
}

func TestCommonSelectedIncrementalRefusesForeignNativeAndUnselectedAuthority(t *testing.T) {
	f := newCommonFixture(t)
	o := prepareCommonSelectedReopen(t, f)
	before := o.Sources.Budget.Snapshot().Used
	wrong := o
	wrong.Authority.Positions[0].Sequence++
	if owner, err := ReopenSelectedStandalone(context.Background(), wrong); owner != nil || !errors.Is(err, authenticatedstate.ErrSealIneligible) {
		t.Fatal(owner, err)
	}
	foreign, err := audit.OpenWithOptions(filepath.Join(f.dir, "foreign-audit.log"), f.key, audit.Options{CacheBudget: o.Sources.Budget, CacheRAMBytes: auditlimits.MinCacheRAMBytes})
	if err != nil {
		t.Fatal(err)
	}
	wrong = o
	wrong.Sources.Audit = foreign
	if owner, err := ReopenSelectedStandalone(context.Background(), wrong); owner != nil || err == nil {
		t.Fatal("ordinary source impersonated selected native", owner, err)
	}
	if err := foreign.Close(); err != nil {
		t.Fatal(err)
	}
	if after := o.Sources.Budget.Snapshot().Used; before != after {
		t.Fatal("rejected qualification changed generation fees", before, after)
	}
}
