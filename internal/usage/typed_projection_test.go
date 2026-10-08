package usage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
	bbolt "go.etcd.io/bbolt"
)

type typedUsageFixture struct {
	native  *ledger.Log
	lease   *ledger.MaintenanceLease
	owner   *ledger.AuthenticatedLedgerProjection
	options AuthenticatedUsageOptions
	records []ledger.Record
}

func newTypedUsageFixture(t *testing.T, late bool) *typedUsageFixture {
	return newTypedUsageFixtureAt(t, late, 0)
}
func newTypedUsageFixtureAt(t *testing.T, late bool, prefix int) *typedUsageFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	key := bytes.Repeat([]byte{0x54}, 32)
	native, err := ledger.OpenWithOptions(filepath.Join(root, "native.wal"), ledger.NewStatus(), ledger.Options{ChainKey: key})
	if err != nil {
		t.Fatal(err)
	}
	var records []ledger.Record
	fixture := finalizationFixture(late, false)
	if prefix == -1 {
		fixture = nil
	}
	for _, r := range fixture {
		actual, _, err := native.AppendRecordWithReceipt(ctx, r.Event)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, actual)
	}
	report, partial, err := ledger.InspectReplayAuthenticated(filepath.Join(root, "native.wal"), key, nil)
	if err != nil || partial {
		t.Fatal(partial, err)
	}
	limits := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: 384 << 20, Disk: 256 << 20}, MaxClaims: 48, MaxOwners: 8, MaxPressureAge: time.Hour}
	for i := range limits.Classes {
		limits.Classes[i] = limits.Total
	}
	limits.Classes[resourcebudget.Recovery] = resourcebudget.Cost{}
	limits.Classes[resourcebudget.Control] = resourcebudget.Cost{RAM: 48 * resourcebudget.ClaimRAMBytes}
	manager, err := resourcebudget.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	var allowed *ledger.ProjectionPrefix
	if prefix > 0 {
		r := records[prefix-1]
		allowed = &ledger.ProjectionPrefix{Head: typedUsageRecordAnchor(r).Head, Hash: r.Hash}
	}
	owner, err := ledger.BuildAuthenticatedLedgerProjection(ctx, filepath.Join(root, "native.wal"), key, filepath.Join(root, "ledger-root"), report, ledger.AuthenticatedLedgerOptions{Identity: sha256.Sum256([]byte("independent-ledger")), MaxDiskBytes: 64 << 20, MaxAppendBytes: 16 << 20, Budget: manager, Prefix: allowed})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := native.BeginMaintenance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f := &typedUsageFixture{native: native, lease: lease, owner: owner, records: records, options: AuthenticatedUsageOptions{Native: native, Owner: owner, Identity: sha256.Sum256([]byte("independent-usage")), UsageKey: bytes.Repeat([]byte{0x91}, 32), ScratchPath: filepath.Join(root, "foreign-scratch"), ScratchMaxDiskBytes: 1 << 30, RootDirectory: filepath.Join(root, "usage-root"), RootMaxDiskBytes: 64 << 20, RootMaxAppendBytes: 16 << 20, Budget: manager}}
	t.Cleanup(func() {
		if err := owner.Discard(); err != nil {
			t.Error(err)
		}
		if err := lease.Close(); err != nil {
			t.Error(err)
		}
		if err := native.Close(); err != nil {
			t.Error(err)
		}
	})
	return f
}

func compareTypedUsageRows(t *testing.T, typed, ordinary *DiskProjection) {
	t.Helper()
	ctx := context.Background()
	count := 0
	if err := typed.engine.scoped(ctx).View(func(rows *usageTx) error {
		return ordinary.db.View(func(tx *bbolt.Tx) error {
			for _, name := range diskUsageBuckets {
				b := rows.Bucket([]byte(name))
				c := b.Cursor()
				k, v := c.First()
				err := tx.Bucket([]byte(name)).ForEach(func(wantKey, wantValue []byte) error {
					if !bytes.Equal(k, wantKey) || !bytes.Equal(v, wantValue) || !bytes.Equal(b.Get(wantKey), wantValue) {
						return fmt.Errorf("missing/different %s/%x: actual %x", name, wantKey, k)
					}
					count++
					k, v = c.Next()
					return nil
				})
				if err != nil {
					return err
				}
				if k != nil {
					return fmt.Errorf("extra authenticated %s/%x", name, k)
				}
			}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	identity, err := typed.engine.cow.RootIdentity(ctx)
	if err != nil || identity.Rows != uint64(count+1) {
		t.Fatal("incomplete typed namespace", count, identity, err)
	}
}

func TestTypedUsageDirectRowsMatchCompleteOrdinaryReducerAndQueries(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(fmt.Sprint(late), func(t *testing.T) {
			f := newTypedUsageFixture(t, late)
			ctx := context.Background()
			foreign := []byte("this file is unrelated to the typed root")
			if err := os.WriteFile(f.options.ScratchPath, foreign, 0600); err != nil {
				t.Fatal(err)
			}
			typed, ref, err := BuildTypedUsageProjection(ctx, f.options, f.lease)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := typed.Discard(); err != nil {
					t.Error(err)
				}
			})
			if typed.db != nil || typed.file != nil || typed.image != nil || ref.Projection.Schema.Codec != TypedUsageSealCodec {
				t.Fatal("materialized DB retained", ref)
			}
			actual, err := os.ReadFile(f.options.ScratchPath)
			if err != nil || !bytes.Equal(actual, foreign) {
				t.Fatal("foreign scratch modified", err)
			}
			ordinary, err := CreateDiskProjection(filepath.Join(t.TempDir(), "reference.db"), f.owner)
			if err != nil {
				t.Fatal(err)
			}
			defer ordinary.Discard()
			for _, r := range f.records {
				if err := ordinary.ApplyContext(ctx, r); err != nil {
					t.Fatal(err)
				}
			}
			compareTypedUsageRows(t, typed, ordinary)
			stats, err := typed.ReadStats(ctx)
			want, err2 := ordinary.ReadStats(ctx)
			if err != nil || err2 != nil || stats.LogicalBytes <= 0 {
				t.Fatal(stats, err, err2)
			}
			stats.LogicalBytes = want.LogicalBytes
			if stats != want {
				t.Fatal(stats, want)
			}
			a, err := typed.AttemptPage(ctx, AttemptQuery{Limit: 100}, DiskUsageCursor{}, diskTestLimits)
			b, err2 := ordinary.AttemptPage(ctx, AttemptQuery{Limit: 100}, DiskUsageCursor{}, diskTestLimits)
			if err != nil || err2 != nil || !a.Done || !b.Done || !reflect.DeepEqual(a.Items, b.Items) || a.Head != b.Head {
				t.Fatal(a, b, err, err2)
			}
			summaries, err := typed.SummaryPage(ctx, DiskSummaryQuery{Limit: 100}, DiskUsageCursor{}, diskTestLimits)
			expected, err2 := ordinary.SummaryPage(ctx, DiskSummaryQuery{Limit: 100}, DiskUsageCursor{}, diskTestLimits)
			if err != nil || err2 != nil || !summaries.Done || !reflect.DeepEqual(summaries.Items, expected.Items) {
				t.Fatal(summaries, expected, err, err2)
			}
			detail, err := typed.RequestDetailPage(ctx, DiskRequestDetailQuery{RequestID: "req", Limit: 100}, DiskRequestDetailCursor{}, diskTestLimits)
			wantDetail, err2 := ordinary.RequestDetailPage(ctx, DiskRequestDetailQuery{RequestID: "req", Limit: 100}, DiskRequestDetailCursor{}, diskTestLimits)
			if err != nil || err2 != nil || !detail.Done || detail.Found != wantDetail.Found || detail.Summary != wantDetail.Summary || !reflect.DeepEqual(detail.Attempts, wantDetail.Attempts) {
				t.Fatal(detail, wantDetail, err, err2)
			}
			hour := f.records[len(f.records)-1].Event.OccurredAt.UTC().Truncate(time.Hour)
			q, err := typed.ReadHourRequestQuality(ctx, hour, DiskUsagePageLimits{ScanRows: 256, InputBytes: 8 << 20, OutputBytes: 8 << 20})
			wantQ, err2 := ordinary.ReadHourRequestQuality(ctx, hour, DiskUsagePageLimits{ScanRows: 256, InputBytes: 8 << 20, OutputBytes: 8 << 20})
			if err != nil || err2 != nil || q != wantQ {
				t.Fatal(q, wantQ, err, err2)
			}
			current, err := typed.TypedSealReference(ctx)
			if err != nil || current != ref {
				t.Fatal("published typed receipt changed", current, ref, err)
			}
		})
	}
}

func TestTypedUsageCopiedNativeHashCannotAuthorizeForgedPayload(t *testing.T) {
	f := newTypedUsageFixture(t, false)
	ctx := context.Background()
	p, _, err := BuildTypedUsageProjection(ctx, f.options, f.lease)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Discard()
	before, err := p.engine.cow.RootIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	forged := f.records[0]
	forged.Event.ProjectID = "forged_project"
	if err := p.ApplyContext(ctx, forged); !errors.Is(err, ErrDiskUsageCorrupt) {
		t.Fatal("forged native body accepted", err)
	}
	after, err := p.engine.cow.RootIdentity(ctx)
	if err != nil || after != before || p.retired.Load() {
		t.Fatal("rejected input changed or retired valid root", before, after, err)
	}
	if err := p.ApplyContext(ctx, f.records[len(f.records)-1]); err != nil {
		t.Fatal("exact durable retry refused", err)
	}
	after, err = p.engine.cow.RootIdentity(ctx)
	if err != nil || after != before {
		t.Fatal("durable retry double counted", err)
	}
}

func TestTypedUsageSelectedRootReadsWithoutMaterializedEngineAndRejectsMismatchBeforeRepair(t *testing.T) {
	f := newTypedUsageFixture(t, true)
	ctx := context.Background()
	p, ref, err := BuildTypedUsageProjection(ctx, f.options, f.lease)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Discard()
	want, err := p.ReadStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	credit, err := p.TypedDiskReservation()
	if err != nil {
		t.Fatal(err)
	}
	defer credit.Release()
	path := filepath.Join(f.options.RootDirectory, "pages")
	fd, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fd.Write([]byte("uncommitted orphan tail")); err != nil {
		t.Fatal(err)
	}
	if err := fd.Close(); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fees := f.options.Budget.Snapshot().Used
	for _, kind := range []string{"schema", "scope", "position", "digest"} {
		t.Run(kind, func(t *testing.T) {
			bad := ref
			switch kind {
			case "schema":
				bad.Projection.Schema.Codec++
			case "scope":
				bad.Projection.Scope[0] ^= 1
			case "position":
				bad.Projection.Position.Sequence++
			case "digest":
				bad.Projection.Root.Digest[0] ^= 1
			}
			if reopened, err := OpenTypedUsageProjection(ctx, f.options, f.lease, bad, credit); err == nil || reopened != nil {
				t.Fatal("mismatch accepted", reopened, err)
			}
			actual, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(actual, original) || f.options.Budget.Snapshot().Used != fees {
				t.Fatal("mismatch repaired tail or refunded fee", err)
			}
		})
	}
	reopened, err := OpenTypedUsageProjection(ctx, f.options, f.lease, ref, credit)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.db != nil || reopened.image != nil || reopened.file != nil {
		t.Fatal("materialized engine reopened")
	}
	actual, err := reopened.ReadStats(ctx)
	if err != nil || actual != want {
		t.Fatal(actual, want, err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.ReadStats(ctx); !errors.Is(err, ErrDiskUsageClosed) {
		t.Fatal(err)
	}
}

func TestTypedUsageAuthenticationErrorNeverReturnsFalseAbsence(t *testing.T) {
	f := newTypedUsageFixture(t, false)
	ctx := context.Background()
	p, _, err := BuildTypedUsageProjection(ctx, f.options, f.lease)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Discard()
	// Deleting a referenced committed pack is a real source fault, not a callback
	// that returns a synthesized error. Even an otherwise absent lookup must fail.
	path := filepath.Join(f.options.RootDirectory, "pages")
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	if result, found, err := p.ReadRequest(ctx, "absent"); err == nil || found || result != (RequestSummary{}) {
		t.Fatal("authentication failure became absence", result, found, err)
	}
	if _, err := p.ReadStats(ctx); err == nil {
		t.Fatal("failed authenticated owner still readable")
	}
}

func TestTypedUsageNativeDeltaAppliesOnlySelectedPrefixAndPreservesLateSettlement(t *testing.T) {
	f := newTypedUsageFixtureAt(t, true, 3)
	ctx := context.Background()
	p, ref, err := BuildTypedUsageProjection(ctx, f.options, f.lease)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Discard()
	if ref.Ledger.Anchor.Head.Sequence != 3 {
		t.Fatal(ref)
	}
	d := &LedgerDerivative{owner: f.owner, disk: p}
	p.reader = d
	defer d.Close()
	last := f.records[len(f.records)-1]
	cut := ledger.ProjectionPrefix{Head: typedUsageRecordAnchor(last).Head, Hash: last.Hash}
	if _, err := f.owner.AdvanceFromNative(ctx, f.native, cut); err != nil {
		t.Fatal(err)
	}
	receipt, err := d.CatchUpAuthenticatedFromNative(ctx, f.native, f.lease, f.options.Budget)
	if err != nil || receipt.From.Head.Sequence != 3 || receipt.To.Head.Sequence != 5 || receipt.AppliedRecords != 2 || receipt.VerifiedRecords != 6 {
		t.Fatal(receipt, err)
	}
	ordinary, err := CreateDiskProjection(filepath.Join(t.TempDir(), "reference.db"), f.owner)
	if err != nil {
		t.Fatal(err)
	}
	defer ordinary.Discard()
	for _, r := range f.records {
		if err := ordinary.ApplyContext(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	compareTypedUsageRows(t, p, ordinary)
	if stats, err := p.ReadStats(ctx); err != nil || stats.Metrics.ActiveRequests != 0 || stats.Totals.Requests != 1 || stats.Totals.Attempts != 1 || stats.Totals.CostMicrosUSD != 7 {
		t.Fatal(stats, err)
	}
	if retry, err := d.CatchUpAuthenticatedFromNative(ctx, f.native, f.lease, f.options.Budget); err != nil || retry.AppliedRecords != 0 || retry.From != retry.To {
		t.Fatal(retry, err)
	}
}

func TestTypedUsageCannotSkipNativeFactsAndBatchPublishesOneRoot(t *testing.T) {
	f := newTypedUsageFixtureAt(t, false, 3)
	ctx := context.Background()
	p, _, err := BuildTypedUsageProjection(ctx, f.options, f.lease)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Discard()
	last := f.records[len(f.records)-1]
	if _, err := f.owner.AdvanceFromNative(ctx, f.native, ledger.ProjectionPrefix{Head: typedUsageRecordAnchor(last).Head, Hash: last.Hash}); err != nil {
		t.Fatal(err)
	}
	before, err := p.engine.cow.RootIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.ApplyContext(ctx, last); !errors.Is(err, ErrDiskUsageCorrupt) {
		t.Fatal("jump over unapplied native facts accepted", err)
	}
	unchanged, err := p.engine.cow.RootIdentity(ctx)
	if err != nil || unchanged != before || p.retired.Load() || f.native.Status().Load() != ledger.AccountingHealthy {
		t.Fatal(before, unchanged, err)
	}
	if err := p.ApplyReplayBatchContext(ctx, f.records[3:]); err != nil {
		t.Fatal(err)
	}
	after, err := p.engine.cow.RootIdentity(ctx)
	if err != nil || after.Revision != before.Revision+1 {
		t.Fatal("batch was not one atomic root", before, after, err)
	}
	if anchor, err := p.ReadReplayAnchor(ctx); err != nil || anchor != typedUsageRecordAnchor(last) {
		t.Fatal(anchor, err)
	}
	before = after
	if err := p.ApplyReplayBatchContext(ctx, f.records[3:]); err != nil {
		t.Fatal(err)
	}
	after, err = p.engine.cow.RootIdentity(ctx)
	if err != nil || after != before {
		t.Fatal("replay retry published another root", err)
	}
}

func TestTypedUsageWindowMaintenanceKeepsNativeCutAndCumulativeTotals(t *testing.T) {
	f := newTypedUsageFixture(t, false)
	ctx := context.Background()
	p, ref, err := BuildTypedUsageProjection(ctx, f.options, f.lease)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Discard()
	before, err := p.ReadStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	done := false
	for i := 0; i < 10; i++ {
		step, err := p.pruneWindowStep(ctx, f.records[len(f.records)-1].Event.OccurredAt.Add(24*time.Hour), before.Watermark.Sequence, DiskWindowLimits{Rows: 128, InputBytes: 8 << 20})
		if err != nil {
			t.Fatal(step, err)
		}
		if step.Done {
			done = true
			break
		}
	}
	if !done {
		t.Fatal("finite window steps did not finish")
	}
	after, err := p.ReadStats(ctx)
	if err != nil || after.Watermark != before.Watermark || after.Totals != before.Totals || after.Metrics != before.Metrics || after.Window.Attempts != 0 || after.Window.Summaries != 0 || after.Window.Epoch <= before.Window.Epoch {
		t.Fatal(before, after, err)
	}
	current, err := p.TypedSealReference(ctx)
	if err != nil || current.Ledger != ref.Ledger || current.Projection.Position != ref.Projection.Position || current.Projection.Root.Digest == ref.Projection.Root.Digest {
		t.Fatal(ref, current, err)
	}
	attempts, err := p.AttemptPage(ctx, AttemptQuery{Limit: 100}, DiskUsageCursor{}, diskTestLimits)
	if err != nil || len(attempts.Items) != 0 {
		t.Fatal(attempts, err)
	}
	if head, hash, valid := f.native.ChainHead(); !valid || head != ref.Ledger.Anchor.Head || hash != ref.Ledger.Anchor.Hash {
		t.Fatal("derived pruning changed native facts", head, hash, valid)
	}
}

func TestTypedUsageEmptyNativeAndPhysicalRollBoundaryPreserveCompleteCut(t *testing.T) {
	for _, empty := range []bool{true, false} {
		t.Run(fmt.Sprint(empty), func(t *testing.T) {
			prefix := 0
			if empty {
				prefix = -1
			}
			f := newTypedUsageFixtureAt(t, false, prefix)
			ctx := context.Background()
			p, ref, err := BuildTypedUsageProjection(ctx, f.options, f.lease)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Discard()
			if anchor, err := p.ReadReplayAnchor(ctx); err != nil || anchor != ref.Ledger.Anchor {
				t.Fatal(anchor, ref, err)
			}
			if empty {
				if ref.Ledger.Anchor.Head.Sequence != 0 || ref.Projection.Root.Rows != 3 {
					t.Fatal(ref)
				}
				return
			}
			// The lease is released only to perform a real native roll. A fresh lease
			// then binds the same source; no scalar hint fabricates a roll boundary.
			if err := f.lease.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := f.native.Roll(); err != nil {
				t.Fatal(err)
			}
			next, err := f.native.BeginMaintenance(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer next.Close()
			f.lease = next
			p.engine.lease = next
			head, hash, _ := f.native.ChainHead()
			if _, err := f.owner.AdvanceFromNative(ctx, f.native, ledger.ProjectionPrefix{Head: head, Hash: hash}); err != nil {
				t.Fatal(err)
			}
			d := &LedgerDerivative{owner: f.owner, disk: p}
			p.reader = d
			defer d.Close()
			receipt, err := d.CatchUpAuthenticatedFromNative(ctx, f.native, next, f.options.Budget)
			if err != nil || receipt.AppliedRecords != 0 || receipt.To.Head != head || receipt.To.Hash != hash {
				t.Fatal(receipt, err)
			}
			after, err := p.ReadStats(ctx)
			if err != nil || after.Watermark != head || after.Totals.Requests != 1 || after.Totals.Attempts != 1 {
				t.Fatal(after, err)
			}
			current, err := p.TypedSealReference(ctx)
			if err != nil || current.Ledger.Anchor.Head != head {
				t.Fatal(current, err)
			}
		})
	}
}

func TestTypedUsageCloseCancelsAndJoinsEntireNativeDeltaJob(t *testing.T) {
	f := newTypedUsageFixtureAt(t, false, 3)
	ctx := context.Background()
	p, _, err := BuildTypedUsageProjection(ctx, f.options, f.lease)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Discard()
	last := f.records[len(f.records)-1]
	if _, err := f.owner.AdvanceFromNative(ctx, f.native, ledger.ProjectionPrefix{Head: typedUsageRecordAnchor(last).Head, Hash: last.Hash}); err != nil {
		t.Fatal(err)
	}
	d := &LedgerDerivative{owner: f.owner, disk: p}
	p.reader = d
	defer d.Close()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	defer finish()
	job := make(chan error, 1)
	go func() {
		_, err := d.catchUpTypedAuthenticated(ctx, f.native, f.lease, f.options.Budget, func(phase string) {
			if phase == "source-verified" {
				close(entered)
				<-release
			}
		})
		job <- err
	}()
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- p.Close() }()
	deadline := time.Now().Add(time.Second)
	for !p.retired.Load() {
		if time.Now().After(deadline) {
			t.Fatal("Close did not withdraw admission")
		}
		runtime.Gosched()
	}
	select {
	case err := <-closed:
		t.Fatal("Close returned before complete source job", err)
	default:
	}
	finish()
	if err := <-job; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if f.native.Status().Load() != ledger.AccountingHealthy {
		t.Fatal("component cancellation poisoned native facts")
	}
	if _, err := p.ReadStats(ctx); !errors.Is(err, ErrDiskUsageClosed) {
		t.Fatal(err)
	}
}

func TestTypedUsageCallerWidthAndCandidateCapacityRejectBeforeAuthorityChanges(t *testing.T) {
	f := newTypedUsageFixture(t, false)
	ctx := context.Background()
	before := f.options.Budget.Snapshot().Used
	tiny := f.options
	tiny.RootMaxDiskBytes = 1024
	if candidate, ref, err := BuildTypedUsageProjection(ctx, tiny, f.lease); err == nil || candidate != nil || ref != (TypedUsageReference{}) {
		t.Fatal("insufficient candidate capacity accepted", candidate, ref, err)
	}
	if f.options.Budget.Snapshot().Used != before || f.native.Status().Load() != ledger.AccountingHealthy {
		t.Fatal("failed candidate refunded someone else's credit or retired native facts", before, f.options.Budget.Snapshot())
	}
	if _, err := os.Lstat(tiny.RootDirectory); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("known failed candidate namespace retained", err)
	}
	p, _, err := BuildTypedUsageProjection(ctx, f.options, f.lease)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Discard()
	before = f.options.Budget.Snapshot().Used
	forged := f.records[0]
	forged.Event.ProjectID = strings.Repeat("x", 2<<20)
	if err := p.ApplyContext(ctx, forged); !errors.Is(err, ErrDiskUsageLimit) {
		t.Fatal(err)
	}
	if f.options.Budget.Snapshot().Used != before || p.retired.Load() || f.native.Status().Load() != ledger.AccountingHealthy {
		t.Fatal("pre-encoding rejection leaked claim or altered authority")
	}
}

func TestTypedUsageActualOldNativeTamperWithdrawsAllReadsDespiteRestoredTime(t *testing.T) {
	f := newTypedUsageFixture(t, false)
	ctx := context.Background()
	p, _, err := BuildTypedUsageProjection(ctx, f.options, f.lease)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Discard()
	path := filepath.Join(filepath.Dir(f.options.RootDirectory), "native.wal")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	fd, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	var original [1]byte
	if _, err := fd.ReadAt(original[:], 100); err != nil {
		t.Fatal(err)
	}
	changed := []byte{original[0] ^ 1}
	if _, err := fd.WriteAt(changed, 100); err != nil {
		t.Fatal(err)
	}
	if err := fd.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := p.ApplyContext(ctx, f.records[len(f.records)-1]); err == nil {
		t.Fatal("actual historical byte tampering accepted")
	}
	if !p.retired.Load() || f.native.Status().Load() != ledger.AccountingRecoveryRequired {
		t.Fatal("actual source fault did not withdraw both owners")
	}
	if stats, err := p.ReadStats(ctx); err == nil || stats != (DiskUsageStats{}) {
		t.Fatal("retired typed owner exposed old statistics", stats, err)
	}
}
