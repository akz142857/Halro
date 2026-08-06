package adminauth

import (
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Heimdall/internal/domain"
)

func TestArgon2IDPasswordVerification(t *testing.T) {
	password := []byte("correct horse battery staple")
	user, err := NewUser("admin", password, domain.AdminRoleAdministrator, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyPassword(user, password) {
		t.Fatal("correct password was rejected")
	}
	if VerifyPassword(user, []byte("wrong password value")) {
		t.Fatal("wrong password was accepted")
	}
	if PasswordNeedsUpgrade(user) {
		t.Fatal("new password unexpectedly needs upgrade")
	}
}

func TestPasswordMinimumUsesUnicodeCharacters(t *testing.T) {
	if _, err := NewUser("admin", []byte(strings.Repeat("密", 7)), domain.AdminRoleAdministrator, time.Now().UTC()); err == nil {
		t.Fatal("seven-character password was accepted")
	}
	password := []byte(strings.Repeat("密", 8))
	user, err := NewUser("admin", password, domain.AdminRoleAdministrator, time.Now().UTC())
	if err != nil {
		t.Fatalf("eight-character Unicode password was rejected: %v", err)
	}
	if !VerifyPassword(user, password) {
		t.Fatal("accepted Unicode password could not be verified")
	}
}
