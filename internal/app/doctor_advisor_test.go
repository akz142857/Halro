package app

import (
	"context"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/store/lock"

	"github.com/akz142857/Halro/internal/advisor"
)

// TestDoctorReportsFindingsOnAFreshDataDirectory. The offline view answers the
// two rules that read only configuration, and says it did not look at the two
// that need a running process. Reporting those as "ok" would have doctor assert
// something it never checked, which is the one thing a fail-closed diagnostic
// must not do.
func TestDoctorReportsFindingsOnAFreshDataDirectory(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	report, err := Doctor(context.Background(), cfg)
	if err != nil || !report.Healthy {
		t.Fatalf("healthy doctor report=%#v err=%v", report, err)
	}
	statuses := make(map[string]advisor.Status, len(report.Findings))
	for _, finding := range report.Findings {
		statuses[finding.Rule] = finding.Status
	}
	for rule, want := range map[string]advisor.Status{
		advisor.RuleAttemptHeaderTimeoutReachable: advisor.StatusOK,
		advisor.RuleRetryFitsRouteBudget:          advisor.StatusOK,
		// The route table is readable offline, and an instance with no routes
		// has a fan-out every budget covers.
		advisor.RuleAttemptBudgetReachesFanOut: advisor.StatusOK,
		advisor.RuleSuspendedScopes:            advisor.StatusUnknown,
		advisor.RuleUnclassifiedRefusals:       advisor.StatusUnknown,
	} {
		if statuses[rule] != want {
			t.Fatalf("offline rule %q is %q, want %q", rule, statuses[rule], want)
		}
	}
}

// TestDoctorFindingsDoNotDecideHealth. A finding describes a configuration
// Halro accepts and runs; a check describes an instance that is or is not
// intact. Folding one into the other would make `halro doctor` exit non-zero
// over a timeout pair an operator chose on purpose, and every deployment script
// that gates on it would stop.
func TestDoctorFindingsDoNotDecideHealth(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	// A header timeout at the route budget: rule one warns, and validation
	// still accepts it, because nothing validates the pair.
	cfg.Gateway.AttemptResponseHeaderTimeout = cfg.Gateway.RouteTotalTimeout
	report, err := Doctor(context.Background(), cfg)
	if err != nil {
		t.Fatalf("doctor failed on a configuration it accepts: %v", err)
	}
	if !report.Healthy {
		t.Fatal("a finding made the instance unhealthy")
	}
	var warned bool
	for _, finding := range report.Findings {
		if finding.Rule == advisor.RuleAttemptHeaderTimeoutReachable && finding.Status == advisor.StatusWarn {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("an unreachable attempt deadline produced no finding: %+v", report.Findings)
	}
}

// TestDoctorFindingsCarryTheirEvidence. The point of the surface is that a
// finding can be checked rather than believed, so every row has to arrive with
// the values its comparison read.
func TestDoctorFindingsCarryTheirEvidence(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	report, err := Doctor(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Findings) == 0 {
		t.Fatal("doctor reported no findings at all")
	}
	for _, finding := range report.Findings {
		if finding.Comparison == "" || finding.Consequence == "" {
			t.Fatalf("rule %q arrived without a comparison or a consequence", finding.Rule)
		}
		if finding.Status == advisor.StatusUnknown {
			continue
		}
		if len(finding.Evidence) == 0 {
			t.Fatalf("rule %q concluded %q with no evidence", finding.Rule, finding.Status)
		}
	}
	if time.Since(report.CheckedAt) > time.Hour {
		t.Fatalf("report timestamp %s is not from this run", report.CheckedAt)
	}
}

// TestDoctorAnswersTheConfigurationRulesWhileTheInstanceIsRunning. Everything
// past the data lock needs exclusive offline access, which a live process
// holds. The two rules that read only configuration do not, and a run that
// printed nothing at all would be silent at exactly the moment the question is
// being asked.
func TestDoctorAnswersTheConfigurationRulesWhileTheInstanceIsRunning(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	held, err := lock.AcquireExistingReadOnly(cfg.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	report, err := Doctor(context.Background(), cfg)
	if err == nil {
		t.Fatal("doctor acquired a lock another holder has")
	}
	statuses := make(map[string]advisor.Status, len(report.Findings))
	for _, finding := range report.Findings {
		statuses[finding.Rule] = finding.Status
	}
	for _, rule := range []string{advisor.RuleAttemptHeaderTimeoutReachable, advisor.RuleRetryFitsRouteBudget} {
		if statuses[rule] != advisor.StatusOK {
			t.Fatalf("rule %q is %q behind a held lock, want %q", rule, statuses[rule], advisor.StatusOK)
		}
	}
	// And the rules that genuinely could not be read still say so.
	if statuses[advisor.RuleAttemptBudgetReachesFanOut] != advisor.StatusUnknown {
		t.Fatalf("the fan-out rule reports %q with no store open", statuses[advisor.RuleAttemptBudgetReachesFanOut])
	}
}
