package domain

import (
	"testing"
	"time"
)

func TestModelCapabilityDetectionRecommendationRequiresVerifiedSupportedResults(t *testing.T) {
	now := time.Now().UTC()
	expires := now.Add(time.Hour)
	d := ModelCapabilityDetection{
		ID: "mcd_one", ProviderID: "prv_one", ProviderRevision: 1, CredentialRevision: 1,
		ProviderModel: "model", ModelRevision: "sha256:model",
		Candidates: []DetectionBindingCandidate{{BindingID: "binding", ProfileID: ProfileOpenAIChatEmbeddings,
			AccessSurface: SurfaceOpenAI, ModelRevision: "sha256:model", Status: ProbeNotProbed}},
		BindingID: "binding", ProfileID: ProfileOpenAIChatEmbeddings,
		AccessSurface: SurfaceOpenAI, TargetKind: TargetModelID, CanonicalTarget: "model",
		SelectionFingerprint: "sha256:selection", TargetFingerprint: "sha256:target",
		DetectorVersion: "v1", RiskTier: "safe_automatic", Status: DetectionCompleted, Source: "verified_probe",
		Results:     map[string]CapabilityProbeResult{"chat": {Status: ProbeSupported, Evidence: EvidenceVerified, BindingID: "binding", ProbeKind: "minimal_chat"}},
		Recommended: ProviderCapabilities{Chat: true}, MaxProviderCalls: 8, CompletedAt: &now, ExpiresAt: &expires,
		CreatedBy: "admin", IdempotencyKeyHash: "sha256:key", RequestHash: "sha256:request", CreatedAt: now, UpdatedAt: now,
	}
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := d
	result := bad.Results["chat"]
	result.Evidence = EvidenceDeclared
	bad.Results = map[string]CapabilityProbeResult{"chat": result}
	if err := bad.Validate(); err == nil {
		t.Fatal("supported result without verified evidence was accepted")
	}
	bad = d
	bad.Recommended.Tools = true
	if err := bad.Validate(); err == nil {
		t.Fatal("recommendation wider than supported results was accepted")
	}
}

func TestDetectionCapabilitySnapshotIsVerifiedAndDoesNotMarkDeploymentTested(t *testing.T) {
	now := time.Now().UTC()
	d := ModelCapabilityDetection{ProviderModel: "model", ModelRevision: "sha256:model",
		Results: map[string]CapabilityProbeResult{"chat": {Status: ProbeSupported, Evidence: EvidenceVerified, BindingID: "binding", ProbeKind: "minimal_chat"}}}
	snapshot := DetectionCapabilitySnapshot(d, ProviderCapabilities{Chat: true}, now)
	if snapshot.Source != "verified_probe" || snapshot.Evidence["chat"] != EvidenceVerified || snapshot.ModelRevision != "sha256:model" {
		t.Fatalf("snapshot=%#v", snapshot)
	}
	deployment := Deployment{ProviderModel: "model", Capabilities: ProviderCapabilities{Chat: true}, ModelCapabilitySnapshot: snapshot}
	if deployment.LastTestRevision != 0 || deployment.LastTestStatus != "" {
		t.Fatal("capability detection pre-filled deployment validation state")
	}
}

// A verification measures what it is allowed to measure and carries the rest of
// the catalog's claim through. Recording that carried half as verified would
// state a measurement that never happened — images cannot be probed at all, and
// nothing about this deployment says so if the whole snapshot reads "verified".
func TestDetectionSnapshotMarksCarriedClaimsAsDeclaredNotVerified(t *testing.T) {
	now := time.Now().UTC()
	d := ModelCapabilityDetection{ProviderModel: "model", ModelRevision: "sha256:model",
		Baseline: &ProviderCapabilities{Chat: true, Images: true},
		Results: map[string]CapabilityProbeResult{
			"chat":   {Status: ProbeSupported, Evidence: EvidenceVerified, BindingID: "binding", ProbeKind: "minimal_chat"},
			"images": {Status: ProbeNotProbed, BindingID: "binding", ProbeKind: "risk_policy"},
		}}
	snapshot := DetectionCapabilitySnapshot(d, ProviderCapabilities{Chat: true, Images: true}, now)
	if snapshot.Evidence["chat"] != EvidenceVerified {
		t.Fatalf("probed capability lost its verified evidence: %#v", snapshot.Evidence)
	}
	if snapshot.Evidence["images"] != EvidenceDeclared {
		t.Fatalf("carried claim recorded as %q, want declared", snapshot.Evidence["images"])
	}
}

// Silence is not a denial. A probe that could not be run, or whose answer proved
// nothing, leaves the baseline claim standing; only an answer moves it. What the
// probes did disprove still binds what the baseline may carry, because a
// recommendation whose dependencies do not hold cannot be stored at all.
func TestRecommendedFromProbesKeepsSilentClaimsAndDropsDisprovedOnes(t *testing.T) {
	baseline := &ProviderCapabilities{Chat: true, Streaming: true, Tools: true, Vision: true, Images: true}
	results := map[string]CapabilityProbeResult{
		"chat":      {Status: ProbeSupported},
		"streaming": {Status: ProbeSupported},
		"tools":     {Status: ProbeUnsupported},
		"vision":    {Status: ProbeInconclusive},
		"reasoning": {Status: ProbeSupported},
	}
	got := RecommendedFromProbes(baseline, results)
	switch {
	case !got.Chat || !got.Streaming:
		t.Fatalf("verified capability lost: %#v", got)
	case got.Tools:
		t.Fatalf("capability the upstream refused was kept: %#v", got)
	case !got.Vision:
		t.Fatalf("inconclusive probe deleted a claim nothing disagreed with: %#v", got)
	case !got.Images:
		t.Fatalf("unprobeable capability lost its claim: %#v", got)
	case !got.Reasoning:
		t.Fatalf("capability the probes established beyond the baseline was dropped: %#v", got)
	}

	// Chat refused underneath: everything that needs it goes with it, however
	// the baseline claimed it.
	got = RecommendedFromProbes(baseline, map[string]CapabilityProbeResult{"chat": {Status: ProbeUnsupported}})
	if got.Chat || got.Streaming || got.Tools || got.Vision || got.StreamUsage {
		t.Fatalf("dependents survived a refused baseline: %#v", got)
	}
	if !got.Images {
		t.Fatalf("an independent claim was dropped with the chat dependents: %#v", got)
	}

	// A model the catalog does not cover has no baseline, so silence still means
	// false there.
	// A model the catalog does not cover has no baseline at all — the field is
	// absent rather than an empty claim — so silence still means false.
	got = RecommendedFromProbes(nil, map[string]CapabilityProbeResult{"chat": {Status: ProbeInconclusive}})
	if got.Chat {
		t.Fatalf("silence became a claim without a baseline: %#v", got)
	}
}
