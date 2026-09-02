package app

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/config"

	"github.com/akz142857/Halro/internal/failurecapture"
	gatewaycore "github.com/akz142857/Halro/internal/gateway"
	"github.com/go-chi/chi/v5"
)

// openFailureCapture opens the store when there is one to open — which is
// whenever capture is enabled, or the directory already holds records from a
// time when it was.
//
// The second half is the point. Turning capture off has to stop new writes
// without stopping the expiry of what is already there; a feature whose "off"
// switch makes captured prompts permanent is worse than one with no switch.
func openFailureCapture(cfg config.Config, sealer failurecapture.Sealer) (*failurecapture.Store, error) {
	root := filepath.Join(cfg.Storage.DataDir, "failures")
	if !cfg.Gateway.FailureCapture.Enabled {
		if _, err := os.Stat(root); err != nil {
			return nil, nil
		}
	}
	store, err := failurecapture.Open(sealer, failurecapture.Options{
		Root:             root,
		MaxBytes:         cfg.Gateway.FailureCapture.ByteLimit(),
		MaxRecordsPerDay: cfg.Gateway.FailureCapture.DailyRecordLimit(),
		Retain:           cfg.Gateway.FailureCapture.RetentionWindow(),
	})
	if err != nil {
		return nil, fmt.Errorf("open failure capture store: %w", err)
	}
	return store, nil
}

// captureFor hands the gateway a store, or a genuinely nil interface when the
// gateway must not write.
//
// A typed nil pointer assigned to an interface is not nil, and the gateway
// tests capture with `== nil`. Passing the pointer straight through would give
// every install with capture switched off a non-nil interface holding nil, and
// the first failed request would dereference it. This is the reason the
// conversion has a function of its own rather than being inline at the call.
//
// `enabled` is separate from "a store exists" because a disabled install still
// opens the store to expire what an earlier enabled one wrote. The gateway
// must not write into that.
func captureFor(enabled bool, store *failurecapture.Store) gatewaycore.FailureCapture {
	if !enabled || store == nil {
		return nil
	}
	return store
}

// purgeFailureCaptures enforces the retention window. Failures here are logged
// and not retried: the next sweep covers the same days, and a retention pass
// that takes the process down with it is worse than one that is late.
func (r *Runtime) purgeFailureCaptures() {
	if r.failureCapture == nil {
		return
	}
	if err := r.failureCapture.Purge(); err != nil {
		r.logger.Warn("failure capture retention sweep failed", "error", err)
	}
}

// adminUsageFailurePayload serves what a failed request carried.
//
// This is the only endpoint that returns material a caller wrote, so it is the
// only one that audits a read. Every other admin GET is a view of Halro's own
// metadata and is covered by the session; this one hands back a prompt, and an
// operator looking at a customer's prompt is an event that should be
// answerable afterwards.
//
// The project is part of the key rather than a filter. The envelope is bound to
// it, so a record cannot be opened under the wrong project even by a caller who
// knows the request ID — and the project comes from the failed-request summary
// rather than from the query string, so it cannot be supplied to steer the
// lookup.
func (r *Runtime) adminUsageFailurePayload(writer http.ResponseWriter, request *http.Request) {
	if !r.syncUsageAdmin(writer, request) {
		return
	}
	if r.failureCapture == nil {
		// Off is not an error. Saying so plainly is what stops an operator
		// concluding the capture failed when it was never asked for.
		writeJSON(writer, http.StatusNotFound, map[string]string{
			"error": "failure capture is not enabled", "code": "failure_capture_disabled",
		})
		return
	}
	requestID := chi.URLParam(request, "requestID")
	if requestID == "" || len(requestID) > 128 {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid request ID"})
		return
	}
	detail, exists := r.usage.RequestDetail(requestID)
	if !exists {
		writeJSON(writer, http.StatusNotFound, map[string]string{"error": "usage request not found"})
		return
	}
	record, found, err := r.failureCapture.Get(requestID, detail.Summary.ProjectID)
	if err != nil {
		// The error is not returned to the caller: it distinguishes "tampered
		// with" from "belongs to another project", and neither is something the
		// console needs in order to say nothing was found.
		r.logger.Warn("failure capture could not be opened", "request_id", requestID, "error", err)
		writeJSON(writer, http.StatusNotFound, map[string]string{"error": "no captured payload for this request"})
		return
	}
	if !found {
		writeJSON(writer, http.StatusNotFound, map[string]string{"error": "no captured payload for this request"})
		return
	}
	// Audited before the body is written, so a read that the client abandons
	// mid-response is still on the record.
	r.auditFailurePayloadRead(request, requestID, detail.Summary.ProjectID)
	writeJSON(writer, http.StatusOK, record)
}

func (r *Runtime) auditFailurePayloadRead(request *http.Request, requestID, projectID string) {
	admin, ok := request.Context().Value(adminContextKey{}).(adminRequestContext)
	if !ok {
		return
	}
	if err := r.appendAdminAuditWithMetadata(
		"admin_user", admin.session.Username, "usage.failure_payload.read",
		"usage_request", requestID, "success", "",
		map[string]any{"project_id": projectID},
	); err != nil {
		r.logger.Warn("failure payload read audit failed", "error", err)
	}
}
