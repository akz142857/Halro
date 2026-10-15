package usage

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
	bbolt "go.etcd.io/bbolt"
)

// A batch names the Ledger position it reaches, and the index never moves
// back from a position it has stored: rows already counted would otherwise be
// counted again by the next replay.
func TestHistoryWatermarkCannotMoveBackwards(t *testing.T) {
	store := newTestHistory(t)
	ahead := &historyBatch{state: HistoryState{Watermark: ledger.Watermark{Generation: 1, Offset: 200, Sequence: 2}}}
	if err := store.commit(ahead); err != nil {
		t.Fatal(err)
	}
	behind := &historyBatch{state: HistoryState{Watermark: ledger.Watermark{Generation: 1, Offset: 100, Sequence: 1}}}
	if err := store.commit(behind); err == nil {
		t.Fatal("a batch behind the stored watermark was accepted")
	}
}

// A row edited on disk, or moved to another key, fails its MAC and the read
// fails with it rather than reporting the altered value.
func TestHistoryRefusesAnAlteredRow(t *testing.T) {
	aggregate := newTestAggregate(t)
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	for index, event := range []ledger.Event{
		{EventID: "settled", Kind: ledger.EventAttemptSettled, RequestID: "req_1", AttemptID: "att_1",
			ProjectID: "project", PeriodID: "2026-09-01", OccurredAt: at, Outcome: "success", CommittedMicrosUSD: ledger.MicrosUSD(7)},
		{EventID: "final", Kind: ledger.EventRequestFinalized, RequestID: "req_1",
			ProjectID: "project", PeriodID: "2026-09-01", OccurredAt: at, Outcome: "success"},
	} {
		if err := aggregate.Apply(ledger.Record{Generation: 1, Sequence: uint64(index + 1), Offset: int64(index+1) * 100, Event: event}); err != nil {
			t.Fatal(err)
		}
	}
	if err := aggregate.Flush(); err != nil {
		t.Fatal(err)
	}
	store := aggregate.History()
	if err := store.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(historyAttempts)
		key := sequenceKey(1)
		raw := append([]byte(nil), bucket.Get(key)...)
		altered := []byte(string(raw[:len(raw)-historyMACSize]))
		for index := range altered {
			if altered[index] == '7' {
				altered[index] = '9'
				break
			}
		}
		return bucket.Put(key, append(altered, raw[len(raw)-historyMACSize:]...))
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := aggregate.RequestDetail("req_1"); !errors.Is(err, errHistoryRowTampered) {
		t.Fatalf("an altered attempt row was read back: err=%v", err)
	}
	// The read fails rather than rebuilding the index under the operator; the
	// counter is what tells them to.
	if got := store.WriteStats().RowAuthFailures; got != 1 {
		t.Fatalf("row authentication failures = %d, want 1", got)
	}
}

// A history.db written under another key — another instance's, or one whose
// Ledger key changed — is emptied on open rather than trusted.
func TestHistoryWithAnotherKeyIsRebuilt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	first, rebuilt, err := OpenHistory(path, testHistoryKey)
	if err != nil || rebuilt {
		t.Fatalf("fresh open rebuilt=%v err=%v", rebuilt, err)
	}
	if err := first.commit(&historyBatch{state: HistoryState{Watermark: ledger.Watermark{Generation: 1, Offset: 100, Sequence: 1}}}); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	other := make([]byte, 32)
	second, rebuilt, err := OpenHistory(path, other)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if !rebuilt {
		t.Fatal("an index written under another key was not rebuilt")
	}
	if _, err := second.State(); !IsHistoryEmpty(err) {
		t.Fatalf("the rebuilt index still holds a position: %v", err)
	}
}

// A successful attempt is evidence for onboarding only once its request has
// succeeded too: an upstream that answered while the request was refused on
// the way out proves nothing about the chain the caller used.
func TestOnboardingEvidenceCountsOnlySucceededRequests(t *testing.T) {
	aggregate := newTestAggregate(t)
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	var sequence uint64
	apply := func(event ledger.Event) {
		t.Helper()
		sequence++
		if err := aggregate.Apply(ledger.Record{Generation: 1, Sequence: sequence, Offset: int64(sequence) * 100, Event: event}); err != nil {
			t.Fatal(err)
		}
	}
	for _, request := range []struct{ id, deployment, outcome string }{
		{"req_ok", "dep_ok", "success"},
		{"req_refused", "dep_refused", "provider_error"},
	} {
		apply(ledger.Event{EventID: request.id + "_settled", Kind: ledger.EventAttemptSettled, RequestID: request.id,
			AttemptID: request.id + "_att", ProjectID: "project", PeriodID: "2026-09-01",
			DeploymentID: request.deployment, RouteID: "route_" + request.deployment, ProviderID: "provider_1",
			OccurredAt: at, Outcome: "success", CommittedMicrosUSD: ledger.MicrosUSD(1)})
		apply(ledger.Event{EventID: request.id + "_final", Kind: ledger.EventRequestFinalized, RequestID: request.id,
			ProjectID: "project", PeriodID: "2026-09-01", OccurredAt: at, Outcome: request.outcome})
	}
	evidence, err := aggregate.OnboardingEvidence()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := evidence.Deployments["dep_ok"]; !ok {
		t.Fatalf("the succeeded request's deployment is missing: %#v", evidence.Deployments)
	}
	if _, ok := evidence.Deployments["dep_refused"]; ok {
		t.Fatalf("a refused request counted as evidence: %#v", evidence.Deployments)
	}
	if evidence.Latest == nil || evidence.Latest.RequestID != "req_refused" || evidence.LatestAttempt == nil {
		t.Fatalf("latest request = %#v attempt = %#v", evidence.Latest, evidence.LatestAttempt)
	}
}
