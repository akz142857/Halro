package adminauth

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

type blockedRefreshStore struct {
	*boltstore.Store
	entered chan struct{}
	resume  chan struct{}
	once    sync.Once
}

func (s *blockedRefreshStore) RefreshAdminSession(
	ctx context.Context,
	observed domain.AdminSession,
	refreshed domain.AdminSession,
	now time.Time,
) (domain.AdminSession, bool, error) {
	s.once.Do(func() { close(s.entered) })
	select {
	case <-s.resume:
	case <-ctx.Done():
		return domain.AdminSession{}, false, ctx.Err()
	}
	return s.Store.RefreshAdminSession(ctx, observed, refreshed, now)
}

func TestSessionHashPersistenceCSRFAndExpiry(t *testing.T) {
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	user, err := NewUser("admin", []byte("correct horse battery staple"), domain.AdminRoleAdministrator, now)
	if err != nil {
		t.Fatal(err)
	}
	user, err = store.PutAdminUser(context.Background(), user, 0)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(store, make([]byte, 32), time.Hour, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	created, err := manager.Create(context.Background(), user, now)
	if err != nil {
		t.Fatal(err)
	}
	if created.Token == "" || created.CSRFToken == "" ||
		!manager.VerifyCSRF(created.Token, created.CSRFToken) ||
		manager.VerifyCSRF(created.Token, "wrong") {
		t.Fatal("invalid session or CSRF token behavior")
	}
	unrefreshed, err := manager.Authenticate(context.Background(), created.Token, now.Add(30*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !unrefreshed.LastSeenAt.Equal(created.Session.LastSeenAt) ||
		!unrefreshed.IdleExpiresAt.Equal(created.Session.IdleExpiresAt) {
		t.Fatal("session was persisted before the refresh interval elapsed")
	}
	if _, err := manager.Authenticate(context.Background(), created.Token, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Authenticate(
		context.Background(), created.Token, now.Add(time.Hour),
	); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("expired session was accepted: %v", err)
	}
}

func TestSessionRefreshCannotRecreateRevokedSession(t *testing.T) {
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	user, err := NewUser("admin", []byte("correct horse battery staple"), domain.AdminRoleAdministrator, now)
	if err != nil {
		t.Fatal(err)
	}
	user, err = store.PutAdminUser(context.Background(), user, 0)
	if err != nil {
		t.Fatal(err)
	}
	gate := &blockedRefreshStore{Store: store, entered: make(chan struct{}), resume: make(chan struct{})}
	manager, err := NewManager(gate, make([]byte, 32), time.Hour, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	created, err := manager.Create(context.Background(), user, now)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, authenticateErr := manager.Authenticate(context.Background(), created.Token, now.Add(3*time.Minute))
		done <- authenticateErr
	}()
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not reach the transaction boundary")
	}
	if err := manager.Revoke(context.Background(), created.Token); err != nil {
		t.Fatal(err)
	}
	close(gate.resume)
	select {
	case err := <-done:
		if !errors.Is(err, ErrInvalidSession) {
			t.Fatalf("delayed refresh error = %v, want ErrInvalidSession", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("delayed refresh did not finish")
	}
	if _, err := store.GetAdminSession(context.Background(), created.Session.IDHash); !errors.Is(err, boltstore.ErrNotFound) {
		t.Fatalf("revoked session was recreated: %v", err)
	}
}
