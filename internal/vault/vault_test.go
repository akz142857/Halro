package vault

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
)

func TestCredentialRoundTripAndAudienceBinding(t *testing.T) {
	master := make([]byte, MasterKeySize)
	if _, err := rand.Read(master); err != nil {
		t.Fatal(err)
	}
	vault, err := New(master)
	clear(master)
	if err != nil {
		t.Fatal(err)
	}
	defer vault.Close()

	secret := []byte("provider-secret-canary")
	envelope, err := vault.EncryptCredential("cred_1", "openai", "https://api.openai.com:443", secret)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(envelope, secret) {
		t.Fatal("ciphertext contains plaintext")
	}
	plaintext, err := vault.DecryptCredential("cred_1", "openai", "https://api.openai.com:443", envelope)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(plaintext)
	if !bytes.Equal(plaintext, secret) {
		t.Fatalf("unexpected plaintext: %q", plaintext)
	}
	if _, err := vault.DecryptCredential("cred_1", "openai", "https://evil.example:443", envelope); err == nil {
		t.Fatal("audience rebind must fail authentication")
	}
}

func TestCredentialTamperFails(t *testing.T) {
	master := bytes.Repeat([]byte{7}, MasterKeySize)
	vault, err := New(master)
	if err != nil {
		t.Fatal(err)
	}
	defer vault.Close()
	envelope, err := vault.EncryptCredential("cred_1", "openai", "https://api.openai.com:443", []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	envelope[len(envelope)-1] ^= 0xff
	if _, err := vault.DecryptCredential("cred_1", "openai", "https://api.openai.com:443", envelope); err == nil {
		t.Fatal("tampered ciphertext must fail")
	}
}

func TestMasterKeyLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.key")
	if err := InitMasterKey(path); err != nil {
		t.Fatal(err)
	}
	if err := InitMasterKey(path); err == nil {
		t.Fatal("init must not overwrite an existing key")
	}
	key, err := LoadMasterKey(path)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	if len(key) != MasterKeySize {
		t.Fatalf("unexpected key size: %d", len(key))
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMasterKey(path); err == nil {
		t.Fatal("weak permissions must fail")
	}
}

func TestMasterKeyRejectsSymlink(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	if err := os.WriteFile(target, bytes.Repeat([]byte{1}, MasterKeySize), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMasterKey(link); err == nil {
		t.Fatal("symlink master key must fail")
	}
}
