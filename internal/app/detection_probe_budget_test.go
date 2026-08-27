package app

import (
	"time"

	"testing"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/provider"
)

// The identification pass runs only the probes that depend on nothing — one or
// two of them — and then stops at the first that answers. It used to divide the
// remaining time by the length of the whole plan anyway, so on a nine-probe plan
// each root probe was given a ninth of the budget rather than the whole of it.
// The main loop had already been taught to count what can actually run; this is
// the same count, and both callers now share it.
//
// A model that needs longer than its slice is reported as a timeout, which the
// console shows as "temporarily unavailable" — an operator reads that as
// upstream flakiness and retries, paying for another round of probes each time.
func TestProbeBudgetCountsOnlyProbesThatCanRun(t *testing.T) {
	plan := []provider.CapabilityProbe{
		{Capability: "chat"},
		{Capability: "tools", DependsOn: []string{"chat"}},
		{Capability: "vision", DependsOn: []string{"chat"}},
		{Capability: "json_object", DependsOn: []string{"chat"}},
		{Capability: "structured_outputs", DependsOn: []string{"json_object"}},
	}
	if got := runnableProbes(map[string]domain.CapabilityProbeResult{}, plan); got != 1 {
		t.Fatalf("runnable probes with nothing established = %d, want 1 — only the root probe can run", got)
	}
	established := map[string]domain.CapabilityProbeResult{
		"chat": {Status: domain.ProbeSupported},
	}
	if got := runnableProbes(established, plan[1:]); got != 3 {
		t.Fatalf("runnable probes after chat = %d, want 3 — tools, vision and json_object, but not what waits on json_object", got)
	}
}

// A dependency that came back unsupported does not make its dependants runnable:
// they will never be asked, so counting them shrinks the share of the probes that
// will be.
func TestProbeBudgetExcludesDependantsOfAnUnsupportedProbe(t *testing.T) {
	plan := []provider.CapabilityProbe{
		{Capability: "tools", DependsOn: []string{"chat"}},
		{Capability: "vision", DependsOn: []string{"chat"}},
	}
	refused := map[string]domain.CapabilityProbeResult{
		"chat": {Status: domain.ProbeUnsupported},
	}
	if got := runnableProbes(refused, plan); got != 0 {
		t.Fatalf("runnable probes after chat was refused = %d, want 0", got)
	}
}

// The same division happens a second time, on the identification pass that
// picks which interface a model answers on, and that copy was not updated when
// the main loop's was. Identification runs only the probes that depend on
// nothing and stops at the first that answers, so dividing by the whole plan
// gave its root probe a sixth of the budget here — and a ninth on the real
// Bedrock Mantle plans, where both routes list nine probes behind a single root.
//
// The model that exposed this is the one the main-loop fix was written for: a
// frontier reasoning model cannot answer one non-streaming completion in ten
// seconds, so identification reported a timeout for an interface that works.
func TestIdentificationRootProbeIsBoundedByTheAttemptTimeout(t *testing.T) {
	runtime, instance, chat, media := twoInterfaceProviderForTest(t)
	chatDetector := &budgetRecordingDetector{}
	mediaDetector := &scriptedCapabilityDetector{supported: map[string]bool{"moderations": true}}
	registerBindingDetectors(t, runtime, instance, map[string]provider.Adapter{chat.ID: chatDetector, media.ID: mediaDetector})
	runtime.config.Admin.ModelCapabilityDetection.TotalTimeout = config.Duration(90 * time.Second)
	runtime.config.Gateway.AttemptResponseHeaderTimeout = config.Duration(60 * time.Second)

	runDetectionForTest(t, runtime, instance, "slow-reasoning-model")

	// Dividing by the six probes the plan lists gave the root 15s. It is now
	// bounded by the attempt timeout, the same bound one gateway attempt gets.
	root := chatDetector.budgetFor("chat")
	if root < 45*time.Second {
		t.Fatalf("identification gave the root probe %s of a 90s budget with a 60s attempt timeout", root)
	}
}
