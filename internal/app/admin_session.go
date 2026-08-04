package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/akz142857/Heimdall/internal/adminauth"
	"github.com/akz142857/Heimdall/internal/audit"
	"github.com/akz142857/Heimdall/internal/domain"
	"github.com/akz142857/Heimdall/internal/id"
)

const adminSessionCookie = "__Host-heimdall_session"

type adminLoginWindow struct {
	minute   time.Time
	attempts int
}

type adminAuditRequest struct {
	event  audit.Event
	result chan error
}

type adminContextKey struct{}

type adminRequestContext struct {
	session domain.AdminSession
	token   string
}

func (r *Runtime) loginAdmin(writer http.ResponseWriter, request *http.Request) {
	if !r.adminSameOrigin(request) {
		r.auditAdminLogin("", "failure", "origin_rejected")
		writeJSON(writer, http.StatusForbidden, map[string]string{"error": "origin rejected"})
		return
	}
	if !r.allowAdminLogin(request.RemoteAddr, time.Now()) {
		r.auditAdminLogin("", "failure", "rate_limited")
		writer.Header().Set("Retry-After", "60")
		writeJSON(writer, http.StatusTooManyRequests, map[string]string{"error": "login rate limit exceeded"})
		return
	}
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeAdminJSON(request, &input); err != nil {
		r.auditAdminLogin("", "failure", "invalid_request")
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	password := []byte(input.Password)
	defer clear(password)
	input.Password = ""
	if input.Username == "" || len(input.Username) > 128 || len(password) > 1024 {
		adminauth.DummyVerify(password)
		r.auditAdminLogin(input.Username, "failure", "invalid_credentials")
		writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "invalid credentials"})
		return
	}
	user, err := r.store.GetAdminUser(request.Context(), input.Username)
	if err != nil {
		adminauth.DummyVerify(password)
		r.auditAdminLogin(input.Username, "failure", "invalid_credentials")
		writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "invalid credentials"})
		return
	}
	if !adminauth.VerifyPassword(user, password) {
		r.auditAdminLogin(input.Username, "failure", "invalid_credentials")
		writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "invalid credentials"})
		return
	}
	authenticators, err := r.store.ListAdminMFAAuthenticators(request.Context(), user.Username)
	if err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "authentication unavailable"})
		return
	}
	hasMFA := false
	for _, authenticator := range authenticators {
		if authenticator.Status == domain.AdminMFAStatusActive {
			hasMFA = true
			break
		}
	}
	if hasMFA {
		token, hash, err := adminauth.NewChallengeToken()
		if err != nil {
			writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "authentication unavailable"})
			return
		}
		now := time.Now().UTC()
		challenge := domain.AdminMFAChallenge{IDHash: hash, Username: user.Username, Purpose: domain.AdminMFAChallengeLogin, CreatedAt: now, ExpiresAt: now.Add(5 * time.Minute), AttemptsRemaining: adminauth.MFAChallengeAttempts, SessionGeneration: user.SessionGeneration}
		if err := r.store.PutAdminMFAChallenge(request.Context(), challenge); err != nil {
			writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "authentication unavailable"})
			return
		}
		writeJSON(writer, http.StatusAccepted, map[string]any{"mfa_required": true, "challenge_token": token, "expires_at": challenge.ExpiresAt})
		return
	}
	created, err := r.adminSessions.Create(request.Context(), user, time.Now())
	if err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "session unavailable"})
		return
	}
	if err := r.appendAdminAudit("admin_user", user.Username, "admin.login", "admin_session", "", "success", ""); err != nil {
		_ = r.adminSessions.Revoke(context.Background(), created.Token)
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "audit unavailable"})
		return
	}
	r.setAdminCookie(writer, created.Token, created.Session.AbsoluteExpiresAt)
	writeJSON(writer, http.StatusOK, map[string]any{
		"username": user.Username, "csrf_token": created.CSRFToken,
		"locale":              domain.NormalizeLocalePreference(user.Locale),
		"appearance":          domain.NormalizeAppearance(user.Appearance),
		"absolute_expires_at": created.Session.AbsoluteExpiresAt,
		"idle_expires_at":     created.Session.IdleExpiresAt,
	})
}

func (r *Runtime) getAdminSession(writer http.ResponseWriter, request *http.Request) {
	admin := request.Context().Value(adminContextKey{}).(adminRequestContext)
	user, err := r.store.GetAdminUser(request.Context(), admin.session.Username)
	if err != nil {
		adminStoreError(writer)
		return
	}
	active, _ := r.activeAdminMFA(request.Context(), admin.session.Username)
	writeJSON(writer, http.StatusOK, map[string]any{
		"username":            admin.session.Username,
		"locale":              domain.NormalizeLocalePreference(user.Locale),
		"appearance":          domain.NormalizeAppearance(user.Appearance),
		"csrf_token":          r.adminSessionsCSRF(admin.token),
		"absolute_expires_at": admin.session.AbsoluteExpiresAt,
		"idle_expires_at":     admin.session.IdleExpiresAt,
		"mfa_setup_required":  r.config.Admin.MFAPolicy == "required" && len(active) == 0,
	})
}

func (r *Runtime) logoutAdmin(writer http.ResponseWriter, request *http.Request) {
	admin := request.Context().Value(adminContextKey{}).(adminRequestContext)
	if err := r.adminSessions.Revoke(request.Context(), admin.token); err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "session unavailable"})
		return
	}
	if err := r.appendAdminAudit(
		"admin_user", admin.session.Username, "admin.logout", "admin_session", "",
		"success", "",
	); err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "audit unavailable"})
		return
	}
	r.clearAdminCookie(writer)
	// Admin secrets and CSRF state are memory-only, and every Admin API response
	// is no-store. Clearing the entire origin cache here makes browsers delay the
	// fetch completion (and therefore navigation) without removing additional
	// sensitive state. Expire the scoped HttpOnly session cookie instead.
	writeJSON(writer, http.StatusOK, map[string]string{"status": "logged_out"})
}

func (r *Runtime) changeAdminPassword(writer http.ResponseWriter, request *http.Request) {
	admin := request.Context().Value(adminContextKey{}).(adminRequestContext)
	var input struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if err := decodeAdminJSON(request, &input); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	currentPassword := []byte(input.CurrentPassword)
	newPassword := []byte(input.NewPassword)
	defer clear(currentPassword)
	defer clear(newPassword)
	input.CurrentPassword, input.NewPassword = "", ""
	user, err := r.store.GetAdminUser(request.Context(), admin.session.Username)
	if err != nil || !adminauth.VerifyPassword(user, currentPassword) {
		writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "invalid current password"})
		return
	}
	replacement, err := adminauth.NewUser(user.Username, newPassword, time.Now())
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	defer clear(replacement.PasswordHash)
	defer clear(replacement.PasswordSalt)
	replacement.CreatedAt = user.CreatedAt
	replacement.Locale = user.Locale
	replacement.Appearance = domain.NormalizeAppearance(user.Appearance)
	replacement.SessionGeneration = user.SessionGeneration + 1
	if _, err := r.store.PutAdminUser(request.Context(), replacement, user.Revision); err != nil {
		writeJSON(writer, http.StatusConflict, map[string]string{"error": "password update conflict"})
		return
	}
	if err := r.store.DeleteAdminSessionsForUser(request.Context(), user.Username); err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "session rotation failed"})
		return
	}
	created, err := r.adminSessions.Create(request.Context(), replacement, time.Now())
	if err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "session rotation failed"})
		return
	}
	if err := r.appendAdminAudit(
		"admin_user", user.Username, "admin.password.change", "admin_user", user.Username,
		"success", "",
	); err != nil {
		_ = r.adminSessions.Revoke(context.Background(), created.Token)
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "audit unavailable"})
		return
	}
	r.setAdminCookie(writer, created.Token, created.Session.AbsoluteExpiresAt)
	writeJSON(writer, http.StatusOK, map[string]any{
		"username": user.Username, "csrf_token": created.CSRFToken,
		"locale":              domain.NormalizeLocalePreference(user.Locale),
		"appearance":          domain.NormalizeAppearance(user.Appearance),
		"absolute_expires_at": created.Session.AbsoluteExpiresAt,
		"idle_expires_at":     created.Session.IdleExpiresAt,
	})
}

func (r *Runtime) requireAdmin(next http.Handler) http.Handler {
	return r.requireAdminBase(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if r.config.Admin.MFAPolicy == "required" {
			admin := request.Context().Value(adminContextKey{}).(adminRequestContext)
			active, err := r.activeAdminMFA(request.Context(), admin.session.Username)
			if err != nil || len(active) == 0 {
				writeJSON(writer, http.StatusForbidden, map[string]string{"error": "MFA setup required", "code": "mfa_setup_required"})
				return
			}
		}
		next.ServeHTTP(writer, request)
	}))
}

func (r *Runtime) requireAdminBase(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		cookie, err := request.Cookie(adminSessionCookie)
		if err != nil {
			writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "admin authentication required"})
			return
		}
		session, err := r.adminSessions.Authenticate(request.Context(), cookie.Value, time.Now())
		if err != nil {
			r.clearAdminCookie(writer)
			writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "admin authentication required"})
			return
		}
		ctx := context.WithValue(request.Context(), adminContextKey{}, adminRequestContext{
			session: session, token: cookie.Value,
		})
		next.ServeHTTP(writer, request.WithContext(ctx))
	})
}

func (r *Runtime) requireAdminSetupMutation(next http.Handler) http.Handler {
	return r.requireAdminBase(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		admin := request.Context().Value(adminContextKey{}).(adminRequestContext)
		if !r.adminSameOrigin(request) || !r.adminSessions.VerifyCSRF(admin.token, request.Header.Get("X-CSRF-Token")) {
			writeJSON(writer, http.StatusForbidden, map[string]string{"error": "CSRF validation failed"})
			return
		}
		next.ServeHTTP(writer, request)
	}))
}

func (r *Runtime) requireAdminMutation(next http.Handler) http.Handler {
	return r.requireAdmin(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		admin := request.Context().Value(adminContextKey{}).(adminRequestContext)
		if !r.adminSameOrigin(request) ||
			!r.adminSessions.VerifyCSRF(admin.token, request.Header.Get("X-CSRF-Token")) {
			writeJSON(writer, http.StatusForbidden, map[string]string{"error": "CSRF validation failed"})
			return
		}
		next.ServeHTTP(writer, request)
	}))
}

func (r *Runtime) adminSessionsCSRF(token string) string {
	return r.adminSessions.CSRFToken(token)
}

func (r *Runtime) setAdminCookie(writer http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(writer, &http.Cookie{
		Name: adminSessionCookie, Value: token, Path: "/", Secure: true,
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Expires: expires,
		MaxAge: int(time.Until(expires).Seconds()),
	})
}

func (r *Runtime) clearAdminCookie(writer http.ResponseWriter) {
	http.SetCookie(writer, &http.Cookie{
		Name: adminSessionCookie, Value: "", Path: "/", Secure: true,
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
		MaxAge: -1, Expires: time.Unix(1, 0).UTC(),
	})
}

func (r *Runtime) adminSameOrigin(request *http.Request) bool {
	expected := r.config.Admin.ExternalOrigin
	if expected == "" {
		scheme := "http"
		if request.TLS != nil {
			scheme = "https"
		}
		expected = scheme + "://" + request.Host
	}
	candidate := request.Header.Get("Origin")
	if candidate == "" {
		candidate = request.Header.Get("Referer")
	}
	parsed, err := url.Parse(candidate)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return false
	}
	return parsed.Scheme+"://"+parsed.Host == expected
}

func (r *Runtime) allowAdminLogin(remoteAddress string, now time.Time) bool {
	return allowAdminRate(&r.adminLoginMu, r.adminLogin, remoteAddress, now, r.config.Admin.LoginRPM)
}

func (r *Runtime) allowAdminSetup(remoteAddress string, now time.Time) bool {
	return allowAdminRate(&r.adminSetupRateMu, r.adminSetupRate, remoteAddress, now, r.config.Admin.LoginRPM)
}

func allowAdminRate(mu *sync.Mutex, windows map[string]adminLoginWindow, remoteAddress string, now time.Time, limit int) bool {
	host, _, err := net.SplitHostPort(remoteAddress)
	if err != nil {
		host = remoteAddress
	}
	minute := now.UTC().Truncate(time.Minute)
	mu.Lock()
	defer mu.Unlock()
	window := windows[host]
	if window.minute != minute {
		window = adminLoginWindow{minute: minute}
	}
	if window.attempts >= limit {
		return false
	}
	window.attempts++
	windows[host] = window
	if len(windows) > 4096 {
		for key, value := range windows {
			if value.minute.Before(minute) {
				delete(windows, key)
			}
		}
	}
	return true
}

func (r *Runtime) auditAdminLogin(username, outcome, reason string) {
	if len(username) > 128 {
		username = username[:128]
	}
	if err := r.appendAdminAudit(
		"admin_user", username, "admin.login", "admin_session", "",
		outcome, reason,
	); err != nil {
		r.logger.Error("admin login audit failed", "error", err)
	}
}

func (r *Runtime) appendAdminAudit(
	actorType, actorID, action, targetType, targetID, outcome, reason string,
) error {
	return r.appendAdminAuditWithMetadata(actorType, actorID, action, targetType, targetID, outcome, reason, nil)
}

func (r *Runtime) appendAdminAuditWithMetadata(
	actorType, actorID, action, targetType, targetID, outcome, reason string,
	metadata map[string]string,
) error {
	eventID, err := id.New("aud")
	if err != nil {
		return err
	}
	request := adminAuditRequest{event: audit.Event{
		EventID: eventID, OccurredAt: time.Now().UTC(), ActorType: actorType,
		ActorID: actorID, Action: action, TargetType: targetType, TargetID: targetID,
		Outcome: outcome, ReasonCode: reason, Metadata: metadata,
	}, result: make(chan error, 1)}
	r.auditBatchMu.Lock()
	r.auditBatchPending = append(r.auditBatchPending, request)
	leader := !r.auditBatchRunning
	if leader {
		r.auditBatchRunning = true
	}
	r.auditBatchMu.Unlock()
	if leader {
		r.flushAdminAuditBatches()
	}
	return <-request.result
}

func (r *Runtime) flushAdminAuditBatches() {
	for {
		time.Sleep(time.Millisecond)
		r.auditBatchMu.Lock()
		batch := r.auditBatchPending
		r.auditBatchPending = nil
		if len(batch) == 0 {
			r.auditBatchRunning = false
			r.auditBatchMu.Unlock()
			return
		}
		r.auditBatchMu.Unlock()
		events := make([]audit.Event, len(batch))
		for index := range batch {
			events[index] = batch[index].event
		}
		_, err := r.audit.AppendBatch(context.Background(), events)
		if err == nil {
			err = checkpointAudit(r.store, r.audit.Summary())
		}
		for index := range batch {
			batch[index].result <- err
		}
	}
}

func decodeAdminJSON(request *http.Request, value any) error {
	return decodeAdminJSONLimit(request, value, 20<<10)
}

func decodeAdminJSONLimit(request *http.Request, value any, limit int64) error {
	defer request.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(request.Body, limit+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("request must contain one JSON value")
	}
	return nil
}
