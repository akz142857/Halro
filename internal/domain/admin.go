package domain

import (
	"errors"
	"strings"
	"time"
)

// Admin roles are deliberately just two: AdminRoleAdministrator (every
// existing capability) and AdminRoleReadOnly (GET only, no exceptions per
// endpoint). A per-endpoint permission matrix was considered and rejected —
// see docs/review/progress.md's P2-23 record — in favor of one rule the
// mutation middleware can enforce without a maintained list that a new
// write endpoint could silently fall outside of.
const (
	AdminRoleAdministrator = "administrator"
	AdminRoleReadOnly      = "read_only"
)

func ValidAdminRole(role string) bool {
	return role == AdminRoleAdministrator || role == AdminRoleReadOnly
}

type AdminUser struct {
	Username          string               `json:"username"`
	Role              string               `json:"role"`
	Locale            string               `json:"locale,omitempty"`
	Appearance        string               `json:"appearance,omitempty"`
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
	if !ValidAdminRole(u.Role) {
		problems = append(problems, errors.New("admin role must be administrator or read_only"))
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
	// Empty appearance is permitted for legacy records (read as dark); any
	// stored non-empty value must be a supported appearance.
	if u.Appearance != "" && !IsSupportedAppearance(u.Appearance) {
		problems = append(problems, errors.New("admin appearance preference is not supported"))
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
