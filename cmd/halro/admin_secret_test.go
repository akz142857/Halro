package main

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/app"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/hostsecurity"
	"gopkg.in/yaml.v3"
)

type panicReader struct{}

func (panicReader) Read([]byte) (int, error) {
	panic("stdin must not be read when --password-file is set")
}

func TestAdminBootstrapResultLineUsesStableStatus(t *testing.T) {
	if got, want := adminBootstrapResultLine(app.BootstrapAdminCreated, "install-production-20260917"), "Admin bootstrap result: created (operation_id=install-production-20260917)"; got != want {
		t.Fatalf("result line=%q, want %q", got, want)
	}
	if got, want := adminBootstrapResultLine(app.BootstrapAdminAlreadyCompleted, "install-production-20260917"), "Admin bootstrap result: already_completed (operation_id=install-production-20260917)"; got != want {
		t.Fatalf("result line=%q, want %q", got, want)
	}
}

func TestReadPasswordInputFromFileDoesNotProbeStdin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admin-password")
	if err := os.WriteFile(path, []byte(" leading and trailing spaces \r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	password, err := readPasswordInput(panicReader{}, path)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(password)
	if got, want := string(password), " leading and trailing spaces "; got != want {
		t.Fatalf("password=%q, want %q", got, want)
	}
}

func TestReadPasswordInputSupportsStdinAndRejectsUnsafeInputs(t *testing.T) {
	password, err := readPasswordInput(strings.NewReader("correct horse battery staple\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer clear(password)
	if string(password) != "correct horse battery staple" {
		t.Fatalf("password=%q", password)
	}
	for _, input := range []string{"legacy password\n\n", "legacy password\r"} {
		legacy, err := readPasswordInput(strings.NewReader(input), "")
		if err != nil || string(legacy) != "legacy password" {
			t.Fatalf("legacy stdin input=%q password=%q err=%v", input, legacy, err)
		}
		clear(legacy)
	}
	maximum := append(bytes.Repeat([]byte("x"), adminPasswordInputLimit), '\n')
	accepted, err := readPasswordInput(bytes.NewReader(maximum), "")
	if err != nil || len(accepted) != adminPasswordInputLimit {
		t.Fatalf("maximum password length=%d err=%v", len(accepted), err)
	}
	clear(accepted)

	for _, test := range []struct {
		name string
		in   []byte
	}{
		{name: "empty", in: nil},
		{name: "newline only", in: []byte("\n")},
		{name: "too long", in: bytes.Repeat([]byte("x"), adminPasswordInputLimit+1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if secret, err := readPasswordInput(bytes.NewReader(test.in), ""); err == nil {
				clear(secret)
				t.Fatal("unsafe password input was accepted")
			}
		})
	}
}

func TestSetupTokenGeneratorHardensProcessBeforeCreatingSecret(t *testing.T) {
	previous := hardenRuntimeCommand
	called := false
	hardenRuntimeCommand = func() (hostsecurity.Report, error) {
		called = true
		return hostsecurity.Report{}, errors.New("simulated hardening failure")
	}
	t.Cleanup(func() { hardenRuntimeCommand = previous })
	path := filepath.Join(t.TempDir(), "setup-token")
	err := run([]string{"admin", "setup-token", "generate", "--output", path}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil || !called {
		t.Fatalf("generator err=%v hardening_called=%v", err, called)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("secret file exists after hardening failure: %v", statErr)
	}
}

func TestAdminBootstrapCLIFileWorkflowIsIdempotent(t *testing.T) {
	previous := hardenRuntimeCommand
	hardenRuntimeCommand = func() (hostsecurity.Report, error) { return hostsecurity.Report{}, nil }
	t.Cleanup(func() { hardenRuntimeCommand = previous })

	root := t.TempDir()
	cfg := config.Default()
	cfg.Storage.DataDir = filepath.Join(root, "data")
	cfg.Storage.MasterKey.File = filepath.Join(root, "master.key")
	payload, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(configPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	passwordPath := filepath.Join(root, "admin-password")
	if err := os.WriteFile(passwordPath, []byte("correct horse battery staple\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := run([]string{"init", "--if-needed", "--config", configPath}, logger); err != nil {
		t.Fatal(err)
	}
	arguments := []string{
		"admin", "bootstrap", "--if-needed", "--operation-id", "install-cli-test",
		"--config", configPath, "--username", "admin", "--password-file", passwordPath,
	}
	if err := run(arguments, logger); err != nil {
		t.Fatalf("created: %v", err)
	}
	if err := run(arguments, logger); err != nil {
		t.Fatalf("already completed: %v", err)
	}
	if err := run([]string{
		"admin", "bootstrap", "--if-needed", "--config", configPath,
		"--username", "admin", "--password-file", passwordPath,
	}, logger); err == nil || !strings.Contains(err.Error(), "--operation-id") {
		t.Fatalf("missing operation id error=%v", err)
	}
}

func TestReadPasswordInputErrorsDoNotDiscloseFilePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sensitive-name")
	_, err := readPasswordInput(strings.NewReader("unused"), path)
	if err == nil {
		t.Fatal("missing file was accepted")
	}
	if strings.Contains(err.Error(), path) || strings.Contains(err.Error(), "sensitive-name") {
		t.Fatalf("error disclosed password file path: %v", err)
	}
	if _, err := readPasswordInput(strings.NewReader("unused"), "relative-password"); err == nil {
		t.Fatal("relative password file was accepted")
	}
}
