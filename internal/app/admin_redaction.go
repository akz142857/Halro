package app

import (
	"errors"
	"net/http"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/id"
	"github.com/akz142857/Halro/internal/redaction"
	"github.com/go-chi/chi/v5"
)

type redactionRuleInput struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Kind        string   `json:"kind"`
	Builtin     string   `json:"builtin"`
	Pattern     string   `json:"pattern"`
	Dictionary  []string `json:"dictionary"`
	Scopes      []string `json:"scopes"`
	Action      string   `json:"action"`
	Replacement string   `json:"replacement"`
	Enabled     bool     `json:"enabled"`
	Priority    int      `json:"priority"`
}

type redactionPolicyInput struct {
	Name    string               `json:"name"`
	Enabled bool                 `json:"enabled"`
	Mode    string               `json:"mode"`
	Rules   []redactionRuleInput `json:"rules"`
}

type redactionTestInput struct {
	Input string `json:"input"`
	Scope string `json:"scope"`
}

type redactionPolicyView struct {
	domain.RedactionPolicy
	BoundProjects int `json:"bound_projects,omitempty"`
}

func (r *Runtime) listAdminRedactionPolicies(writer http.ResponseWriter, request *http.Request) {
	items, err := r.store.ListRedactionPolicies(request.Context())
	if err != nil {
		adminStoreError(writer)
		return
	}
	bindings := make(map[string]int)
	projects, err := r.store.ListProjects(request.Context())
	if err != nil {
		adminStoreError(writer)
		return
	}
	for _, project := range projects {
		if project.DeletedAt == nil && project.RedactionPolicyID != "" {
			bindings[project.RedactionPolicyID]++
		}
	}
	active := make([]redactionPolicyView, 0, len(items))
	for _, item := range items {
		if item.DeletedAt == nil {
			active = append(active, redactionPolicyView{RedactionPolicy: item, BoundProjects: bindings[item.ID]})
		}
	}
	writeResourcePage(writer, request, active, func(item redactionPolicyView) string { return item.ID })
}

func (r *Runtime) getAdminRedactionPolicy(writer http.ResponseWriter, request *http.Request) {
	policy, err := r.store.GetRedactionPolicy(request.Context(), chi.URLParam(request, "id"))
	if err != nil || policy.DeletedAt != nil {
		adminNotFound(writer)
		return
	}
	writer.Header().Set("ETag", revisionETag(policy.Revision))
	writeJSON(writer, http.StatusOK, policy)
}

func (r *Runtime) createAdminRedactionPolicy(writer http.ResponseWriter, request *http.Request) {
	var input redactionPolicyInput
	if err := decodeAdminJSONLimit(request, &input, 256<<10); err != nil {
		adminBadRequestCode(writer, "invalid_request", "invalid request")
		return
	}
	policyID, err := id.New("rdp")
	if err != nil {
		adminStoreError(writer)
		return
	}
	now := time.Now().UTC()
	policy, err := input.policy(policyID, now, now)
	if err != nil {
		adminBadRequest(writer, err.Error())
		return
	}
	r.adminProjectMu.Lock()
	defer r.adminProjectMu.Unlock()
	intent, intentErr := r.newAdminAuditIntent(request, "redaction_policy.create", "redaction_policy", policy.ID)
	if intentErr != nil {
		adminStoreError(writer)
		return
	}
	policy, err = r.store.PutRedactionPolicy(request.Context(), policy, 0, intent)
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	r.activateRedactionPolicies()
	r.completeAdminMutation(writer, request, *intent)
	writer.Header().Set("ETag", revisionETag(policy.Revision))
	writeJSON(writer, http.StatusCreated, policy)
}

func (r *Runtime) updateAdminRedactionPolicy(writer http.ResponseWriter, request *http.Request) {
	expected, ok := requireRevision(writer, request)
	if !ok {
		return
	}
	var input struct {
		redactionPolicyInput
		stepUpMaterial
	}
	if err := decodeAdminJSONLimit(request, &input, 256<<10); err != nil {
		adminBadRequestCode(writer, "invalid_request", "invalid request")
		return
	}
	// Before the store is touched, for the reason deleteAdminProject gives: the
	// revision header is a parse, this is an Argon2id verification.
	if !r.requireStepUpMaterial(writer, request, input.stepUpMaterial) {
		return
	}
	r.adminProjectMu.Lock()
	defer r.adminProjectMu.Unlock()
	current, err := r.store.GetRedactionPolicy(request.Context(), chi.URLParam(request, "id"))
	if err != nil || current.DeletedAt != nil {
		adminNotFound(writer)
		return
	}
	if current.Revision != expected {
		adminPreconditionFailed(writer)
		return
	}
	policy, err := input.policy(current.ID, current.CreatedAt, time.Now().UTC())
	if err != nil {
		adminBadRequest(writer, err.Error())
		return
	}
	if err := r.validateRedactionCanDeactivate(request, policy.ID, policy.Enabled); err != nil {
		writeJSON(writer, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	intent, intentErr := r.newAdminAuditIntent(request, "redaction_policy.update", "redaction_policy", policy.ID)
	if intentErr != nil {
		adminStoreError(writer)
		return
	}
	policy, err = r.store.PutRedactionPolicy(request.Context(), policy, expected, intent)
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	r.activateRedactionPolicies()
	r.completeAdminMutation(writer, request, *intent)
	writer.Header().Set("ETag", revisionETag(policy.Revision))
	writeJSON(writer, http.StatusOK, policy)
}

func (r *Runtime) deleteAdminRedactionPolicy(writer http.ResponseWriter, request *http.Request) {
	expected, ok := requireRevision(writer, request)
	if !ok {
		return
	}
	// Deleting is not undoable and a stolen session should not be enough to do
	// it. The revision precondition is checked first: it costs a header parse,
	// while the step-up costs an Argon2id verification, and a request that
	// cannot succeed anyway should not buy one.
	if !r.requireDestructiveStepUp(writer, request) {
		return
	}
	r.adminProjectMu.Lock()
	defer r.adminProjectMu.Unlock()
	policy, err := r.store.GetRedactionPolicy(request.Context(), chi.URLParam(request, "id"))
	if err != nil || policy.DeletedAt != nil {
		adminNotFound(writer)
		return
	}
	if policy.Revision != expected {
		adminPreconditionFailed(writer)
		return
	}
	if err := r.validateRedactionCanDelete(request, policy.ID); err != nil {
		writeJSON(writer, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	now := time.Now().UTC()
	policy.Enabled = false
	policy.UpdatedAt = now
	policy.DeletedAt = &now
	intent, intentErr := r.newAdminAuditIntent(request, "redaction_policy.delete", "redaction_policy", policy.ID)
	if intentErr != nil {
		adminStoreError(writer)
		return
	}
	policy, err = r.store.PutRedactionPolicy(request.Context(), policy, expected, intent)
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	r.activateRedactionPolicies()
	r.completeAdminMutation(writer, request, *intent)
	writer.Header().Set("ETag", revisionETag(policy.Revision))
	writer.WriteHeader(http.StatusNoContent)
}

func (r *Runtime) testAdminRedactionPolicy(writer http.ResponseWriter, request *http.Request) {
	policy, err := r.store.GetRedactionPolicy(request.Context(), chi.URLParam(request, "id"))
	if err != nil || policy.DeletedAt != nil {
		adminNotFound(writer)
		return
	}
	var input redactionTestInput
	if err := decodeAdminJSON(request, &input); err != nil || len(input.Input) > 16<<10 {
		adminBadRequestCode(writer, "invalid_request", "invalid request")
		return
	}
	matches, err := r.redactor.Test(policy.ID, input.Input, input.Scope)
	input.Input = ""
	if err != nil {
		adminBadRequest(writer, err.Error())
		return
	}
	if err := r.auditAdminMutation(request, "redaction_policy.test", "redaction_policy", policy.ID); err != nil {
		adminAuditError(writer)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"matches": matches, "match_count": len(matches),
	})
}

func (input redactionPolicyInput) policy(
	policyID string,
	createdAt, updatedAt time.Time,
) (domain.RedactionPolicy, error) {
	policy := domain.RedactionPolicy{
		ID: policyID, Name: input.Name, Enabled: input.Enabled, Mode: input.Mode,
		CreatedAt: createdAt, UpdatedAt: updatedAt,
		Rules: make([]domain.RedactionRule, 0, len(input.Rules)),
	}
	for _, value := range input.Rules {
		ruleID := value.ID
		if ruleID == "" {
			var err error
			ruleID, err = id.New("rrl")
			if err != nil {
				return domain.RedactionPolicy{}, err
			}
		}
		policy.Rules = append(policy.Rules, domain.RedactionRule{
			ID: ruleID, Name: value.Name, Kind: value.Kind, Builtin: value.Builtin,
			Pattern: value.Pattern, Dictionary: value.Dictionary, Scopes: value.Scopes,
			Action: value.Action, Replacement: value.Replacement,
			Enabled: value.Enabled, Priority: value.Priority,
		})
	}
	return redaction.CompilePolicy(policy)
}

// validateRedactionCanDeactivate refuses to switch a policy off while a project
// that is serving traffic still routes through it. A project that is itself
// switched off is not consulted: nothing flows through it, and the policy it
// names still exists.
func (r *Runtime) validateRedactionCanDeactivate(
	request *http.Request,
	id string,
	enabled bool,
) error {
	if enabled {
		return nil
	}
	return r.validateNoRedactionReference(request, id, func(project domain.Project) bool {
		return project.Enabled
	})
}

// validateRedactionCanDelete is the stricter half: a disabled project keeps its
// policy reference and can be switched back on at any time, so removing the
// policy it names leaves a dangling ID in the store — and the engine's lookup
// miss is fail-open. The re-enable path does refuse the project later, but by
// then the deletion has happened and the operator is left with a project they
// can no longer switch on.
func (r *Runtime) validateRedactionCanDelete(request *http.Request, id string) error {
	return r.validateNoRedactionReference(request, id, func(domain.Project) bool { return true })
}

func (r *Runtime) validateNoRedactionReference(
	request *http.Request,
	id string,
	consider func(domain.Project) bool,
) error {
	projects, err := r.store.ListProjects(request.Context())
	if err != nil {
		return errors.New("projects are unavailable")
	}
	for _, project := range projects {
		if project.DeletedAt == nil && consider(project) && project.RedactionPolicyID == id {
			return errors.New("remove this policy from the projects that reference it first")
		}
	}
	return nil
}

// activateRedactionPolicies carries a durable redaction policy change into the
// live redactor.
//
// Like the topology and auth snapshots it runs on the runtime's own context, so
// an Admin client that disconnects cannot decide whether the change takes
// effect, and a failure marks the runtime stale instead of failing the request.
// Redaction is a control that applies to live traffic: running it from a
// snapshot known to be behind the store is the fail-open direction.
func (r *Runtime) activateRedactionPolicies() {
	ctx, cancel := r.activationContext()
	defer cancel()
	policies, err := r.store.ListRedactionPolicies(ctx)
	if err == nil {
		err = r.redactor.ReplacePolicies(policies)
	}
	if err != nil {
		r.logger.Error("redaction policy activation failed after a durable mutation", "error", err)
		r.activation.markStale(activationDomainRedaction, "redaction policies: "+err.Error(), time.Now().UTC())
		return
	}
	r.activation.markCurrent(activationDomainRedaction)
}
