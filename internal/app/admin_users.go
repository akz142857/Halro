package app

import (
	"net/http"
	"strings"
	"time"

	"github.com/akz142857/Halro/internal/adminauth"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/go-chi/chi/v5"
)

// verifyAdminReauthentication is verifyAdminStepUp under the name that matches
// what it is used for here. It shares the rate limit and the audit trail with
// every other step-up in the console: a per-endpoint budget would let an
// attacker take the same number of guesses again at each endpoint.
func (r *Runtime) verifyAdminReauthentication(writer http.ResponseWriter, request *http.Request, username, passwordText, code string) bool {
	return r.verifyAdminStepUp(writer, request, username, passwordText, code)
}

type adminUserView struct {
	Username string `json:"username"`
	Role     string `json:"role"`
}

func toAdminUserView(user domain.AdminUser) adminUserView {
	return adminUserView{Username: user.Username, Role: user.Role}
}

func (r *Runtime) listAdminUsers(writer http.ResponseWriter, request *http.Request) {
	users, err := r.store.ListAdminUsers(request.Context())
	if err != nil {
		adminStoreError(writer)
		return
	}
	views := make([]adminUserView, len(users))
	for index, user := range users {
		views[index] = toAdminUserView(user)
	}
	writeJSON(writer, http.StatusOK, map[string]any{"users": views})
}

// createAdminUser is itself only reachable by an administrator (the mutation
// middleware already enforces that), and is additionally step-up gated: the
// caller resupplies their own current password and a fresh TOTP code — the
// same re-authentication admin_prices.go's pricing endpoints already require
// — because minting a new credential with system access is exactly the kind
// of action that pattern exists for.
func (r *Runtime) createAdminUser(writer http.ResponseWriter, request *http.Request) {
	admin := request.Context().Value(adminContextKey{}).(adminRequestContext)
	var input struct {
		Username        string `json:"username"`
		Password        string `json:"password"`
		Role            string `json:"role"`
		CurrentPassword string `json:"current_password"`
		TOTPCode        string `json:"totp_code"`
	}
	if err := decodeAdminJSON(request, &input); err != nil {
		adminBadRequest(writer, "invalid request")
		return
	}
	// []byte only where NewUser needs one; not scrubbed, see
	// adminauth.VerifyPassword for why nothing here can be.
	newPassword := []byte(input.Password)
	if !r.verifyAdminReauthentication(writer, request, admin.session.Username, input.CurrentPassword, input.TOTPCode) {
		return
	}
	if !domain.ValidAdminRole(input.Role) {
		adminBadRequest(writer, "role must be administrator or read_only")
		return
	}
	username := strings.TrimSpace(input.Username)
	r.adminIdentityMu.Lock()
	defer r.adminIdentityMu.Unlock()
	user, err := adminauth.NewUser(username, newPassword, input.Role, time.Now().UTC())
	if err != nil {
		adminBadRequest(writer, err.Error())
		return
	}
	defer clear(user.PasswordHash)
	defer clear(user.PasswordSalt)
	created, err := r.store.PutAdminUser(request.Context(), user, 0)
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	if err := r.appendAdminAudit(
		"admin_user", admin.session.Username, "admin_user.create", "admin_user", created.Username,
		"success", "",
	); err != nil {
		adminAuditError(writer)
		return
	}
	writeJSON(writer, http.StatusCreated, toAdminUserView(created))
}

// deleteAdminUser refuses to remove the caller's own account (self-delete
// through this path would strand every other tab of theirs mid-request
// rather than logging out cleanly — session/logout is the right way to end
// your own access) and refuses to remove the last administrator: with zero
// administrators left, nothing but an offline CLI break-glass path could
// ever create another one, which is a bigger outage than any single
// permission mistake this endpoint is meant to correct.
func (r *Runtime) deleteAdminUser(writer http.ResponseWriter, request *http.Request) {
	admin := request.Context().Value(adminContextKey{}).(adminRequestContext)
	username := chi.URLParam(request, "username")
	var input struct {
		CurrentPassword string `json:"current_password"`
		TOTPCode        string `json:"totp_code"`
	}
	if err := decodeAdminJSON(request, &input); err != nil {
		adminBadRequest(writer, "invalid request")
		return
	}
	if !r.verifyAdminReauthentication(writer, request, admin.session.Username, input.CurrentPassword, input.TOTPCode) {
		return
	}
	if username == admin.session.Username {
		adminBadRequestCode(writer, "cannot_delete_self", "use session/logout to end your own access")
		return
	}
	r.adminIdentityMu.Lock()
	defer r.adminIdentityMu.Unlock()
	target, err := r.store.GetAdminUser(request.Context(), username)
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	if target.Role == domain.AdminRoleAdministrator {
		users, err := r.store.ListAdminUsers(request.Context())
		if err != nil {
			adminStoreError(writer)
			return
		}
		administrators := 0
		for _, user := range users {
			if user.Role == domain.AdminRoleAdministrator {
				administrators++
			}
		}
		if administrators <= 1 {
			adminBadRequestCode(writer, "last_administrator", "cannot remove the last administrator account")
			return
		}
	}
	if err := r.store.DeleteAdminUser(request.Context(), username, target.Revision); err != nil {
		adminMutationError(writer, err)
		return
	}
	if err := r.appendAdminAudit(
		"admin_user", admin.session.Username, "admin_user.delete", "admin_user", username,
		"success", "",
	); err != nil {
		adminAuditError(writer)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}
