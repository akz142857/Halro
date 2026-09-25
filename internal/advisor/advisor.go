// Package advisor puts Halro's own numbers next to each other and says what
// their combination means.
//
// Most of what an operator has to be told after a bad afternoon is arithmetic
// this process already holds. A request that failed at 60021ms under a 1m0s
// attempt_response_header_timeout is its own timeout firing, not a network
// fault; a four-candidate fan-out behind an attempt budget that reaches two is
// two routes that are configured and never called. Both are one subtraction and
// one division, and neither was said anywhere by anything.
//
// The package deliberately owns no I/O and no clock. Callers assemble an Input
// from configuration, topology and the live admission gate, and Evaluate is a
// pure function of it — so a finding is reproducible, testable against a table,
// and free of anything a caller wrote. Nothing here reads a prompt, a response
// body, a credential or a source address; the inputs are configuration values,
// counts and enumerations, which is what makes the result safe to render in the
// console and print from an offline `halro doctor`.
//
// A rule that finds nothing keeps its row. An advisor whose quiet rules vanish
// leaves a reader unable to tell "checked and fine" from "never ran", and the
// second is the state worth knowing about.
package advisor

import (
	"fmt"
	"sort"
	"strconv"
	"time"
)

// Status is what one rule concluded. The three are deliberately distinct:
// "checked and fine" and "could not check" are different answers and an advisor
// that collapses them is worse than no advisor.
type Status string

const (
	// StatusOK means the rule ran against every input it needs and the
	// combination it looks for is not present.
	StatusOK Status = "ok"
	// StatusWarn means the rule ran and found what it looks for. It is not an
	// error: every rule here describes a configuration Halro accepts and runs.
	StatusWarn Status = "warn"
	// StatusUnknown means an input the rule needs was not available — the
	// offline view has no admission gate, an unreadable route table has no
	// fan-out. The row stays so its absence is visible.
	StatusUnknown Status = "unknown"
)

// Term is one value a rule read, under the name an operator can search for.
// Configuration keys are spelled exactly as they appear in config.yaml; derived
// numbers are named for the question they answer.
type Term struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Finding is one rule's answer.
//
// It carries the evidence that produced it rather than a sentence about it. The
// sentence is here too, in English, because `halro doctor` prints JSON to a
// terminal and has nowhere to look a translation up — but the console renders
// its own copy from Rule and Status and uses these as the fallback, so adding a
// rule never leaves a reader with an untranslated blank.
type Finding struct {
	// Rule is the stable identifier. It is what the console translates on and
	// what an operator quotes in an issue; it never changes meaning.
	Rule   string `json:"rule"`
	Status Status `json:"status"`
	// Evidence is every value the comparison read, in the order the rule reads
	// them. It is the part that makes a finding checkable rather than believed.
	Evidence []Term `json:"evidence"`
	// Comparison is the arithmetic, written out: "1m0s >= 2m0s".
	Comparison string `json:"comparison"`
	// Consequence is one line on what the comparison means for traffic. It is
	// written for the status that was found, so an ok row says what holds
	// rather than what would have gone wrong.
	Consequence string `json:"consequence"`
}

// Suspension is one scope the admission gate is holding out of service, reduced
// to what a rule needs. Scope keys are identifiers the operator configured
// (deployment, credential, provider), never caller content.
type Suspension struct {
	ScopeKind  string
	ScopeKey   string
	Reason     string
	Status     int
	Indefinite bool
}

// ReasonCount is one row of the refusal classification counter: how many
// upstream refusals landed on a reason, and the HTTP status they carried when
// they did. Status is zero for a refusal that carried none — a transport
// failure has no status to report.
type ReasonCount struct {
	Reason string
	Status int
	Count  uint64
}

// FanOut is the widest a public model's candidate list gets, and which model
// that is. Halro walks candidates per public model, so this — not the total
// route count — is the number an attempt budget has to cover.
type FanOut struct {
	PublicModel string
	Candidates  int
}

// Input is everything the rules read. Every field is a value the caller has
// already narrowed: durations and counts from configuration, identifiers from
// the route table, enumerations from the gate.
type Input struct {
	AttemptResponseHeaderTimeout time.Duration
	RouteTotalTimeout            time.Duration
	MaxTotalAttempts             int
	MaxAttemptsPerTarget         int

	// TopologyKnown says whether the route table could be read at all. False
	// leaves the fan-out rule unknown rather than reporting a fan-out of zero,
	// which would read as "nothing to worry about".
	TopologyKnown bool
	WidestFanOut  FanOut

	// GateKnown says whether the live admission gate was reachable. The offline
	// doctor runs against a data directory with no process behind it, so it is
	// false there.
	GateKnown   bool
	Suspensions []Suspension

	// RefusalsKnown says whether the refusal classification counter was
	// readable. Same reason: offline it is not.
	RefusalsKnown bool
	Refusals      []ReasonCount
}

// Rule identifiers. They are exported because the console translates on them
// and tests assert on them; a renamed rule is a breaking change to both.
const (
	RuleAttemptHeaderTimeoutReachable = "attempt_header_timeout_reachable"
	RuleRetryFitsRouteBudget          = "retry_fits_route_budget"
	RuleAttemptBudgetReachesFanOut    = "attempt_budget_reaches_fanout"
	RuleSuspendedScopes               = "suspended_scopes"
	RuleUnclassifiedRefusals          = "unclassified_refusals"
)

// UnclassifiedReason is the label the refusal counter uses for a refusal Halro
// took no meaning from. It is duplicated rather than imported so this package
// keeps no dependency on the gateway; the caller's mapping is what binds them,
// and a test in internal/app asserts the two agree.
const UnclassifiedReason = "unclassified"

// Evaluate runs every rule against one Input and returns one Finding per rule,
// always in the same order. It is pure: same Input, same findings, which is
// what lets the result be recorded next to an audited action without becoming
// the non-deterministic thing in an otherwise verifiable system.
func Evaluate(input Input) []Finding {
	return []Finding{
		attemptHeaderTimeoutReachable(input),
		retryFitsRouteBudget(input),
		attemptBudgetReachesFanOut(input),
		suspendedScopes(input),
		unclassifiedRefusals(input),
	}
}

// attemptHeaderTimeoutReachable answers the question the #343 triage started
// from: was the 60021ms failure the network, or this instance's own clock?
//
// attempt_response_header_timeout bounds one attempt's wait for response
// headers; route_total_timeout bounds the whole request. Configure the first at
// or above the second and it can never be the thing that fires — the route
// deadline always cuts first — so a slow upstream costs the entire route budget
// and leaves nothing for a second candidate. Nothing validates the pair today,
// and a configuration that sets it that way starts cleanly.
func attemptHeaderTimeoutReachable(input Input) Finding {
	header, route := input.AttemptResponseHeaderTimeout, input.RouteTotalTimeout
	finding := Finding{
		Rule: RuleAttemptHeaderTimeoutReachable,
		Evidence: []Term{
			{Name: "gateway.attempt_response_header_timeout", Value: header.String()},
			{Name: "gateway.route_total_timeout", Value: route.String()},
		},
	}
	if header >= route {
		finding.Status = StatusWarn
		finding.Comparison = fmt.Sprintf("%s >= %s", header, route)
		finding.Consequence = "the per-attempt header timeout can never fire; the route deadline cuts first, " +
			"so a hanging upstream spends the whole request budget and no other candidate is reached"
		return finding
	}
	finding.Status = StatusOK
	finding.Comparison = fmt.Sprintf("%s < %s", header, route)
	finding.Consequence = fmt.Sprintf(
		"a hanging attempt is cut at %s, leaving %s of the request budget for another candidate", header, route-header)
	return finding
}

// retryFitsRouteBudget asks whether the per-target retry an operator configured
// can actually happen.
//
// The attempt budget is shared by the whole request, so the retries one target
// may spend have to fit inside route_total_timeout alongside everything else.
// Where they do not, the second attempt against a slow target is cut by the
// route deadline mid-flight and the retry is configuration that never runs.
//
// The rule is written against max_attempts_per_target rather than
// max_total_attempts on purpose. The total is what a fan-out spends and it
// routinely exceeds the budget in the worst case — with the shipped defaults
// four attempts of 1m0s cannot all fit in 2m0s — but that worst case needs
// every attempt to hang for its full timeout, which is not the common shape of
// a failure. Warning on it would fire on a default install and teach operators
// to ignore the panel. The number is reported as evidence instead, next to the
// candidate count, which is where it answers something.
func retryFitsRouteBudget(input Input) Finding {
	header, route := input.AttemptResponseHeaderTimeout, input.RouteTotalTimeout
	perTarget := input.MaxAttemptsPerTarget
	finding := Finding{
		Rule: RuleRetryFitsRouteBudget,
		Evidence: []Term{
			{Name: "gateway.attempt_response_header_timeout", Value: header.String()},
			{Name: "retry.max_attempts_per_target", Value: strconv.Itoa(perTarget)},
			{Name: "gateway.route_total_timeout", Value: route.String()},
			{Name: "attempts_that_fit_the_request_budget", Value: strconv.FormatInt(attemptsThatFit(header, route), 10)},
		},
	}
	needed := time.Duration(perTarget) * header
	if perTarget > 0 && needed > route {
		finding.Status = StatusWarn
		finding.Comparison = fmt.Sprintf("%s × %d = %s > %s", header, perTarget, needed, route)
		finding.Consequence = "the retries configured against one target cannot all fit in the request budget; " +
			"the last of them is cut by the route deadline rather than answered"
		return finding
	}
	finding.Status = StatusOK
	finding.Comparison = fmt.Sprintf("%s × %d = %s <= %s", header, perTarget, needed, route)
	// The count stays in the evidence rather than in the sentence. It reads
	// badly at one ("1 attempts"), and the console renders its own copy for this
	// rule and status — a sentence that varies with a number the console cannot
	// see is a sentence the two surfaces will disagree about.
	finding.Consequence = "the attempts configured against one target fit inside the request budget"
	return finding
}

// attemptsThatFit is how many attempts of the full header timeout the request
// budget holds in the worst case. It is a floor and deliberately pessimistic:
// an attempt refused before dispatch, or one that fails fast, spends less.
func attemptsThatFit(header, route time.Duration) int64 {
	if header <= 0 {
		return 0
	}
	return int64(route / header)
}

// attemptBudgetReachesFanOut is the multiplication the #343 triage needed and
// nothing performed: the number of distinct candidates a request can reach is
// ceil(max_total_attempts / max_attempts_per_target), and a route configured
// past that point is enabled, healthy, and never called.
//
// The comparison is against the widest fan-out on any one public model, because
// candidates are walked per alias. A hundred routes across a hundred aliases
// need a budget of one; four routes on one alias need four.
func attemptBudgetReachesFanOut(input Input) Finding {
	total, perTarget := input.MaxTotalAttempts, input.MaxAttemptsPerTarget
	reachable := reachableCandidates(total, perTarget)
	finding := Finding{
		Rule: RuleAttemptBudgetReachesFanOut,
		Evidence: []Term{
			{Name: "gateway.max_total_attempts", Value: strconv.Itoa(total)},
			{Name: "retry.max_attempts_per_target", Value: strconv.Itoa(perTarget)},
			{Name: "candidates_the_budget_reaches", Value: strconv.Itoa(reachable)},
		},
	}
	if !input.TopologyKnown {
		finding.Status = StatusUnknown
		finding.Comparison = fmt.Sprintf("ceil(%d / %d) = %d, widest fan-out unread", total, perTarget, reachable)
		finding.Consequence = "the route table was not available, so the budget has nothing to be compared against"
		return finding
	}
	widest := input.WidestFanOut
	finding.Evidence = append(finding.Evidence,
		Term{Name: "widest_fan_out", Value: strconv.Itoa(widest.Candidates)})
	if widest.PublicModel != "" {
		finding.Evidence = append(finding.Evidence,
			Term{Name: "widest_fan_out_public_model", Value: widest.PublicModel})
	}
	if reachable < widest.Candidates {
		finding.Status = StatusWarn
		finding.Comparison = fmt.Sprintf("ceil(%d / %d) = %d < %d", total, perTarget, reachable, widest.Candidates)
		finding.Consequence = fmt.Sprintf(
			"%q has %d candidates and the attempt budget reaches %d of them; the rest are configured and never called",
			widest.PublicModel, widest.Candidates, reachable)
		return finding
	}
	finding.Status = StatusOK
	finding.Comparison = fmt.Sprintf("ceil(%d / %d) = %d >= %d", total, perTarget, reachable, widest.Candidates)
	if widest.Candidates <= 1 {
		finding.Consequence = "no public model has more than one candidate yet, so the attempt budget covers every fan-out configured"
		return finding
	}
	finding.Consequence = fmt.Sprintf(
		"the attempt budget reaches every candidate of the widest fan-out, %q with %d", widest.PublicModel, widest.Candidates)
	return finding
}

// reachableCandidates is the ceiling division the two attempt ceilings imply.
// It is a floor on what a request reaches, not a promise: a target refused
// before dispatch spends no budget, so later candidates can still be walked.
func reachableCandidates(total, perTarget int) int {
	if perTarget < 1 || total < 1 {
		return 0
	}
	return (total + perTarget - 1) / perTarget
}

// suspendedScopes names what the admission gate is currently refusing.
//
// The gate already holds this and the console already lists it. The rule's job
// is to put it in the same reading as the arithmetic above, because the two
// answer one question together. A scope suspended with no reason was suspended
// by the availability policy, which counts consecutive connect failures,
// timeouts and 5xx without separating them — so the attempt deadline that
// produced some of those failures is the first number to check, and the rules
// above are the ones that judge it.
func suspendedScopes(input Input) Finding {
	finding := Finding{Rule: RuleSuspendedScopes}
	if !input.GateKnown {
		finding.Status = StatusUnknown
		finding.Comparison = "admission gate not read"
		finding.Consequence = "suspensions live in the running process; this view has no process to ask"
		return finding
	}
	held := append([]Suspension(nil), input.Suspensions...)
	sort.Slice(held, func(left, right int) bool {
		if held[left].Indefinite != held[right].Indefinite {
			return held[left].Indefinite
		}
		if held[left].ScopeKind != held[right].ScopeKind {
			return held[left].ScopeKind < held[right].ScopeKind
		}
		return held[left].ScopeKey < held[right].ScopeKey
	})
	finding.Evidence = append(finding.Evidence,
		Term{Name: "suspended_scopes", Value: strconv.Itoa(len(held))})
	indefinite := 0
	unclassified := 0
	for _, suspension := range held {
		if suspension.Indefinite {
			indefinite++
		}
		if suspension.Reason == UnclassifiedReason || suspension.Reason == "" {
			unclassified++
		}
		finding.Evidence = append(finding.Evidence, Term{
			Name:  suspension.ScopeKind + ":" + suspension.ScopeKey,
			Value: suspensionValue(suspension),
		})
	}
	if len(held) == 0 {
		finding.Status = StatusOK
		finding.Comparison = "0 scopes suspended"
		finding.Consequence = "every configured candidate is admissible right now"
		return finding
	}
	// The header timeout is named alongside because an availability suspension
	// counts the failures this instance's own deadline produced, and that number
	// is the first thing to check before looking at the upstream.
	if unclassified > 0 {
		finding.Evidence = append(finding.Evidence, Term{
			Name:  "gateway.attempt_response_header_timeout",
			Value: input.AttemptResponseHeaderTimeout.String(),
		})
	}
	finding.Status = StatusWarn
	finding.Comparison = fmt.Sprintf("%d scopes suspended, %d of them indefinitely", len(held), indefinite)
	switch {
	case indefinite > 0:
		finding.Consequence = "an indefinite suspension ends when the credential's revision advances or an operator clears it, not on a clock"
	case unclassified > 0:
		finding.Consequence = "a scope suspended without a reason was held by the availability policy, which counts the attempts this instance's own deadline cut; check that number before the upstream"
	default:
		finding.Consequence = "these scopes are held out of routing until their window expires"
	}
	return finding
}

func suspensionValue(suspension Suspension) string {
	value := suspension.Reason
	if suspension.Status > 0 {
		value += " " + strconv.Itoa(suspension.Status)
	}
	if suspension.Indefinite {
		value += " (indefinite)"
	}
	return value
}

// unclassifiedRefusals reports refusals Halro took no meaning from.
//
// A refusal that classifies drives routing: an exhausted subscription is
// suspended on a different clock from a rate limit. A refusal that does not
// falls to the availability policy, which counts consecutive failures and
// suspends briefly — the right handling for an upstream having a bad minute and
// the wrong handling for an account out of money. The pair kept here, reason
// and the status it carried, is the record that turns a vendor classification
// table from recollection into evidence, and until now it existed only in a
// Prometheus series an operator had to already be scraping.
func unclassifiedRefusals(input Input) Finding {
	finding := Finding{Rule: RuleUnclassifiedRefusals}
	if !input.RefusalsKnown {
		finding.Status = StatusUnknown
		finding.Comparison = "refusal classification not read"
		finding.Consequence = "the classification counter lives in the running process; this view has no process to ask"
		return finding
	}
	var total, unclassified uint64
	statuses := map[int]uint64{}
	for _, row := range input.Refusals {
		total += row.Count
		if row.Reason != UnclassifiedReason {
			continue
		}
		unclassified += row.Count
		statuses[row.Status] += row.Count
	}
	finding.Evidence = []Term{
		{Name: "refusals_observed", Value: strconv.FormatUint(total, 10)},
		{Name: "refusals_unclassified", Value: strconv.FormatUint(unclassified, 10)},
	}
	for _, status := range sortedStatuses(statuses) {
		finding.Evidence = append(finding.Evidence, Term{
			Name:  "unclassified_provider_status:" + statusLabel(status),
			Value: strconv.FormatUint(statuses[status], 10),
		})
	}
	if unclassified == 0 {
		finding.Status = StatusOK
		finding.Comparison = fmt.Sprintf("%d of %d refusals unclassified", unclassified, total)
		if total == 0 {
			finding.Consequence = "no upstream refusal has been observed since this process started"
			return finding
		}
		finding.Consequence = "every refusal observed so far carried a reason the admission gate could act on"
		return finding
	}
	finding.Status = StatusWarn
	finding.Comparison = fmt.Sprintf("%d of %d refusals unclassified", unclassified, total)
	finding.Consequence = "these refusals fell to the availability policy, which suspends for seconds; " +
		"a quota or credential refusal among them is being retried on the wrong clock"
	return finding
}

func sortedStatuses(statuses map[int]uint64) []int {
	result := make([]int, 0, len(statuses))
	for status := range statuses {
		result = append(result, status)
	}
	sort.Ints(result)
	return result
}

// statusLabel keeps the no-status case legible. A transport failure carries no
// HTTP status, and printing "0" would claim one that never existed.
func statusLabel(status int) string {
	if status <= 0 {
		return "none"
	}
	return strconv.Itoa(status)
}
