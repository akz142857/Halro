package app

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/akz142857/Heimdall/internal/config"
	boltstore "github.com/akz142857/Heimdall/internal/store/bolt"
)

// auditAnchorPollInterval governs how often runAuditAnchorMaintenance checks
// whether an anchor is due. It is deliberately much finer than any
// configured Audit.Anchor.Interval so a record-delta burst is caught close
// to when it happens, not just on the interval tick (ADR 0015: "a burst of
// activity cannot slip between two ticks"). A var, not a const, so tests can
// poll faster than a production 10s tick without waiting for one.
var auditAnchorPollInterval = 10 * time.Second

// runAuditAnchorMaintenance emits an anchor whenever the configured interval
// has elapsed or the audit log has advanced by the configured record delta,
// whichever comes first. Emission is fail-open by design (ADR 0015): a
// failure here is logged and counted, never surfaced as an error that could
// block an audit append or gateway traffic — an unreachable anchor sink must
// not become a new availability attack surface.
func (r *Runtime) runAuditAnchorMaintenance(ctx context.Context) {
	if !r.config.Audit.Anchor.Enabled {
		return
	}
	ticker := time.NewTicker(auditAnchorPollInterval)
	defer ticker.Stop()
	var lastEmittedAt time.Time
	var lastEmittedRecords uint64
	emit := func() {
		summary := r.audit.Summary()
		if summary.Records == lastEmittedRecords {
			return
		}
		due := lastEmittedAt.IsZero() ||
			time.Since(lastEmittedAt) >= r.config.Audit.Anchor.Interval.Value() ||
			summary.Records-lastEmittedRecords >= uint64(r.config.Audit.Anchor.RecordDelta)
		if !due {
			return
		}
		latest, err := r.store.LatestAuditAnchor()
		sequence := uint64(1)
		if err == nil {
			sequence = latest.Sequence + 1
		} else if err != boltstore.ErrNotFound {
			r.logger.Warn("audit anchor sequence lookup failed", "error", err)
			r.anchorEmitFailures.Add(1)
			return
		}
		now := time.Now().UTC()
		if err := r.store.AppendAuditAnchor(boltstore.AuditAnchor{
			Sequence: sequence, Records: summary.Records, LastHash: summary.LastHash,
			Bytes: summary.Bytes, InstanceID: r.instanceID, ObservedAt: now,
		}); err != nil {
			r.logger.Warn("audit anchor emission failed", "error", err)
			r.anchorEmitFailures.Add(1)
			return
		}
		lastEmittedAt, lastEmittedRecords = now, summary.Records
		r.anchorLastEmitUnix.Store(now.Unix())
	}
	for {
		select {
		case <-ticker.C:
			emit()
		case <-ctx.Done():
			return
		}
	}
}

// adminAuditAnchors serves GET /audit/anchors?since=<seq> on the metrics
// listener: every anchor with a sequence greater than since, ascending, for
// a dead-man probe to pull incrementally. Authentication mirrors the metrics
// endpoint's bearer scheme but uses an independent credential domain — the
// two are rotated on unrelated schedules and one compromise must not imply
// the other.
func (r *Runtime) adminAuditAnchors(writer http.ResponseWriter, request *http.Request) {
	if !r.authorizeAuditAnchors(request) {
		r.anchorAuthFailed.Add(1)
		writer.Header().Set("WWW-Authenticate", `Bearer realm="heimdall-audit-anchors"`)
		writeJSON(writer, http.StatusUnauthorized, map[string]any{
			"error": "audit anchor authentication required",
		})
		return
	}
	since := uint64(0)
	if raw := request.URL.Query().Get("since"); raw != "" {
		parsed, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			writeJSON(writer, http.StatusBadRequest, map[string]any{"error": "since must be a non-negative integer"})
			return
		}
		since = parsed
	}
	anchors, err := r.store.AuditAnchorsSince(since)
	if err != nil {
		r.logger.Warn("audit anchor lookup failed", "error", err)
		writeJSON(writer, http.StatusInternalServerError, map[string]any{"error": "audit anchor lookup failed"})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"anchors": anchors})
}

func (r *Runtime) authorizeAuditAnchors(request *http.Request) bool {
	if r.anchorAuthorizer == nil {
		return false
	}
	authorization := request.Header.Get("Authorization")
	if !strings.HasPrefix(authorization, "Bearer ") {
		return false
	}
	token := strings.TrimSpace(strings.TrimPrefix(authorization, "Bearer "))
	if token == "" {
		return false
	}
	authorized, err := r.anchorAuthorizer.Authorize(token, time.Now())
	if err != nil {
		r.logger.Error("audit anchor credential authorization failed", "error", err)
		return false
	}
	return authorized
}

// warnAboutMissingAnchorSink mirrors warnAboutReachableWorkbench: mode:file
// has no witness of its own for the audit chain (ADR 0015), and the operator
// who can fix that has to be told, not left to discover the gap. The default
// stays anchoring-off; this only makes the trade-off visible.
func (r *Runtime) warnAboutMissingAnchorSink() {
	if r.config.Storage.MasterKey.Mode != config.MasterKeyModeFile {
		return
	}
	if r.config.Audit.Anchor.Enabled {
		return
	}
	r.logger.Warn(
		"mode:file has no external witness for the audit chain; an operator holding master.key can "+
			"rewrite audit history undetected by anyone other than themselves",
		"remedy", "enable audit.anchor with a dead-man probe pulling it, or accept mode:key_slots' non-repudiation instead",
	)
}
