package config

import (
	"errors"
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
  master_key:
    mode: file
    file: "./master.key"
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

func TestKeySlotsConfigurationIsValidatedStatically(t *testing.T) {
	keySlots := strings.Replace(validConfig, `  master_key:
    mode: file
    file: "./master.key"`, `  master_key:
    mode: key_slots
    primary_slot: slot_aws_primary
    recovery_slot: slot_aws_recovery
    startup_deadline: 60s
    call_timeout: 5s
    allowed_kms_keys:
      - purpose: primary
        provider: aws-kms
        region: ap-southeast-1
        account: "123456789012"
        key_id: arn:aws:kms:ap-southeast-1:123456789012:key/primary
        endpoint: https://kms.invalid.example
      - purpose: recovery
        provider: aws-kms
        region: ap-southeast-2
        account: "210987654321"
        key_id: arn:aws:kms:ap-southeast-2:210987654321:key/recovery`, 1)
	cfg, err := Decode(strings.NewReader(keySlots))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(LoadOptions{}); err != nil {
		t.Fatalf("valid key_slots configuration was rejected: %v", err)
	}
}

func TestMasterKeyConfigurationRejectsModeMixing(t *testing.T) {
	cfg, err := Decode(strings.NewReader(strings.Replace(validConfig, `    file: "./master.key"`, `    file: "./master.key"
    primary_slot: forbidden`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(LoadOptions{}); err == nil || !strings.Contains(err.Error(), "key_slots fields cannot be set in file mode") {
		t.Fatalf("expected mode-mixing error, got %v", err)
	}
}

func TestDecodeRejectsLegacyMasterKeyFile(t *testing.T) {
	legacy := strings.Replace(validConfig, `  master_key:
    mode: file
    file: "./master.key"`, `  master_key_file: "./master.key"`, 1)
	if _, err := Decode(strings.NewReader(legacy)); err == nil {
		t.Fatal("legacy storage.master_key_file was accepted")
	}
}

func TestKeySlotsConfigurationRejectsUnsafeCombinations(t *testing.T) {
	valid := MasterKey{
		Mode:            MasterKeyModeKeySlots,
		PrimarySlot:     "slot_primary",
		RecoverySlot:    "slot_recovery",
		StartupDeadline: Duration(time.Minute),
		CallTimeout:     Duration(5 * time.Second),
		AllowedKMSKeys: []AllowedKMSKey{
			{Purpose: "primary", Provider: "aws-kms", Region: "ap-southeast-1", Account: "123456789012", KeyID: "arn:aws:kms:ap-southeast-1:123456789012:key/11111111-1111-4111-8111-111111111111"},
			{Purpose: "recovery", Provider: "aws-kms", Region: "ap-southeast-2", Account: "210987654321", KeyID: "arn:aws:kms:ap-southeast-2:210987654321:key/22222222-2222-4222-8222-222222222222"},
		},
	}
	tests := []struct {
		name   string
		mutate func(*MasterKey)
		want   string
	}{
		{
			name: "same slot", mutate: func(value *MasterKey) { value.RecoverySlot = value.PrimarySlot },
			want: "primary_slot and recovery_slot must be different",
		},
		{
			name: "call timeout reaches total deadline", mutate: func(value *MasterKey) { value.CallTimeout = value.StartupDeadline },
			want: "call_timeout must be positive and less than startup_deadline",
		},
		{
			name: "same KMS key", mutate: func(value *MasterKey) {
				value.AllowedKMSKeys[1] = value.AllowedKMSKeys[0]
				value.AllowedKMSKeys[1].Purpose = "recovery"
			},
			want: "primary and recovery allowlists must not use the same KMS key",
		},
		{
			name: "endpoint query", mutate: func(value *MasterKey) {
				value.AllowedKMSKeys[0].Endpoint = "https://kms.example.test?credential=forbidden"
			},
			want: "endpoint must be an HTTPS origin without userinfo, path, query, or fragment",
		},
		{
			name: "unknown provider", mutate: func(value *MasterKey) { value.AllowedKMSKeys[0].Provider = "future-kms" },
			want: "provider is not available in this release",
		},
		{
			name: "account mismatch", mutate: func(value *MasterKey) { value.AllowedKMSKeys[0].Account = "999999999999" },
			want: "full KMS Key ARN matching region and account",
		},
		{
			name: "region mismatch", mutate: func(value *MasterKey) { value.AllowedKMSKeys[0].Region = "us-east-1" },
			want: "full KMS Key ARN matching region and account",
		},
		{
			name: "alias not key", mutate: func(value *MasterKey) {
				value.AllowedKMSKeys[0].KeyID = "arn:aws:kms:ap-southeast-1:123456789012:alias/primary"
			},
			want: "full KMS Key ARN matching region and account",
		},
		{
			name: "asymmetric algorithm", mutate: func(value *MasterKey) { value.AllowedKMSKeys[0].Algorithm = "RSAES_OAEP_SHA_256" },
			want: "algorithm must be SYMMETRIC_DEFAULT",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := valid
			candidate.AllowedKMSKeys = append([]AllowedKMSKey(nil), valid.AllowedKMSKeys...)
			test.mutate(&candidate)
			err := errors.Join(validateMasterKey(candidate)...)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected %q, got %v", test.want, err)
			}
		})
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

func TestMetricsNonLoopbackRequiresDedicatedMutualTLS(t *testing.T) {
	cfg, err := Decode(strings.NewReader(strings.Replace(validConfig, `127.0.0.1:9090`, `0.0.0.0:9090`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(LoadOptions{}); err == nil {
		t.Fatal("non-loopback plaintext Metrics listener was accepted")
	}
	cfg.TLS.Enabled = true
	cfg.TLS.CertFile = "gateway.crt"
	cfg.TLS.KeyFile = "gateway.key"
	cfg.Metrics.CredentialFile = "metrics-credentials.json"
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(LoadOptions{}); err == nil {
		t.Fatal("global server-only TLS was accepted as Metrics mutual identity")
	}
	cfg.Metrics.TLS = MetricsTLS{
		Enabled: true, CertFile: "metrics.crt", KeyFile: "metrics.key", ClientCAFile: "metrics-ca.crt",
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(LoadOptions{}); err != nil {
		t.Fatalf("dedicated Metrics mTLS was rejected: %v", err)
	}
	cfg.Metrics.TLS.ClientCAFile = ""
	if err := cfg.Validate(LoadOptions{}); err == nil {
		t.Fatal("Metrics TLS without a client CA was accepted")
	}
}
