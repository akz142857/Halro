package app

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/replication"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/usage"
)

type generationNativeSink struct{ native *ledger.Log }

func (s generationNativeSink) Persist(f replication.Frame) error {
	if f.Kind == replication.KindLeadershipEstablished {
		return nil
	}
	return s.native.AppendReplicated(f.StoreGeneration, f.StoreSequenceFirst, f.StoreSequenceLast, f.Payload)
}

func replicaGenerationFixture(t *testing.T, rolled ...bool) (*replicationRuntime, *ledger.Log, *boltstore.Store, string) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	key := bytes.Repeat([]byte{0x52}, 32)
	var frames []replication.Frame
	segments := make(map[uint64]ledger.Segment)
	withRoll := len(rolled) > 0 && rolled[0]
	primary, err := ledger.OpenWithOptions(filepath.Join(root, "primary", "ledger.wal"), ledger.NewStatus(), ledger.Options{ChainKey: key, MaxBatch: 1,
		AfterDurable: func(b ledger.DurableBatch) (uint64, error) {
			frames = append(frames, replication.Frame{Kind: replication.KindData, Store: replication.StoreLedger, StoreGeneration: b.Generation, StoreSequenceFirst: b.FirstSequence, StoreSequenceLast: b.LastSequence, Payload: append([]byte(nil), b.Frames...)})
			return uint64(len(frames)), nil
		},
		WaitConfirmed: func(context.Context, uint64) error { return nil },
		BeforeRoll:    func(uint64, uint64) error { return nil },
		AfterRoll: func(segment ledger.Segment) error {
			decode := func(value string) (result [32]byte) {
				if value == "" {
					return result
				}
				raw, err := hex.DecodeString(value)
				if err != nil || len(raw) != len(result) {
					t.Fatal("invalid native roll hash", value, err)
				}
				copy(result[:], raw)
				return result
			}
			roll := replication.LedgerRollMetadata{Generation: segment.Generation, FirstSequence: segment.FirstSequence, LastSequence: segment.LastSequence, Length: uint64(segment.Length), EndEpoch: uint64(segment.EndEpoch), StartHash: decode(segment.StartHash), EndHash: decode(segment.EndHash), PlainChecksum: decode(segment.PlainChecksum), SealedAtUnix: segment.SealedAt.UnixNano()}
			encoded, err := roll.MarshalBinary()
			if err != nil {
				return err
			}
			segments[segment.Generation] = segment
			frames = append(frames, replication.Frame{Kind: replication.KindLedgerRoll, Store: replication.StoreNone, Metadata: encoded})
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	base := projectedWriterAcceptance("candidate")
	for i, kind := range []ledger.EventKind{ledger.EventRequestAccepted, ledger.EventReservationCreated, ledger.EventAttemptStarted, ledger.EventRequestFinalized, ledger.EventAttemptSettled} {
		e := base
		e.Kind = kind
		e.EventID = "candidate_" + string(rune('a'+i))
		e.OccurredAt = e.OccurredAt.Add(time.Duration(i) * time.Second)
		switch kind {
		case ledger.EventReservationCreated:
			e.AttemptID = "att_candidate"
			e.ReservationMicrosUSD = ledger.MicrosUSD(100)
		case ledger.EventAttemptStarted:
			e.AttemptID = "att_candidate"
		case ledger.EventAttemptSettled:
			e.AttemptID = "att_candidate"
			e.CommittedMicrosUSD = ledger.MicrosUSD(7)
			e.Outcome = "recovered_started_unknown_result"
			e.FailurePhase = "accounting"
			e.TokenEstimated = true
			e.CostEstimated = true
			e.Ambiguous = true
		case ledger.EventRequestFinalized:
			e.Outcome = "interrupted"
		}
		if _, err := primary.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
		if withRoll && i == 3 {
			if _, err := primary.Roll(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := primary.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "replica", "ledger.wal")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	native, err := ledger.OpenWithOptions(path, ledger.NewStatus(), ledger.Options{ChainKey: key, Replica: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = native.Close() })
	journal, err := replication.OpenOrderingJournal(filepath.Join(root, "ordering.journal"), key, replication.OrderingHeader{ClusterID: "production-a", Incarnation: "inc_01"}, 0, [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	durableIndex := uint64(len(frames)) + 1
	for index := uint64(1); index <= durableIndex; index++ {
		f := replication.Frame{Kind: replication.KindLeadershipEstablished}
		if index > 1 {
			f = frames[index-2]
			if f.Kind == replication.KindData {
				if err := native.AppendReplicated(f.StoreGeneration, f.StoreSequenceFirst, f.StoreSequenceLast, f.Payload); err != nil {
					t.Fatal(err)
				}
			} else {
				roll, err := replication.DecodeLedgerRollMetadata(f.Metadata)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := native.ApplyReplicatedRoll(segments[roll.Generation]); err != nil {
					t.Fatal(err)
				}
			}
		}
		f.ClusterID, f.Incarnation, f.Index, f.Term = "production-a", "inc_01", index, 7
		_, digest, err := f.MarshalBinaryAndDigest()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := journal.Append(replication.OrderingRecord{Kind: f.Kind, Store: f.Store, StoreGeneration: f.StoreGeneration, StoreSequenceFirst: f.StoreSequenceFirst, StoreSequenceLast: f.StoreSequenceLast, Index: f.Index, Term: f.Term, FrameDigest: digest, Metadata: f.Metadata}); err != nil {
			t.Fatal(err)
		}
	}
	_, _, mac := journal.Head()
	member := replication.MemberState{Version: replication.StateVersion, ClusterID: "production-a", Incarnation: "inc_01", NodeID: "halro-1", Role: replication.RoleReplica, Term: 7, PromisedTerm: 7, DurableIndex: durableIndex, ConfirmedIndex: durableIndex, AppliedIndex: durableIndex - 1, OrderingHeadMAC: mac, Projection: replication.ProjectionState{Index: durableIndex - 1}, Peers: []replication.StatePeer{{Name: "halro-0", Address: "127.0.0.1:9310", SPKISHA256: "sha256:" + strings.Repeat("a", 64)}}}
	publisher, err := replication.NewStatePublisher(filepath.Join(root, "state.json"), key, member)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(publisher.Close)
	seed, err := boltstore.Open(filepath.Join(root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	metadata, err := boltstore.OpenReplica(filepath.Join(root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = metadata.Close() })
	return &replicationRuntime{role: replication.RoleReplica, journal: journal, publisher: publisher}, native, metadata, filepath.Join(root, "candidate.db")
}

func TestReplicaAccountingGenerationFreshRoleCutAndLateConfirmedApply(t *testing.T) {
	r, native, metadata, output := replicaGenerationFixture(t)
	ctx := context.Background()
	// Local, untrusted derived content must neither seed the candidate nor be
	// reset before a coherent generation publication owns this Metadata view.
	oldHead := []byte("untrusted old Usage checkpoint")
	oldWM := ledger.Watermark{Generation: 1, Sequence: 99, Offset: 9999}
	if err := metadata.PutUsageCheckpoint(oldWM, oldHead, nil, nil, domain.RollupVersion, nil); err != nil {
		t.Fatal(err)
	}
	before := r.publisher.Snapshot()
	g, err := r.prepareReplicaAccountingGeneration(ctx, native, metadata, output)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close() })
	if !reflect.DeepEqual(before, r.publisher.Snapshot()) {
		t.Fatal("candidate construction published member")
	}
	a := g.usageDisk
	snap := replicaDiskUsageSnapshot(t, a)
	if snap.Watermark.Sequence != 4 || snap.Totals.Requests != 1 || snap.Totals.Attempts != 0 || replicaDiskUsageMetrics(t, a).ActiveRequests != 0 {
		t.Fatal("durable settlement entered candidate", snap)
	}
	query := usage.FailureQuery{Limit: 1, RequestID: "candidate"}
	limits := usage.DiskUsagePageLimits{ScanRows: 8, InputBytes: 1 << 20, OutputBytes: 1 << 20}
	failures, err := a.FailurePage(ctx, query, usage.DiskFailureCursor{}, limits)
	if err != nil || failures.Head.Sequence != 4 || len(failures.Items) != 1 || failures.Items[0].LastFailure != nil {
		t.Fatal("durable settlement leaked into allowed failure query", failures, err)
	}
	detailQuery := usage.DiskRequestDetailQuery{RequestID: "candidate", Limit: 1}
	detail, err := a.RequestDetailPage(ctx, detailQuery, usage.DiskRequestDetailCursor{}, limits)
	if err != nil || !detail.Found || !detail.Done || detail.Head.Sequence != 4 || detail.SummarySequence != 4 || detail.Summary.Attempts != 0 || len(detail.Attempts) != 0 {
		t.Fatal("durable settlement leaked into allowed request detail", detail, err)
	}
	qualityPeriod := usage.Period{ID: detail.Summary.PeriodID, TimezoneVersion: detail.Summary.PeriodTimezoneVersion}
	qualityLimits := usage.DiskUsagePageLimits{ScanRows: 256, InputBytes: 1 << 20, OutputBytes: 1 << 20}
	qualityBefore, err := a.ReadPeriodRequestQuality(ctx, qualityPeriod, qualityLimits)
	if err != nil || qualityBefore.Watermark.Sequence != 4 || qualityBefore.Bucket.Requests != 1 || qualityBefore.Bucket.RequestErrors != 1 || qualityBefore.Bucket.RequestLatencySamples != 1 || qualityBefore.Bucket.RequestLatencyP95Millis != detail.Summary.CompletedAt.Sub(detail.Summary.AcceptedAt).Milliseconds() {
		t.Fatal("candidate request quality disagrees with finalized prefix", qualityBefore, err)
	}
	dashboardLimits := usage.DiskDashboardLimits{Hours: 256, ScanRows: 65536, InputBytes: 1 << 20, OutputBytes: 1 << 20}
	dashboardBefore, err := a.ReadDashboard(ctx, detail.Summary.CompletedAt, qualityPeriod, dashboardLimits)
	if err != nil || dashboardBefore.Head.Sequence != 4 || dashboardBefore.Dashboard.Today.Requests != 1 || dashboardBefore.Dashboard.Today.Attempts != 0 || len(dashboardBefore.Dashboard.RecentAnomalies) != 0 {
		t.Fatal("durable settlement leaked into candidate Dashboard", dashboardBefore, err)
	}
	rollupQuery := usage.DiskRollupQuery{Start: qualityPeriod.ID, End: qualityPeriod.ID, Dimensions: []string{domain.RollupDimensionTotal, domain.RollupDimensionProject}, Limit: 1}
	rollupFirst, err := a.RollupPage(ctx, rollupQuery, usage.DiskRollupCursor{}, limits)
	if err != nil || rollupFirst.Head.Sequence != 4 || rollupFirst.Done || len(rollupFirst.Items) != 1 || rollupFirst.Items[0].Row.Requests != 1 || rollupFirst.Items[0].Row.Attempts != 0 {
		t.Fatal("durable settlement leaked into allowed rollup page", rollupFirst, err)
	}
	wm, payload, err := metadata.UsageCheckpoint()
	if err != nil || wm != oldWM || !bytes.Equal(payload, oldHead) {
		t.Fatal("candidate mutated shared derived Metadata", wm, err)
	}
	receiver, err := replication.NewReplicaReceiver(before.ClusterID, before.Incarnation, before.NodeID, "halro-0", before.Term, before.PromisedTerm, before.ConfirmedIndex, before.AppliedIndex, r.journal, generationNativeSink{native}, replication.ReplicaReceiverOptions{PersistState: r.publisher.PublishReplica, Projection: before.Projection})
	if err != nil {
		t.Fatal(err)
	}
	applier, err := replication.NewReplicaApplier(receiver, r.journal, g)
	if err != nil {
		t.Fatal(err)
	}
	if applied, err := applier.ApplyConfirmed(ctx); err != nil || applied != 6 {
		t.Fatal(applied, err)
	}
	snap = replicaDiskUsageSnapshot(t, a)
	if snap.Watermark.Sequence != 5 || snap.Totals.Attempts != 1 || snap.Totals.CostMicrosUSD != 7 || snap.Requests[0].Attempts != 0 || replicaDiskUsageMetrics(t, a).ActiveRequests != 0 {
		t.Fatal("late settlement altered terminal facts", snap)
	}
	failures, err = a.FailurePage(ctx, query, usage.DiskFailureCursor{}, limits)
	if err != nil || failures.Head.Sequence != 5 || len(failures.Items) != 1 || failures.Items[0].Attempts != 0 || failures.Items[0].LastFailure == nil || failures.Items[0].LastFailure.AttemptID != "att_candidate" || failures.Items[0].LastFailure.FailurePhase != "accounting" || !failures.Items[0].LastFailure.Ambiguous {
		t.Fatal("confirmed late settlement missing from failure context", failures, err)
	}
	detail, err = a.RequestDetailPage(ctx, detailQuery, usage.DiskRequestDetailCursor{}, limits)
	if err != nil || !detail.Found || detail.Head.Sequence != 5 || detail.SummarySequence != 4 || detail.Summary.Sequence != 4 || detail.Summary.Attempts != 0 || len(detail.Attempts) != 1 || detail.Attempts[0].AttemptID != "att_candidate" || detail.Attempts[0].FailurePhase != "accounting" {
		t.Fatal("confirmed late settlement changed terminal detail summary", detail, err)
	}
	qualityAfter, err := a.ReadPeriodRequestQuality(ctx, qualityPeriod, qualityLimits)
	if err != nil || qualityAfter.Watermark.Sequence != 5 || qualityAfter.Bucket != qualityBefore.Bucket {
		t.Fatal("confirmed late settlement changed finalized quality", qualityBefore, qualityAfter, err)
	}
	dashboardAfter, err := a.ReadDashboard(ctx, detail.Summary.CompletedAt, qualityPeriod, dashboardLimits)
	if err != nil || dashboardAfter.Head.Sequence != 5 || dashboardAfter.Dashboard.Today.Requests != 1 || dashboardAfter.Dashboard.Today.Attempts != 1 || dashboardAfter.Dashboard.Today.CostMicrosUSD != 7 || len(dashboardAfter.Dashboard.RecentAnomalies) != 1 || dashboardAfter.Dashboard.Breakdowns["project"]["calls"][0].Calls != 1 {
		t.Fatal("confirmed late settlement missing from candidate Dashboard", dashboardAfter, err)
	}
	rollupOld, err := a.RollupPage(ctx, rollupQuery, rollupFirst.Next, limits)
	if err != nil || rollupOld.Head.Sequence != 4 || len(rollupOld.Items) != 1 || rollupOld.Items[0].Row.Requests != 1 || rollupOld.Items[0].Row.Attempts != 0 {
		t.Fatal("confirmed late settlement crossed old rollup cursor", rollupOld, err)
	}
	rollupQuery.Limit = 100
	rollupFresh, err := a.RollupPage(ctx, rollupQuery, usage.DiskRollupCursor{}, limits)
	if err != nil || rollupFresh.Head.Sequence != 5 || len(rollupFresh.Items) != 2 || rollupFresh.Items[0].Row.Requests != 1 || rollupFresh.Items[0].Row.Attempts != 1 || rollupFresh.Items[0].Row.CostMicrosUSD != 7 {
		t.Fatal("confirmed settlement missing from fresh rollup page", rollupFresh, err)
	}
	disk := g.ledger
	usageDisk := g.usageDisk
	if g.usage.Aggregate() != nil {
		t.Fatal("candidate retained historical Usage Aggregate")
	}
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	if err := g.ApplyLedgerThrough(ctx, 1, 5); !errors.Is(err, errReplicaAccountingRetired) {
		t.Fatal("retired applier regained access", err)
	}
	if g.ledger != nil || g.usage != nil || g.usageDisk != nil || g.projection != nil {
		t.Fatal("retired generation retained components")
	}
	if _, _, err := disk.ReadPosition(ctx); err == nil {
		t.Fatal("derived DB not closed")
	}
	if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("owned candidate file retained", err)
	}
	if _, err := os.Stat(output + ".usage"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("owned Usage file retained", err)
	}
	if _, err := usageDisk.ReadStats(ctx); !errors.Is(err, usage.ErrDiskUsageClosed) {
		t.Fatal("old Usage disk alias remained live", err)
	}
	if _, err := native.ReplayAuthenticated(ctx, ledger.ReplayAnchor{}, nil); err != nil {
		t.Fatal("shared native closed", err)
	}
	wm, payload, err = metadata.UsageCheckpoint()
	if err != nil || wm != oldWM || !bytes.Equal(payload, oldHead) {
		t.Fatal("shared Metadata closed/mutated", err)
	}
}

func TestReplicaAccountingGenerationDiskUsageFailurePreservesExistingPath(t *testing.T) {
	r, native, metadata, output := replicaGenerationFixture(t)
	existing := []byte("foreign file, not this generation")
	if err := os.WriteFile(output+".usage", existing, 0o600); err != nil {
		t.Fatal(err)
	}
	if g, err := r.prepareReplicaAccountingGeneration(context.Background(), native, metadata, output); g != nil || err == nil {
		t.Fatal("existing Usage file trusted or replaced", g, err)
	}
	if got, err := os.ReadFile(output + ".usage"); err != nil || !bytes.Equal(got, existing) {
		t.Fatal("failure cleanup removed unowned file", err)
	}
	if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed Ledger candidate retained", err)
	}
	if _, err := metadata.ReadMetadataAppliedPosition(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestReplicaAccountingGenerationClosePreservesReplacedOwnedPaths(t *testing.T) {
	for _, suffix := range []string{"", ".usage"} {
		t.Run(map[string]string{"": "ledger", ".usage": "usage"}[suffix], func(t *testing.T) {
			r, native, metadata, output := replicaGenerationFixture(t)
			g, err := r.prepareReplicaAccountingGeneration(context.Background(), native, metadata, output)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = g.Close() })
			path := output + suffix
			if err := os.Rename(path, path+".moved"); err != nil {
				t.Fatal(err)
			}
			foreign := []byte("unrelated replacement")
			if err := os.WriteFile(path, foreign, 0600); err != nil {
				t.Fatal(err)
			}
			first := g.Close()
			if first == nil || g.Close() != first {
				t.Fatal("cleanup failure was not retained", first)
			}
			if actual, err := os.ReadFile(path); err != nil || !bytes.Equal(actual, foreign) {
				t.Fatal("replacement deleted", err)
			}
			if _, err := os.Stat(path + ".moved"); err != nil {
				t.Fatal("moved physical owner removed", err)
			}
			if g.ledger != nil || g.usageDisk != nil || g.projection != nil {
				t.Fatal("failed cleanup retained live resources")
			}
		})
	}
}

func TestReplicaAccountingGenerationCancelAndMetadataMismatchPreserveSharedOwners(t *testing.T) {
	r, native, metadata, output := replicaGenerationFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if g, err := r.prepareReplicaAccountingGeneration(ctx, native, metadata, output); g != nil || !errors.Is(err, context.Canceled) {
		t.Fatal(g, err)
	}
	before := r.publisher.Snapshot()
	projected := before.Projection
	projected.MetadataEpoch = 1
	if err := r.publisher.PublishReplica(replication.ReplicaProgress{DurableIndex: before.DurableIndex, ConfirmedIndex: before.ConfirmedIndex, AppliedIndex: before.AppliedIndex, OrderingHeadMAC: before.OrderingHeadMAC, Projection: projected}); err != nil {
		t.Fatal(err)
	}
	if g, err := r.prepareReplicaAccountingGeneration(context.Background(), native, metadata, output); g != nil || err == nil {
		t.Fatal("Metadata applied mismatch accepted", g, err)
	}
	metadata.Close()
	if g, err := r.prepareReplicaAccountingGeneration(context.Background(), native, metadata, output); g != nil || err == nil {
		t.Fatal("closed Metadata accepted", g, err)
	}
	if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed admission created output", err)
	}
	if _, err := native.ReplayAuthenticated(context.Background(), ledger.ReplayAnchor{}, nil); err != nil {
		t.Fatal(err)
	}
}

type generationProbeContext struct {
	context.Context
	probe func()
}

func (c generationProbeContext) Err() error { c.probe(); return c.Context.Err() }

func TestReplicaAccountingGenerationDiscardOnBuildCancellationOrPromise(t *testing.T) {
	for _, phase := range []string{"cancel", "promise", "metadata"} {
		t.Run(phase, func(t *testing.T) {
			r, native, metadata, output := replicaGenerationFixture(t)
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			var triggered atomic.Bool
			var promiseErr error
			ctx := generationProbeContext{Context: base, probe: func() {
				if _, err := os.Stat(output); err != nil {
					return
				}
				if !triggered.CompareAndSwap(false, true) {
					return
				}
				if phase == "cancel" {
					cancel()
				} else if phase == "promise" {
					_, promiseErr = r.publisher.Promise(8)
				} else {
					promiseErr = metadata.PutUsageCheckpoint(ledger.Watermark{Generation: 1, Sequence: 99, Offset: 9999}, []byte("concurrent Metadata transaction"), nil, nil, domain.RollupVersion, nil)
				}
			}}
			g, err := r.prepareReplicaAccountingGeneration(ctx, native, metadata, output)
			if !triggered.Load() || promiseErr != nil || g != nil || err == nil {
				t.Fatal("changed build returned candidate", triggered.Load(), promiseErr, g, err)
			}
			if phase == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed candidate file retained", err)
			}
			if native.Status().Load() != ledger.AccountingHealthy {
				t.Fatal("caller failure poisoned native")
			}
		})
	}
}

func TestReplicaAccountingGenerationRetirementJoinsWholeAdapterCall(t *testing.T) {
	r, native, metadata, output := replicaGenerationFixture(t)
	g, err := r.prepareReplicaAccountingGeneration(context.Background(), native, metadata, output)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close() })
	entered, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	called := make(chan error, 1)
	go func() {
		called <- g.withProjection(context.Background(), func(p *replication.NativeProjection) error {
			close(entered)
			<-release
			return p.ValidateLedgerForPublication(context.Background())
		})
	}()
	awaitDrainSignal(t, entered)
	closed := make(chan error, 1)
	go func() { closed <- g.Close() }()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for !g.retired.Load() {
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("generation failed to withdraw")
		}
	}
	if err := g.ValidateLedgerForPublication(context.Background()); !errors.Is(err, errReplicaAccountingRetired) {
		t.Fatal("new adapter call admitted", err)
	}
	select {
	case err := <-closed:
		t.Fatal("generation closed before adapter exit", err)
	default:
	}
	if _, err := os.Stat(output); err != nil {
		t.Fatal("DB reclaimed before adapter join", err)
	}
	close(release)
	if err := <-called; err != nil {
		t.Fatal("admitted native verifier interrupted", err)
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("generation did not join")
	}
	if g.ledger != nil || g.usage != nil || g.usageDisk != nil || g.projection != nil {
		t.Fatal("generation retained retired owners")
	}
}

func TestReplicaAccountingGenerationStructuralRollCutContinuesIntoNextGeneration(t *testing.T) {
	r, native, metadata, output := replicaGenerationFixture(t, true)
	ctx := context.Background()
	g, err := r.prepareReplicaAccountingGeneration(ctx, native, metadata, output)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close() })
	if g.prefix.Head != (ledger.Watermark{Generation: 2, Sequence: 4}) {
		t.Fatal("candidate failed to preserve structural roll cut", g.prefix.Head)
	}
	a := g.usageDisk
	wm := replicaDiskUsageSnapshot(t, a).Watermark
	if wm.Generation != 1 || wm.Sequence != 4 || wm.Offset == 0 {
		t.Fatal("Usage must retain last semantic event position", wm)
	}
	before := r.publisher.Snapshot()
	receiver, err := replication.NewReplicaReceiver(before.ClusterID, before.Incarnation, before.NodeID, "halro-0", before.Term, before.PromisedTerm, before.ConfirmedIndex, before.AppliedIndex, r.journal, generationNativeSink{native}, replication.ReplicaReceiverOptions{PersistState: r.publisher.PublishReplica, Projection: before.Projection})
	if err != nil {
		t.Fatal(err)
	}
	applier, err := replication.NewReplicaApplier(receiver, r.journal, g)
	if err != nil {
		t.Fatal(err)
	}
	if applied, err := applier.ApplyConfirmed(ctx); err != nil || applied != before.ConfirmedIndex {
		t.Fatal(applied, err)
	}
	head, hash, err := g.ledger.ReadPosition(ctx)
	nativeHead, nativeHash, nativeAuthenticated := native.ChainHead()
	if err != nil || !nativeAuthenticated || head != nativeHead || hash != nativeHash || head.Generation != 2 || head.Sequence != 5 || head.Offset == 0 {
		t.Fatal("next-generation first frame did not reach actual native head", head, nativeHead, err, nativeAuthenticated)
	}
	snap := replicaDiskUsageSnapshot(t, a)
	if snap.Watermark != head || snap.Totals.Attempts != 1 || snap.Totals.CostMicrosUSD != 7 || snap.Requests[0].Attempts != 0 || replicaDiskUsageMetrics(t, a).ActiveRequests != 0 {
		t.Fatal("cross-roll late settlement changed terminal summary", snap)
	}
}

// Quiescent test-only assembly; production readers use bounded pages and current
// short-View stats, never reconstruct a resident Snapshot from all disk history.
func replicaDiskUsageSnapshot(t *testing.T, p *usage.DiskProjection) usage.Snapshot {
	t.Helper()
	stats, err := p.ReadStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	page, err := p.SummaryPage(context.Background(), usage.DiskSummaryQuery{Limit: 100}, usage.DiskUsageCursor{}, usage.DiskUsagePageLimits{ScanRows: 100, InputBytes: 8 << 20, OutputBytes: 8 << 20})
	if err != nil || !page.Done {
		t.Fatal("fixture exceeds bounded summary page", err)
	}
	return usage.Snapshot{Watermark: stats.Watermark, Totals: stats.Totals, Requests: page.Items}
}
func replicaDiskUsageMetrics(t *testing.T, p *usage.DiskProjection) usage.Metrics {
	t.Helper()
	stats, err := p.ReadStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return stats.Metrics
}

func TestReplicaAccountingGenerationRollupRangeMatchesCheckpointStore(t *testing.T) {
	r, native, metadata, output := replicaGenerationFixture(t)
	ctx := context.Background()
	g, err := r.prepareReplicaAccountingGeneration(ctx, native, metadata, output)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	ref, err := usage.NewAggregateWithFinalizationReader(g.ledger)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := native.ReplayAuthenticated(ctx, ledger.ReplayAnchor{}, func(record ledger.Record) error {
		if record.Sequence > 4 {
			return nil
		}
		return ref.Apply(record)
	}); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := ref.TakeCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "reference.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	segments := make([]boltstore.UsageCheckpointSegment, 0, len(checkpoint.Segments))
	for _, segment := range checkpoint.Segments {
		segments = append(segments, boltstore.UsageCheckpointSegment{ID: segment.ID, Payload: segment.Payload})
	}
	if err := store.PutUsageCheckpoint(checkpoint.Watermark, checkpoint.Head, segments, checkpoint.RemovedSegments, domain.RollupVersion, checkpoint.Rollup); err != nil {
		t.Fatal(err)
	}
	period := ref.Snapshot().Requests[0].PeriodID
	expected := make(map[domain.RollupKey]domain.DailyRollup)
	if err := store.UsageRollupRange(period, period, func(key domain.RollupKey, row domain.DailyRollup) error { expected[key] = row; return nil }); err != nil {
		t.Fatal(err)
	}
	got := make(map[domain.RollupKey]domain.DailyRollup)
	for _, dimension := range []string{domain.RollupDimensionTotal, domain.RollupDimensionProject, domain.RollupDimensionRequestedModel, domain.RollupDimensionProvider, domain.RollupDimensionDeployment, domain.RollupDimensionProviderModel} {
		query := usage.DiskRollupQuery{Start: period, End: period, Dimensions: []string{dimension}, Limit: 1}
		var cursor usage.DiskRollupCursor
		for calls := 0; calls < 100; calls++ {
			page, err := g.usageDisk.RollupPage(ctx, query, cursor, usage.DiskUsagePageLimits{ScanRows: 4, InputBytes: 1 << 20, OutputBytes: 1 << 20})
			if err != nil {
				t.Fatal(err)
			}
			if page.Head != checkpoint.Watermark {
				t.Fatal("candidate/read watermark differs")
			}
			for _, item := range page.Items {
				if _, exists := got[item.Key]; exists {
					t.Fatal("duplicate physical row")
				}
				got[item.Key] = item.Row
			}
			if page.Done {
				break
			}
			if page.Next == cursor {
				t.Fatal("no-progress source page")
			}
			cursor = page.Next
			if calls == 99 {
				t.Fatal("range failed to terminate")
			}
		}
	}
	if !reflect.DeepEqual(got, expected) {
		t.Fatal("candidate rollups differ from real checkpoint Store", got, expected)
	}
}
