package auth

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/akz142857/Heimdall/internal/domain"
)

var (
	ErrInvalidKey      = errors.New("invalid gateway key")
	ErrKeyDisabled     = errors.New("gateway key is disabled")
	ErrKeyExpired      = errors.New("gateway key is expired")
	ErrProjectDisabled = errors.New("project is disabled")
)

type SnapshotSource interface {
	ListGatewayKeys(context.Context) ([]domain.GatewayKey, error)
	ListProjects(context.Context) ([]domain.Project, error)
}

type AuthResult struct {
	Key     domain.GatewayKey
	Project domain.Project
}

type snapshotData struct {
	keys     map[[32]byte]domain.GatewayKey
	projects map[string]domain.Project
}

type Snapshot struct {
	current atomic.Pointer[snapshotData]
}

func NewSnapshot() *Snapshot {
	snapshot := &Snapshot{}
	snapshot.current.Store(&snapshotData{
		keys:     make(map[[32]byte]domain.GatewayKey),
		projects: make(map[string]domain.Project),
	})
	return snapshot
}

func (s *Snapshot) Refresh(ctx context.Context, source SnapshotSource) error {
	projects, err := source.ListProjects(ctx)
	if err != nil {
		return err
	}
	keys, err := source.ListGatewayKeys(ctx)
	if err != nil {
		return err
	}
	next := &snapshotData{
		keys:     make(map[[32]byte]domain.GatewayKey, len(keys)),
		projects: make(map[string]domain.Project, len(projects)),
	}
	for _, project := range projects {
		if project.DeletedAt == nil {
			next.projects[project.ID] = project
		}
	}
	for _, key := range keys {
		if key.DeletedAt == nil {
			next.keys[key.KeyHash] = key
		}
	}
	s.current.Store(next)
	return nil
}

func (s *Snapshot) Authenticate(plaintext string, now time.Time) (AuthResult, error) {
	if err := ValidateGatewayKeyFormat(plaintext); err != nil {
		return AuthResult{}, ErrInvalidKey
	}
	hash := HashGatewayKey(plaintext)
	current := s.current.Load()
	key, ok := current.keys[hash]
	if !ok || !ConstantTimeHashMatch(key.KeyHash, hash) {
		return AuthResult{}, ErrInvalidKey
	}
	if !key.Enabled {
		return AuthResult{}, ErrKeyDisabled
	}
	if key.ExpiresAt != nil && !now.Before(*key.ExpiresAt) {
		return AuthResult{}, ErrKeyExpired
	}
	project, ok := current.projects[key.ProjectID]
	if !ok || !project.Enabled {
		return AuthResult{}, ErrProjectDisabled
	}
	return AuthResult{Key: key, Project: project}, nil
}
