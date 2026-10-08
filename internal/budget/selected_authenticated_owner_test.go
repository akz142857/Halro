package budget

import (
	"context"
	"crypto/sha256"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

type selectedBudgetFixture struct {
	native    *ledger.Log
	owner     *ledger.AuthenticatedLedgerProjection
	resources *resourcebudget.Manager
	lease     *ledger.MaintenanceLease
	clock     func() time.Time
	path      string
}

func newSelectedBudgetFixture(t *testing.T) *selectedBudgetFixture {
	t.Helper()
	ctx := context.Background()
	projectionPath := filepath.Join(t.TempDir(), "selected-projection")
	m, creator, path, resources := newAuthenticatedManagerFixtureWithResources(t, ledger.Options{}, projectionPath)
	native, clock := m.log, m.now
	seed, err := m.BeginRequest(ctx, "prj_seed", "req_seed")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Finalize(ctx, seed, "success"); err != nil {
		t.Fatal(err)
	}
	if err := m.Retire(ctx); err != nil {
		t.Fatal(err)
	}
	lease, err := native.BeginMaintenance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	identity := sha256.Sum256([]byte("independent-accounting-test-identity"))
	directoryOptions := ledger.AuthenticatedDirectoryOptions{Identity: identity, Budget: resources, MaxDiskBytes: 16 << 20, MaxAppendBytes: 4 << 20}
	directoryPath := filepath.Join(t.TempDir(), "selected-directory")
	directory, err := ledger.BuildAuthenticatedDirectory(ctx, native, lease, creator, directoryPath, directoryOptions)
	if err != nil {
		t.Fatal(err)
	}
	dr, err := directory.SealReference(ctx)
	if err != nil {
		t.Fatal(err)
	}
	lr, err := creator.SealReference(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := directory.Close(); err != nil {
		t.Fatal(err)
	}
	directoryCredit, err := directory.DiskReservation()
	if err != nil {
		t.Fatal(err)
	}
	directoryOptions.DiskReservation = directoryCredit
	if err := creator.Close(); err != nil {
		t.Fatal(err)
	}
	ledgerCredit, err := creator.DiskReservation()
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	guard, err := lock.Acquire(filepath.Dir(path))
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
	fee, err := resources.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: 32 << 10})
	if err != nil {
		t.Fatal(err)
	}
	pub, err := authenticatedstate.OpenSealCandidatePublisher(filepath.Dir(path), guard, testChainKey, hard, resources, fee)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pub.Publish(ctx, seal); err != nil {
		t.Fatal(err)
	}
	authority := authenticatedstate.SealEligibility{SchemaVersion: 1, ApplyVersion: 1, IndexVersion: 1, KeyDerivation: 1, Instance: seal.Instance, Incarnation: seal.Incarnation, Role: seal.Role, Coverage: seal.Coverage}
	copy(authority.Positions[:], seal.Positions[:])
	if err := native.Close(); err != nil {
		t.Fatal(err)
	}
	native, receipt, err := ledger.OpenWithSelectedSeal(ctx, path, ledger.NewStatus(), ledger.Options{ChainKey: testChainKey}, ledger.SelectedSealOpenOptions{Publisher: pub, Guard: guard, Authority: authority, Directory: directoryPath, DirectoryOptions: directoryOptions})
	if err != nil || receipt.DecodedRecords != 0 {
		t.Fatal(receipt, err)
	}
	lease, err = native.BeginMaintenance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	prefix := ledger.ProjectionPrefix{Head: lr.Anchor.Head, Hash: lr.Anchor.Hash}
	owner, reopen, err := ledger.OpenSelectedAuthenticatedLedgerProjection(ctx, native, projectionPath, ledger.AuthenticatedLedgerOpenOptions{AuthenticatedLedgerOptions: ledger.AuthenticatedLedgerOptions{Identity: identity, Budget: resources, MaxDiskBytes: 128 << 20, MaxAppendBytes: 8 << 20, Prefix: &prefix}, SavedAnchor: lr.Anchor, DiskReservation: ledgerCredit}, ledger.SelectedLedgerProjectionOpenOptions{Publisher: pub, Guard: guard, Authority: authority})
	if err != nil || reopen.SourceVerified != 0 {
		t.Fatal(reopen, err)
	}
	t.Cleanup(func() {
		if err := lease.Close(); err != nil {
			t.Error(err)
		}
		if err := native.Close(); err != nil {
			t.Error(err)
		}
		if err := owner.Close(); err != nil {
			t.Error(err)
		}
		if err := creator.AdoptClosedSuccessor(owner); err != nil {
			t.Error(err)
		}
		if err := ledgerCredit.Release(); err != nil {
			t.Error(err)
		}
		if err := directory.Discard(); err != nil {
			t.Error(err)
		}
		if err := directoryCredit.Release(); err != nil {
			t.Error(err)
		}
		if err := pub.Close(); err != nil {
			t.Error(err)
		}
		if err := pub.Retire(); err != nil {
			t.Error(err)
		}
		if err := fee.Release(); err != nil {
			t.Error(err)
		}
		if err := guard.Close(); err != nil {
			t.Error(err)
		}
	})
	return &selectedBudgetFixture{native: native, owner: owner, resources: resources, lease: lease, clock: clock, path: path}
}

func TestSelectedAuthenticatedManagerActualAppendReadsAndRevocation(t *testing.T) {
	f := newSelectedBudgetFixture(t)
	ctx := context.Background()
	periods := mustResolver(t, "UTC")
	options := Options{Now: f.clock}
	if m, err := NewWithHASelectedAuthenticatedProjection(ctx, f.native, f.owner, periods, f.lease, f.resources, options); m != nil || !errors.Is(err, ledger.ErrProjectionSource) {
		t.Fatal("Standalone selected provenance acquired an HA writer grant", m, err)
	}
	resourceFeesBefore := f.resources.Snapshot().Used
	if err := f.owner.EnableHASelectedBusinessReads(ctx, f.native, f.lease, f.resources); !errors.Is(err, ledger.ErrProjectionSource) || f.resources.Snapshot().Used != resourceFeesBefore {
		t.Fatal("Standalone provenance acquired an HA business read lane or lost fees", err)
	}
	if m, err := NewWithSelectedAuthenticatedProjection(ctx, f.native, f.owner, periods, f.lease, f.resources, options); m != nil || !errors.Is(err, ledger.ErrStartupAuditPending) {
		t.Fatal("constructor invented history readiness", m, err)
	}
	a, err := f.native.StartSelectedStartupAudit(ctx, f.resources, ledger.StartupAuditOptions{Deadline: time.Second, ProgressTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	m, err := NewWithSelectedAuthenticatedProjection(ctx, f.native, f.owner, periods, f.lease, f.resources, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := m.Retire(ctx); err != nil {
			t.Error(err)
		}
	})
	owner, ok := m.owner.(selectedAuthenticatedAccountingOwner)
	if !ok || owner.AuthenticatedLedgerProjection != f.owner || owner.native != f.native || m.reads != m.owner || m.legacyPending != nil {
		t.Fatal("selected manager lost exact actual reader/apply owner")
	}
	if err := f.lease.Close(); err != nil {
		t.Fatal(err)
	}
	request, err := m.BeginRequestDetailed(ctx, "prj_actual", "key_actual", "req_actual", "model_actual")
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := m.ReserveAttempt(ctx, request, 100, 9)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.MarkStarted(ctx, attempt); err != nil {
		t.Fatal(err)
	}
	if err := m.Settle(ctx, attempt, Settlement{CommittedMicrosUSD: 4, Outcome: "success"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Finalize(ctx, request, "success"); err != nil {
		t.Fatal(err)
	}
	oracle := ledger.NewState()
	report, partial, err := ledger.InspectReplayAuthenticated(f.path, testChainKey, oracle.Apply)
	if err != nil || partial {
		t.Fatal(report, partial, err)
	}
	head, hash, err := f.owner.ReadPosition(ctx)
	if err != nil || head != report.Head || hash != report.ChainHash || head.Sequence != 7 {
		t.Fatal("durable Advance did not follow actual native facts", head, report, err)
	}
	balanceKey := ledger.BalanceKey{ProjectID: request.ProjectID, PeriodID: request.Period.ID, TimezoneVersion: request.Period.TimezoneVersion}
	balance, err := m.ReadBalance(ctx, balanceKey)
	if err != nil || balance != oracle.Balance(request.ProjectID, request.Period.ID, request.Period.TimezoneVersion) || balance.CommittedMicrosUSD != 4 || balance.ReservedMicrosUSD != 0 {
		t.Fatal(balance, err)
	}
	actual, found, err := f.owner.ReadRequest(ctx, request.RequestID)
	if err != nil || !found || actual.Finalized == nil || actual.Finalized.Event.Outcome != "success" {
		t.Fatal(actual, found, err)
	}
	before := f.native.Stats().Records
	if err := f.native.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ReadBalance(ctx, balanceKey); err == nil {
		t.Fatal("closed native grant allowed DAO read")
	}
	if _, err := m.BeginRequest(ctx, "prj_actual", "req_after_withdrawal"); err == nil {
		t.Fatal("closed native grant admitted new request")
	}
	if f.native.Stats().Records != before || m.activeOperations != 0 {
		t.Fatal("withdrawal appended or retained an operation ticket")
	}
}

func TestSelectedAuthenticatedManagerRequiresSameActualOwnerBudgetAndLease(t *testing.T) {
	f := newSelectedBudgetFixture(t)
	ctx := context.Background()
	if err := f.native.AuditSelectedStartup(ctx, f.resources); err != nil {
		t.Fatal(err)
	}
	periods := mustResolver(t, "UTC")
	options := Options{Now: f.clock}
	if m, err := NewWithAuthenticatedProjection(ctx, f.native, f.owner, periods, f.lease, options); m != nil || !errors.Is(err, ledger.ErrProjectionSource) {
		t.Fatal("fresh constructor bypassed selected operation/ACK grant", m, err)
	}
	for _, test := range []struct {
		name    string
		lease   *ledger.MaintenanceLease
		manager *resourcebudget.Manager
	}{
		{"missing-lease", nil, f.resources}, {"missing-manager", f.lease, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := f.resources.Snapshot().Used
			if m, err := NewWithSelectedAuthenticatedProjection(ctx, f.native, f.owner, periods, test.lease, test.manager, options); m != nil || err == nil {
				t.Fatal(m, err)
			}
			if f.resources.Snapshot().Used != before {
				t.Fatal("known constructor rejection changed resource ownership")
			}
		})
	}
	ordinary, projection, _, resources := newAuthenticatedManagerFixtureWithResources(t, ledger.Options{})
	lease, err := ordinary.log.BeginMaintenance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if m, err := NewWithSelectedAuthenticatedProjection(ctx, ordinary.log, projection, periods, lease, resources, options); m != nil || !errors.Is(err, ledger.ErrProjectionSource) {
		t.Fatal("fresh/caller-matching cut invented selected provenance", m, err)
	}
	if m, err := NewWithSelectedAuthenticatedProjection(ctx, f.native, f.owner, periods, f.lease, resources, options); m != nil || !errors.Is(err, ledger.ErrProjectionSource) {
		t.Fatal("foreign resource manager was accepted", m, err)
	}
	if m, err := NewWithSelectedAuthenticatedProjection(ctx, ordinary.log, f.owner, periods, lease, f.resources, options); m != nil || !errors.Is(err, ledger.ErrProjectionSource) {
		t.Fatal("foreign native writer was accepted", m, err)
	}
}

func TestSelectedAuthenticatedManagerRefusesACKAfterActualNativeWithdrawal(t *testing.T) {
	f := newSelectedBudgetFixture(t)
	ctx := context.Background()
	if err := f.native.AuditSelectedStartup(ctx, f.resources); err != nil {
		t.Fatal(err)
	}
	m, err := NewWithSelectedAuthenticatedProjection(ctx, f.native, f.owner, mustResolver(t, "UTC"), f.lease, f.resources, Options{Now: f.clock})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := m.Retire(ctx); err != nil {
			t.Error(err)
		}
	})
	if err := f.lease.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.AddObserver(func(ledger.Record) { f.native.Status().RequireRecovery() }); err != nil {
		t.Fatal(err)
	}
	before := f.native.Stats().Records
	if _, err := m.BeginRequest(ctx, "prj_actual", "req_durable_then_withdrawn"); err == nil {
		t.Fatal("post-Apply native withdrawal still acknowledged business success")
	}
	if f.native.Stats().Records != before+1 {
		t.Fatal("actual accepted durable fact was lost")
	}
	oracle := ledger.NewState()
	var accepted ledger.Record
	if _, partial, err := ledger.InspectReplayAuthenticated(f.path, testChainKey, func(r ledger.Record) error {
		if r.Event.RequestID == "req_durable_then_withdrawn" && r.Event.Kind == ledger.EventRequestAccepted {
			accepted = r
		}
		return oracle.Apply(r)
	}); err != nil || partial {
		t.Fatal(partial, err)
	}
	if accepted.Sequence != 3 {
		t.Fatal("withdrawal reported native fact as absent", accepted)
	}
	if _, err := m.BeginRequest(ctx, "prj_actual", "req_followup"); err == nil {
		t.Fatal("withdrawn owner accepted subsequent business")
	}
	if f.native.Stats().Records != before+1 {
		t.Fatal("followup extended recovery-required native")
	}
}
