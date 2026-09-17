package app

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/akz142857/Halro/internal/adminauth"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/id"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

type SetupStatus struct {
	InstanceInitialized bool `json:"instance_initialized"`
	SetupRequired       bool `json:"setup_required"`
	TokenRequired       bool `json:"token_required"`
}

// TakeGeneratedSetupToken returns a generated token exactly once for the
// interactive start command. File-backed tokens are never exposed here.
func (r *Runtime) TakeGeneratedSetupToken(ctx context.Context) (string, bool, error) {
	count, err := r.store.AdminUserCount(ctx)
	if err != nil {
		return "", false, err
	}
	if count != 0 || !r.setupTokenNeeded {
		return "", false, nil
	}
	r.setupMu.Lock()
	defer r.setupMu.Unlock()
	if r.setupToken.source != setupTokenSourceGenerated || len(r.setupToken.generated) == 0 {
		return "", false, nil
	}
	token := string(r.setupToken.generated)
	clear(r.setupToken.generated)
	r.setupToken.generated = nil
	return token, true, nil
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
	// []byte for the comparison and the hash below; not scrubbed, see
	// adminauth.VerifyPassword for why nothing here can be.
	password := []byte(input.Password)
	confirmation := []byte(input.PasswordConfirmation)
	if subtle.ConstantTimeCompare(password, confirmation) != 1 {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "password confirmation does not match"})
		return
	}
	if r.setupTokenNeeded {
		if !r.verifySetupToken(input.SetupToken) {
			if !r.allowAdminSetup(request.RemoteAddr, r.clockNow()) {
				writer.Header().Set("Retry-After", "60")
				writeJSON(writer, http.StatusTooManyRequests, map[string]string{"error": "setup rate limit exceeded"})
				return
			}
			writeJSON(writer, http.StatusForbidden, map[string]string{"error": "invalid setup token"})
			return
		}
	} else if !r.allowAdminSetup(request.RemoteAddr, r.clockNow()) {
		writer.Header().Set("Retry-After", "60")
		writeJSON(writer, http.StatusTooManyRequests, map[string]string{"error": "setup rate limit exceeded"})
		return
	}
	input.SetupToken = ""
	// The first administrator created through setup is always full
	// administrator — there is no one yet to have granted a lesser role.
	user, err := adminauth.NewUser(strings.TrimSpace(input.Username), password, domain.AdminRoleAdministrator, time.Now().UTC())
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	defer clear(user.PasswordHash)
	defer clear(user.PasswordSalt)
	r.adminIdentityMu.Lock()
	defer r.adminIdentityMu.Unlock()
	operationID, err := id.New("setupop")
	if err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "administrator setup unavailable"})
		return
	}
	eventID, err := id.New("aud")
	if err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "administrator setup unavailable"})
		return
	}
	source := r.setupTokenSource()
	intent := domain.AdminAuditIntent{
		EventID: eventID, OccurredAt: r.clockNow().UTC(), ActorType: "local_web",
		ActorID: user.Username, Action: "admin.bootstrap", TargetType: "admin_user", TargetID: user.Username,
		CorrelationID: strings.TrimSpace(request.Header.Get("X-Request-ID")),
		Metadata:      map[string]string{"operation_id": operationID, "source": source},
	}
	completion := boltstore.AdminBootstrapCompletion{
		OperationID: operationID, Username: user.Username, AuditEventID: eventID, CommittedAt: intent.OccurredAt,
	}
	createdUser, _, _, err := r.store.CreateFirstAdminWithAuditIntent(request.Context(), user, completion, intent)
	if errors.Is(err, boltstore.ErrAdminInitialized) || errors.Is(err, boltstore.ErrAdminBootstrapConflict) {
		writeJSON(writer, http.StatusConflict, map[string]string{"error": "administrator setup is already complete"})
		return
	}
	if err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "administrator setup unavailable"})
		return
	}
	r.clearSetupToken()
	r.completeAdminMutation(writer, request, intent)
	created, err := r.adminSessions.Create(request.Context(), createdUser, time.Now())
	if err != nil {
		writeJSON(writer, http.StatusConflict, map[string]string{
			"error": "administrator setup is complete; sign in",
			"code":  "setup_committed_login_required",
		})
		return
	}
	r.setAdminCookie(writer, created.Token, created.Session.AbsoluteExpiresAt)
	writeJSON(writer, http.StatusCreated, map[string]any{
		"username": createdUser.Username, "csrf_token": created.CSRFToken,
		"role":                createdUser.Role,
		"locale":              "system",
		"appearance":          domain.NormalizeAppearance(createdUser.Appearance),
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
	if !r.setupToken.present || len(candidate) > setupTokenInputLimit || !r.clockNow().Before(r.setupToken.expiresAt) {
		return false
	}
	candidateHash := sha256.Sum256([]byte(candidate))
	return subtle.ConstantTimeCompare(candidateHash[:], r.setupToken.hash[:]) == 1
}

func (r *Runtime) setupTokenSource() string {
	r.setupMu.Lock()
	defer r.setupMu.Unlock()
	return r.setupToken.source.String()
}

func (r *Runtime) clearSetupToken() {
	r.setupMu.Lock()
	defer r.setupMu.Unlock()
	r.setupToken.clear()
}

func setupRequiresToken(cfg config.Config) bool {
	if cfg.Admin.SetupTokenFile != "" {
		return true
	}
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
