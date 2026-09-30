package app

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/akz142857/Halro/internal/bearercred"
	"github.com/akz142857/Halro/internal/replication"
)

// haStatusRuntime owns the machine-only authentication, denial counter, and
// admission limit as one lifecycle-bound unit of the Metrics listener.
type haStatusRuntime struct {
	authorizer *bearercred.Authorizer
	authFailed atomic.Uint64
	requests   chan struct{}
}

// haMemberStatus is a machine-only view of the local member. It deliberately
// shares the sanitized response with the human Admin status route, while using
// an independent credential on the mTLS Metrics listener.
func (r *Runtime) haMemberStatus(writer http.ResponseWriter, request *http.Request) {
	release, ok := r.haStatusAccess(writer, request)
	if !ok {
		return
	}
	defer release()
	state, transitions := r.replication.publisher.SnapshotWithLiveTransitions()
	view := r.clusterStatusViewFor(state)
	view["live_transitions"] = transitions
	if r.replication.coordinator != nil {
		availability := r.replication.coordinator.AvailabilityTransitions()
		view["availability_transitions"] = availability
		view["replication_unavailable"] = availability.Current == "unavailable"
	}
	if r.replication.receiver != nil {
		view["replica_stage_transitions"] = r.replication.receiver.StageTransitions()
	}
	if r.config.Metrics.HAStatus.IncludePrefixDigest {
		if state.DurableIndex > 0 {
			if r.replication.journal == nil {
				r.haStatusLog(request, "unavailable", "prefix_unavailable")
				writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "authenticated prefix unavailable"})
				return
			}
			record, err := r.replication.journal.Record(state.DurableIndex)
			if err != nil || record.MAC != state.OrderingHeadMAC {
				if r.logger != nil {
					r.logger.Error("HA status authenticated prefix does not match durable member state", "error", err)
				}
				r.haStatusLog(request, "unavailable", "prefix_unavailable")
				writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "authenticated prefix unavailable"})
				return
			}
			digest := sha256.Sum256(state.OrderingHeadMAC[:])
			view["prefix_digest"] = map[string]any{
				"index": state.DurableIndex, "last_frame_term": record.Term,
				"ordering_head_sha256": hex.EncodeToString(digest[:]),
			}
		}
	}
	r.haStatusLog(request, "allowed", "")
	writeJSON(writer, http.StatusOK, view)
}

func (r *Runtime) haMemberTransitions(writer http.ResponseWriter, request *http.Request) {
	release, ok := r.haStatusAccess(writer, request)
	if !ok {
		return
	}
	defer release()
	query, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil {
		r.haStatusLog(request, "denied", "invalid_transition_cursor")
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid transition cursor"})
		return
	}
	if len(query) > 3 || len(query["after"]) > 1 || len(query["digest"]) > 1 || len(query["journal_id"]) > 1 {
		r.haStatusLog(request, "denied", "invalid_transition_cursor")
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid transition cursor"})
		return
	}
	for name := range query {
		if name != "after" && name != "digest" && name != "journal_id" {
			r.haStatusLog(request, "denied", "invalid_transition_cursor")
			writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid transition cursor"})
			return
		}
	}
	var after uint64
	if query.Has("after") {
		after, err = strconv.ParseUint(query.Get("after"), 10, 64)
		if err != nil {
			r.haStatusLog(request, "denied", "invalid_transition_cursor")
			writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid transition cursor"})
			return
		}
	}
	if after > 0 && (query.Get("digest") == "" || query.Get("journal_id") == "") ||
		after == 0 && (query.Has("digest") || query.Has("journal_id")) {
		r.haStatusLog(request, "denied", "invalid_transition_cursor")
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid transition cursor"})
		return
	}
	page, err := r.replication.publisher.DurableTransitionPage(after, query.Get("digest"), 64)
	if err != nil {
		if errors.Is(err, replication.ErrInvalidTransitionCursor) {
			r.haStatusLog(request, "denied", "transition_cursor_mismatch")
			writeJSON(writer, http.StatusConflict, map[string]string{"error": "transition cursor does not match committed chain"})
			return
		}
		r.haStatusLog(request, "unavailable", "transition_journal_unavailable")
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "durable transitions unavailable"})
		return
	}
	if after > 0 && query.Get("journal_id") != page.NextCursor.JournalID {
		r.haStatusLog(request, "denied", "transition_journal_identity_changed")
		writeJSON(writer, http.StatusConflict, map[string]string{"error": "transition journal identity changed"})
		return
	}
	r.haStatusLog(request, "allowed", "")
	writeJSON(writer, http.StatusOK, page)
}

func (r *Runtime) haStatusAccess(writer http.ResponseWriter, request *http.Request) (func(), bool) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	if r.replication == nil || !r.config.Metrics.HAStatus.Enabled || r.haStatus.authorizer == nil {
		writeJSON(writer, http.StatusNotFound, map[string]string{"error": "not found"})
		return nil, false
	}
	if request.TLS == nil || len(request.TLS.PeerCertificates) == 0 || len(request.TLS.VerifiedChains) == 0 {
		r.haStatusDenied(writer, request, "client_certificate_required")
		return nil, false
	}
	authorization := request.Header.Values("Authorization")
	if len(authorization) != 1 || !strings.HasPrefix(authorization[0], "Bearer ") {
		r.haStatusDenied(writer, request, "bearer_credential_required")
		return nil, false
	}
	token := strings.TrimPrefix(authorization[0], "Bearer ")
	if token == "" || strings.TrimSpace(token) != token {
		r.haStatusDenied(writer, request, "bearer_credential_required")
		return nil, false
	}
	authorized, err := r.haStatus.authorizer.Authorize(token, time.Now())
	if err != nil {
		if r.logger != nil {
			r.logger.Error("HA status credential verification failed", "error", err)
		}
		r.haStatusDenied(writer, request, "credential_unavailable")
		return nil, false
	}
	if !authorized {
		r.haStatusDenied(writer, request, "invalid_credential")
		return nil, false
	}
	select {
	case r.haStatus.requests <- struct{}{}:
		return func() { <-r.haStatus.requests }, true
	default:
		r.haStatusLog(request, "busy", "concurrency_limit")
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "HA status concurrency limit reached"})
		return nil, false
	}
}

func (r *Runtime) haStatusDenied(writer http.ResponseWriter, request *http.Request, reason string) {
	r.haStatus.authFailed.Add(1)
	r.haStatusLog(request, "denied", reason)
	writer.Header().Set("WWW-Authenticate", `Bearer realm="halro-ha-status"`)
	writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "HA status authentication required"})
}

func (r *Runtime) haStatusLog(request *http.Request, outcome, reason string) {
	if r.logger != nil {
		fingerprint := "unverified"
		if request.TLS != nil && len(request.TLS.PeerCertificates) > 0 && len(request.TLS.VerifiedChains) > 0 {
			fingerprintBytes := sha256.Sum256(request.TLS.PeerCertificates[0].RawSubjectPublicKeyInfo)
			fingerprint = hex.EncodeToString(fingerprintBytes[:])
		}
		r.logger.Info("HA member status access", "node_id", r.replication.publisher.Snapshot().NodeID,
			"client_spki_sha256", fingerprint, "outcome", outcome, "reason", reason,
			"prefix_digest_enabled", r.config.Metrics.HAStatus.IncludePrefixDigest)
	}
}
