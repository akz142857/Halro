package app

import (
	"net/http"
	"sort"
	"time"

	"github.com/akz142857/Halro/internal/routegate"
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
	// CredentialRevision is the revision the refusal was observed against.
	// Saving a new secret advances it and clears the suspension, so this is the
	// number an operator is comparing against when they wonder why it is still
	// here.
	CredentialRevision uint64 `json:"credential_revision,omitempty"`
}

func (r *Runtime) listAdminRouteSuspensions(writer http.ResponseWriter, request *http.Request) {
	now := r.clockNow().UTC()
	suspensions := r.routes.Snapshot(now)
	items := make([]routeSuspensionView, 0, len(suspensions))
	for _, suspension := range suspensions {
		view := routeSuspensionView{
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
