package config

import (
	"strings"
	"testing"
	"time"
)

const validConfig = `
version: 1
server:
  gateway_listen: "127.0.0.1:8080"
  admin_listen: "127.0.0.1:8081"
  metrics_listen: "127.0.0.1:9090"
  read_header_timeout: 5s
  read_body_timeout: 15s
  max_header_bytes: 32768
  max_request_bytes: 10485760
tls:
  enabled: false
  cert_file: ""
  key_file: ""
storage:
  data_dir: "./data"
  metadata_file: "heimdall.db"
  master_key_file: "./master.key"
usage:
  durability: balanced
  timezone: UTC
gateway:
  route_total_timeout: 120s
  attempt_connect_timeout: 5s
  attempt_response_header_timeout: 60s
  stream_idle_timeout: 60s
  downstream_write_timeout: 15s
  stream_max_duration: 10m
  max_total_attempts: 3
security:
  allow_private_provider_endpoints: false
  allow_private_webhooks: false
  trust_proxy_headers: false
  trusted_proxy_cidrs: []
metrics:
  enabled: true
  require_auth: true
`

func TestDecodeAndValidate(t *testing.T) {
	cfg, err := Decode(strings.NewReader(validConfig))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	if cfg.Gateway.StreamMaxDuration.Value() != 10*time.Minute {
		t.Fatalf("unexpected stream duration: %s", cfg.Gateway.StreamMaxDuration.Value())
	}
}

func TestDecodeRejectsUnknownFields(t *testing.T) {
	_, err := Decode(strings.NewReader(validConfig + "\nunknown: true\n"))
	if err == nil {
		t.Fatal("expected unknown field error")
	}
}

func TestPublicPlaintextListenerPolicy(t *testing.T) {
	cfg, err := Decode(strings.NewReader(strings.Replace(validConfig, `127.0.0.1:8080`, `0.0.0.0:8080`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(LoadOptions{}); err == nil {
		t.Fatal("expected public plaintext gateway to fail")
	}
	if err := cfg.Validate(LoadOptions{AllowInsecurePublicGateway: true}); err != nil {
		t.Fatalf("gateway override should be allowed: %v", err)
	}
}

func TestPublicAdminCannotUseGatewayOverride(t *testing.T) {
	cfg, err := Decode(strings.NewReader(strings.Replace(validConfig, `127.0.0.1:8081`, `0.0.0.0:8081`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(LoadOptions{AllowInsecurePublicGateway: true}); err == nil {
		t.Fatal("public plaintext admin must always fail")
	}
}
