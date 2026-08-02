package domain

import (
	"errors"
	"strings"
	"time"
)

type AdminUser struct {
	Username          string               `json:"username"`
	Locale            string               `json:"locale,omitempty"`
	PasswordVersion   uint16               `json:"password_version"`
	PasswordSalt      []byte               `json:"password_salt"`
	PasswordHash      []byte               `json:"password_hash"`
	ArgonMemoryKiB    uint32               `json:"argon_memory_kib"`
	ArgonIterations   uint32               `json:"argon_iterations"`
	ArgonParallelism  uint8                `json:"argon_parallelism"`
	SessionGeneration uint64               `json:"session_generation"`
	CreatedAt         time.Time            `json:"created_at"`
	UpdatedAt         time.Time            `json:"updated_at"`
	Revision          uint64               `json:"revision"`
	PendingMFAAudit   *AdminMFAAuditIntent `json:"pending_mfa_audit,omitempty"`
}

func (u *AdminUser) GetRevision() uint64      { return u.Revision }
func (u *AdminUser) SetRevision(value uint64) { u.Revision = value }

func (u AdminUser) Validate() error {
	var problems []error
	if strings.TrimSpace(u.Username) == "" {
		problems = append(problems, errors.New("admin username is required"))
	}
	if len(u.Username) > 128 {
		problems = append(problems, errors.New("admin username is too long"))
	}
	if u.PasswordVersion == 0 || len(u.PasswordSalt) < 16 || len(u.PasswordHash) < 32 {
		problems = append(problems, errors.New("admin password hash is invalid"))
	}
	if u.ArgonMemoryKiB < 64*1024 || u.ArgonIterations < 3 || u.ArgonParallelism < 1 {
		problems = append(problems, errors.New("admin Argon2id parameters are too weak"))
	}
	if u.SessionGeneration == 0 {
		problems = append(problems, errors.New("admin session generation is required"))
	}
	if !IsSupportedLocalePreference(u.Locale) {
		problems = append(problems, errors.New("admin locale preference is not supported"))
	}
	if u.CreatedAt.IsZero() || u.UpdatedAt.IsZero() {
		problems = append(problems, errors.New("admin timestamps are required"))
	}
	if u.PendingMFAAudit != nil {
		if err := u.PendingMFAAudit.Validate(); err != nil {
			problems = append(problems, err)
		}
	}
	return errors.Join(problems...)
}

type AdminSession struct {
	IDHash            [32]byte  `json:"id_hash"`
	Username          string    `json:"username"`
	Generation        uint64    `json:"generation"`
	CreatedAt         time.Time `json:"created_at"`
	LastSeenAt        time.Time `json:"last_seen_at"`
	AbsoluteExpiresAt time.Time `json:"absolute_expires_at"`
	IdleExpiresAt     time.Time `json:"idle_expires_at"`
}

func (s AdminSession) Validate() error {
	var problems []error
	if s.IDHash == [32]byte{} || s.Username == "" || s.Generation == 0 {
		problems = append(problems, errors.New("admin session identity is invalid"))
	}
	if s.CreatedAt.IsZero() || s.LastSeenAt.IsZero() ||
		s.AbsoluteExpiresAt.IsZero() || s.IdleExpiresAt.IsZero() {
		problems = append(problems, errors.New("admin session timestamps are required"))
	}
	if !s.AbsoluteExpiresAt.After(s.CreatedAt) || !s.IdleExpiresAt.After(s.LastSeenAt) {
		problems = append(problems, errors.New("admin session expiry is invalid"))
	}
	return errors.Join(problems...)
}
