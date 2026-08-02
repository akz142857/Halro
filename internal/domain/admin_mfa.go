package domain

import (
	"errors"
	"strings"
	"time"
)

const (
	AdminMFATypeTOTP       = "totp"
	AdminMFAStatusPending  = "pending"
	AdminMFAStatusActive   = "active"
	AdminMFAStatusRevoked  = "revoked"
	AdminMFAChallengeLogin = "login"
)

type AdminMFAAuditIntent struct {
	EventID    string    `json:"event_id"`
	OccurredAt time.Time `json:"occurred_at"`
	ActorID    string    `json:"actor_id"`
	Action     string    `json:"action"`
	TargetType string    `json:"target_type"`
	TargetID   string    `json:"target_id"`
}

func (i AdminMFAAuditIntent) Validate() error {
	if i.EventID == "" || i.OccurredAt.IsZero() || i.ActorID == "" || i.Action == "" || i.TargetType == "" {
		return errors.New("invalid MFA audit intent")
	}
	return nil
}

type AdminMFAAuthenticator struct {
	ID                   string     `json:"id"`
	Username             string     `json:"username"`
	Name                 string     `json:"name"`
	Type                 string     `json:"type"`
	SecretCiphertext     []byte     `json:"secret_ciphertext,omitempty"`
	Status               string     `json:"status"`
	CreatedAt            time.Time  `json:"created_at"`
	ConfirmedAt          *time.Time `json:"confirmed_at,omitempty"`
	LastUsedAt           *time.Time `json:"last_used_at,omitempty"`
	LastAcceptedTimeStep int64      `json:"last_accepted_time_step,omitempty"`
	ExpiresAt            *time.Time `json:"expires_at,omitempty"`
	Revision             uint64     `json:"revision"`
}

func (a *AdminMFAAuthenticator) GetRevision() uint64  { return a.Revision }
func (a *AdminMFAAuthenticator) SetRevision(v uint64) { a.Revision = v }

func (a AdminMFAAuthenticator) Validate() error {
	var problems []error
	if a.ID == "" || a.Username == "" {
		problems = append(problems, errors.New("MFA authenticator identity is required"))
	}
	if n := len([]rune(strings.TrimSpace(a.Name))); n < 1 || n > 64 {
		problems = append(problems, errors.New("MFA authenticator name must contain 1 to 64 characters"))
	}
	if a.Type != AdminMFATypeTOTP {
		problems = append(problems, errors.New("unsupported MFA authenticator type"))
	}
	if a.Status != AdminMFAStatusPending && a.Status != AdminMFAStatusActive && a.Status != AdminMFAStatusRevoked {
		problems = append(problems, errors.New("invalid MFA authenticator status"))
	}
	if a.CreatedAt.IsZero() {
		problems = append(problems, errors.New("MFA authenticator creation time is required"))
	}
	if a.Status != AdminMFAStatusRevoked && len(a.SecretCiphertext) == 0 {
		problems = append(problems, errors.New("MFA authenticator secret is required"))
	}
	if a.Status == AdminMFAStatusPending && (a.ExpiresAt == nil || !a.ExpiresAt.After(a.CreatedAt)) {
		problems = append(problems, errors.New("pending MFA authenticator expiry is required"))
	}
	if a.Status == AdminMFAStatusActive && a.ConfirmedAt == nil {
		problems = append(problems, errors.New("active MFA authenticator confirmation is required"))
	}
	return errors.Join(problems...)
}

type AdminMFARecoveryCode struct {
	ID         string     `json:"id"`
	Username   string     `json:"username"`
	CodeHash   [32]byte   `json:"code_hash"`
	CreatedAt  time.Time  `json:"created_at"`
	UsedAt     *time.Time `json:"used_at,omitempty"`
	Generation uint64     `json:"generation"`
}

func (c AdminMFARecoveryCode) Validate() error {
	if c.ID == "" || c.Username == "" || c.CodeHash == [32]byte{} || c.CreatedAt.IsZero() || c.Generation == 0 {
		return errors.New("invalid MFA recovery code")
	}
	return nil
}

type AdminMFAChallenge struct {
	IDHash            [32]byte  `json:"id_hash"`
	Username          string    `json:"username"`
	Purpose           string    `json:"purpose"`
	CreatedAt         time.Time `json:"created_at"`
	ExpiresAt         time.Time `json:"expires_at"`
	AttemptsRemaining uint8     `json:"attempts_remaining"`
	SessionGeneration uint64    `json:"session_generation"`
	Claimed           bool      `json:"claimed,omitempty"`
}

func (c AdminMFAChallenge) Validate() error {
	if c.IDHash == [32]byte{} || c.Username == "" || c.Purpose != AdminMFAChallengeLogin || c.CreatedAt.IsZero() || !c.ExpiresAt.After(c.CreatedAt) || c.AttemptsRemaining == 0 || c.SessionGeneration == 0 {
		return errors.New("invalid MFA challenge")
	}
	return nil
}
