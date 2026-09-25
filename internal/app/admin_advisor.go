package app

import (
	"net/http"

	"github.com/akz142857/Halro/internal/advisor"
)

// listAdminAdvisorFindings is the live half of the advisor: the same rules the
// offline doctor runs, against a process that can also answer what the
// admission gate is holding and how its refusals classified.
//
// Read-only, and it reaches nothing outside this process. Every value it
// returns is a configuration key, a count, an enumeration or an identifier the
// operator themselves configured — no prompt, no response body, no credential,
// no source address — which is what lets it be rendered in a browser at all.
func (r *Runtime) listAdminAdvisorFindings(writer http.ResponseWriter, request *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]any{
		"items": advisor.Evaluate(r.advisorInput()),
	})
}

// advisorInput assembles the running instance's answer to every rule.
func (r *Runtime) advisorInput() advisor.Input {
	input := advisorConfigInput(r.config)
	if r.providers != nil {
		publicModel, candidates := r.providers.WidestFanOut()
		input.TopologyKnown = true
		input.WidestFanOut = advisor.FanOut{PublicModel: publicModel, Candidates: candidates}
	}
	if r.routes != nil {
		input.GateKnown = true
		for _, suspension := range r.routes.Snapshot(r.clockNow().UTC()) {
			input.Suspensions = append(input.Suspensions, advisor.Suspension{
				ScopeKind: string(suspension.Scope.Kind),
				// The credential-and-model scope joins its halves with a NUL so
				// they cannot collide inside the gate. That byte has no business
				// in JSON, and the listing renders it the same way.
				ScopeKey:   routeSuspensionKey(suspension.Scope),
				Reason:     failureReasonLabel(suspension.Reason),
				Status:     suspension.Evidence.Status,
				Indefinite: suspension.Indefinite,
			})
		}
	}
	if r.gatewayService != nil {
		input.RefusalsKnown = true
		for _, row := range r.gatewayService.ProviderFailureReasons().Counts {
			input.Refusals = append(input.Refusals, advisor.ReasonCount{
				Reason: row.Reason, Status: row.Status, Count: row.Count,
			})
		}
	}
	return input
}
