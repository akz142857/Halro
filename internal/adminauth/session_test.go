package adminauth

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Heimdall/internal/domain"
	boltstore "github.com/akz142857/Heimdall/internal/store/bolt"
)

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
