package advisor

import (
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
)

// findingFor is the lookup every test below uses. Evaluate returns one row per
// rule whether or not the rule fired, so a missing row is a bug in Evaluate and
// not a case the caller has to handle.
func findingFor(t *testing.T, findings []Finding, rule string) Finding {
	t.Helper()
	for _, finding := range findings {
		if finding.Rule == rule {
			return finding
		}
	}
	t.Fatalf("no finding for rule %q", rule)
	return Finding{}
}

func evidenceFor(t *testing.T, finding Finding, name string) string {
	t.Helper()
	for _, term := range finding.Evidence {
		if term.Name == name {
			return term.Value
		}
	}
	t.Fatalf("rule %q carries no evidence term %q", finding.Rule, name)
	return ""
}

// healthyInput is the shipped default, stated as durations so a test reads as
// the arithmetic it is checking.
func healthyInput() Input {
	return Input{
		AttemptResponseHeaderTimeout: time.Minute,
		RouteTotalTimeout:            2 * time.Minute,
		MaxTotalAttempts:             4,
		MaxAttemptsPerTarget:         1,
		TopologyKnown:                true,
		WidestFanOut:                 FanOut{PublicModel: "gpt-4o", Candidates: 3},
		GateKnown:                    true,
		RefusalsKnown:                true,
	}
}

// TestEveryRuleKeepsItsRow is the property the whole surface rests on: a rule
// that found nothing still reports, because "checked and fine" and "never ran"
// are different answers and a panel that shows only problems cannot tell them
// apart.
func TestEveryRuleKeepsItsRow(t *testing.T) {
	findings := Evaluate(healthyInput())
	want := []string{
		RuleAttemptHeaderTimeoutReachable,
		RuleRetryFitsRouteBudget,
		RuleAttemptBudgetReachesFanOut,
		RuleSuspendedScopes,
		RuleUnclassifiedRefusals,
	}
	if len(findings) != len(want) {
		t.Fatalf("got %d findings, want %d", len(findings), len(want))
	}
	for index, rule := range want {
		if findings[index].Rule != rule {
			t.Fatalf("finding %d is %q, want %q", index, findings[index].Rule, rule)
		}
		if findings[index].Status != StatusOK {
			t.Fatalf("rule %q on a healthy input is %q", rule, findings[index].Status)
		}
		if findings[index].Comparison == "" || findings[index].Consequence == "" {
			t.Fatalf("rule %q reported without a comparison or a consequence", rule)
		}
	}
}

// TestEvaluateIsDeterministic guards the property that lets a finding be shown
// next to an audited action: the same numbers produce the same sentences, in
// the same order, every time.
func TestEvaluateIsDeterministic(t *testing.T) {
	input := healthyInput()
	input.Suspensions = []Suspension{
		{ScopeKind: "credential", ScopeKey: "cred-b", Reason: "invalid_credential", Status: 401, Indefinite: true},
		{ScopeKind: "deployment", ScopeKey: "dep-a", Reason: UnclassifiedReason, Status: 503},
		{ScopeKind: "credential", ScopeKey: "cred-a", Reason: "rate_limited", Status: 429},
	}
	input.Refusals = []ReasonCount{
		{Reason: UnclassifiedReason, Status: 500, Count: 2},
		{Reason: "rate_limited", Status: 429, Count: 7},
		{Reason: UnclassifiedReason, Status: 402, Count: 3},
	}
	first, second := Evaluate(input), Evaluate(input)
	if len(first) != len(second) {
		t.Fatalf("two evaluations returned %d and %d findings", len(first), len(second))
	}
	for index := range first {
		if first[index].Rule != second[index].Rule ||
			first[index].Status != second[index].Status ||
			first[index].Comparison != second[index].Comparison ||
			first[index].Consequence != second[index].Consequence ||
			len(first[index].Evidence) != len(second[index].Evidence) {
			t.Fatalf("rule %q differs between two evaluations of the same input", first[index].Rule)
		}
		for term := range first[index].Evidence {
			if first[index].Evidence[term] != second[index].Evidence[term] {
				t.Fatalf("rule %q evidence differs between two evaluations", first[index].Rule)
			}
		}
	}
}

// TestHeaderTimeoutAtOrAboveRouteBudget is the #343 incident stated as a table.
// The failing configuration is one Halro accepts and starts on, which is why it
// took reading three files to find.
func TestHeaderTimeoutAtOrAboveRouteBudget(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		header time.Duration
		route  time.Duration
		want   Status
	}{
		{"header below the route budget", time.Minute, 2 * time.Minute, StatusOK},
		{"header equal to the route budget", 2 * time.Minute, 2 * time.Minute, StatusWarn},
		{"header above the route budget", 5 * time.Minute, 2 * time.Minute, StatusWarn},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			input := healthyInput()
			input.AttemptResponseHeaderTimeout, input.RouteTotalTimeout = testCase.header, testCase.route
			finding := findingFor(t, Evaluate(input), RuleAttemptHeaderTimeoutReachable)
			if finding.Status != testCase.want {
				t.Fatalf("status %q, want %q (%s)", finding.Status, testCase.want, finding.Comparison)
			}
			if got := evidenceFor(t, finding, "gateway.attempt_response_header_timeout"); got != testCase.header.String() {
				t.Fatalf("evidence carries %q, want %q", got, testCase.header)
			}
		})
	}
}

// TestRetryFitsRouteBudget checks the second multiplication from the incident,
// and pins the evidence term that carries the worst-case attempt count — the
// number the rule deliberately does not warn on.
func TestRetryFitsRouteBudget(t *testing.T) {
	input := healthyInput()
	input.AttemptResponseHeaderTimeout = 90 * time.Second
	input.MaxAttemptsPerTarget = 2
	finding := findingFor(t, Evaluate(input), RuleRetryFitsRouteBudget)
	if finding.Status != StatusWarn {
		t.Fatalf("90s × 2 against a 2m budget is %q", finding.Status)
	}
	if got := evidenceFor(t, finding, "attempts_that_fit_the_request_budget"); got != "1" {
		t.Fatalf("attempts that fit is %q, want 1", got)
	}
}

// TestDefaultConfigurationRaisesNoWarning is the noise test. Rules that fire on
// a stock install are rules operators learn to scroll past, so the shipped
// defaults have to come out clean — and if a default ever changes into a
// combination a rule objects to, this is where it is noticed.
func TestDefaultConfigurationRaisesNoWarning(t *testing.T) {
	cfg := config.Default()
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	input := Input{
		AttemptResponseHeaderTimeout: cfg.Gateway.AttemptResponseHeaderTimeout.Value(),
		RouteTotalTimeout:            cfg.Gateway.RouteTotalTimeout.Value(),
		MaxTotalAttempts:             cfg.Gateway.MaxTotalAttempts,
		MaxAttemptsPerTarget:         cfg.Retry.MaxAttemptsPerTarget,
		TopologyKnown:                true,
		WidestFanOut:                 FanOut{PublicModel: "gpt-4o", Candidates: 4},
		GateKnown:                    true,
		RefusalsKnown:                true,
	}
	for _, finding := range Evaluate(input) {
		if finding.Status != StatusOK {
			t.Fatalf("the shipped defaults make rule %q report %q: %s", finding.Rule, finding.Status, finding.Comparison)
		}
	}
}

// TestFanOutBeyondTheAttemptBudget is the third row of the incident table: the
// route that is configured, enabled, healthy and never called.
func TestFanOutBeyondTheAttemptBudget(t *testing.T) {
	input := healthyInput()
	input.MaxTotalAttempts, input.MaxAttemptsPerTarget = 3, 2
	input.WidestFanOut = FanOut{PublicModel: "chat-fast", Candidates: 3}
	finding := findingFor(t, Evaluate(input), RuleAttemptBudgetReachesFanOut)
	if finding.Status != StatusWarn {
		t.Fatalf("ceil(3/2)=2 against a fan-out of 3 is %q", finding.Status)
	}
	if got := evidenceFor(t, finding, "candidates_the_budget_reaches"); got != "2" {
		t.Fatalf("reachable candidates is %q, want 2", got)
	}
	if !strings.Contains(finding.Consequence, "chat-fast") {
		t.Fatalf("the consequence does not name the alias: %q", finding.Consequence)
	}
}

// TestUnreadInputsAreUnknownNotFine is the fail-closed half of the design. An
// offline run has no process to ask, and reporting that as "ok" would be the
// advisor asserting something it never checked.
func TestUnreadInputsAreUnknownNotFine(t *testing.T) {
	input := healthyInput()
	input.TopologyKnown, input.GateKnown, input.RefusalsKnown = false, false, false
	findings := Evaluate(input)
	for _, rule := range []string{RuleAttemptBudgetReachesFanOut, RuleSuspendedScopes, RuleUnclassifiedRefusals} {
		if status := findingFor(t, findings, rule).Status; status != StatusUnknown {
			t.Fatalf("rule %q with no input reports %q, want %q", rule, status, StatusUnknown)
		}
	}
	// The two pure-configuration rules still answer, because configuration is
	// readable with nothing running.
	for _, rule := range []string{RuleAttemptHeaderTimeoutReachable, RuleRetryFitsRouteBudget} {
		if status := findingFor(t, findings, rule).Status; status == StatusUnknown {
			t.Fatalf("rule %q needs only configuration and reported %q", rule, status)
		}
	}
}

// TestSuspensionsNameTheAttemptDeadline pins the reason the gate's own listing
// is not enough on its own: an availability suspension counts failures this
// instance's deadline produced, so the deadline belongs in the same row.
func TestSuspensionsNameTheAttemptDeadline(t *testing.T) {
	input := healthyInput()
	input.Suspensions = []Suspension{{ScopeKind: "deployment", ScopeKey: "dep-a", Reason: UnclassifiedReason, Status: 504}}
	finding := findingFor(t, Evaluate(input), RuleSuspendedScopes)
	if finding.Status != StatusWarn {
		t.Fatalf("a held suspension reports %q", finding.Status)
	}
	if got := evidenceFor(t, finding, "gateway.attempt_response_header_timeout"); got != time.Minute.String() {
		t.Fatalf("the attempt deadline is %q, want %q", got, time.Minute)
	}
	if got := evidenceFor(t, finding, "deployment:dep-a"); got != "unclassified 504" {
		t.Fatalf("suspension evidence is %q", got)
	}
}

// TestIndefiniteSuspensionLeadsTheConsequence: an indefinite suspension is the
// only one an operator has to act on, so it decides the sentence even when a
// timed suspension is also held.
func TestIndefiniteSuspensionLeadsTheConsequence(t *testing.T) {
	input := healthyInput()
	input.Suspensions = []Suspension{
		{ScopeKind: "deployment", ScopeKey: "dep-a", Reason: UnclassifiedReason, Status: 504},
		{ScopeKind: "credential", ScopeKey: "cred-a", Reason: "invalid_credential", Status: 401, Indefinite: true},
	}
	finding := findingFor(t, Evaluate(input), RuleSuspendedScopes)
	if !strings.Contains(finding.Consequence, "revision") {
		t.Fatalf("the consequence does not say what ends an indefinite suspension: %q", finding.Consequence)
	}
	if finding.Evidence[1].Name != "credential:cred-a" {
		t.Fatalf("the indefinite suspension is not listed first: %q", finding.Evidence[1].Name)
	}
}

// TestUnclassifiedRefusalsNameTheirStatuses is the discovery signal #319 asked
// for, reported to the operator rather than only to a Prometheus scrape: a
// refusal Halro took no meaning from, with the status it was looking at.
func TestUnclassifiedRefusalsNameTheirStatuses(t *testing.T) {
	input := healthyInput()
	input.Refusals = []ReasonCount{
		{Reason: "rate_limited", Status: 429, Count: 5},
		{Reason: UnclassifiedReason, Status: 402, Count: 3},
		{Reason: UnclassifiedReason, Status: 0, Count: 1},
	}
	finding := findingFor(t, Evaluate(input), RuleUnclassifiedRefusals)
	if finding.Status != StatusWarn {
		t.Fatalf("four unclassified refusals report %q", finding.Status)
	}
	if got := evidenceFor(t, finding, "refusals_observed"); got != "9" {
		t.Fatalf("refusals observed is %q, want 9", got)
	}
	if got := evidenceFor(t, finding, "unclassified_provider_status:402"); got != "3" {
		t.Fatalf("402 count is %q, want 3", got)
	}
	// A transport failure carries no status, and "0" would claim one.
	if got := evidenceFor(t, finding, "unclassified_provider_status:none"); got != "1" {
		t.Fatalf("statusless count is %q, want 1", got)
	}
}

// TestNoRefusalsIsNotAClassificationProblem separates the two ok cases: nothing
// has failed yet, and everything that failed classified. They read very
// differently to someone deciding whether to trust the panel.
func TestNoRefusalsIsNotAClassificationProblem(t *testing.T) {
	quiet := findingFor(t, Evaluate(healthyInput()), RuleUnclassifiedRefusals)
	if !strings.Contains(quiet.Consequence, "no upstream refusal") {
		t.Fatalf("a quiet instance says %q", quiet.Consequence)
	}
	input := healthyInput()
	input.Refusals = []ReasonCount{{Reason: "rate_limited", Status: 429, Count: 4}}
	busy := findingFor(t, Evaluate(input), RuleUnclassifiedRefusals)
	if busy.Status != StatusOK || strings.Contains(busy.Consequence, "no upstream refusal") {
		t.Fatalf("classified refusals report %q: %q", busy.Status, busy.Consequence)
	}
}

// TestDegenerateAttemptCeilingsDoNotPanic. Validation refuses these before a
// start, but doctor evaluates a configuration that failed validation and the
// advisor must not be the thing that crashes on it.
func TestDegenerateAttemptCeilingsDoNotPanic(t *testing.T) {
	input := healthyInput()
	input.MaxTotalAttempts, input.MaxAttemptsPerTarget = 0, 0
	input.AttemptResponseHeaderTimeout = 0
	for _, finding := range Evaluate(input) {
		if finding.Comparison == "" || finding.Consequence == "" {
			t.Fatalf("rule %q reported nothing on a degenerate configuration", finding.Rule)
		}
	}
}
