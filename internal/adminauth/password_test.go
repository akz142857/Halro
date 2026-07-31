package adminauth

import (
	"testing"
	"time"
)

func TestArgon2IDPasswordVerification(t *testing.T) {
	password := []byte("correct horse battery staple")
	user, err := NewUser("admin", password, time.Now().UTC())
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
