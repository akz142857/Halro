package app

import (
	"net/http"

	"github.com/akz142857/Halro/internal/modelcatalog"
)

func (r *Runtime) observeModelCatalogRefresh(event modelcatalog.RefreshEvent) {
	if r.capabilityMetrics != nil {
		r.capabilityMetrics.recordSignedCatalog(event)
	}
	action := "model_catalog.download.failed"
	outcome := "failure"
	if event.Outcome == "success" {
		action = "model_catalog.download.succeeded"
		outcome = "success"
		if event.Recovered {
			action = "model_catalog.last_known_good.recovered"
		}
	} else if event.ErrorClass != "download" {
		action = "model_catalog.download.refused"
	}
	if err := r.appendAdminAuditWithMetadata("system", "halro", action, "model_catalog", "signed", outcome, event.ErrorClass, map[string]any{
		"state": event.Status.State, "source": event.Status.Source,
		"revision": event.Status.Revision, "previous_revision": event.PreviousRevision,
		"sequence": event.Status.Sequence, "error_class": event.ErrorClass,
	}); err != nil && r.logger != nil {
		r.logger.Error("append signed model catalog audit", "error", err)
	}
	if event.Outcome == "success" {
		r.clearAllInvocationTargetCatalogs()
		return
	}
	// A rejected refresh or runtime expiry makes signed facts unavailable for
	// new resolution. Reconcile the Gateway registry under the topology
	// coordinator as well: otherwise a route withheld by the last signed
	// narrowing would remain absent after the Manager fell back to bundled facts.
	// Expiry may be discovered by Current() while an Admin mutation already owns
	// adminTopologyMu. Reconcile asynchronously so the observer never attempts a
	// recursive topology lock; the mutation that discovered expiry is itself
	// already rebuilding from the bundled/unavailable state.
	backgroundCtx := r.backgroundCtx
	if backgroundCtx == nil {
		// During Open, the initial registry was built from the Manager state before
		// the observer was installed. The background refresh started after context
		// initialization will reconcile any transition that occurs in this narrow
		// startup window.
		return
	}
	r.backgroundWait.Add(1)
	go func() {
		defer r.backgroundWait.Done()
		if err := r.reconcileProviderRegistryWithCatalogState(backgroundCtx); err != nil && r.logger != nil {
			r.logger.Error("reconcile providers after signed model catalog degradation", "error", err)
		}
	}()
}

func (r *Runtime) auditModelCatalogConfiguration() {
	if r.capabilityResolution.catalog == nil {
		return
	}
	status := r.capabilityResolution.catalog.Status()
	action := "model_catalog.updates.disabled"
	if status.Enabled {
		action = "model_catalog.updates.enabled"
	}
	if err := r.appendAdminAuditWithMetadata("system", "halro", action, "model_catalog", "signed", "success", "", map[string]any{
		"state": status.State, "source": status.Source, "revision": status.Revision,
		"pinned": status.PinnedRevision != "", "trust_root_count": len(modelcatalog.ProductionTrustRoots()),
	}); err != nil && r.logger != nil {
		r.logger.Error("append model catalog configuration audit", "error", err)
	}
}

func (r *Runtime) getAdminModelCatalog(writer http.ResponseWriter, _ *http.Request) {
	if r.capabilityResolution.catalog == nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]any{"error": "model catalog manager is unavailable"})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"status":                        r.capabilityResolution.catalog.Status(),
		"bundled_revision":              modelcatalog.BundledSnapshot().CatalogRevision,
		"effective_revision":            r.effectiveModelCatalog().Revision(),
		"schema":                        map[string]int{"min_readable": modelcatalog.MinReadableSchema, "max_readable": modelcatalog.MaxReadableSchema},
		"capability_dictionary_version": modelcatalog.CapabilityDictionaryVersion,
		"trust_root_count":              len(modelcatalog.ProductionTrustRoots()),
	})
}

func (r *Runtime) refreshAdminModelCatalog(writer http.ResponseWriter, request *http.Request) {
	if r.capabilityResolution.catalog == nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]any{"error": "model catalog manager is unavailable"})
		return
	}
	if err := r.capabilityResolution.catalog.Refresh(request.Context()); err != nil {
		writeJSON(writer, http.StatusBadGateway, map[string]any{
			"error":  "signed model catalog refresh was rejected; Halro continues with its last-known-good catalog",
			"status": r.capabilityResolution.catalog.Status(),
		})
		return
	}
	r.clearAllInvocationTargetCatalogs()
	writeJSON(writer, http.StatusOK, map[string]any{"status": r.capabilityResolution.catalog.Status()})
}
