package adminauth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

const (
	sessionPrefix      = "hms_"
	sessionBytes       = 32
	maxRefreshInterval = time.Minute
)

var ErrInvalidSession = errors.New("invalid admin session")

type SessionStore interface {
	GetAdminUser(context.Context, string) (domain.AdminUser, error)
	PutAdminSession(context.Context, domain.AdminSession) error
	GetAdminSession(context.Context, [32]byte) (domain.AdminSession, error)
	RefreshAdminSession(context.Context, domain.AdminSession, domain.AdminSession, time.Time) (domain.AdminSession, bool, error)
	DeleteAdminSession(context.Context, [32]byte) error
}

type Manager struct {
	store       SessionStore
	csrfKey     []byte
	absoluteTTL time.Duration
	idleTimeout time.Duration
}

type CreatedSession struct {
	Token     string
	CSRFToken string
	Session   domain.AdminSession
}

func NewManager(
	store SessionStore,
	csrfKey []byte,
	absoluteTTL time.Duration,
	idleTimeout time.Duration,
) (*Manager, error) {
	if store == nil || len(csrfKey) != 32 {
		return nil, errors.New("admin session store and 32-byte CSRF key are required")
	}
	if absoluteTTL <= 0 || idleTimeout <= 0 || idleTimeout > absoluteTTL {
		return nil, errors.New("admin session expiry configuration is invalid")
	}
	return &Manager{
		store: store, csrfKey: append([]byte(nil), csrfKey...),
		absoluteTTL: absoluteTTL, idleTimeout: idleTimeout,
	}, nil
}

func (m *Manager) Close() {
	clear(m.csrfKey)
	m.csrfKey = nil
}

func (m *Manager) Create(
	ctx context.Context,
	user domain.AdminUser,
	now time.Time,
) (CreatedSession, error) {
	random := make([]byte, sessionBytes)
	if _, err := rand.Read(random); err != nil {
		return CreatedSession{}, err
	}
	token := sessionPrefix + base64.RawURLEncoding.EncodeToString(random)
	clear(random)
	hash := sha256.Sum256([]byte(token))
	now = now.UTC()
	session := domain.AdminSession{
		IDHash: hash, Username: user.Username, Generation: user.SessionGeneration,
		CreatedAt: now, LastSeenAt: now, AbsoluteExpiresAt: now.Add(m.absoluteTTL),
		IdleExpiresAt: now.Add(m.idleTimeout),
	}
	if err := m.store.PutAdminSession(ctx, session); err != nil {
		return CreatedSession{}, err
	}
	return CreatedSession{
		Token: token, CSRFToken: m.csrfToken(token), Session: session,
	}, nil
}

func (m *Manager) Authenticate(
	ctx context.Context,
	token string,
	now time.Time,
) (domain.AdminSession, error) {
	if !validSessionToken(token) {
		return domain.AdminSession{}, ErrInvalidSession
	}
	hash := sha256.Sum256([]byte(token))
	session, err := m.store.GetAdminSession(ctx, hash)
	if err != nil {
		return domain.AdminSession{}, ErrInvalidSession
	}
	now = now.UTC()
	if !now.Before(session.AbsoluteExpiresAt) || !now.Before(session.IdleExpiresAt) {
		_ = m.store.DeleteAdminSession(context.WithoutCancel(ctx), hash)
		return domain.AdminSession{}, ErrInvalidSession
	}
	user, err := m.store.GetAdminUser(ctx, session.Username)
	if err != nil || user.SessionGeneration != session.Generation {
		_ = m.store.DeleteAdminSession(context.WithoutCancel(ctx), hash)
		return domain.AdminSession{}, ErrInvalidSession
	}
	refreshInterval := min(m.idleTimeout/4, maxRefreshInterval)
	if now.Sub(session.LastSeenAt) < refreshInterval {
		return session, nil
	}
	original := session
	session.LastSeenAt = now
	session.IdleExpiresAt = now.Add(m.idleTimeout)
	if session.IdleExpiresAt.After(session.AbsoluteExpiresAt) {
		session.IdleExpiresAt = session.AbsoluteExpiresAt
	}
	refreshed, valid, err := m.store.RefreshAdminSession(ctx, original, session, now)
	if err != nil {
		return domain.AdminSession{}, err
	}
	if !valid {
		return domain.AdminSession{}, ErrInvalidSession
	}
	return refreshed, nil
}

func (m *Manager) VerifyCSRF(sessionToken, supplied string) bool {
	if !validSessionToken(sessionToken) || supplied == "" {
		return false
	}
	expected := m.csrfToken(sessionToken)
	return subtle.ConstantTimeCompare([]byte(expected), []byte(supplied)) == 1
}

func (m *Manager) CSRFToken(sessionToken string) string {
	if !validSessionToken(sessionToken) {
		return ""
	}
	return m.csrfToken(sessionToken)
}

func (m *Manager) Revoke(ctx context.Context, token string) error {
	if !validSessionToken(token) {
		return nil
	}
	return m.store.DeleteAdminSession(ctx, sha256.Sum256([]byte(token)))
}

func (m *Manager) csrfToken(sessionToken string) string {
	mac := hmac.New(sha256.New, m.csrfKey)
	mac.Write([]byte("halro:admin-csrf:v1:"))
	mac.Write([]byte(sessionToken))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func validSessionToken(token string) bool {
	if !strings.HasPrefix(token, sessionPrefix) {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(token, sessionPrefix))
	return err == nil && len(decoded) == sessionBytes
}
