package app

import (
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/routegate"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/go-chi/chi/v5"
)

// routeSuspensionView is one scope the admission gate is holding out of service.
//
// This is where identity lives. The caller-facing 503 names nothing and the
// metrics carry only enumerations, both deliberately — but an operator answering
// HalroCredentialUnusable needs to know *which* credential, and this is the
// surface on the trusted side of that line.
type routeSuspensionView struct {
	ScopeKind string `json:"scope_kind"`
	ScopeKey  string `json:"scope_key"`
	Reason    string `json:"reason"`
	// Status and Code are what the upstream said, already narrowed through
	// provider.SafeProviderIdentifier before the gate ever saw them.
	Status     int    `json:"provider_status,omitempty"`
	Code       string `json:"provider_code,omitempty"`
	ObservedAt string `json:"observed_at"`
	// Until is absent for a suspension no clock will end.
	Until string `json:"until,omitempty"`
	// Indefinite says the operator is the only thing that ends this. It is
	// stated rather than inferred from an absent Until, because "no end time"
	// and "ends at a time nobody recorded" would otherwise read the same.
	Indefinite bool `json:"indefinite"`
	// ScopeID is the handle the clear takes. A scope is a pair rather than an
	// identifier, and one half of one kind of pair carries a NUL byte, so the
	// listing hands out an opaque handle instead of asking a caller to spell a
	// composite key into a URL.
	ScopeID string `json:"scope_id"`
	// Clearable says whether this suspension is one the clear can act on.
	// Only the long refusals are stored, and only a stored row has somewhere to
	// commit the clear's audit record; a thirty-second availability window ends
	// on its own well before an operator could reach it.
	Clearable bool `json:"clearable"`
	// CredentialRevision is the revision the refusal was observed against.
	// Saving a new secret advances it and clears the suspension, so this is the
	// number an operator is comparing against when they wonder why it is still
	// here.
	CredentialRevision uint64 `json:"credential_revision,omitempty"`
}

func (r *Runtime) listAdminRouteSuspensions(writer http.ResponseWriter, request *http.Request) {
	now := r.clockNow().UTC()
	suspensions := r.routes.Snapshot(now)
	stored := make(map[string]struct{})
	if rows, err := r.store.ListRouteSuspensions(request.Context()); err == nil {
		for _, row := range rows {
			stored[row.ScopeID()] = struct{}{}
		}
	} else {
		r.logger.Error("stored route suspensions could not be read", "error", err)
	}
	items := make([]routeSuspensionView, 0, len(suspensions))
	for _, suspension := range suspensions {
		scopeID := domain.EncodeRouteScopeID(string(suspension.Scope.Kind), suspension.Scope.Key)
		_, clearable := stored[scopeID]
		view := routeSuspensionView{
			ScopeID:            scopeID,
			Clearable:          clearable,
			ScopeKind:          string(suspension.Scope.Kind),
			ScopeKey:           routeSuspensionKey(suspension.Scope),
			Reason:             failureReasonLabel(suspension.Reason),
			Status:             suspension.Evidence.Status,
			Code:               suspension.Evidence.Code,
			ObservedAt:         suspension.Evidence.ObservedAt.UTC().Format(time.RFC3339),
			Indefinite:         suspension.Indefinite,
			CredentialRevision: suspension.Evidence.CredentialRevision,
		}
		if !suspension.Indefinite && !suspension.Until.IsZero() {
			view.Until = suspension.Until.UTC().Format(time.RFC3339)
		}
		items = append(items, view)
	}
	// Ordered so two reads of an unchanged gate render identically, and so the
	// ones an operator has to act on come first.
	sort.Slice(items, func(left, right int) bool {
		if items[left].Indefinite != items[right].Indefinite {
			return items[left].Indefinite
		}
		if items[left].ScopeKind != items[right].ScopeKind {
			return items[left].ScopeKind < items[right].ScopeKind
		}
		return items[left].ScopeKey < items[right].ScopeKey
	})
	writeJSON(writer, http.StatusOK, map[string]any{"items": items})
}

// routeSuspensionKey renders a scope key for a human. The credential-and-model
// scope joins its two halves with a NUL so they cannot collide inside the gate;
// that byte has no business in JSON.
func routeSuspensionKey(scope routegate.Scope) string {
	key := []rune(scope.Key)
	for index, symbol := range key {
		if symbol == 0 {
			key[index] = '/'
		}
	}
	return string(key)
}

// clearAdminRouteSuspension is the operator's escape hatch for a scope the gate
// is still holding over something already dealt with upstream.
//
// It acts on stored suspensions only, and that is the whole reason this action
// waited for persistence. Clearing is an administrative mutation, and here an
// administrative record commits *with* the change it describes — every
// …WithAuditIntent method pairs the two so they cannot diverge. A clear that
// wrote nothing would have had nowhere to commit its record, and inventing a
// standalone intent path would weaken exactly the property that pairing exists
// for. The short suspensions are not stored, so they are not clearable; they
// also end on their own inside five minutes, which is faster than an operator
// can reach them.
//
// The stored row and the audit record commit together; the live gate is cleared
// after. A crash between the two leaves the suspension in memory until it ends
// or the process restarts, which is the safe direction: the gate is the
// authority while it is running, and the row it would have been restored from
// is already gone.
func (r *Runtime) clearAdminRouteSuspension(writer http.ResponseWriter, request *http.Request) {
	kind, key, ok := domain.DecodeRouteScopeID(chi.URLParam(request, "scopeID"))
	if !ok {
		adminNotFound(writer)
		return
	}
	scope := routegate.Scope{Kind: routegate.ScopeKind(kind), Key: key}
	intent, intentErr := r.newAdminAuditIntent(
		request, "route_suspension.clear", "route_suspension",
		domain.EncodeRouteScopeID(kind, key),
	)
	if intentErr != nil {
		adminStoreError(writer)
		return
	}
	err := r.store.DeleteRouteSuspensionWithAuditIntent(request.Context(), kind, key, intent)
	switch {
	case errors.Is(err, boltstore.ErrNotFound):
		// A suspension the gate is holding but never stored is a live one an
		// operator can see and cannot clear. Saying so is better than a 404,
		// which would read as "no such suspension" while the listing shows it.
		if r.routeScopeIsSuspended(scope) {
			writeJSON(writer, http.StatusConflict, map[string]string{
				"code":  "route_suspension_not_clearable",
				"error": "this suspension is not durable and ends on its own; only the long refusals can be cleared",
			})
			return
		}
		adminNotFound(writer)
		return
	case err != nil:
		adminMutationError(writer, err)
		return
	}
	r.routes.Clear(scope)
	r.completeAdminMutation(writer, request, *intent)
	writer.WriteHeader(http.StatusNoContent)
}

func (r *Runtime) routeScopeIsSuspended(scope routegate.Scope) bool {
	for _, suspension := range r.routes.Snapshot(r.clockNow().UTC()) {
		if suspension.Scope == scope {
			return true
		}
	}
	return false
}
