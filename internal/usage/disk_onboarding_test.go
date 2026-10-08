package usage

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
	bbolt "go.etcd.io/bbolt"
)

var onboardingTestLimits = DiskOnboardingLimits{Rows: 65536, InputBytes: 64 << 20, OutputBytes: 8 << 20, Targets: 4096, MaxDuration: time.Minute}

func TestDiskOnboardingAuthenticatedJoinLatestTimeAndCompletePages(t *testing.T) {
	var events []ledger.Event
	for i := 0; i < 166; i++ {
		rows := diskFailureRequest(fmt.Sprint("proof_", i), "success", 1, false, true)
		for j := range rows {
			rows[j].RouteID = "route"
			rows[j].OccurredAt = rows[j].OccurredAt.Add(time.Duration(166-i) * time.Hour)
		}
		events = append(events, rows...)
	}
	// A later successful attempt inside a failed finalized request is no proof.
	for _, c := range []struct {
		id, outcome string
		late        bool
	}{{"failed", "provider_error", false}, {"late", "interrupted", true}} {
		rows := diskFailureRequest(c.id, c.outcome, 1, c.late, true)
		for j := range rows {
			rows[j].OccurredAt = rows[j].OccurredAt.Add(300 * time.Hour)
		}
		events = append(events, rows...)
	}
	// An unfinished successful attempt is likewise insufficient.
	rows := diskFailureRequest("pending", "success", 1, false, true)
	events = append(events, rows[:len(rows)-1]...)
	f := newDiskFailureFixture(t, events)
	got, err := f.p.ReadOnboarding(context.Background(), OnboardingTargets{Providers: []string{"provider_a", "absent"}, Deployments: []string{"dep_a"}, Routes: []string{"route"}}, onboardingTestLimits)
	if err != nil {
		t.Fatal(err)
	}
	wantAt := diskFailureBase("").OccurredAt.Add(166*time.Hour + time.Second)
	if len(got.Providers) != 1 || got.Providers["provider_a"] != wantAt || got.Deployments["dep_a"] != wantAt || got.Routes["route"] != wantAt {
		t.Fatal("proof lost timestamp order or credited failed/unfinished request", got)
	}
	snapshot := f.a.Snapshot()
	wantRequest := snapshot.Requests[len(snapshot.Requests)-1]
	var wantAttempt *AttemptEvent
	for i := len(snapshot.Attempts) - 1; i >= 0; i-- {
		if snapshot.Attempts[i].RequestID == wantRequest.RequestID {
			a := snapshot.Attempts[i]
			wantAttempt = &a
			break
		}
	}
	if got.Head != f.a.Watermark() || got.Metrics != f.a.Metrics() || !reflect.DeepEqual(got.LastRequest, &wantRequest) || !reflect.DeepEqual(got.LastAttempt, wantAttempt) {
		t.Fatal("cut/latest arrival/late attempt differ", got.LastRequest, got.LastAttempt, wantRequest, wantAttempt)
	}
	if got.Scanned < 160 || f.p.db.Stats().OpenTxN != 0 || len(f.p.readers) != 0 {
		t.Fatal("pagination or reader lifetime", got.Scanned)
	}
	stats, err := f.p.ReadStats(context.Background())
	if err != nil || stats.LogicalBytes <= 0 || stats.Metrics != got.Metrics || stats.Watermark != got.Head {
		t.Fatal("constant observation differs", stats, err)
	}
}

func TestDiskOnboardingBudgetCancellationRetirementAndCorruptionNoPartialProof(t *testing.T) {
	f := newDiskFailureFixture(t, diskFailureRequest("proof", "success", 2, false, true))
	q := OnboardingTargets{Providers: []string{"provider_a", "provider_b"}}
	for _, edit := range []func(*DiskOnboardingLimits){func(l *DiskOnboardingLimits) { l.Rows = 1 }, func(l *DiskOnboardingLimits) { l.InputBytes = 1 }, func(l *DiskOnboardingLimits) { l.OutputBytes = 1 }, func(l *DiskOnboardingLimits) { l.Targets = 1 }, func(l *DiskOnboardingLimits) { l.MaxDuration = 0 }} {
		limits := onboardingTestLimits
		edit(&limits)
		got, err := f.p.ReadOnboarding(context.Background(), q, limits)
		if !errors.Is(err, ErrDiskUsageLimit) || !reflect.DeepEqual(got, OnboardingEvidence{}) {
			t.Fatal("budget returned partial proof", got, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := f.p.ReadOnboarding(ctx, q, onboardingTestLimits)
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(got, OnboardingEvidence{}) {
		t.Fatal(got, err)
	}
	// Malformation and deletion must both fail; the immutable final version
	// prevents an absent association being mistaken for a pending request.
	var original []byte
	if err := f.p.db.View(func(tx *bbolt.Tx) error {
		original = append([]byte(nil), tx.Bucket([]byte("summary_request")).Get(diskUsageID("proof"))...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, missing := range []bool{false, true} {
		if err := f.p.db.Update(func(tx *bbolt.Tx) error {
			b := tx.Bucket([]byte("summary_request"))
			if missing {
				return b.Delete(diskUsageID("proof"))
			}
			return b.Put(diskUsageID("proof"), []byte("bad"))
		}); err != nil {
			t.Fatal(err)
		}
		got, err = f.p.ReadOnboarding(context.Background(), q, onboardingTestLimits)
		if !errors.Is(err, ErrDiskUsageCorrupt) || !reflect.DeepEqual(got, OnboardingEvidence{}) {
			t.Fatal("corrupt/missing join returned proof", missing, got, err)
		}
		if err := f.p.db.Update(func(tx *bbolt.Tx) error {
			return tx.Bucket([]byte("summary_request")).Put(diskUsageID("proof"), original)
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.p.Close(); err != nil {
		t.Fatal(err)
	}
	got, err = f.p.ReadOnboarding(context.Background(), q, onboardingTestLimits)
	if !errors.Is(err, ErrDiskUsageClosed) || !reflect.DeepEqual(got, OnboardingEvidence{}) {
		t.Fatal(got, err)
	}
}
