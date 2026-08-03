package main

import (
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Heimdall/internal/config"
	"gopkg.in/yaml.v3"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestConfigCheckValidatesKeySlotsWithoutCallingKMS(t *testing.T) {
	cfg := config.Default()
	cfg.Storage.MasterKey = config.MasterKey{
		Mode:            config.MasterKeyModeKeySlots,
		PrimarySlot:     "slot_aws_primary",
		RecoverySlot:    "slot_aws_recovery",
		StartupDeadline: config.Duration(time.Minute),
		CallTimeout:     config.Duration(5 * time.Second),
		AllowedKMSKeys: []config.AllowedKMSKey{
			{
				Purpose: "primary", Provider: "aws-kms", Region: "ap-southeast-1",
				Account: "123456789012", KeyID: "primary", Endpoint: "https://kms.invalid.example",
			},
			{
				Purpose: "recovery", Provider: "aws-kms", Region: "ap-southeast-2",
				Account: "210987654321", KeyID: "recovery", Endpoint: "https://kms.invalid.example",
			},
		},
	}
	contents, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := run([]string{"config", "check", "--config", path}, logger); err != nil {
		t.Fatalf("static config check attempted runtime behavior or rejected valid key_slots config: %v", err)
	}
}

func TestHealthcheckAcceptsOnlySuccessfulLoopbackReadiness(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		status := http.StatusServiceUnavailable
		if request.URL.Path == "/ready" {
			status = http.StatusOK
		} else if request.URL.Path == "/redirect" {
			status = http.StatusFound
		}
		return &http.Response{
			StatusCode: status,
			Body:       io.NopCloser(strings.NewReader("health")),
			Header:     make(http.Header),
			Request:    request,
		}, nil
	})}
	if err := runHealthcheckWithClient("http://127.0.0.1:8080/ready", time.Second, client); err != nil {
		t.Fatal(err)
	}
	if err := runHealthcheckWithClient("http://127.0.0.1:8080/unready", time.Second, client); err == nil {
		t.Fatal("unready endpoint passed healthcheck")
	}
	if err := runHealthcheckWithClient("http://127.0.0.1:8080/redirect", time.Second, client); err == nil {
		t.Fatal("redirect passed healthcheck")
	}
	if err := runHealthcheckWithClient("https://example.com/health/ready", time.Second, client); err == nil {
		t.Fatal("non-loopback healthcheck URL was accepted")
	}
	if err := runHealthcheckWithClient("http://127.0.0.1:8080/ready?token=secret", time.Second, client); err == nil {
		t.Fatal("healthcheck URL with query was accepted")
	}
}
