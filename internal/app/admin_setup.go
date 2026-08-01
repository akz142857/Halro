package app

import (
	"context"
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/akz142857/Heimdall/internal/adminauth"
	"github.com/akz142857/Heimdall/internal/config"
	boltstore "github.com/akz142857/Heimdall/internal/store/bolt"
)

type SetupStatus struct {
	InstanceInitialized bool `json:"instance_initialized"`
	SetupRequired       bool `json:"setup_required"`
	TokenRequired       bool `json:"token_required"`
}

// SetupToken returns the transient token only while a first administrator is
// still required. Callers should display it to an operator, never log it.
func (r *Runtime) SetupToken(ctx context.Context) (string, bool, error) {
	count, err := r.store.AdminUserCount(ctx)
	if err != nil {
		return "", false, err
	}
	if count != 0 || !r.setupTokenNeeded {
		return "", false, nil
	}
	r.setupMu.Lock()
	defer r.setupMu.Unlock()
	return r.setupToken, r.setupToken != "", nil
}

func (r *Runtime) SetupStatus(ctx context.Context) (SetupStatus, error) {
	count, err := r.store.AdminUserCount(ctx)
	if err != nil {
		return SetupStatus{}, err
	}
	required := count == 0
	return SetupStatus{
		InstanceInitialized: true,
		SetupRequired:       required,
		TokenRequired:       required && r.setupTokenNeeded,
	}, nil
}

func (r *Runtime) getAdminSetupStatus(writer http.ResponseWriter, request *http.Request) {
	status, err := r.SetupStatus(request.Context())
	if err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "setup state unavailable"})
		return
	}
	writeJSON(writer, http.StatusOK, status)
}

func (r *Runtime) setupAdmin(writer http.ResponseWriter, request *http.Request) {
	if !r.adminSetupSameOrigin(request) {
		writeJSON(writer, http.StatusForbidden, map[string]string{"error": "origin rejected"})
		return
	}
	if count, err := r.store.AdminUserCount(request.Context()); err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "setup state unavailable"})
		return
	} else if count != 0 {
		writeJSON(writer, http.StatusConflict, map[string]string{"error": "administrator setup is already complete"})
		return
	}
	var input struct {
		Username             string `json:"username"`
		Password             string `json:"password"`
		PasswordConfirmation string `json:"password_confirmation"`
		SetupToken           string `json:"setup_token"`
	}
	if err := decodeAdminJSON(request, &input); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	password := []byte(input.Password)
	confirmation := []byte(input.PasswordConfirmation)
	defer clear(password)
	defer clear(confirmation)
	input.Password, input.PasswordConfirmation = "", ""
	if subtle.ConstantTimeCompare(password, confirmation) != 1 {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "password confirmation does not match"})
		return
	}
	if r.setupTokenNeeded {
		if !r.verifySetupToken(input.SetupToken) {
			if !r.allowAdminLogin(request.RemoteAddr, time.Now()) {
				writer.Header().Set("Retry-After", "60")
				writeJSON(writer, http.StatusTooManyRequests, map[string]string{"error": "setup rate limit exceeded"})
				return
			}
			writeJSON(writer, http.StatusForbidden, map[string]string{"error": "invalid setup token"})
			return
		}
	} else if !r.allowAdminLogin(request.RemoteAddr, time.Now()) {
		writer.Header().Set("Retry-After", "60")
		writeJSON(writer, http.StatusTooManyRequests, map[string]string{"error": "setup rate limit exceeded"})
		return
	}
	input.SetupToken = ""
	user, err := adminauth.NewUser(strings.TrimSpace(input.Username), password, time.Now().UTC())
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	defer clear(user.PasswordHash)
	defer clear(user.PasswordSalt)
	r.adminMutationMu.Lock()
	defer r.adminMutationMu.Unlock()
	createdUser, err := r.store.CreateFirstAdmin(request.Context(), user)
	if errors.Is(err, boltstore.ErrAdminInitialized) {
		writeJSON(writer, http.StatusConflict, map[string]string{"error": "administrator setup is already complete"})
		return
	}
	if err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "administrator setup unavailable"})
		return
	}
	if err := r.appendAdminAudit(
		"local_web", createdUser.Username, "admin.bootstrap", "admin_user", createdUser.Username,
		"success", "",
	); err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "audit unavailable"})
		return
	}
	created, err := r.adminSessions.Create(request.Context(), createdUser, time.Now())
	if err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "session unavailable"})
		return
	}
	r.setupMu.Lock()
	r.setupToken = ""
	r.setupMu.Unlock()
	r.setAdminCookie(writer, created.Token, created.Session.AbsoluteExpiresAt)
	writeJSON(writer, http.StatusCreated, map[string]any{
		"username": createdUser.Username, "csrf_token": created.CSRFToken,
		"locale":              "system",
		"absolute_expires_at": created.Session.AbsoluteExpiresAt,
		"idle_expires_at":     created.Session.IdleExpiresAt,
	})
}

// adminSetupSameOrigin additionally pins tokenless local setup to the
// configured listener. Deriving trust from the HTTP Host header alone would
// allow a DNS-rebinding origin to claim an otherwise loopback-only instance.
func (r *Runtime) adminSetupSameOrigin(request *http.Request) bool {
	if !r.adminSameOrigin(request) {
		return false
	}
	if r.setupTokenNeeded || r.config.Admin.ExternalOrigin != "" {
		return true
	}
	return strings.EqualFold(request.Host, r.config.Server.AdminListen)
}

func (r *Runtime) verifySetupToken(candidate string) bool {
	r.setupMu.Lock()
	defer r.setupMu.Unlock()
	if r.setupToken == "" || len(candidate) != len(r.setupToken) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(r.setupToken)) == 1
}

func setupRequiresToken(cfg config.Config) bool {
	if cfg.Admin.ExternalOrigin != "" {
		return true
	}
	host, _, err := net.SplitHostPort(cfg.Server.AdminListen)
	if err != nil {
		return true
	}
	host = strings.Trim(host, "[]")
	if host == "localhost" {
		return false
	}
	address, err := netip.ParseAddr(host)
	return err != nil || !address.IsLoopback()
}
