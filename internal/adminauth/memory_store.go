package adminauth

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

type AdminUserReader interface {
	GetAdminUser(context.Context, string) (domain.AdminUser, error)
}

// MemorySessionStore keeps Replica Admin sessions node-local. User identity,
// password hashes and session generation are still read from the replicated
// metadata projection, while login/logout/refresh never mutate that projection.
type MemorySessionStore struct {
	users    AdminUserReader
	mu       sync.Mutex
	sessions map[[32]byte]domain.AdminSession
}

func NewMemorySessionStore(users AdminUserReader) (*MemorySessionStore, error) {
	if users == nil {
		return nil, errors.New("memory session store requires an Admin user reader")
	}
	return &MemorySessionStore{users: users, sessions: make(map[[32]byte]domain.AdminSession)}, nil
}

func (s *MemorySessionStore) GetAdminUser(ctx context.Context, username string) (domain.AdminUser, error) {
	return s.users.GetAdminUser(ctx, username)
}

func (s *MemorySessionStore) PutAdminSession(ctx context.Context, session domain.AdminSession) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := session.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[session.IDHash] = session
	return nil
}

func (s *MemorySessionStore) GetAdminSession(ctx context.Context, hash [32]byte) (domain.AdminSession, error) {
	if err := ctx.Err(); err != nil {
		return domain.AdminSession{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[hash]
	if !ok {
		return domain.AdminSession{}, ErrInvalidSession
	}
	return session, nil
}

func (s *MemorySessionStore) RefreshAdminSession(ctx context.Context, original, replacement domain.AdminSession, now time.Time) (domain.AdminSession, bool, error) {
	if err := ctx.Err(); err != nil {
		return domain.AdminSession{}, false, err
	}
	if err := replacement.Validate(); err != nil {
		return domain.AdminSession{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.sessions[original.IDHash]
	if !ok || current != original || !now.Before(current.AbsoluteExpiresAt) || !now.Before(current.IdleExpiresAt) {
		return domain.AdminSession{}, false, nil
	}
	s.sessions[replacement.IDHash] = replacement
	return replacement, true, nil
}

func (s *MemorySessionStore) DeleteAdminSession(ctx context.Context, hash [32]byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, hash)
	return nil
}

func (s *MemorySessionStore) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.sessions)
}
