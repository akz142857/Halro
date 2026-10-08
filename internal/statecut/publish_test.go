package statecut

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/auditlimits"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/backup"
	"github.com/akz142857/Halro/internal/buildinfo"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/governancelimits"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/metadatastate"
	"github.com/akz142857/Halro/internal/resourcebudget"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/usage"
)

type commonFixture struct {
	owners    StandaloneOwners
	base      authenticatedstate.CandidateSeal
	authority authenticatedstate.SealEligibility
	dir       string
	key       []byte
}

func commonGovernanceEvent() governance.Event {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	return governance.Event{EventID: "gov_out_common", ProjectID: "prj_common", WorkUnitID: "wku_common", DefinitionID: "odef_result", DefinitionVersion: 1, OutcomeID: "out_common", Value: "accepted", ReporterKeyID: "key_common", ObservedAt: now, IngestedAt: now, Revision: 1, IdempotencyKeyHash: "sha256:" + strings.Repeat("1", 64), RequestFingerprint: "sha256:" + strings.Repeat("2", 64)}
}

func newCommonFixture(t *testing.T) *commonFixture {
	return newCommonFixtureWithGovernanceOptions(t, governance.Options{})
}

func newCommonFixtureWithGovernanceOptions(t *testing.T, governanceOptions governance.Options) *commonFixture {
	t.Helper()
	var f *commonFixture
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{0x71}, 32)
	identity := sha256.Sum256([]byte("independent-common-cut-instance"))
	limits := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: 1 << 30, Disk: 1 << 30}, MaxClaims: 128, MaxOwners: 16, MaxPressureAge: time.Hour}
	for i := range limits.Classes {
		limits.Classes[i] = limits.Total
	}
	limits.Classes[resourcebudget.Recovery] = resourcebudget.Cost{RAM: 8 << 20}
	limits.RecoveryReserve = resourcebudget.Cost{RAM: 8 << 20}
	limits.RecoveryClaims = 4
	limits.Classes[resourcebudget.Control] = resourcebudget.Cost{RAM: 128 * resourcebudget.ClaimRAMBytes}
	manager, err := resourcebudget.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	guard, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		current := guard
		if f != nil {
			current = f.owners.Guard
		}
		if err := current.Close(); err != nil {
			t.Error(err)
		}
	})
	nativePath := filepath.Join(dir, "ledger.wal")
	native, err := ledger.OpenWithOptions(nativePath, ledger.NewStatus(), ledger.Options{ChainKey: key})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := native.Close(); err != nil {
			t.Error(err)
		}
	})
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	for i, kind := range []ledger.EventKind{ledger.EventRequestAccepted, ledger.EventReservationCreated, ledger.EventAttemptStarted, ledger.EventAttemptSettled, ledger.EventRequestFinalized} {
		e := ledger.Event{EventID: fmt.Sprintf("evt_common_%d", i), Kind: kind, RequestID: "req_common", ProjectID: "prj_common", PeriodID: "2026-10-06", PeriodTimezone: "UTC", PeriodTimezoneVersion: 1, OccurredAt: now}
		if i >= 1 && i <= 3 {
			e.AttemptID = "att_common"
		}
		if i == 1 {
			amount := int64(20)
			e.ReservationMicrosUSD = &amount
		}
		if i == 3 {
			amount := int64(12)
			e.CommittedMicrosUSD = &amount
			e.Outcome = "success"
			e.ProviderInputTokens = 10
		}
		if _, err := native.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	auditPath := filepath.Join(dir, "audit.log")
	a, err := audit.OpenWithOptions(auditPath, key, audit.Options{CacheBudget: manager, CacheRAMBytes: auditlimits.MinCacheRAMBytes})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := a.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := a.Append(ctx, audit.Event{EventID: "evt_audit_common", OccurredAt: now, ActorType: "system", Action: "common.test", Outcome: "success"}); err != nil {
		t.Fatal(err)
	}
	governanceOptions.CacheBudget = manager
	governanceOptions.CacheRAMBytes = governancelimits.MinCacheRAMBytes
	g, err := governance.OpenWithOptions(filepath.Join(dir, "governance.log"), key, governanceOptions)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := g.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := g.Append(ctx, commonGovernanceEvent()); err != nil {
		t.Fatal(err)
	}
	metadataPath := filepath.Join(dir, "metadata.db")
	s, err := boltstore.Open(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AttachMetadataJournal(key, "common-cut-fixture", 16<<20); err != nil {
		t.Fatal(err)
	}
	instanceID, err := s.SeedInstanceID("ins_common_test")
	if err != nil {
		t.Fatal(err)
	}
	info, err := s.Info()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("test: common-cut\n"), 0600); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(dir, "source.hmbk")
	backupKey := bytes.Repeat([]byte{0x39}, 32)
	if _, err := backup.CreatePaged(backup.CreateOptions{OutputPath: archivePath, BackupKey: backupKey, Metadata: info, LedgerFeatureEpoch: 4, MinimumLedgerReaderVersion: "v4", MasterKeyFingerprint: "sha256:" + strings.Repeat("0", 64), PricingStateSHA256: "sha256:" + strings.Repeat("1", 64), PendingIntentSHA256: "sha256:" + strings.Repeat("2", 64), Build: buildinfo.Info{Version: "test"}, Files: []backup.SourceFile{{ArchivePath: "config/config.yaml", LocalPath: configPath}, {ArchivePath: "data/metadata.db", LocalPath: metadataPath}, {ArchivePath: "data/ledger/ledger.wal", LocalPath: nativePath}, {ArchivePath: "data/audit/audit.log", LocalPath: auditPath}}}); err != nil {
		t.Fatal(err)
	}
	archive, err := backup.OpenVerified(ctx, archivePath, backupKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := archive.Close(); err != nil {
			t.Error(err)
		}
	})
	sourceDir := filepath.Join(dir, "source")
	if err := os.Mkdir(sourceDir, 0700); err != nil {
		t.Fatal(err)
	}
	m, err := metadatastate.BuildFromBackup(ctx, archive, key, filepath.Join(sourceDir, "metadata.db"), filepath.Join(dir, "metadata-root"), metadatastate.BaselineOptions{Identity: identity, InstanceID: instanceID, KeyGeneration: 1, Budget: manager, MaxSourceBytes: 64 << 20, MaxDiskBytes: 64 << 20, MaxAppendBytes: 8 << 20})
	if err != nil {
		t.Fatal("authenticated baseline", err)
	}
	t.Cleanup(func() {
		if err := m.Discard(); err != nil {
			t.Error(err)
		}
	})
	if _, err := m.StartNewEpoch(ctx, key, filepath.Join(dir, "metadata.bound.journal"), info.MetadataJournalEpoch+1, 16<<20); err != nil {
		t.Fatal("bound native epoch", err)
	}
	if _, err := m.AppendFrame(ctx, []metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte("authenticated-common-row"), Value: []byte("one")}}); err != nil {
		t.Fatal(err)
	}
	report, partial, err := ledger.InspectReplayAuthenticated(nativePath, key, nil)
	if err != nil || partial {
		t.Fatal(partial, err)
	}
	l, err := ledger.BuildAuthenticatedLedgerProjection(ctx, nativePath, key, filepath.Join(dir, "ledger-root"), report, ledger.AuthenticatedLedgerOptions{Identity: identity, Budget: manager, MaxDiskBytes: 64 << 20, MaxAppendBytes: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := l.Discard(); err != nil {
			t.Error(err)
		}
	})
	lease, err := native.BeginMaintenance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lease.Close(); err != nil {
			t.Error(err)
		}
	})
	u, _, err := usage.BuildTypedUsageProjection(ctx, usage.AuthenticatedUsageOptions{Native: native, Owner: l, Identity: identity, UsageKey: key, Budget: manager, RootDirectory: filepath.Join(dir, "usage-root"), RootMaxDiskBytes: 64 << 20, RootMaxAppendBytes: 16 << 20}, lease)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := u.Discard(); err != nil {
			t.Error(err)
		}
	})
	gp, _, err := governance.BuildTypedGovernanceProjection(ctx, governance.AuthenticatedGovernanceOptions{Native: g, Identity: identity, RootKey: key, Budget: manager, RootDirectory: filepath.Join(dir, "governance-root"), RootMaxDiskBytes: 64 << 20, RootMaxAppendBytes: 8 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := gp.Discard(); err != nil {
			t.Error(err)
		}
	})
	d, err := ledger.BuildAuthenticatedDirectory(ctx, native, lease, l, filepath.Join(dir, "directory-root"), ledger.AuthenticatedDirectoryOptions{Identity: identity, Budget: manager, MaxDiskBytes: 16 << 20, MaxAppendBytes: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := d.Discard(); err != nil {
			t.Error(err)
		}
	})
	reservation, err := manager.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: 32 << 10})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reservation.Release(); err != nil {
			t.Error(err)
		}
	})
	publisher, err := authenticatedstate.OpenSealCandidatePublisher(dir, guard, key, HardLimits(), manager, reservation)
	if err != nil {
		t.Fatal(err)
	}
	f = &commonFixture{dir: dir, key: key, owners: StandaloneOwners{Budget: manager, Guard: guard, Publisher: publisher, NativeLedger: native, Lease: lease, Ledger: l, Directory: d, Usage: u, NativeGovernance: g, Governance: gp, Audit: a, Metadata: m, InstanceID: instanceID, KeyGeneration: 1}}
	t.Cleanup(func() {
		if err := f.owners.Publisher.Close(); err != nil {
			t.Error(err)
		}
		if err := f.owners.Publisher.Retire(); err != nil {
			t.Error(err)
		}
	})
	lr, err := l.SealReference(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mr, err := m.SealReference(ctx)
	if err != nil {
		t.Fatal(err)
	}
	gr, err := gp.TypedSealReference(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ar, err := a.SealReference(ctx, manager, a.Summary())
	if err != nil {
		t.Fatal(err)
	}
	ur, err := u.TypedSealReference(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f.authority = authenticatedstate.SealEligibility{SchemaVersion: 1, ApplyVersion: 1, IndexVersion: 1, KeyDerivation: 1, Instance: identity, Incarnation: [16]byte{1}, Role: authenticatedstate.SealStandalone, Coverage: authenticatedstate.SealCoverageAuthenticated}
	f.authority.Positions[0] = authenticatedstate.SealPosition{Component: authenticatedstate.SealComponentLedger, Generation: lr.Anchor.Head.Generation, Sequence: lr.Anchor.Head.Sequence, Offset: uint64(lr.Anchor.Head.Offset), Epoch: uint64(lr.Anchor.Epoch), Hash: lr.Anchor.Hash}
	f.authority.Positions[1], f.authority.Positions[2], f.authority.Positions[3], f.authority.Positions[4] = mr.Position, gr.Journal.Position, ar.Position, ur.Projection.Position
	hard := HardLimits()
	f.base = authenticatedstate.CandidateSeal{Generation: 1, Revision: 1, MaxPageBytes: hard.MaxPageBytes, MaxValueBytes: hard.MaxValueBytes, MaxTreeHeight: hard.MaxTreeHeight}
	return f
}

func TestCommonStandaloneSealFreezesCompleteNamespaceAndReopensPublisher(t *testing.T) {
	f := newCommonFixture(t)
	fees := f.owners.Budget.Snapshot().Used
	result, err := publishStandalone(context.Background(), f.owners, f.base, f.authority, func(ctx context.Context, candidate authenticatedstate.CandidateSeal) error {
		if candidate.RootCount != 7 || candidate.PositionCount != 5 {
			t.Fatal("incomplete common namespace", candidate.RootCount, candidate.PositionCount)
		}
		if _, err := f.owners.NativeLedger.Append(ctx, ledger.Event{EventID: "evt_frozen_refused", Kind: ledger.EventRequestAccepted, RequestID: "req_refused", ProjectID: "prj_common", PeriodID: "2026-10-06", OccurredAt: time.Now()}); !errors.Is(err, ledger.ErrLedgerMaintenance) {
			t.Fatal("Ledger write entered", err)
		}
		if _, err := f.owners.NativeGovernance.Append(ctx, commonGovernanceEvent()); !errors.Is(err, governance.ErrNativeFrozen) {
			t.Fatal("Governance write entered", err)
		}
		if _, err := f.owners.Audit.Append(ctx, audit.Event{EventID: "evt_refused", OccurredAt: time.Now().UTC(), ActorType: "system", Action: "test.refused", Outcome: "success"}); !errors.Is(err, audit.ErrNativeFrozen) {
			t.Fatal("Audit write entered", err)
		}
		if _, err := f.owners.Metadata.AppendFrame(ctx, nil); !errors.Is(err, authenticatedstate.ErrBusy) {
			t.Fatal("Metadata reducer entered", err)
		}
		return nil
	})
	if err != nil || result.Seal.RootCount != 7 || result.CleanupError() != nil || f.owners.Budget.Snapshot().Used != fees {
		t.Fatal("common publication failed or leaked", err, result.CleanupError(), f.owners.Budget.Snapshot())
	}
	selected, found, err := f.owners.Publisher.Select(f.authority)
	if err != nil || !found || selected != result.Seal {
		t.Fatal("durable seal differs", found, err)
	}
	if err := f.owners.Publisher.Close(); err != nil {
		t.Fatal(err)
	}
	credit, err := f.owners.Publisher.DiskReservation()
	if err != nil {
		t.Fatal(err)
	}
	defer credit.Release()
	if err := f.owners.Guard.Close(); err != nil {
		t.Fatal(err)
	}
	f.owners.Guard, err = lock.Acquire(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	f.owners.Publisher, err = authenticatedstate.OpenSealCandidatePublisher(f.dir, f.owners.Guard, f.key, HardLimits(), f.owners.Budget, credit)
	if err != nil {
		t.Fatal(err)
	}
	selected, found, err = f.owners.Publisher.Select(f.authority)
	if err != nil || !found || selected != result.Seal {
		t.Fatal("reopened seal differs", found, err)
	}
}

func TestCommonStandaloneSealRejectsWrongCutAndOwnerBeforePublication(t *testing.T) {
	f := newCommonFixture(t)
	fees := f.owners.Budget.Snapshot().Used
	for _, tc := range []string{"cut", "identity", "role", "missing-root", "foreign-lock"} {
		t.Run(tc, func(t *testing.T) {
			o, authority := f.owners, f.authority
			switch tc {
			case "cut":
				authority.Positions[0].Sequence++
			case "identity":
				authority.Instance[0] ^= 1
			case "role":
				authority.Role = authenticatedstate.SealPrimary
			case "missing-root":
				o.Directory = nil
			case "foreign-lock":
				var err error
				o.Guard, err = lock.Acquire(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				defer o.Guard.Close()
			}
			if result, err := PublishStandalone(context.Background(), o, f.base, authority); err == nil || result.Seal != (authenticatedstate.CandidateSeal{}) || result.CleanupError() != nil {
				t.Fatal("unqualified common cut accepted", err)
			}
			if _, found, err := f.owners.Publisher.Current(); err != nil || found {
				t.Fatal("refusal published a seal", found, err)
			}
			if f.owners.Budget.Snapshot().Used != fees {
				t.Fatal("known refusal changed fees")
			}
		})
	}
}

func TestCommonStandaloneSealDirectoryRetirementCancelsAndJoinsWholeJob(t *testing.T) {
	f := newCommonFixture(t)
	entered, release, canceled := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	done := make(chan error, 1)
	go func() {
		_, err := publishStandalone(context.Background(), f.owners, f.base, f.authority, func(ctx context.Context, _ authenticatedstate.CandidateSeal) error {
			close(entered)
			<-ctx.Done()
			close(canceled)
			<-release
			return nil
		})
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("complete job did not enter")
	}
	fees := f.owners.Budget.Snapshot().Used
	closed := make(chan error, 1)
	go func() { closed <- f.owners.Directory.Discard() }()
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("actual directory retirement did not cancel publication")
	}
	select {
	case err := <-closed:
		t.Fatal("directory retired before publication joined", err)
	case <-time.After(25 * time.Millisecond):
	}
	if f.owners.Budget.Snapshot().Used != fees {
		t.Fatal("accepted common job refunded before exit")
	}
	close(release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("canceled complete cut accepted", err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if _, found, err := f.owners.Publisher.Current(); err != nil || found {
		t.Fatal("canceled job published", found, err)
	}
}

func TestCommonStandaloneSealReauthenticatesActualOldLedgerBytes(t *testing.T) {
	f := newCommonFixture(t)
	path := filepath.Join(f.dir, "ledger.wal")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil || len(content) < 64 {
		t.Fatal(err)
	}
	content[len(content)-1] ^= 1
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	fees := f.owners.Budget.Snapshot().Used
	if result, err := PublishStandalone(context.Background(), f.owners, f.base, f.authority); err == nil || result.Seal != (authenticatedstate.CandidateSeal{}) {
		t.Fatal("old native tamper accepted", err)
	}
	if f.owners.NativeLedger.Status().Load() != ledger.AccountingRecoveryRequired {
		t.Fatal("actual source corruption did not withdraw Ledger")
	}
	if _, found, err := f.owners.Publisher.Current(); err != nil || found {
		t.Fatal("corrupt source published", found, err)
	}
	if f.owners.Budget.Snapshot().Used != fees {
		t.Fatal("known source refusal leaked fees")
	}
}

func TestCommonStandaloneSealCannotRelabelInstalledReplicationOwner(t *testing.T) {
	f := newCommonFixtureWithGovernanceOptions(t, governance.Options{AfterDurable: func(governance.DurableBatch) error { return nil }})
	fees := f.owners.Budget.Snapshot().Used
	if result, err := PublishStandalone(context.Background(), f.owners, f.base, f.authority); !errors.Is(err, governance.ErrGovernanceSealSource) || result.Seal != (authenticatedstate.CandidateSeal{}) {
		t.Fatal("replication owner relabeled standalone", err)
	}
	if _, found, err := f.owners.Publisher.Current(); err != nil || found {
		t.Fatal("wrong native mode published", found, err)
	}
	if f.owners.Budget.Snapshot().Used != fees {
		t.Fatal("known mode rejection changed fees")
	}
}
