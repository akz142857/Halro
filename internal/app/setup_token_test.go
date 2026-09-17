package app

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGenerateSetupTokenFileIsFixedFormatExclusiveAndPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "setup-token")
	if err := GenerateSetupTokenFile(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode=%#o, want 0600", got)
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSuffix(payload, []byte("\n")), []byte("\n"))
	if len(lines) != 2 || !setupTokenPattern.Match(lines[0]) || !bytes.HasPrefix(lines[1], []byte("expires_at=")) {
		t.Fatalf("generated token has invalid wire format (length=%d)", len(payload))
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, strings.TrimPrefix(string(lines[1]), "expires_at="))
	if err != nil || !expiresAt.After(time.Now()) || expiresAt.After(time.Now().Add(setupTokenFileTTL+time.Minute)) {
		t.Fatalf("generated expiry=%v err=%v", expiresAt, err)
	}
	if err := GenerateSetupTokenFile(path); err == nil {
		t.Fatal("generator overwrote an existing secret")
	}
}

func TestReadSetupTokenFileIsBoundedAndStrict(t *testing.T) {
	valid, err := generateSetupToken()
	if err != nil {
		t.Fatal(err)
	}
	defer clear(valid)
	root := t.TempDir()
	expiresAt := time.Now().UTC().Add(20 * time.Minute).Truncate(time.Second)
	envelope := func(lineEnding string) []byte {
		return []byte(string(valid) + "\nexpires_at=" + expiresAt.Format(time.RFC3339Nano) + lineEnding)
	}
	tests := []struct {
		name    string
		payload []byte
		valid   bool
	}{
		{name: "lf", payload: envelope("\n"), valid: true},
		{name: "crlf", payload: envelope("\r\n"), valid: true},
		{name: "bare token", payload: append([]byte(nil), valid...)},
		{name: "empty", payload: nil},
		{name: "whitespace", payload: append(envelope(""), ' ')},
		{name: "extra line", payload: append(envelope("\n"), '\n')},
		{name: "oversized", payload: []byte(strings.Repeat("x", setupTokenInputLimit+32))},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(root, test.name)
			if err := os.WriteFile(path, test.payload, 0o600); err != nil {
				t.Fatal(err)
			}
			got, gotExpiry, err := readSetupTokenFile(path)
			defer clear(got)
			if test.valid && (err != nil || string(got) != string(valid) || !gotExpiry.Equal(expiresAt)) {
				t.Fatalf("token length=%d expiry=%v err=%v", len(got), gotExpiry, err)
			}
			if !test.valid && err == nil {
				t.Fatal("invalid token file was accepted")
			}
		})
	}
}

func TestSetupTokenFileExpirySurvivesProcessRestart(t *testing.T) {
	token, err := generateSetupToken()
	if err != nil {
		t.Fatal(err)
	}
	defer clear(token)
	expiredAt := time.Now().UTC().Add(-time.Minute)
	path := filepath.Join(t.TempDir(), "expired-setup-token")
	payload := []byte(string(token) + "\nexpires_at=" + expiredAt.Format(time.RFC3339Nano) + "\n")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	first, firstExpiry, err := readSetupTokenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	firstState := newSetupTokenState(setupTokenSourceFile, first, firstExpiry)
	second, secondExpiry, err := readSetupTokenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	secondState := newSetupTokenState(setupTokenSourceFile, second, secondExpiry)
	if time.Now().Before(firstState.expiresAt) || time.Now().Before(secondState.expiresAt) || !firstState.expiresAt.Equal(secondState.expiresAt) {
		t.Fatalf("restart extended expiry: first=%v second=%v", firstState.expiresAt, secondState.expiresAt)
	}
}
