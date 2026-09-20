package routegate

import (
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/provider"
)

var base = time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)

func targetOn(routeID, deploymentID, credentialID, providerModel, providerID string, revision uint64) provider.Target {
	return provider.Target{
		ID: routeID, DeploymentID: deploymentID, ProviderID: providerID,
		PublicModel: "chat", ProviderModel: providerModel,
		CredentialID: credentialID, CredentialRevision: revision,
	}
}

func ids(targets []provider.Target) []string {
	result := make([]string, 0, len(targets))
	for _, target := range targets {
		result = append(result, target.ID)
	}
	return result
}

func equal(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// The reason a credential is a scope at all: one secret commonly backs several
// deployments, so a refusal learned from one of them is knowledge about all of
// them. Rediscovering it per deployment is what an operator sees as the same
// upstream error repeating, and it is the waste the whole design exists to stop.
func TestOneCredentialRefusalRemovesEveryDeploymentBehindIt(t *testing.T) {
	gate := New(Config{})
	first := targetOn("route_1", "dep_1", "cred_1", "model-a", "provider_1", 3)
	second := targetOn("route_2", "dep_2", "cred_1", "model-b", "provider_1", 3)
	third := targetOn("route_3", "dep_3", "cred_2", "model-c", "provider_2", 1)
	all := []provider.Target{first, second, third}

	gate.Observe(first, Observation{
		Reason: provider.FailureReasonInvalidCredential, Status: 401,
	}, base)

	if got := ids(gate.Filter(all, base)); !equal(got, []string{"route_3"}) {
		t.Fatalf("filtered = %v, want only the target on the other credential", got)
	}
	if _, err := gate.Admit(second, base); err != ErrSuspended {
		t.Fatalf("a deployment that never failed was admitted on a dead credential: %v", err)
	}
}

// A dead credential does not heal, so no backoff against one is anything but a
// slower way of hammering it. What ends it is the operator replacing the secret,
// which advances the revision — so the fix *is* the un-suspend.
func TestACredentialSuspensionEndsOnlyWhenTheSecretIsReplaced(t *testing.T) {
	gate := New(Config{})
	target := targetOn("route_1", "dep_1", "cred_1", "model-a", "provider_1", 3)
	gate.Observe(target, Observation{
		Reason: provider.FailureReasonSubscriptionInactive, Status: 403,
	}, base)

	// No amount of time is enough.
	if got := gate.Filter([]provider.Target{target}, base.Add(30*24*time.Hour)); len(got) != 0 {
		t.Fatalf("an indefinite suspension expired on the clock: %v", ids(got))
	}
	snapshot := gate.Snapshot(base)
	if len(snapshot) != 1 || !snapshot[0].Indefinite || snapshot[0].Scope.Kind != ScopeCredential {
		t.Fatalf("snapshot = %+v", snapshot)
	}

	// The same target, once its credential has been replaced.
	rotated := targetOn("route_1", "dep_1", "cred_1", "model-a", "provider_1", 4)
	if got := ids(gate.Filter([]provider.Target{rotated}, base)); !equal(got, []string{"route_1"}) {
		t.Fatalf("replacing the credential did not clear its suspension: %v", got)
	}
	if _, err := gate.Admit(rotated, base); err != nil {
		t.Fatalf("admit after rotation: %v", err)
	}
}

// Filtering everything away is the answer, not a failure of nerve: the window is
// what bounds how often a failing upstream is retried, and overriding it because
// nothing else is left removes the bound exactly when the upstream is least
// likely to answer. A fast refusal beats a slow one, and the window ends in a
// probe regardless.
func TestEverySuspendedCandidateLeavesNothing(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		reason provider.FailureReason
		status int
	}{
		{name: "quota exhausted", reason: provider.FailureReasonSubscriptionQuotaExhausted, status: 402},
		{name: "rate limited", reason: provider.FailureReasonRateLimited, status: 429},
		{name: "availability", status: 503},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			gate := New(Config{AvailabilityThreshold: 1})
			target := targetOn("route_1", "dep_1", "cred_1", "model-a", "provider_1", 1)
			gate.Observe(target, Observation{
				Reason: testCase.reason, Status: testCase.status, Class: provider.ErrorProvider5xx,
			}, base)

			if got := gate.Filter([]provider.Target{target}, base); len(got) != 0 {
				t.Fatalf("a suspended candidate was offered anyway: %v", ids(got))
			}
			if _, err := gate.Admit(target, base); err != ErrSuspended {
				t.Fatalf("admit = %v, want ErrSuspended", err)
			}
		})
	}
}

// A healthy candidate must never be displaced by the last-resort probe: the
// probe exists for the case where refusing is the only alternative.
func TestAProbeIsOfferedOnlyWhenNothingHealthyRemains(t *testing.T) {
	gate := New(Config{AvailabilityThreshold: 1})
	sick := targetOn("route_1", "dep_1", "cred_1", "model-a", "provider_1", 1)
	well := targetOn("route_2", "dep_2", "cred_2", "model-b", "provider_2", 1)
	gate.Observe(sick, Observation{Status: 503}, base)

	if got := ids(gate.Filter([]provider.Target{sick, well}, base)); !equal(got, []string{"route_2"}) {
		t.Fatalf("filtered = %v, want only the healthy target", got)
	}
}

// Availability splits on whether an upstream answered at all. A 500 is one model
// answering badly, which on a large provider is routine and says nothing about
// the others behind the same address; a refused dial is about the address.
// Collapsing the two would take out deployments that are serving.
func TestAvailabilityScopeFollowsWhetherTheUpstreamAnswered(t *testing.T) {
	for _, testCase := range []struct {
		name         string
		observation  Observation
		wantSurvivor string
	}{
		{
			name:        "a 5xx is about the deployment that sent it",
			observation: Observation{Status: 503},
			// The sibling deployment on the same provider keeps serving.
			wantSurvivor: "route_2",
		},
		{
			name:         "a failure with no answer is about the endpoint",
			observation:  Observation{Class: provider.ErrorConnect},
			wantSurvivor: "",
		},
		{
			// The status is evidence, not the signal. An adapter that classified
			// a 5xx without filling one in must not have the endpoint suspended
			// on its behalf.
			name:         "a 5xx with no status is still about the deployment",
			observation:  Observation{Class: provider.ErrorProvider5xx},
			wantSurvivor: "route_2",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			gate := New(Config{AvailabilityThreshold: 1})
			first := targetOn("route_1", "dep_1", "cred_1", "model-a", "provider_1", 1)
			sibling := targetOn("route_2", "dep_2", "cred_1", "model-b", "provider_1", 1)
			gate.Observe(first, testCase.observation, base)

			got := ids(gate.Filter([]provider.Target{first, sibling}, base))
			switch testCase.wantSurvivor {
			case "":
				// Both are gone; the endpoint is the thing suspended. One is
				// handed back as the last-resort probe.
				if len(got) == 2 {
					t.Fatalf("an endpoint failure left both deployments admissible: %v", got)
				}
			default:
				if !equal(got, []string{testCase.wantSurvivor}) {
					t.Fatalf("filtered = %v, want %v", got, testCase.wantSurvivor)
				}
			}
		})
	}
}

// The threshold is per reason. A single 5xx is noise and must not suspend
// anything; a refusal the upstream stated outright is not noise, and counting to
// five before believing it means four more wasted round trips.
func TestThresholdsDifferBetweenNoiseAndAStatedRefusal(t *testing.T) {
	gate := New(Config{AvailabilityThreshold: 3})
	target := targetOn("route_1", "dep_1", "cred_1", "model-a", "provider_1", 1)

	for attempt := 1; attempt < 3; attempt++ {
		gate.Observe(target, Observation{Status: 500}, base)
		if got := gate.Filter([]provider.Target{target}, base); len(got) != 1 {
			t.Fatalf("suspended after %d of 3 availability failures", attempt)
		}
	}
	gate.Observe(target, Observation{Status: 500}, base)
	if _, err := gate.Admit(target, base); err != ErrSuspended {
		t.Fatalf("not suspended at the threshold: %v", err)
	}

	stated := New(Config{})
	gate2Target := targetOn("route_2", "dep_2", "cred_2", "model-b", "provider_2", 1)
	stated.Observe(gate2Target, Observation{
		Reason: provider.FailureReasonSubscriptionQuotaExhausted, Status: 402,
	}, base)
	if _, err := stated.Admit(gate2Target, base); err != ErrSuspended {
		t.Fatalf("a stated refusal did not suspend on its first observation: %v", err)
	}
}

// The upstream's own number beats Halro's guess, and a failed probe doubles the
// window rather than retrying at the same cadence forever.
func TestWindowsHonourRetryAfterAndThenDouble(t *testing.T) {
	gate := New(Config{})
	target := targetOn("route_1", "dep_1", "cred_1", "model-a", "provider_1", 1)
	gate.Observe(target, Observation{
		Reason: provider.FailureReasonRateLimited, Status: 429, RetryAfter: 20 * time.Second,
	}, base)

	if _, err := gate.Admit(target, base.Add(19*time.Second)); err != ErrSuspended {
		t.Fatal("the upstream asked for 20s and was admitted at 19s")
	}
	lease, err := gate.Admit(target, base.Add(20*time.Second))
	if err != nil {
		t.Fatalf("no probe at the end of the window: %v", err)
	}
	// Only one probe: the rest wait.
	if _, err := gate.Admit(target, base.Add(20*time.Second)); err != ErrSuspended {
		t.Fatal("a second probe was admitted into the half-open window")
	}
	lease.Done(&Observation{Reason: provider.FailureReasonRateLimited, Status: 429}, base.Add(20*time.Second))

	// 20s doubled, so still suspended at 39s past the second failure.
	if _, err := gate.Admit(target, base.Add(59*time.Second)); err != ErrSuspended {
		t.Fatal("the window did not double after the probe failed")
	}
	if _, err := gate.Admit(target, base.Add(61*time.Second)); err != nil {
		t.Fatalf("the doubled window did not expire: %v", err)
	}
}

// A window is not allowed to grow past its reason's ceiling, or a scope that
// keeps failing would drift out to a suspension nobody chose.
func TestWindowsStopAtTheirCeiling(t *testing.T) {
	gate := New(Config{})
	target := targetOn("route_1", "dep_1", "cred_1", "model-a", "provider_1", 1)
	at := base
	for round := 0; round < 12; round++ {
		gate.Observe(target, Observation{Reason: provider.FailureReasonRateLimited, Status: 429}, at)
		at = at.Add(2 * time.Hour)
	}
	// The rate-limit ceiling is a minute, so two hours later it is always over.
	if _, err := gate.Admit(target, at); err != nil {
		t.Fatalf("a bounded window grew past its ceiling: %v", err)
	}
}

// An attempt that never reached the upstream learned nothing. Letting it report
// success would clear a suspension over an upstream still refusing; letting it
// keep the slot would strand the scope past its own window.
func TestAnAbandonedProbeNeitherClearsNorStrandsTheSuspension(t *testing.T) {
	gate := New(Config{AvailabilityThreshold: 1})
	target := targetOn("route_1", "dep_1", "cred_1", "model-a", "provider_1", 1)
	gate.Observe(target, Observation{Status: 503}, base)

	after := base.Add(31 * time.Second)
	probe, err := gate.Admit(target, after)
	if err != nil {
		t.Fatalf("no probe at the end of the window: %v", err)
	}
	probe.Abandon()

	if _, err := gate.Admit(target, after); err != nil {
		t.Fatalf("the abandoned slot was not returned: %v", err)
	}
	if len(gate.Snapshot(after)) != 1 {
		t.Fatal("abandoning a probe cleared the suspension")
	}
}

// A completed request proves the endpoint answered, the deployment served and
// the credential was accepted. Clearing only the narrowest scope would leave a
// credential suspended while requests using it visibly succeed.
func TestASuccessClearsEveryScopeTheTargetBelongsTo(t *testing.T) {
	gate := New(Config{AvailabilityThreshold: 1})
	target := targetOn("route_1", "dep_1", "cred_1", "model-a", "provider_1", 1)
	gate.Observe(target, Observation{Reason: provider.FailureReasonRateLimited, Status: 429}, base)
	gate.Observe(target, Observation{Status: 503}, base)

	after := base.Add(time.Hour)
	lease, err := gate.Admit(target, after)
	if err != nil {
		t.Fatalf("no probe after both windows expired: %v", err)
	}
	lease.Done(nil, after)

	if snapshot := gate.Snapshot(after); len(snapshot) != 0 {
		t.Fatalf("a success left scopes suspended: %+v", snapshot)
	}
}

// A target with no credential identity — an older registration, a test target —
// must not be pooled with every other such target under one empty-keyed scope.
func TestATargetWithoutACredentialSuspendsNothingWider(t *testing.T) {
	gate := New(Config{})
	bare := provider.Target{ID: "route_1", DeploymentID: "dep_1", PublicModel: "chat"}
	other := provider.Target{ID: "route_2", DeploymentID: "dep_2", PublicModel: "chat"}
	gate.Observe(bare, Observation{
		Reason: provider.FailureReasonInvalidCredential, Status: 401,
	}, base)

	if got := ids(gate.Filter([]provider.Target{bare, other}, base)); !equal(got, []string{"route_1", "route_2"}) {
		t.Fatalf("a credential refusal on a target with no credential suspended something: %v", got)
	}
}

// Clear is the operator's escape hatch for a suspension dealt with outside
// Halro's view.
func TestClearRemovesASuspension(t *testing.T) {
	gate := New(Config{})
	target := targetOn("route_1", "dep_1", "cred_1", "model-a", "provider_1", 1)
	gate.Observe(target, Observation{
		Reason: provider.FailureReasonInvalidCredential, Status: 401,
	}, base)
	scope := Scope{Kind: ScopeCredential, Key: "cred_1"}
	if !gate.Clear(scope) {
		t.Fatal("Clear reported nothing to clear")
	}
	if gate.Clear(scope) {
		t.Fatal("Clear reported a second removal of the same scope")
	}
	if _, err := gate.Admit(target, base); err != nil {
		t.Fatalf("admit after clear: %v", err)
	}
}

// The probe endpoint is a model listing. It consumes no quota, so an account at
// zero balance answers it perfectly well — and if a passing probe cleared a
// credential suspension, traffic would go back to an upstream that is still
// refusing, once every probe interval, forever.
func TestAPassingProbeDoesNotClearACredentialSuspension(t *testing.T) {
	gate := New(Config{})
	target := targetOn("route_1", "dep_1", "cred_1", "model-a", "provider_1", 1)
	gate.Observe(target, Observation{
		Reason: provider.FailureReasonSubscriptionQuotaExhausted, Status: 402,
	}, base)

	gate.ObserveProbe("dep_1", DeploymentProbe{Healthy: true, ObservedAt: base}, base)

	if _, err := gate.Admit(target, base); err != ErrSuspended {
		t.Fatalf("a model-list probe cleared a quota suspension: %v", err)
	}
}

// A probe is evidence about the deployment it probed and nothing wider: it asked
// about one model, so it cannot tell an endpoint outage from one model
// misbehaving.
func TestAFailingProbeSuspendsOnlyItsOwnDeployment(t *testing.T) {
	gate := New(Config{AvailabilityThreshold: 1})
	first := targetOn("route_1", "dep_1", "cred_1", "model-a", "provider_1", 1)
	sibling := targetOn("route_2", "dep_2", "cred_1", "model-b", "provider_1", 1)
	gate.ObserveProbe("dep_1", DeploymentProbe{ObservedAt: base, ErrorClass: "connect"}, base)

	if got := ids(gate.Filter([]provider.Target{first, sibling}, base)); !equal(got, []string{"route_2"}) {
		t.Fatalf("filtered = %v, want the sibling deployment to survive", got)
	}
	probes := gate.DeploymentProbes()
	if probe, ok := probes["dep_1"]; !ok || probe.Healthy || probe.ErrorClass != "connect" {
		t.Fatalf("probe verdict not readable for the console: %+v", probes)
	}
}

// State outliving its deployment is how a metrics exporter came to emit a series
// for an ID that no longer exists.
func TestRetainDeploymentsForgetsProbesAndSuspensionsAlike(t *testing.T) {
	gate := New(Config{AvailabilityThreshold: 1})
	gone := targetOn("route_1", "dep_gone", "cred_1", "model-a", "provider_1", 1)
	gate.ObserveProbe("dep_gone", DeploymentProbe{ObservedAt: base, ErrorClass: "connect"}, base)
	gate.Observe(gone, Observation{Status: 500}, base)

	gate.RetainDeployments([]string{"dep_still_here"})

	if probes := gate.DeploymentProbes(); len(probes) != 0 {
		t.Fatalf("a deleted deployment kept its probe verdict: %+v", probes)
	}
	for _, suspension := range gate.Snapshot(base) {
		if suspension.Scope.Kind == ScopeDeployment && suspension.Scope.Key == "dep_gone" {
			t.Fatalf("a deleted deployment kept its suspension: %+v", suspension)
		}
	}
}
