package adminauth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

type memoryUserReader struct{ user domain.AdminUser }

func (r memoryUserReader) GetAdminUser(_ context.Context, username string) (domain.AdminUser, error) {
	if username != r.user.Username {
		return domain.AdminUser{}, errors.New("not found")
	}
	return r.user, nil
}

func TestMemorySessionStoreCreatesRefreshesAndRevokesWithoutAuthoritativeWrites(t *testing.T) {
	user := domain.AdminUser{Username: "admin", SessionGeneration: 3}
	store, err := NewMemorySessionStore(memoryUserReader{user: user})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(store, []byte("0123456789abcdef0123456789abcdef"), time.Hour, 20*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	now := time.Now().UTC()
	created, err := manager.Create(context.Background(), user, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Authenticate(context.Background(), created.Token, now.Add(6*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := manager.Revoke(context.Background(), created.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Authenticate(context.Background(), created.Token, now.Add(7*time.Minute)); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("revoked session error=%v", err)
	}
}
