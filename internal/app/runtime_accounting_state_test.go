package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/usage"
)

// The second Err executes inside the actual bbolt read transaction, under the
// projection lifetime RLock. This controlled context stalls real IO ownership;
// an export callback alone holds no physical reader and would not test Close.
type blockedProjectionReadContext struct {
	context.Context
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (c *blockedProjectionReadContext) Err() error {
	if c.calls.Add(1) == 2 {
		close(c.entered)
		<-c.release
	}
	return nil
}

func accountingReaderHead(t *testing.T, reader runtimeAccountingReader) ledger.Watermark {
	t.Helper()
	head, _, err := reader.ReadPosition(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return head
}

func TestStandaloneDefaultDiskOwnerRefusesConfiguredAuthorityCollisions(t *testing.T) {
	for _, kind := range []string{"metadata", "master", "tls-key", "metadata-hardlink", "metadata-symlink"} {
		t.Run(kind, func(t *testing.T) {
			cfg := testConfig(t)
			if kind == "metadata" {
				cfg.Storage.MetadataFile = "ledger-projection.db"
			}
			if err := Initialize(cfg); err != nil {
				t.Fatal(err)
			}
			path := standaloneProjectionPath(cfg)
			file := cfg.MetadataPath()
			switch kind {
			case "master":
				raw, err := os.ReadFile(cfg.Storage.MasterKey.File)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, raw, 0o600); err != nil {
					t.Fatal(err)
				}
				cfg.Storage.MasterKey.File, file = path, path
			case "tls-key":
				if err := os.WriteFile(path, []byte("configured key must survive"), 0o600); err != nil {
					t.Fatal(err)
				}
				cfg.TLS.Certificates = []config.TLSCertificate{{KeyFile: path}}
				file = path
			case "metadata-hardlink":
				if err := os.Link(file, path); err != nil {
					t.Fatal(err)
				}
			case "metadata-symlink":
				if err := os.Symlink(file, path); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if r, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil || r != nil {
				t.Fatal("configured authority collision was accepted", r, err)
			}
			after, err := os.ReadFile(file)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("refusal changed authority bytes", err)
			}
			if _, err := os.Lstat(path); err != nil {
				t.Fatal("refusal removed authority/alias", err)
			}
		})
	}
}

func TestStandaloneFailedOpenKeepsDataLockUntilActualProjectionReaderJoins(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	bad := cfg
	bad.Usage.ExportFormat = "invalid-export-format"
	entered, release, cleanup := make(chan struct{}), make(chan struct{}), make(chan struct{})
	viewResult := make(chan error, 1)
	var once sync.Once
	defer once.Do(func() { close(release) })
	options := OpenOptions{accountingPrepared: func(p *ledger.LedgerProjection) func() {
		blocked := &blockedProjectionReadContext{Context: context.Background(), entered: entered, release: release}
		go func() { _, _, err := p.ReadPosition(blocked); viewResult <- err }()
		awaitDrainSignal(t, entered)
		return func() { close(cleanup) }
	}}
	failed := make(chan error, 1)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	go func() { _, err := OpenWithOptions(context.Background(), bad, logger, options); failed <- err }()
	awaitDrainSignal(t, cleanup)
	// Cleanup has started and the REAL DB borrower still owns the view.
	// A second Open must not acquire the lock/rebuild this same pathname.
	if r, err := Open(context.Background(), cfg, logger); !errors.Is(err, lock.ErrAlreadyLocked) || r != nil {
		if r != nil {
			r.Close()
		}
		t.Fatal("failed startup released directory ownership before DB join", err)
	}
	select {
	case err := <-failed:
		t.Fatal("failed Open returned before borrower joined", err)
	default:
	}
	once.Do(func() { close(release) })
	if err := <-viewResult; err != nil {
		t.Fatal(err)
	}
	if err := <-failed; err == nil {
		t.Fatal("invalid exporter succeeded")
	}
	r, err := Open(context.Background(), cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := os.Stat(standaloneProjectionPath(cfg)); err != nil {
		t.Fatal("old cleanup removed new runtime file", err)
	}
}

func TestStandaloneOwnedProjectionCloseDoesNotRemoveReplacementPath(t *testing.T) {
	r, _, _ := openBootstrappedRuntime(t)
	path := standaloneProjectionPath(r.config)
	moved := filepath.Join(r.config.Storage.DataDir, "moved-derived.db")
	if err := os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}
	other := []byte("replacement belongs to another file")
	if err := os.WriteFile(path, other, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err == nil {
		t.Fatal("replacement path was treated as this owner's file")
	}
	got, err := os.ReadFile(path)
	if err != nil || !reflect.DeepEqual(got, other) {
		t.Fatal("Close removed replacement path", err)
	}
}

func TestStandaloneDefaultDiskOwnerRecoversBothLevelsBeforePublication(t *testing.T) {
	r, bootstrap, reopen := openBootstrappedRuntime(t)
	defer r.Close()
	ctx := context.Background()
	p := r.accountingState.projection
	if p == nil || r.accountingState.reader != p {
		t.Fatal("default Standalone did not select one disk Ledger owner")
	}
	intent := func(name string) budget.GovernanceIntent {
		return budget.GovernanceIntent{Operation: name, IdempotencyKeyHash: outcomePageTestLabel(name), RequestFingerprint: outcomePageTestLabel("fp-" + name)}
	}
	w, _, err := r.accounting.CreateWorkUnit(ctx, bootstrap.ProjectID, bootstrap.KeyID, 10, intent("disk-open-unit"))
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := r.accounting.CreateRun(ctx, bootstrap.ProjectID, bootstrap.KeyID, w.ID, 1000, time.Hour, 10, intent("disk-open-run"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	price, err := domain.NewVersionedPriceSnapshot(domain.DeploymentPriceVersion{
		ID: "price_recovery", DeploymentID: "dep_recovery", Version: 1, Revision: 1,
		BillingMode: domain.BillingModeMetered, Currency: "USD", FormulaVersion: domain.PriceFormulaUSDTokensV1,
		InputMicrosPerMillion: 1_000_000, OutputMicrosPerMillion: 2_000_000,
		EffectiveFrom: now.Add(-time.Hour), CreatedAt: now.Add(-time.Hour), CreatedBy: "test",
		Source: domain.PriceSource{Type: domain.PriceSourceManual, Assurance: domain.PriceAssuranceAsserted,
			ReceivedAt: now.Add(-time.Hour), ContentSHA256: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Reference: "fixture", AssertedWithoutArchive: true},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	requests := make([]budget.Request, 4)
	for i, id := range []string{"req_orphan", "req_started", "req_late", "req_unstarted"} {
		requests[i], err = r.accounting.BeginRequestAttributed(ctx, bootstrap.ProjectID, bootstrap.KeyID, id, "model", w.ID, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			continue
		}
		amount := []int64{0, 7, 11, 13}[i]
		attempt, err := r.accounting.ReserveLeaseDetailed(ctx, requests[i], 1000, budget.LeaseSpec{
			Mode: ledger.LeaseModeMetered, PriceSnapshot: &price, ReservationMicrosUSD: amount, PreparedInputTokens: amount,
			TokenGuardPricingViewDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		}, budget.AttemptMetadata{DeploymentID: "dep_recovery"})
		if err != nil {
			t.Fatal(err)
		}
		if i < 3 {
			if err := r.accounting.MarkStarted(ctx, attempt); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := r.accounting.Finalize(ctx, requests[2], "interrupted"); err != nil {
		t.Fatal(err)
	}
	original, found, err := p.ReadFinalization(ctx, requests[2].RequestID)
	if err != nil || !found {
		t.Fatal(original, found, err)
	}
	if err := r.usageCollector.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	var originalSummary usage.RequestSummary
	for _, summary := range usageSnapshotForTest(t, r).Requests {
		if summary.RequestID == requests[2].RequestID {
			originalSummary = summary
		}
	}
	if originalSummary.Sequence != original.Sequence || originalSummary.Attempts != 0 {
		t.Fatal("fixture did not freeze a terminal summary before settlement", originalSummary)
	}
	before := accountingReaderHead(t, p)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.ReadPosition(ctx); err == nil {
		t.Fatal("terminal Runtime Close retained the owned disk DB")
	}
	path := standaloneProjectionPath(r.config)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("clean close retained a derived file", err)
	}
	// A crash leftover is never opened/trusted. Even invalid bbolt bytes are
	// discarded under the directory lock and replaced from the authenticated WAL.
	if err := os.WriteFile(path, []byte("untrusted saved derived contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	next := reopen()
	defer next.Close()
	if next.accountingState.projection == nil || next.accountingState.projection == p || next.draining.Load() {
		t.Fatal("reopen did not publish a fresh default disk owner")
	}
	head, hash, _ := next.ledger.ChainHead()
	applied, appliedHash, err := next.accountingState.reader.ReadPosition(ctx)
	if err != nil || head != applied || hash != appliedHash || head.Sequence != before.Sequence+6 {
		t.Fatal("recovery publication did not cover three attempts plus three requests", head, applied, err)
	}
	pending, err := next.accounting.ReadPendingLeaseStats(ctx, time.Now())
	if err != nil || pending.Count != 0 {
		t.Fatal(pending, err)
	}
	view, found, err := next.accounting.ReadWorkUnitAccounting(ctx, bootstrap.ProjectID, w.ID, true)
	if err != nil || !found || view.Inflight {
		t.Fatal("orphan request left active accounting", view, found, err)
	}
	balance := accountingReaderBalance(t, next.accountingState.reader, ledger.BalanceKey{ProjectID: bootstrap.ProjectID, PeriodID: requests[0].Period.ID, TimezoneVersion: requests[0].Period.TimezoneVersion})
	if balance.ReservedMicrosUSD != 0 || balance.CommittedMicrosUSD != 18 {
		t.Fatal("recovery lost conservative started costs or retained unstarted reservation", balance)
	}
	final, found, err := next.accountingState.projection.ReadFinalization(ctx, requests[2].RequestID)
	if err != nil || !found || !reflect.DeepEqual(final, original) {
		t.Fatal("late settlement replaced original finalization", final, original, err)
	}
	oracle, err := usage.NewAggregateWithFinalizationReader(next.accountingState.projection)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := next.ledger.Replay(ledger.Watermark{}, oracle.Apply); err != nil {
		t.Fatal(err)
	}
	if usageWatermarkForTest(t, next) != head || next.usageCollector.Stats().Lagging || !reflect.DeepEqual(usageSnapshotForTest(t, next), oracle.Snapshot()) || usageMetricsForTest(t, next) != oracle.Metrics() || usageMetricsForTest(t, next).ActiveRequests != 0 {
		t.Fatalf("Open published before Usage observed recovery suffix: watermark=%v head=%v actual=%#v expected=%#v actualMetrics=%#v expectedMetrics=%#v", usageWatermarkForTest(t, next), head, usageSnapshotForTest(t, next), oracle.Snapshot(), usageMetricsForTest(t, next), oracle.Metrics())
	}
	for _, summary := range usageSnapshotForTest(t, next).Requests {
		if summary.RequestID == requests[2].RequestID && !reflect.DeepEqual(summary, originalSummary) {
			t.Fatal("recovery changed original terminal summary", summary, originalSummary)
		}
	}
	if err := next.Close(); err != nil {
		t.Fatal(err)
	}
	again := reopen()
	defer again.Close()
	if accountingReaderHead(t, again.accountingState.reader) != head || usageMetricsForTest(t, again) != oracle.Metrics() {
		t.Fatal("second restart duplicated recovery facts or Usage totals")
	}
}

func TestStandaloneDefaultDiskOwnerLegacyCrossPeriodLeasesKeepOwnBalances(t *testing.T) {
	r, bootstrap, reopen := openBootstrappedRuntime(t)
	defer r.Close()
	// Write actual old-format facts, without an Accepted event, and apply them
	// to the currently selected owner. This is a native compatibility fixture,
	// not an alternate startup backend or a fabricated modern acceptance.
	r.stopBackgroundWork()
	for i, period := range []string{"legacy_period_1", "legacy_period_2"} {
		e := ledger.Event{EventID: period, Kind: ledger.EventReservationCreated, RequestID: "legacy_multi", AttemptID: period,
			ProjectID: bootstrap.ProjectID, KeyID: bootstrap.KeyID, RequestedModel: "legacy-model", PeriodID: period,
			PeriodTimezone: "UTC", PeriodTimezoneVersion: uint64(i + 1), OccurredAt: time.Now().UTC(), ReservationMicrosUSD: ledger.MicrosUSD(int64(i + 7))}
		record, _, err := r.ledger.AppendRecordWithReceipt(context.Background(), e)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.accountingState.projection.Apply(context.Background(), record); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	next := reopen()
	defer next.Close()
	for i, period := range []string{"legacy_period_1", "legacy_period_2"} {
		balance := accountingReaderBalance(t, next.accountingState.reader, ledger.BalanceKey{ProjectID: bootstrap.ProjectID, PeriodID: period, TimezoneVersion: uint64(i + 1)})
		if balance.ReservedMicrosUSD != 0 || balance.CommittedMicrosUSD != 0 {
			t.Fatal("legacy recovery changed period money", period, balance)
		}
	}
	row, found, err := next.accountingState.projection.ReadRequest(context.Background(), "legacy_multi")
	if err != nil || !found || row.Accepted.Sequence != 0 || row.LegacyLease == nil || row.Finalized == nil {
		t.Fatal("legacy recovery lost real origin or fabricated acceptance", row, found, err)
	}
	if usageMetricsForTest(t, next).ActiveRequests != 0 || len(usageSnapshotForTest(t, next).Requests) != 1 || accountingReaderHead(t, next.accountingState.reader).Sequence != 5 {
		t.Fatal("legacy recovery left activity or duplicated request summary", usageMetricsForTest(t, next), next.ledger.Stats())
	}
	head := accountingReaderHead(t, next.accountingState.reader)
	if err := next.Close(); err != nil {
		t.Fatal(err)
	}
	again := reopen()
	defer again.Close()
	if accountingReaderHead(t, again.accountingState.reader) != head || usageMetricsForTest(t, again).ActiveRequests != 0 {
		t.Fatal("second legacy restart rewrote facts or revived requests")
	}
}

func TestStandaloneDefaultDiskOwnerRebuildsOldCheckpointTotalsAndRollupTogether(t *testing.T) {
	r, bootstrap, reopen := openBootstrappedRuntime(t)
	defer r.Close()
	ctx := context.Background()
	request, err := r.accounting.BeginRequest(ctx, bootstrap.ProjectID, "legacy_duplicate_terminal")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.accounting.Finalize(ctx, request, "interrupted"); err != nil {
		t.Fatal(err)
	}
	if err := r.accounting.Finalize(ctx, request, "interrupted"); err != nil {
		t.Fatal(err)
	}
	if err := r.usageCollector.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	cold, metrics := usageSnapshotForTest(t, r), usageMetricsForTest(t, r)
	if len(cold.Requests) != 1 || metrics.ActiveRequests != 0 || metrics.RequestsError != 1 {
		t.Fatal("selected reducer did not preserve first terminal", cold, metrics)
	}
	r.stopBackgroundWork()
	// Reproduce the previous DEFAULT plain consumer's checkpoint from this
	// exact native WAL. Active=0 means checking phantom rows alone misses it.
	old := usage.NewAggregate()
	if _, err := r.ledger.Replay(ledger.Watermark{}, old.Apply); err != nil {
		t.Fatal(err)
	}
	if old.Metrics().ActiveRequests != 0 || old.Metrics().RequestsError != 2 {
		t.Fatal("old checkpoint fixture did not reproduce doubled totals", old.Metrics())
	}
	snapshot, err := old.TakeCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	if err := r.store.ResetUsageDerivatives(); err != nil {
		t.Fatal(err)
	}
	if err := r.store.PutUsageCheckpoint(snapshot.Watermark, snapshot.Head, usageCheckpointSegments(snapshot), snapshot.RemovedSegments, domain.RollupVersion, snapshot.Rollup); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	next := reopen()
	defer next.Close()
	if !reflect.DeepEqual(usageSnapshotForTest(t, next), cold) || usageMetricsForTest(t, next) != metrics {
		t.Fatal("old checkpoint changed same-WAL default result", usageSnapshotForTest(t, next), usageMetricsForTest(t, next))
	}
	// A normal close publishes the reader-semantics checkpoint. A second Open
	// must keep its exact result rather than importing the old cumulative rows.
	if err := next.Close(); err != nil {
		t.Fatal(err)
	}
	again := reopen()
	defer again.Close()
	if !reflect.DeepEqual(usageSnapshotForTest(t, again), cold) || usageMetricsForTest(t, again) != metrics {
		t.Fatal("same-reader warm restart diverged from cold replay")
	}
}

func TestStandaloneDefaultDiskOwnerFailedOpenCleansCandidateAndReleasesLock(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	bad := cfg
	bad.Usage.ExportFormat = "invalid-export-format"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if runtime, err := Open(context.Background(), bad, logger); err == nil || runtime != nil {
		t.Fatal("later startup failure returned a runtime", runtime, err)
	}
	if _, err := os.Stat(standaloneProjectionPath(cfg)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed Open retained a derived candidate", err)
	}
	runtime, err := Open(context.Background(), cfg, logger)
	if err != nil {
		t.Fatal("failed Open leaked directory lock/native owner", err)
	}
	defer runtime.Close()
	if runtime.accountingState.projection == nil {
		t.Fatal("retry fell back to historical State")
	}
}

func TestStandaloneAccountingOwnerAuthenticationFailureRejectsSavedCandidate(t *testing.T) {
	native, path, _ := projectedWriterLog(t, []ledger.Event{projectedWriterAcceptance("auth_failure")}, ledger.Options{})
	cfg := testConfig(t)
	if err := os.MkdirAll(cfg.Storage.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(standaloneProjectionPath(cfg), []byte("saved owner cannot authorize damaged source"), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 1
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	owner, lease, _, err := prepareRuntimeAccountingState(context.Background(), cfg, native, nil)
	if err == nil || owner.reader != nil || lease != nil || native.Status().Load() != ledger.AccountingRecoveryRequired {
		t.Fatal("damaged native source published an owner", owner, lease, err)
	}
	if _, err := os.Stat(standaloneProjectionPath(cfg)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed authentication retained a candidate", err)
	}
}

func accountingReaderBalance(t *testing.T, reader runtimeAccountingReader, key ledger.BalanceKey) ledger.Balance {
	t.Helper()
	balance, err := reader.ReadBalance(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	return balance
}
