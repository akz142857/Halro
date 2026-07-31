package tokenguard

import (
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Heimdall/internal/domain"
)

func testPolicy() domain.TokenGuardPolicy {
	return domain.TokenGuardPolicy{
		ID: "guard_1", Name: "strict", Enabled: true, Action: "temporary_block",
		RequestTokens: 100, MinimumSamples: 2, ViolationsBeforeBlock: 2,
		BlockTTL: time.Minute, Cooldown: 30 * time.Second,
	}
}

func TestSingleViolationDoesNotBlockAndRepeatedViolationDoes(t *testing.T) {
	manager, err := New([]domain.TokenGuardPolicy{testPolicy()})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	input := Input{
		PolicyID: "guard_1", ProjectID: "project_1", KeyID: "key_1",
		EstimatedTokens: 101, Now: now,
	}
	first := manager.Admit(input)
	if !first.Allowed || first.Status != StatusSuspicious {
		t.Fatalf("first=%#v", first)
	}
	input.Now = now.Add(time.Second)
	second := manager.Admit(input)
	if second.Allowed || second.Status != StatusTemporarilyBlocked {
		t.Fatalf("second=%#v", second)
	}
	input.Now = now.Add(2 * time.Second)
	if decision := manager.Admit(input); decision.Allowed {
		t.Fatal("blocked key was admitted")
	}
}

func TestTemporaryBlockAutomaticallyRecovers(t *testing.T) {
	manager, _ := New([]domain.TokenGuardPolicy{testPolicy()})
	now := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	input := Input{PolicyID: "guard_1", ProjectID: "p", KeyID: "k", EstimatedTokens: 101, Now: now}
	manager.Admit(input)
	input.Now = now.Add(time.Second)
	manager.Admit(input)
	input.EstimatedTokens = 1
	input.Now = now.Add(time.Minute + 2*time.Second)
	if decision := manager.Admit(input); !decision.Allowed || decision.Status != StatusNormal {
		t.Fatalf("decision=%#v", decision)
	}
}

func TestUnblockProjectClearsEveryKeyOnlyInThatProject(t *testing.T) {
	manager, _ := New([]domain.TokenGuardPolicy{testPolicy()})
	now := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	for _, subject := range []struct{ project, key string }{{"p1", "k1"}, {"p1", "k2"}, {"p2", "k3"}} {
		input := Input{PolicyID: "guard_1", ProjectID: subject.project, KeyID: subject.key, EstimatedTokens: 101, Now: now}
		manager.Admit(input)
		input.Now = now.Add(time.Second)
		manager.Admit(input)
	}
	if count := manager.UnblockProject("p1"); count != 2 {
		t.Fatalf("unblocked=%d", count)
	}
	for _, key := range []string{"k1", "k2"} {
		decision := manager.Admit(Input{PolicyID: "guard_1", ProjectID: "p1", KeyID: key, EstimatedTokens: 1, Now: now.Add(2 * time.Second)})
		if !decision.Allowed || decision.Status != StatusNormal {
			t.Fatalf("key %s remained blocked: %#v", key, decision)
		}
	}
	if decision := manager.Admit(Input{PolicyID: "guard_1", ProjectID: "p2", KeyID: "k3", EstimatedTokens: 1, Now: now.Add(2 * time.Second)}); decision.Allowed {
		t.Fatalf("other project was unblocked: %#v", decision)
	}
}

func TestRejectedRequestsDoNotFeedRollingWindow(t *testing.T) {
	policy := testPolicy()
	policy.RequestTokens = 0
	policy.TokensPerMinute = 100
	manager, _ := New([]domain.TokenGuardPolicy{policy})
	now := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	input := Input{PolicyID: policy.ID, ProjectID: "p", KeyID: "k", EstimatedTokens: 60, Now: now}
	if decision := manager.Admit(input); !decision.Allowed {
		t.Fatal(decision)
	}
	input.Now = now.Add(time.Second)
	if decision := manager.Admit(input); !decision.Allowed {
		t.Fatal("first threshold violation should only be suspicious")
	}
	input.Now = now.Add(2 * time.Second)
	if decision := manager.Admit(input); decision.Allowed {
		t.Fatal("repeated violation should block")
	}
	input.Now = now.Add(3 * time.Second)
	if decision := manager.Admit(input); decision.Allowed {
		t.Fatal("rejections must not clear or inflate the block")
	}
}

func TestAlertDeduplicatesWithinCooldown(t *testing.T) {
	policy := testPolicy()
	policy.Action = "alert"
	manager, _ := New([]domain.TokenGuardPolicy{policy})
	now := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	input := Input{PolicyID: policy.ID, ProjectID: "p", KeyID: "k", EstimatedTokens: 101, Now: now}
	manager.Admit(input)
	input.Now = now.Add(time.Second)
	manager.Admit(input)
	if got := len(manager.events); got != 1 {
		t.Fatalf("events=%d", got)
	}
}

func TestErrorRateUsesCompletedAcceptedRequests(t *testing.T) {
	policy := testPolicy()
	policy.Action = "alert"
	policy.RequestTokens = 0
	policy.ErrorRate = 0.5
	policy.MinimumSamples = 2
	manager, _ := New([]domain.TokenGuardPolicy{policy})
	now := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	input := Input{PolicyID: policy.ID, ProjectID: "p", KeyID: "k", EstimatedTokens: 1, Now: now}
	manager.Admit(input)
	manager.Complete(policy.ID, "p", "k", now, true)
	input.Now = now.Add(time.Second)
	manager.Admit(input)
	manager.Complete(policy.ID, "p", "k", input.Now, true)
	input.Now = now.Add(2 * time.Second)
	decision := manager.Admit(input)
	if !decision.Allowed || decision.Reason != "error_rate" {
		t.Fatalf("decision=%#v", decision)
	}
}

func ewmaPolicy() domain.TokenGuardPolicy {
	return domain.TokenGuardPolicy{
		ID: "guard_ewma", Name: "EWMA", Enabled: true, Action: "temporary_block",
		ViolationsBeforeBlock: 2, BlockTTL: time.Minute, Cooldown: time.Minute,
		EWMAEnabled: true, EWMAAlpha: 0.5, EWMAMultiplier: 2,
		EWMAMinimumSamples: 10, EWMAWarmup: 10 * time.Second,
		EWMAEvaluationWindow: 10 * time.Second, EWMACooldown: time.Minute,
		EWMAAbsoluteRPM: 100,
	}
}

func TestEWMAConfigurationRequiresSafeDetectOnlyBounds(t *testing.T) {
	for name, mutate := range map[string]func(*domain.TokenGuardPolicy){
		"alpha":      func(policy *domain.TokenGuardPolicy) { policy.EWMAAlpha = 0 },
		"multiplier": func(policy *domain.TokenGuardPolicy) { policy.EWMAMultiplier = 1 },
		"samples":    func(policy *domain.TokenGuardPolicy) { policy.EWMAMinimumSamples = 9 },
		"window":     func(policy *domain.TokenGuardPolicy) { policy.EWMAEvaluationWindow = 11 * time.Second },
		"warmup":     func(policy *domain.TokenGuardPolicy) { policy.EWMAWarmup = time.Second },
		"cooldown":   func(policy *domain.TokenGuardPolicy) { policy.EWMACooldown = 0 },
		"no floors":  func(policy *domain.TokenGuardPolicy) { policy.EWMAAbsoluteRPM = 0 },
		"negative":   func(policy *domain.TokenGuardPolicy) { policy.EWMAAbsoluteTPM = -1 },
	} {
		t.Run(name, func(t *testing.T) {
			policy := ewmaPolicy()
			mutate(&policy)
			if _, err := New([]domain.TokenGuardPolicy{policy}); err == nil {
				t.Fatal("unsafe EWMA configuration was accepted")
			}
		})
	}
}

func TestEWMADetectsCompletedWindowButNeverBlocks(t *testing.T) {
	policy := ewmaPolicy()
	manager, err := New([]domain.TokenGuardPolicy{policy})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	input := Input{PolicyID: policy.ID, ProjectID: "p", KeyID: "k", EstimatedTokens: 1}
	// Ten requests in the first window establish a 60 RPM baseline.
	for index := 0; index < 10; index++ {
		input.Now = start.Add(time.Duration(index) * time.Second)
		if decision := manager.Admit(input); !decision.Allowed || decision.Reason != "" {
			t.Fatalf("warmup decision %d=%#v", index, decision)
		}
	}
	// Ten requests in the next window evaluate to 60 RPM too; use 20 requests
	// in the following completed window to exceed both floor and multiplier.
	input.Now = start.Add(10 * time.Second)
	manager.Admit(input) // advances and records the first request in window 2
	for index := 1; index < 10; index++ {
		input.Now = start.Add(10*time.Second + time.Duration(index)*time.Second)
		manager.Admit(input)
	}
	input.Now = start.Add(20 * time.Second)
	manager.Admit(input) // advances the normal window
	for index := 1; index <= 20; index++ {
		input.Now = start.Add(20*time.Second + time.Duration(index)*400*time.Millisecond)
		manager.Admit(input)
	}
	input.Now = start.Add(30 * time.Second)
	decision := manager.Admit(input)
	if !decision.Allowed || decision.Status != StatusSuspicious || decision.Reason != "ewma_rpm" {
		t.Fatalf("detect-only decision=%#v", decision)
	}
	// Repeated anomalous windows remain allowed even though the fixed-threshold
	// action for this policy is temporary_block.
	for window := 3; window < 6; window++ {
		for index := 1; index < 20; index++ {
			input.Now = start.Add(time.Duration(window)*10*time.Second + time.Duration(index)*400*time.Millisecond)
			if current := manager.Admit(input); !current.Allowed {
				t.Fatalf("EWMA blocked at window=%d index=%d: %#v", window, index, current)
			}
		}
		input.Now = start.Add(time.Duration(window+1) * 10 * time.Second)
		if current := manager.Admit(input); !current.Allowed {
			t.Fatalf("EWMA boundary blocked: %#v", current)
		}
	}
	select {
	case event := <-manager.Events():
		if event.Type != "token_guard_ewma_detected" || event.Reason != "ewma_rpm" || event.Status != StatusSuspicious {
			t.Fatalf("unexpected EWMA event: %#v", event)
		}
	default:
		t.Fatal("EWMA did not emit a detect-only event")
	}
	select {
	case duplicate := <-manager.Events():
		t.Fatalf("EWMA cooldown did not deduplicate event: %#v", duplicate)
	default:
	}
}

func TestEWMARelativeRulesCoverRPMTPMAverageTokensAndCost(t *testing.T) {
	started := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	baseline := ewmaBaseline{
		StartedAt: started, WindowStart: started, Samples: 100, Initialized: true,
		RPM: 10, TPM: 100, TokensPerRequest: 10, CostPerMinute: 100,
	}
	basePolicy := ewmaPolicy()
	basePolicy.EWMAMinimumSamples = 10
	basePolicy.EWMAWarmup = 10 * time.Second
	basePolicy.EWMAMultiplier = 2
	for _, test := range []struct {
		name   string
		policy domain.TokenGuardPolicy
		window ewmaWindow
		want   string
	}{
		{"rpm", func() domain.TokenGuardPolicy { p := basePolicy; p.EWMAAbsoluteRPM = 15; return p }(), ewmaWindow{rpm: 21}, "ewma_rpm"},
		{"tpm", func() domain.TokenGuardPolicy {
			p := basePolicy
			p.EWMAAbsoluteRPM = 0
			p.EWMAAbsoluteTPM = 150
			return p
		}(), ewmaWindow{tpm: 201}, "ewma_tpm"},
		{"average tokens", func() domain.TokenGuardPolicy {
			p := basePolicy
			p.EWMAAbsoluteRPM = 0
			p.EWMAAbsoluteTokensPerRequest = 15
			return p
		}(), ewmaWindow{tokensPerRequest: 21}, "ewma_tokens_per_request"},
		{"cost", func() domain.TokenGuardPolicy {
			p := basePolicy
			p.EWMAAbsoluteRPM = 0
			p.EWMAAbsoluteCostMicrosPerMinute = 150
			return p
		}(), ewmaWindow{costPerMinute: 201}, "ewma_cost_rate"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := ewmaViolationReason(test.policy, baseline, test.window, started.Add(20*time.Second)); got != test.want {
				t.Fatalf("reason=%q want=%q", got, test.want)
			}
		})
	}
}

func TestEWMAAbsoluteFloorAndCheckpointRecovery(t *testing.T) {
	policy := ewmaPolicy()
	policy.EWMAAbsoluteRPM = 1_000 // the relative spike remains below the floor
	manager, _ := New([]domain.TokenGuardPolicy{policy})
	start := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	input := Input{PolicyID: policy.ID, ProjectID: "p", KeyID: "k", EstimatedTokens: 1}
	for index := 0; index < 10; index++ {
		input.Now = start.Add(time.Duration(index) * time.Second)
		manager.Admit(input)
	}
	input.Now = start.Add(10 * time.Second)
	manager.Admit(input)
	payload, err := manager.MarshalCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	restored, _ := New([]domain.TokenGuardPolicy{policy})
	if err := restored.RestoreCheckpoint(payload); err != nil {
		t.Fatal(err)
	}
	reencoded, err := restored.MarshalCheckpoint()
	if err != nil || string(reencoded) != string(payload) {
		t.Fatalf("checkpoint changed after restore: err=%v\n%s\n%s", err, payload, reencoded)
	}
	if err := restored.RestoreCheckpoint([]byte(`{"version":1,"subjects":[{"policy_id":"guard_ewma"}]}`)); err == nil {
		t.Fatal("corrupt baseline checkpoint was accepted")
	}
}

func TestPolicyRevisionChangeDropsBaseline(t *testing.T) {
	policy := ewmaPolicy()
	manager, _ := New([]domain.TokenGuardPolicy{policy})
	start := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	for index := 0; index <= 10; index++ {
		manager.Admit(Input{PolicyID: policy.ID, ProjectID: "p", KeyID: "k", EstimatedTokens: 1, Now: start.Add(time.Duration(index) * time.Second)})
	}
	before, _ := manager.MarshalCheckpoint()
	if !strings.Contains(string(before), `"subjects":[{`) {
		t.Fatalf("baseline was not checkpointed: %s", before)
	}
	policy.Revision++
	if err := manager.ReplacePolicies([]domain.TokenGuardPolicy{policy}); err != nil {
		t.Fatal(err)
	}
	after, _ := manager.MarshalCheckpoint()
	if strings.Contains(string(after), `"subjects":[{`) {
		t.Fatalf("stale revision baseline survived: %s", after)
	}
}

func TestFixedThresholdAnomalyCannotPoisonEWMABaseline(t *testing.T) {
	policy := ewmaPolicy()
	policy.Action = "alert"
	policy.RequestTokens = 100
	manager, _ := New([]domain.TokenGuardPolicy{policy})
	start := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	input := Input{PolicyID: policy.ID, ProjectID: "p", KeyID: "k", EstimatedTokens: 1}
	for index := 0; index < 10; index++ {
		input.Now = start.Add(time.Duration(index) * time.Second)
		manager.Admit(input)
	}
	input.Now = start.Add(10 * time.Second)
	manager.Admit(input)
	manager.mu.Lock()
	baselineSamples := manager.subjects["p:k"].baseline.Samples
	manager.mu.Unlock()
	if baselineSamples != 10 {
		t.Fatalf("baseline samples=%d", baselineSamples)
	}
	for index := 1; index < 10; index++ {
		input.Now = start.Add(10*time.Second + time.Duration(index)*time.Second)
		input.EstimatedTokens = 101
		decision := manager.Admit(input)
		if !decision.Allowed || decision.Reason != "request_tokens" {
			t.Fatalf("hard detect decision=%#v", decision)
		}
	}
	input.Now = start.Add(20 * time.Second)
	input.EstimatedTokens = 1
	manager.Admit(input)
	manager.mu.Lock()
	got := manager.subjects["p:k"].baseline.Samples
	manager.mu.Unlock()
	if got != baselineSamples {
		t.Fatalf("tainted fixed-threshold window changed baseline samples: before=%d after=%d", baselineSamples, got)
	}
}
