package app

import (
	"context"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/modelcatalog"
)

// The capability audit actions required by §11.3 of the model-aware capability
// design. They are distinct from deployment.create/update because the question
// they answer is different: not "who changed this deployment" but "what was
// established about this model, by whom, against which catalog".
//
// deployment.operation_bindings.changed is deliberately absent, and now
// permanently so. A deployment carries one internal binding — composing several
// capabilities into one outward-facing model is the route layer's job — so
// there is no operation-binding state to report, and an action that can never
// fire would only make the audit surface look complete.
const (
	auditCapabilitySnapshotCreated    = "deployment.capability_snapshot.created"
	auditCapabilitySnapshotReviewed   = "deployment.capability_snapshot.reviewed"
	auditCapabilityDriftDetected      = "deployment.capability_drift.detected"
	auditRouteReferenceWithheld       = "route.reference_withheld"
	auditOperatorCapabilitiesDeclared = "deployment.operator_capabilities.declared"
)

// capabilitySnapshotMetadata describes what was established, and against what.
// Deployment, provider, model, catalog revision and administrator identity are
// required by §11.3; the administrator arrives as the audit actor.
//
// No credential, key or endpoint appears here. Everything recorded is either an
// identifier an operator can look up or a capability name.
func capabilitySnapshotMetadata(deployment domain.Deployment) map[string]any {
	snapshot := deployment.ModelCapabilitySnapshot
	metadata := map[string]any{
		"provider_id":    deployment.ProviderID,
		"provider_model": deployment.ProviderModel,
		"profile_id":     string(deployment.ProfileID),
		"source":         snapshot.Source,
		"status":         snapshot.Status,
		"model_revision": snapshot.ModelRevision,
		"capabilities":   enabledCapabilityNames(deployment.Capabilities),
	}
	if deployment.BindingID != "" {
		metadata["binding_id"] = deployment.BindingID
	}
	if snapshot.CatalogRevision != "" {
		metadata["catalog_revision"] = snapshot.CatalogRevision
	}
	if snapshot.ResolutionRevision != "" {
		metadata["resolution_revision"] = snapshot.ResolutionRevision
	}
	if snapshot.ProviderRevision != 0 {
		metadata["provider_revision"] = snapshot.ProviderRevision
	}
	if snapshot.TargetFingerprint != "" {
		metadata["target_fingerprint"] = snapshot.TargetFingerprint
	}
	if len(snapshot.ClaimRevisions) != 0 {
		metadata["claim_revisions"] = append([]string(nil), snapshot.ClaimRevisions...)
	}
	if deployment.Region != "" {
		metadata["region"] = deployment.Region
	}
	return metadata
}

// capabilityChangeMetadata adds the before/after summary §11.3 asks for. The
// summary is the capability names that moved, not the whole struct twice: what
// a reviewer needs is which capabilities changed hands.
func capabilityChangeMetadata(before, after domain.Deployment) map[string]any {
	metadata := capabilitySnapshotMetadata(after)
	metadata["previous_model_revision"] = before.ModelCapabilitySnapshot.ModelRevision
	metadata["previous_source"] = before.ModelCapabilitySnapshot.Source
	if enabled := modelcatalog.GainedCapabilities(before.Capabilities, after.Capabilities); len(enabled) != 0 {
		metadata["enabled"] = enabled
	}
	if disabled := modelcatalog.LostCapabilities(before.Capabilities, after.Capabilities); len(disabled) != 0 {
		metadata["disabled"] = disabled
	}
	return metadata
}

func enabledCapabilityNames(capabilities domain.ProviderCapabilities) []string {
	// The difference from "nothing" is exactly the set that is on.
	return modelcatalog.GainedCapabilities(domain.ProviderCapabilities{}, capabilities)
}

// capabilityChanged reports whether an update touched what the deployment
// claims about its model, as opposed to its name, weight or concurrency. Only
// the former is a capability review.
func capabilityChanged(before, after domain.Deployment) bool {
	return before.Capabilities != after.Capabilities ||
		before.ModelCapabilitySnapshot.ModelRevision != after.ModelCapabilitySnapshot.ModelRevision ||
		before.ModelCapabilitySnapshot.Source != after.ModelCapabilitySnapshot.Source ||
		before.ModelCapabilitySnapshot.Capabilities != after.ModelCapabilitySnapshot.Capabilities
}

// auditCapabilityWithholdings records each deployment the reconciliation took
// out of the routing candidates. §4.4 requires this to reach the audit trail
// and not only `halro doctor` — a deployment that silently stopped routing is
// the case an operator most needs a durable record of.
//
// The actor is the system: no administrator asked for this, a catalog or a
// binary upgrade caused it.
// It also records routes withheld because what they reference cannot produce a
// Target. Those are deliberately kept out of the drift action and the drift
// metric: drift means the deployment's claim outgrew what this build supports,
// while a dangling reference means the stored topology disagrees with itself.
// Counting the second as the first would make the drift metric mean two things
// and hide the one that needs repair rather than review.
func (r *Runtime) auditCapabilityWithholdings(ctx context.Context, report loadReport) {
	r.auditReferenceWithholdings(report.Dangling)
	for _, item := range report.Drifted {
		metadata := map[string]any{
			"route_id":                item.RouteID,
			"capability_review_state": string(item.State),
		}
		if item.Reason != "" {
			metadata["reason"] = item.Reason
		}
		if len(item.NoLongerSupported) != 0 {
			metadata["no_longer_supported"] = item.NoLongerSupported
		}
		if deployment, err := r.store.GetDeployment(ctx, item.DeploymentID); err == nil {
			for key, value := range capabilitySnapshotMetadata(deployment) {
				metadata[key] = value
			}
		}
		// The reason is only knowable with the deployment and its provider in
		// hand. When either read fails the drift still happened, so it is
		// counted rather than dropped — a metric that silently undercounts the
		// exact case it exists to catch would be worse than a coarse one.
		if item.Reason == "" {
			r.capabilityMetrics.recordDrift(driftReasonProfile)
		} else {
			r.capabilityMetrics.recordDrift(driftMetricReason(item.Reason))
		}
		// A failed audit append must not take the process down: the deployment
		// is already fail-closed, and refusing to start over an audit write
		// would reintroduce exactly the outage this reconciliation avoids. The
		// log keeps it visible.
		if err := r.appendAdminAuditWithMetadata("system", "", auditCapabilityDriftDetected,
			"deployment", item.DeploymentID, "success", "", metadata); err != nil {
			r.logger.Error("capability drift audit append failed",
				"deployment", item.DeploymentID, "error", err)
		}
	}
}

// auditReferenceWithholdings records each route that stopped routing because the
// records it points at cannot produce a Target. The route is fail-closed, which
// means an operator's alias silently stopped answering; that has to be durable
// somewhere they will find it.
//
// As with drift, a failed append is logged rather than fatal — refusing to
// finish activation over an audit write would put back the outage that
// withholding exists to avoid.
func (r *Runtime) auditReferenceWithholdings(withheld []referenceWithholding) {
	for _, item := range withheld {
		if err := r.appendAdminAuditWithMetadata("system", "", auditRouteReferenceWithheld,
			"route", item.RouteID, "success", "", map[string]any{
				"deployment_id": item.DeploymentID,
				"provider_id":   item.ProviderID,
				"binding_id":    item.BindingID,
				"reason":        item.Reason,
			}); err != nil {
			r.logger.Error("route reference withholding audit append failed",
				"route", item.RouteID, "reason", item.Reason, "error", err)
		}
	}
}
