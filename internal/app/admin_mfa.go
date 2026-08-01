package app

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/akz142857/Heimdall/internal/adminauth"
	"github.com/akz142857/Heimdall/internal/domain"
	"github.com/akz142857/Heimdall/internal/id"
	"github.com/go-chi/chi/v5"
)

type adminMFAPublicAuthenticator struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Type       string     `json:"type"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	Revision   uint64     `json:"revision"`
}

func (r *Runtime) activeAdminMFA(ctx context.Context, username string) ([]domain.AdminMFAAuthenticator, error) {
	all, err := r.store.ListAdminMFAAuthenticators(ctx, username)
	if err != nil {
		return nil, err
	}
	active := make([]domain.AdminMFAAuthenticator, 0, len(all))
	for _, a := range all {
		if a.Status == domain.AdminMFAStatusActive {
			active = append(active, a)
		}
	}
	return active, nil
}

func (r *Runtime) getAdminMFA(w http.ResponseWriter, req *http.Request) {
	admin := req.Context().Value(adminContextKey{}).(adminRequestContext)
	active, err := r.activeAdminMFA(req.Context(), admin.session.Username)
	if err != nil {
		adminStoreError(w)
		return
	}
	items := make([]adminMFAPublicAuthenticator, 0, len(active))
	for _, a := range active {
		items = append(items, adminMFAPublicAuthenticator{a.ID, a.Name, a.Type, a.CreatedAt, a.LastUsedAt, a.Revision})
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": len(items) > 0, "policy": r.config.Admin.MFAPolicy, "authenticators": items})
}

func (r *Runtime) createAdminMFAAuthenticator(w http.ResponseWriter, req *http.Request) {
	admin := req.Context().Value(adminContextKey{}).(adminRequestContext)
	var input struct {
		Name            string `json:"name"`
		CurrentPassword string `json:"current_password"`
		Code            string `json:"code"`
	}
	if decodeAdminJSON(req, &input) != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid request"})
		return
	}
	user, err := r.store.GetAdminUser(req.Context(), admin.session.Username)
	password := []byte(input.CurrentPassword)
	defer clear(password)
	if err != nil || !adminauth.VerifyPassword(user, password) {
		writeJSON(w, 401, map[string]string{"error": "invalid credentials"})
		return
	}
	active, err := r.activeAdminMFA(req.Context(), user.Username)
	if err != nil {
		adminStoreError(w)
		return
	}
	if len(active) >= 5 {
		writeJSON(w, 409, map[string]string{"error": "authenticator limit reached"})
		return
	}
	all, err := r.store.ListAdminMFAAuthenticators(req.Context(), user.Username)
	if err != nil {
		adminStoreError(w)
		return
	}
	for _, previous := range all {
		if previous.Status == domain.AdminMFAStatusPending {
			previous.Status = domain.AdminMFAStatusRevoked
			clear(previous.SecretCiphertext)
			previous.SecretCiphertext = nil
			if _, err := r.store.PutAdminMFAAuthenticator(req.Context(), previous, previous.Revision); err != nil {
				adminStoreError(w)
				return
			}
		}
	}
	if len(active) > 0 {
		if _, ok := r.verifyAnyTOTP(req.Context(), active, input.Code, time.Now()); !ok {
			writeJSON(w, 401, map[string]string{"error": "invalid credentials"})
			return
		}
	}
	authID, err := id.New("mfa")
	if err != nil {
		adminStoreError(w)
		return
	}
	secret, err := adminauth.NewTOTPSecret()
	if err != nil {
		adminStoreError(w)
		return
	}
	defer clear(secret)
	ciphertext, err := r.vault.EncryptAdminMFA(authID, user.Username, secret)
	if err != nil {
		adminStoreError(w)
		return
	}
	now := time.Now().UTC()
	expiry := now.Add(10 * time.Minute)
	value := domain.AdminMFAAuthenticator{ID: authID, Username: user.Username, Name: strings.TrimSpace(input.Name), Type: domain.AdminMFATypeTOTP, SecretCiphertext: ciphertext, Status: domain.AdminMFAStatusPending, CreatedAt: now, ExpiresAt: &expiry}
	stored, err := r.store.PutAdminMFAAuthenticator(req.Context(), value, 0)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid authenticator"})
		return
	}
	_ = r.appendAdminAudit("admin_user", user.Username, "admin.mfa.enrollment.started", "admin_mfa_authenticator", authID, "success", "")
	writeJSON(w, 201, map[string]any{"id": stored.ID, "name": stored.Name, "secret": adminauth.TOTPSecretBase32(secret), "otpauth_uri": adminauth.TOTPUri("Heimdall", user.Username, secret), "expires_at": expiry, "revision": stored.Revision})
}

func (r *Runtime) confirmAdminMFAAuthenticator(w http.ResponseWriter, req *http.Request) {
	admin := req.Context().Value(adminContextKey{}).(adminRequestContext)
	var input struct {
		Code string `json:"code"`
	}
	if decodeAdminJSON(req, &input) != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid request"})
		return
	}
	a, err := r.store.GetAdminMFAAuthenticator(req.Context(), admin.session.Username, chi.URLParam(req, "id"))
	if err != nil || a.Status != domain.AdminMFAStatusPending || a.ExpiresAt == nil || !time.Now().Before(*a.ExpiresAt) {
		writeJSON(w, 404, map[string]string{"error": "authenticator not found"})
		return
	}
	secret, err := r.vault.DecryptAdminMFA(a.ID, a.Username, a.SecretCiphertext)
	if err != nil {
		adminStoreError(w)
		return
	}
	defer clear(secret)
	step, ok := adminauth.VerifyTOTP(secret, input.Code, time.Now(), 0)
	if !ok {
		writeJSON(w, 401, map[string]string{"error": "invalid credentials"})
		return
	}
	now := time.Now().UTC()
	a.Status = domain.AdminMFAStatusActive
	a.ConfirmedAt = &now
	a.ExpiresAt = nil
	a.LastAcceptedTimeStep = step
	if _, err = r.store.PutAdminMFAAuthenticator(req.Context(), a, a.Revision); err != nil {
		adminStoreError(w)
		return
	}
	active, _ := r.activeAdminMFA(req.Context(), a.Username)
	recovery := []string(nil)
	if len(active) == 1 {
		recovery, err = r.replaceRecoveryCodes(req.Context(), a.Username, 1)
		if err != nil {
			adminStoreError(w)
			return
		}
	}
	if err := r.rotateAdminIdentity(req.Context(), w, a.Username); err != nil {
		adminStoreError(w)
		return
	}
	_ = r.appendAdminAudit("admin_user", a.Username, "admin.mfa.authenticator.added", "admin_mfa_authenticator", a.ID, "success", "")
	writeJSON(w, 200, map[string]any{"status": "enabled", "recovery_codes": recovery})
}

func (r *Runtime) completeAdminMFATOTP(w http.ResponseWriter, req *http.Request) {
	var in struct {
		ChallengeToken string `json:"challenge_token"`
		Code           string `json:"code"`
	}
	if decodeAdminJSON(req, &in) != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid request"})
		return
	}
	r.completeMFA(w, req, in.ChallengeToken, in.Code, false)
}
func (r *Runtime) completeAdminMFARecovery(w http.ResponseWriter, req *http.Request) {
	var in struct {
		ChallengeToken string `json:"challenge_token"`
		RecoveryCode   string `json:"recovery_code"`
	}
	if decodeAdminJSON(req, &in) != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid request"})
		return
	}
	r.completeMFA(w, req, in.ChallengeToken, in.RecoveryCode, true)
}

func (r *Runtime) cancelAdminMFAChallenge(w http.ResponseWriter, req *http.Request) {
	if !r.adminSameOrigin(req) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "origin rejected"})
		return
	}
	var input struct {
		ChallengeToken string `json:"challenge_token"`
	}
	if decodeAdminJSON(req, &input) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	hash, err := adminauth.HashChallengeToken(input.ChallengeToken)
	if err == nil {
		_ = r.store.DeleteAdminMFAChallenge(req.Context(), hash)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}

func (r *Runtime) completeMFA(w http.ResponseWriter, req *http.Request, token, code string, recovery bool) {
	if !r.adminSameOrigin(req) || !r.allowAdminLogin(req.RemoteAddr, time.Now()) {
		writeJSON(w, 403, map[string]string{"error": "authentication failed"})
		return
	}
	hash, err := adminauth.HashChallengeToken(token)
	if err != nil {
		writeJSON(w, 401, map[string]string{"error": "invalid credentials"})
		return
	}
	challenge, err := r.store.GetAdminMFAChallenge(req.Context(), hash)
	if err != nil || !time.Now().Before(challenge.ExpiresAt) {
		_ = r.store.DeleteAdminMFAChallenge(context.Background(), hash)
		writeJSON(w, 401, map[string]string{"error": "invalid credentials"})
		return
	}
	user, err := r.store.GetAdminUser(req.Context(), challenge.Username)
	if err != nil || user.SessionGeneration != challenge.SessionGeneration {
		_ = r.store.DeleteAdminMFAChallenge(context.Background(), hash)
		writeJSON(w, 401, map[string]string{"error": "invalid credentials"})
		return
	}
	ok := false
	if recovery {
		_, err = r.store.ConsumeAdminMFARecoveryCode(req.Context(), user.Username, adminauth.RecoveryCodeHash(code), time.Now())
		ok = err == nil
	} else {
		active, _ := r.activeAdminMFA(req.Context(), user.Username)
		_, ok = r.verifyAnyTOTP(req.Context(), active, code, time.Now())
	}
	if !ok {
		_ = r.store.FailAdminMFAChallenge(context.Background(), hash)
		writeJSON(w, 401, map[string]string{"error": "invalid credentials"})
		return
	}
	_ = r.store.DeleteAdminMFAChallenge(context.Background(), hash)
	created, err := r.adminSessions.Create(req.Context(), user, time.Now())
	if err != nil {
		adminStoreError(w)
		return
	}
	action := "admin.mfa.login.success"
	if recovery {
		action = "admin.mfa.recovery_code.used"
	}
	if err = r.appendAdminAudit("admin_user", user.Username, action, "admin_session", "", "success", ""); err != nil {
		_ = r.adminSessions.Revoke(context.Background(), created.Token)
		adminStoreError(w)
		return
	}
	r.setAdminCookie(w, created.Token, created.Session.AbsoluteExpiresAt)
	writeJSON(w, 200, map[string]any{"username": user.Username, "csrf_token": created.CSRFToken, "locale": domain.NormalizeLocalePreference(user.Locale), "absolute_expires_at": created.Session.AbsoluteExpiresAt, "idle_expires_at": created.Session.IdleExpiresAt})
}

func (r *Runtime) verifyAnyTOTP(ctx context.Context, active []domain.AdminMFAAuthenticator, code string, now time.Time) (string, bool) {
	for _, a := range active {
		secret, err := r.vault.DecryptAdminMFA(a.ID, a.Username, a.SecretCiphertext)
		if err != nil {
			continue
		}
		step, ok := adminauth.VerifyTOTP(secret, code, now, a.LastAcceptedTimeStep)
		clear(secret)
		if ok && r.store.AcceptAdminMFATimeStep(ctx, a.Username, a.ID, step, now) == nil {
			return a.ID, true
		}
	}
	return "", false
}

func (r *Runtime) replaceRecoveryCodes(ctx context.Context, username string, generation uint64) ([]string, error) {
	displays := make([]string, 10)
	records := make([]domain.AdminMFARecoveryCode, 10)
	now := time.Now().UTC()
	for i := range records {
		display, hash, err := adminauth.NewRecoveryCode()
		if err != nil {
			return nil, err
		}
		codeID, err := id.New("mrc")
		if err != nil {
			return nil, err
		}
		displays[i] = display
		records[i] = domain.AdminMFARecoveryCode{ID: codeID, Username: username, CodeHash: hash, CreatedAt: now, Generation: generation}
	}
	return displays, r.store.ReplaceAdminMFARecoveryCodes(ctx, username, records)
}

func (r *Runtime) rotateAdminIdentity(ctx context.Context, w http.ResponseWriter, username string) error {
	user, err := r.store.GetAdminUser(ctx, username)
	if err != nil {
		return err
	}
	user.SessionGeneration++
	if _, err = r.store.PutAdminUser(ctx, user, user.Revision); err != nil {
		return err
	}
	if err = r.store.DeleteAdminSessionsForUser(ctx, username); err != nil {
		return err
	}
	created, err := r.adminSessions.Create(ctx, user, time.Now())
	if err != nil {
		return err
	}
	r.setAdminCookie(w, created.Token, created.Session.AbsoluteExpiresAt)
	return nil
}

func (r *Runtime) renameAdminMFAAuthenticator(w http.ResponseWriter, req *http.Request) {
	admin := req.Context().Value(adminContextKey{}).(adminRequestContext)
	var in struct {
		Name string `json:"name"`
	}
	if decodeAdminJSON(req, &in) != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid request"})
		return
	}
	a, err := r.store.GetAdminMFAAuthenticator(req.Context(), admin.session.Username, chi.URLParam(req, "id"))
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "authenticator not found"})
		return
	}
	a.Name = strings.TrimSpace(in.Name)
	if _, err = r.store.PutAdminMFAAuthenticator(req.Context(), a, a.Revision); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid authenticator"})
		return
	}
	_ = r.appendAdminAudit("admin_user", a.Username, "admin.mfa.authenticator.renamed", "admin_mfa_authenticator", a.ID, "success", "")
	writeJSON(w, 200, map[string]string{"status": "renamed"})
}

func (r *Runtime) deleteAdminMFAAuthenticator(w http.ResponseWriter, req *http.Request) {
	admin := req.Context().Value(adminContextKey{}).(adminRequestContext)
	var in struct {
		CurrentPassword string `json:"current_password"`
		Code            string `json:"code"`
	}
	if decodeAdminJSON(req, &in) != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid request"})
		return
	}
	user, err := r.store.GetAdminUser(req.Context(), admin.session.Username)
	p := []byte(in.CurrentPassword)
	defer clear(p)
	if err != nil || !adminauth.VerifyPassword(user, p) {
		writeJSON(w, 401, map[string]string{"error": "invalid credentials"})
		return
	}
	active, _ := r.activeAdminMFA(req.Context(), user.Username)
	target := chi.URLParam(req, "id")
	others := make([]domain.AdminMFAAuthenticator, 0)
	for _, a := range active {
		if a.ID != target {
			others = append(others, a)
		}
	}
	if len(others) == 0 && r.config.Admin.MFAPolicy == "required" {
		writeJSON(w, 409, map[string]string{"error": "MFA is required"})
		return
	}
	if len(others) > 0 {
		if _, ok := r.verifyAnyTOTP(req.Context(), others, in.Code, time.Now()); !ok {
			writeJSON(w, 401, map[string]string{"error": "invalid credentials"})
			return
		}
	} else {
		if _, ok := r.verifyAnyTOTP(req.Context(), active, in.Code, time.Now()); !ok {
			writeJSON(w, 401, map[string]string{"error": "invalid credentials"})
			return
		}
	}
	a, err := r.store.GetAdminMFAAuthenticator(req.Context(), user.Username, target)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "authenticator not found"})
		return
	}
	a.Status = domain.AdminMFAStatusRevoked
	clear(a.SecretCiphertext)
	a.SecretCiphertext = nil
	if _, err = r.store.PutAdminMFAAuthenticator(req.Context(), a, a.Revision); err != nil {
		adminStoreError(w)
		return
	}
	if len(others) == 0 {
		_ = r.store.ReplaceAdminMFARecoveryCodes(req.Context(), user.Username, nil)
	}
	if err = r.rotateAdminIdentity(req.Context(), w, user.Username); err != nil {
		adminStoreError(w)
		return
	}
	_ = r.appendAdminAudit("admin_user", user.Username, "admin.mfa.authenticator.revoked", "admin_mfa_authenticator", target, "success", "")
	writeJSON(w, 200, map[string]string{"status": "revoked"})
}

func (r *Runtime) regenerateAdminMFARecoveryCodes(w http.ResponseWriter, req *http.Request) {
	admin := req.Context().Value(adminContextKey{}).(adminRequestContext)
	var in struct {
		CurrentPassword string `json:"current_password"`
		Code            string `json:"code"`
	}
	if decodeAdminJSON(req, &in) != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid request"})
		return
	}
	user, err := r.store.GetAdminUser(req.Context(), admin.session.Username)
	p := []byte(in.CurrentPassword)
	defer clear(p)
	active, _ := r.activeAdminMFA(req.Context(), admin.session.Username)
	if err != nil || !adminauth.VerifyPassword(user, p) {
		writeJSON(w, 401, map[string]string{"error": "invalid credentials"})
		return
	}
	if _, ok := r.verifyAnyTOTP(req.Context(), active, in.Code, time.Now()); !ok {
		writeJSON(w, 401, map[string]string{"error": "invalid credentials"})
		return
	}
	codes, err := r.replaceRecoveryCodes(req.Context(), user.Username, uint64(time.Now().UnixNano()))
	if err != nil {
		adminStoreError(w)
		return
	}
	if err = r.rotateAdminIdentity(req.Context(), w, user.Username); err != nil {
		adminStoreError(w)
		return
	}
	_ = r.appendAdminAudit("admin_user", user.Username, "admin.mfa.recovery_codes.regenerated", "admin_user", user.Username, "success", "")
	writeJSON(w, 200, map[string]any{"recovery_codes": codes})
}

func (r *Runtime) disableAdminMFA(w http.ResponseWriter, req *http.Request) {
	if r.config.Admin.MFAPolicy == "required" {
		writeJSON(w, 409, map[string]string{"error": "MFA is required"})
		return
	}
	admin := req.Context().Value(adminContextKey{}).(adminRequestContext)
	var in struct {
		CurrentPassword string `json:"current_password"`
		Code            string `json:"code"`
	}
	if decodeAdminJSON(req, &in) != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid request"})
		return
	}
	user, err := r.store.GetAdminUser(req.Context(), admin.session.Username)
	p := []byte(in.CurrentPassword)
	defer clear(p)
	active, _ := r.activeAdminMFA(req.Context(), user.Username)
	if err != nil || !adminauth.VerifyPassword(user, p) {
		writeJSON(w, 401, map[string]string{"error": "invalid credentials"})
		return
	}
	if _, ok := r.verifyAnyTOTP(req.Context(), active, in.Code, time.Now()); !ok {
		writeJSON(w, 401, map[string]string{"error": "invalid credentials"})
		return
	}
	if err = r.store.DeleteAdminMFAForUser(req.Context(), user.Username); err != nil {
		adminStoreError(w)
		return
	}
	if err = r.rotateAdminIdentity(req.Context(), w, user.Username); err != nil {
		adminStoreError(w)
		return
	}
	_ = r.appendAdminAudit("admin_user", user.Username, "admin.mfa.disabled", "admin_user", user.Username, "success", "")
	writeJSON(w, 200, map[string]string{"status": "disabled"})
}
