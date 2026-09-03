package auth

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/akz142857/Halro/internal/domain"
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
	byID     map[string]domain.GatewayKey
	projects map[string]domain.Project
}

type Snapshot struct {
	// refreshMu serializes Refresh end to end, so the store read and the
	// install travel together: the snapshot installed last is always the
	// snapshot read last. Without it, two concurrent refreshes — the
	// background activation-recovery loop and an admin mutation's own
	// refresh — race last-writer-wins, and the loser's older read can land
	// after the winner's newer one, silently restoring a Gateway Key the
	// operator just watched being revoked. Authenticate stays lock-free:
	// it only ever loads the pointer.
	refreshMu sync.Mutex
	current   atomic.Pointer[snapshotData]
}

func NewSnapshot() *Snapshot {
	snapshot := &Snapshot{}
	snapshot.current.Store(&snapshotData{
		keys:     make(map[[32]byte]domain.GatewayKey),
		byID:     make(map[string]domain.GatewayKey),
		projects: make(map[string]domain.Project),
	})
	return snapshot
}

func (s *Snapshot) Refresh(ctx context.Context, source SnapshotSource) error {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
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
		byID:     make(map[string]domain.GatewayKey, len(keys)),
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
			next.byID[key.ID] = key
		}
	}
	s.current.Store(next)
	return nil
}

// AuthorizeKeyID answers the same question as Authenticate for a request whose
// plaintext key is long gone.
//
// Work that was queued on one connection and runs on another cannot present the
// key again — and must not, because storing a Gateway Key to replay it later
// would be a far worse thing than anything it enables. The identifier is enough
// to ask whether the key is still one this instance would accept, which is the
// question that matters at the moment the work actually reaches an upstream:
// revoking a key has to stop the work it authorised, not only the work it has
// not yet submitted.
func (s *Snapshot) AuthorizeKeyID(keyID string, now time.Time) (AuthResult, error) {
	if keyID == "" {
		return AuthResult{}, ErrInvalidKey
	}
	current := s.current.Load()
	key, ok := current.byID[keyID]
	if !ok {
		return AuthResult{}, ErrInvalidKey
	}
	return authorize(current, key, now)
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
	return authorize(current, key, now)
}

func authorize(current *snapshotData, key domain.GatewayKey, now time.Time) (AuthResult, error) {
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
