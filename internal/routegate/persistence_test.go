package routegate

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/provider"
)

// recordingStore counts writes as well as keeping rows, because "no bbolt write
// on the request path" is a claim about the number of calls rather than about
// what ends up stored.
type recordingStore struct {
	mu      sync.Mutex
	rows    map[string]domain.RouteSuspension
	puts    int
	deletes int
}

func newRecordingStore() *recordingStore {
	return &recordingStore{rows: make(map[string]domain.RouteSuspension)}
}

func (s *recordingStore) PutRouteSuspension(_ context.Context, suspension domain.RouteSuspension) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.puts++
	s.rows[suspension.ScopeKind+"\x1f"+suspension.ScopeKey] = suspension
	return nil
}

func (s *recordingStore) DeleteRouteSuspension(_ context.Context, kind, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deletes++
	delete(s.rows, kind+"\x1f"+key)
	return nil
}

func (s *recordingStore) list() []domain.RouteSuspension {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows := make([]domain.RouteSuspension, 0, len(s.rows))
	for _, row := range s.rows {
		rows = append(rows, row)
	}
	return rows
}

func (s *recordingStore) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.puts, s.deletes
}

func gateWithStore(store SuspensionStore) *Gate {
	gate := New(Config{})
	gate.UsePersistence(store, nil)
	return gate
}

func invalidCredential(status int) Observation {
	return Observation{
		Reason: provider.FailureReasonInvalidCredential,
		Class:  provider.ErrorAuthentication, Status: status,
	}
}

func quotaExhausted() Observation {
	return Observation{
		Reason: provider.FailureReasonSubscriptionQuotaExhausted,
		Class:  provider.ErrorRateLimit, Status: 429,
	}
}

// The gate this issue exists for: a credential the upstream will not accept is
// suspended with no clock to end it, and a restart that forgot it would hand
// the dead key a request from every caller all over again.
func TestAnIndefiniteCredentialSuspensionSurvivesARestart(t *testing.T) {
	store := newRecordingStore()
	gate := gateWithStore(store)
	target := targetOn("route_1", "dep_1", "cred_1", "model-a", "provider_1", 3)

	gate.Observe(target, invalidCredential(401), base)
	rows := store.list()
	if len(rows) != 1 {
		t.Fatalf("stored %d rows, want the credential suspension", len(rows))
	}
	if rows[0].ScopeKind != string(ScopeCredential) || rows[0].ScopeKey != "cred_1" || !rows[0].Indefinite {
		t.Fatalf("stored %+v, want an indefinite credential-scope row", rows[0])
	}
	if rows[0].CredentialRevision != 3 {
		t.Fatalf("stored revision %d, want the 3 the refusal was seen against", rows[0].CredentialRevision)
	}

	restarted := gateWithStore(store)
	restarted.Restore(store.list())
	if admitted := restarted.Filter([]provider.Target{target}, base.Add(24*time.Hour)); len(admitted) != 0 {
		t.Fatal("the restarted gate admitted a credential the upstream refuses")
	}
	snapshot := restarted.Snapshot(base.Add(24 * time.Hour))
	if len(snapshot) != 1 || !snapshot[0].Indefinite {
		t.Fatalf("snapshot = %+v, want one indefinite suspension", snapshot)
	}
	if _, err := restarted.Admit(target, base.Add(24*time.Hour)); err != ErrSuspended {
		t.Fatalf("Admit err = %v, want ErrSuspended: no clock ends this one", err)
	}
}

// Replacing the secret is the whole of the fix, and it has to stay the whole of
// it across a restart: the registry stamps current revisions onto targets at
// startup, so the first resolve is what notices.
func TestACredentialReplacedWhileDownIsAdmittedOnTheFirstResolve(t *testing.T) {
	store := newRecordingStore()
	gate := gateWithStore(store)
	target := targetOn("route_1", "dep_1", "cred_1", "model-a", "provider_1", 3)
	gate.Observe(target, invalidCredential(403), base)

	restarted := gateWithStore(store)
	restarted.Restore(store.list())
	replaced := targetOn("route_1", "dep_1", "cred_1", "model-a", "provider_1", 4)
	admitted := restarted.Filter([]provider.Target{replaced}, base.Add(time.Minute))
	if len(admitted) != 1 {
		t.Fatal("a credential the operator replaced while the process was down was still refused")
	}
	// And the row goes with it, so the next restart does not resurrect it.
	if rows := store.list(); len(rows) != 0 {
		t.Fatalf("stored rows after the stale clear = %+v, want none", rows)
	}
}

// An availability window is thirty seconds and a restart is longer. Storing one
// would be a disk write for a suspension that expires before the instance
// finishes coming up.
func TestAnAvailabilitySuspensionIsNeverStored(t *testing.T) {
	store := newRecordingStore()
	gate := gateWithStore(store)
	target := targetOn("route_1", "dep_1", "cred_1", "model-a", "provider_1", 1)
	for range 5 {
		gate.Observe(target, Observation{Class: provider.ErrorProvider5xx, Status: 503}, base)
	}
	if len(gate.Snapshot(base)) != 1 {
		t.Fatal("five 5xx did not suspend anything, so this test proves nothing")
	}
	if rows := store.list(); len(rows) != 0 {
		t.Fatalf("an availability window was stored: %+v", rows)
	}
	puts, deletes := store.counts()
	if puts != 0 || deletes != 0 {
		t.Fatalf("store calls = %d puts, %d deletes; an availability suspension must touch neither", puts, deletes)
	}
}

// The cost claim: a period of refusals writes once, not once per attempt. A
// request that merely fails against an already-suspended scope must not open a
// transaction at all.
func TestRefusalsAgainstASuspendedScopeDoNotWriteAgain(t *testing.T) {
	store := newRecordingStore()
	gate := gateWithStore(store)
	target := targetOn("route_1", "dep_1", "cred_1", "model-a", "provider_1", 1)
	gate.Observe(target, invalidCredential(401), base)
	puts, _ := store.counts()
	if puts != 1 {
		t.Fatalf("puts after the first refusal = %d, want 1", puts)
	}
	for attempt := range 50 {
		now := base.Add(time.Duration(attempt) * time.Second)
		if admitted := gate.Filter([]provider.Target{target}, now); len(admitted) != 0 {
			t.Fatal("a suspended target was admitted")
		}
		if _, err := gate.Admit(target, now); err != ErrSuspended {
			t.Fatalf("Admit err = %v, want ErrSuspended", err)
		}
	}
	puts, deletes := store.counts()
	if puts != 1 || deletes != 0 {
		t.Fatalf("store calls after 50 refused requests = %d puts, %d deletes; want 1 and 0", puts, deletes)
	}
}

// A quota window doubles on each failed probe, and a row still naming the old
// one would restore a suspension that ends earlier than the gate decided.
func TestADoubledQuotaWindowIsWrittenThrough(t *testing.T) {
	store := newRecordingStore()
	gate := gateWithStore(store)
	target := targetOn("route_1", "dep_1", "cred_1", "model-a", "provider_1", 1)
	gate.Observe(target, quotaExhausted(), base)
	first := store.list()
	if len(first) != 1 || first[0].Indefinite {
		t.Fatalf("stored %+v, want one windowed quota row", first)
	}
	if first[0].WindowSeconds != int64((15 * time.Minute).Seconds()) {
		t.Fatalf("window = %ds, want the policy's opening 15 minutes", first[0].WindowSeconds)
	}

	expired := first[0].Until.Add(time.Second)
	lease, err := gate.Admit(target, expired)
	if err != nil {
		t.Fatalf("the expired window did not admit a probe: %v", err)
	}
	observation := quotaExhausted()
	lease.Done(&observation, expired)

	second := store.list()
	if len(second) != 1 {
		t.Fatalf("stored %+v after the failed probe", second)
	}
	if second[0].WindowSeconds != 2*first[0].WindowSeconds {
		t.Fatalf("window = %ds, want it doubled from %ds", second[0].WindowSeconds, first[0].WindowSeconds)
	}
	if !second[0].Until.After(first[0].Until) {
		t.Fatal("the stored end time did not move with the doubled window")
	}
}

// A restored window that has already passed is not special-cased on the way in:
// the request path admits a probe through it, which is what it does for one
// that expires while the process is up.
func TestARestoredWindowThatHasExpiredAdmitsAProbe(t *testing.T) {
	store := newRecordingStore()
	gate := gateWithStore(store)
	target := targetOn("route_1", "dep_1", "cred_1", "model-a", "provider_1", 1)
	gate.Observe(target, quotaExhausted(), base)

	restarted := gateWithStore(store)
	restarted.Restore(store.list())
	after := base.Add(time.Hour)
	if _, err := restarted.Admit(target, after); err != nil {
		t.Fatalf("a restored window that has expired refused a probe: %v", err)
	}
}

// A successful request proves the refusal is over, and the row has to go with
// the memory of it.
func TestASuccessRemovesTheStoredRow(t *testing.T) {
	store := newRecordingStore()
	gate := gateWithStore(store)
	target := targetOn("route_1", "dep_1", "cred_1", "model-a", "provider_1", 1)
	gate.Observe(target, quotaExhausted(), base)
	if len(store.list()) != 1 {
		t.Fatal("nothing was stored, so this test proves nothing")
	}
	gate.ObserveSuccess(target, base.Add(time.Hour))
	if rows := store.list(); len(rows) != 0 {
		t.Fatalf("stored rows after a success = %+v, want none", rows)
	}
}

// A reload is where a replaced credential is forgiven without waiting for
// traffic the suspension itself discouraged. The stored row goes too.
func TestForgettingOutdatedCredentialsRemovesTheStoredRow(t *testing.T) {
	store := newRecordingStore()
	gate := gateWithStore(store)
	target := targetOn("route_1", "dep_1", "cred_1", "model-a", "provider_1", 2)
	gate.Observe(target, invalidCredential(401), base)

	gate.ForgetOutdatedCredentials(map[string]uint64{"cred_1": 3})
	if len(gate.Snapshot(base)) != 0 {
		t.Fatal("the gate still holds a suspension against a replaced credential")
	}
	if rows := store.list(); len(rows) != 0 {
		t.Fatalf("stored rows = %+v, want none", rows)
	}
}

// A row whose reason no longer has a policy would be a suspension the gate has
// nothing to end — created by a version change nobody was told about.
func TestARestoredRowWithNoPolicyIsDiscarded(t *testing.T) {
	store := newRecordingStore()
	gate := gateWithStore(store)
	gate.Restore([]domain.RouteSuspension{{
		ScopeKind: string(ScopeCredential), ScopeKey: "cred_1",
		Reason: "reason_from_a_future_version", ObservedAt: base, Indefinite: true,
	}})
	if len(gate.Snapshot(base)) != 0 {
		t.Fatal("a row with no policy was restored into a suspension nothing can end")
	}
	if rows := store.list(); len(rows) != 0 {
		t.Fatalf("the unusable row was kept: %+v", rows)
	}
}

// A gate with no store is the ordinary one every test and the offline tooling
// build, and it must behave identically apart from forgetting.
func TestAGateWithNoStoreStillSuspends(t *testing.T) {
	gate := New(Config{})
	target := targetOn("route_1", "dep_1", "cred_1", "model-a", "provider_1", 1)
	gate.Observe(target, invalidCredential(401), base)
	if admitted := gate.Filter([]provider.Target{target}, base); len(admitted) != 0 {
		t.Fatal("a gate without persistence stopped suspending")
	}
}

// blockingStore holds a write until the test lets it finish, so an interleaving
// that is otherwise a matter of scheduling can be asserted.
type blockingStore struct {
	*recordingStore
	entered chan struct{}
	release chan struct{}
}

func newBlockingStore() *blockingStore {
	return &blockingStore{
		recordingStore: newRecordingStore(),
		entered:        make(chan struct{}, 8),
		release:        make(chan struct{}),
	}
}

func (s *blockingStore) PutRouteSuspension(ctx context.Context, suspension domain.RouteSuspension) error {
	s.entered <- struct{}{}
	<-s.release
	return s.recordingStore.PutRouteSuspension(ctx, suspension)
}

// Concurrent attempts against one scope all report, and once it is suspended
// the second and third move only the observation timestamp. Writing the row
// again for each would put a bbolt commit on the request path for a suspension
// that already says everything it will say.
func TestRepeatedRefusalsAgainstASuspendedScopeWriteOnce(t *testing.T) {
	store := newRecordingStore()
	gate := gateWithStore(store)
	target := targetOn("route_1", "dep_1", "cred_1", "model-a", "provider_1", 1)
	for attempt := range 20 {
		gate.Observe(target, invalidCredential(401), base.Add(time.Duration(attempt)*time.Second))
	}
	puts, deletes := store.counts()
	if puts != 1 || deletes != 0 {
		t.Fatalf("store calls = %d puts, %d deletes; the suspension never changed after the first", puts, deletes)
	}
	// A window is different: every observation against a suspended one doubles
	// it, and a longer suspension is a real change the row has to carry. What
	// must not write is an observation that moves nothing — which is where a
	// window that has reached its ceiling ends up.
	quota := targetOn("route_2", "dep_2", "cred_2", "model-b", "provider_1", 1)
	policy := DefaultPolicies()[provider.FailureReasonSubscriptionQuotaExhausted]
	capped := base
	for gate.windowOf(Scope{Kind: ScopeCredentialModel, Key: credentialModelKey("cred_2", "model-b")}) < policy.MaxWindow {
		gate.Observe(quota, quotaExhausted(), capped)
	}
	atCeiling, _ := store.counts()
	for range 10 {
		gate.Observe(quota, quotaExhausted(), capped)
	}
	puts, _ = store.counts()
	if puts != atCeiling {
		t.Fatalf("puts = %d, want %d: a window at its ceiling, re-observed at the same instant, moves nothing", puts, atCeiling)
	}
}

// The ordering the clear depends on: a row queued by a failed attempt is
// written before the durable removal runs, never after it. Without that the
// operator's clear commits, the late row lands, and the next start restores a
// suspension there is an audit record for having cleared.
func TestClearWaitsForAWriteAlreadyInFlight(t *testing.T) {
	store := newBlockingStore()
	gate := gateWithStore(store)
	target := targetOn("route_1", "dep_1", "cred_1", "model-a", "provider_1", 1)
	scope := Scope{Kind: ScopeCredential, Key: "cred_1"}

	observed := make(chan struct{})
	go func() {
		gate.Observe(target, invalidCredential(401), base)
		close(observed)
	}()
	<-store.entered // the suspension's row is mid-write

	cleared := make(chan error, 1)
	go func() {
		cleared <- gate.ClearDurable(scope, func() error {
			return store.DeleteRouteSuspension(context.Background(), string(scope.Kind), scope.Key)
		})
	}()
	// The clear must not have run yet: the write it has to come after is still
	// in flight.
	select {
	case err := <-cleared:
		t.Fatalf("the clear ran while a row was still being written: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(store.release)
	<-observed
	if err := <-cleared; err != nil {
		t.Fatal(err)
	}
	if rows := store.list(); len(rows) != 0 {
		t.Fatalf("stored rows after the clear = %+v, want none", rows)
	}
	if len(gate.Snapshot(base)) != 0 {
		t.Fatal("the gate still holds a suspension the operator cleared")
	}
}

// A refusal that arrives while the clear is committing is dropped with the
// scope rather than written back. The upstream refusing again suspends it
// again, which is an honest new suspension rather than a resurrected row.
func TestAQueuedRowForAClearedScopeIsDropped(t *testing.T) {
	store := newRecordingStore()
	gate := gateWithStore(store)
	target := targetOn("route_1", "dep_1", "cred_1", "model-a", "provider_1", 1)
	scope := Scope{Kind: ScopeCredential, Key: "cred_1"}
	if err := gate.ClearDurable(scope, func() error {
		// Queued from inside the durable half: the same window a late attempt
		// reporting concurrently would land in.
		gate.mu.Lock()
		gate.scopes[scope] = &scopeState{
			policy:     DefaultPolicies()[provider.FailureReasonInvalidCredential],
			indefinite: true, persisted: true,
			evidence: Evidence{Reason: provider.FailureReasonInvalidCredential, ObservedAt: base},
		}
		gate.pending = append(gate.pending, persistAction{
			kind: string(scope.Kind), key: scope.Key,
			record: recordFor(scope, gate.scopes[scope]),
		})
		gate.mu.Unlock()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	gate.Filter([]provider.Target{target}, base) // drives a flush
	if rows := store.list(); len(rows) != 0 {
		t.Fatalf("a row queued for the cleared scope was written anyway: %+v", rows)
	}
}

// The store and the gate must agree once the traffic stops, whatever order the
// failures and successes arrived in. This is the shape the reordering bug took:
// a row outliving the memory that would have removed it.
func TestStoreAgreesWithTheGateUnderConcurrentTraffic(t *testing.T) {
	store := newRecordingStore()
	gate := gateWithStore(store)
	targets := []provider.Target{
		targetOn("route_1", "dep_1", "cred_1", "model-a", "provider_1", 1),
		targetOn("route_2", "dep_2", "cred_2", "model-b", "provider_1", 1),
		targetOn("route_3", "dep_3", "cred_3", "model-c", "provider_2", 1),
	}
	var waiting sync.WaitGroup
	for worker := range 12 {
		waiting.Add(1)
		go func() {
			defer waiting.Done()
			target := targets[worker%len(targets)]
			for round := range 40 {
				now := base.Add(time.Duration(round) * time.Second)
				if (worker+round)%3 == 0 {
					gate.ObserveSuccess(target, now)
					continue
				}
				gate.Observe(target, invalidCredential(401), now)
			}
		}()
	}
	waiting.Wait()
	// One last pass with no concurrency, so every queued write has been applied.
	gate.Filter(targets, base)

	live := make(map[string]bool)
	for _, suspension := range gate.Snapshot(base) {
		live[suspension.Scope.String()] = true
	}
	for _, row := range store.list() {
		if !live[row.ScopeKind+":"+row.ScopeKey] {
			t.Fatalf("stored %s/%s outlived the gate's own memory of it", row.ScopeKind, row.ScopeKey)
		}
	}
	for scope := range live {
		found := false
		for _, row := range store.list() {
			if row.ScopeKind+":"+row.ScopeKey == scope {
				found = true
			}
		}
		if !found {
			t.Fatalf("the gate holds %s with nothing stored for it", scope)
		}
	}
}

// windowOf reads one scope's current window, for a test that has to drive a
// policy to its ceiling without hardcoding how many doublings that takes.
func (g *Gate) windowOf(scope Scope) time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	if state := g.scopes[scope]; state != nil {
		return state.window
	}
	return 0
}

func (g *Gate) pendingLen() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.pending)
}

// The queue is taken inside the store lock, not before it.
//
// That is the whole of the ordering fix, and it is a property of the mechanism
// rather than of any single outcome: the bad interleaving needs one flush to be
// descheduled between taking the queue and acquiring the lock, which no test
// can schedule. What a test can pin is that the window does not exist — while a
// write is in flight, a second flush has not taken anything, so it cannot
// afterwards apply an older decision on top of a newer one.
func TestAFlushTakesTheQueueOnlyWithTheStoreLockHeld(t *testing.T) {
	store := newBlockingStore()
	gate := gateWithStore(store)
	target := targetOn("route_1", "dep_1", "cred_1", "model-a", "provider_1", 1)

	observed := make(chan struct{})
	go func() {
		gate.Observe(target, invalidCredential(401), base)
		close(observed)
	}()
	<-store.entered // the row is mid-write, so the store lock is held

	succeeded := make(chan struct{})
	go func() {
		gate.ObserveSuccess(target, base.Add(time.Second))
		close(succeeded)
	}()
	// Give the second flush every chance to take the queue if it is able to.
	deadline := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(deadline) {
		if gate.pendingLen() == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if queued := gate.pendingLen(); queued != 1 {
		t.Fatalf("pending = %d while a write is in flight; the delete was taken out of the queue ahead of its turn", queued)
	}

	close(store.release)
	<-observed
	<-succeeded
	if rows := store.list(); len(rows) != 0 {
		t.Fatalf("stored rows = %+v, want none: the success ended the suspension", rows)
	}
}
