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
	d := ModelCapabilityDetection{ProviderModel: "model", ModelRevision: "sha256:model"}
	snapshot := DetectionCapabilitySnapshot(d, ProviderCapabilities{Chat: true}, now)
	if snapshot.Source != "verified_probe" || snapshot.Evidence["chat"] != EvidenceVerified || snapshot.ModelRevision != "sha256:model" {
		t.Fatalf("snapshot=%#v", snapshot)
	}
	deployment := Deployment{ProviderModel: "model", Capabilities: ProviderCapabilities{Chat: true}, ModelCapabilitySnapshot: snapshot}
	if deployment.LastTestRevision != 0 || deployment.LastTestStatus != "" {
		t.Fatal("capability detection pre-filled deployment validation state")
	}
}
