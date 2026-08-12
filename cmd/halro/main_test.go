package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/app"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/hostsecurity"
	"github.com/akz142857/Halro/internal/masterkey"
	"gopkg.in/yaml.v3"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestRuntimeFailsClosedWhenHostHardeningFails(t *testing.T) {
	previous := hardenRuntimeCommand
	hardenRuntimeCommand = func() (hostsecurity.Report, error) {
		return hostsecurity.Report{}, errors.New("simulated host hardening failure")
	}
	t.Cleanup(func() { hardenRuntimeCommand = previous })
	err := runRuntime(config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)), false)
	if err == nil || !strings.Contains(err.Error(), "before Master Key unlock") {
		t.Fatalf("runRuntime error=%v", err)
	}
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
				Account: "123456789012", KeyID: "arn:aws:kms:ap-southeast-1:123456789012:key/11111111-1111-4111-8111-111111111111", Endpoint: "https://kms.invalid.example",
			},
			{
				Purpose: "recovery", Provider: "aws-kms", Region: "ap-southeast-2",
				Account: "210987654321", KeyID: "arn:aws:kms:ap-southeast-2:210987654321:key/22222222-2222-4222-8222-222222222222", Endpoint: "https://kms.invalid.example",
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

func TestKeySlotInitAndRecoveryCLIUseExplicitOfflinePaths(t *testing.T) {
	cfg := config.Default()
	cfg.Storage.DataDir = filepath.Join(t.TempDir(), "data")
	cfg.Storage.MasterKey = config.MasterKey{
		Mode: config.MasterKeyModeKeySlots, PrimarySlot: "slot_aws_primary", RecoverySlot: "slot_aws_recovery",
		StartupDeadline: config.Duration(time.Minute), CallTimeout: config.Duration(5 * time.Second),
		AllowedKMSKeys: []config.AllowedKMSKey{
			{Purpose: "primary", Provider: "aws-kms", Region: "ap-southeast-1", Account: "123456789012", KeyID: "arn:aws:kms:ap-southeast-1:123456789012:key/11111111-1111-4111-8111-111111111111"},
			{Purpose: "recovery", Provider: "aws-kms", Region: "ap-southeast-2", Account: "210987654321", KeyID: "arn:aws:kms:ap-southeast-2:210987654321:key/22222222-2222-4222-8222-222222222222"},
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
	previousInitialize := initializeCommand
	previousRecovery := verifyRecoveryCommand
	previousRotateKMS := rotateKMSCommand
	previousRewrapKMS := rewrapKMSCommand
	previousRevokeKMS := revokeKMSCommand
	previousInspectSlots := inspectKMSSlotsCommand
	previousDoctor := doctorCommand
	previousRestore := restoreBackupCommand
	t.Cleanup(func() {
		initializeCommand = previousInitialize
		verifyRecoveryCommand = previousRecovery
		rotateKMSCommand = previousRotateKMS
		rewrapKMSCommand = previousRewrapKMS
		revokeKMSCommand = previousRevokeKMS
		inspectKMSSlotsCommand = previousInspectSlots
		doctorCommand = previousDoctor
		restoreBackupCommand = previousRestore
	})
	initialized := false
	initializeCommand = func(loaded config.Config) error {
		initialized = loaded.Storage.MasterKey.Mode == config.MasterKeyModeKeySlots
		return nil
	}
	recovered := false
	verifyRecoveryCommand = func(_ context.Context, loaded config.Config, confirmed string) (app.RecoveryVerificationResult, error) {
		recovered = loaded.Storage.MasterKey.Mode == config.MasterKeyModeKeySlots && confirmed == loaded.Storage.MasterKey.RecoverySlot
		return app.RecoveryVerificationResult{SlotID: confirmed, VerifiedAt: time.Now().UTC()}, nil
	}
	rotated := false
	rotateKMSCommand = func(_ context.Context, loaded config.Config, operationID string) (app.KeyRotationResult, error) {
		rotated = loaded.Storage.MasterKey.Mode == config.MasterKeyModeKeySlots && operationID == "rotation-001"
		return app.KeyRotationResult{OldKeyVersion: 1, NewKeyVersion: 2}, nil
	}
	rewrapped := false
	rewrapKMSCommand = func(_ context.Context, loaded config.Config, options app.KMSRewrapOptions) (app.KMSRewrapResult, error) {
		rewrapped = loaded.Storage.MasterKey.Mode == config.MasterKeyModeKeySlots && options.Purpose == masterkey.KeySlotPrimary &&
			options.SlotID == "slot_aws_primary" && options.KeyReference == loaded.Storage.MasterKey.AllowedKMSKeys[0].KeyID
		return app.KMSRewrapResult{Purpose: options.Purpose, ActiveSlot: options.SlotID}, nil
	}
	revoked := false
	revokeKMSCommand = func(_ context.Context, loaded config.Config, options app.KMSRevokeOptions) (app.KMSRevokeResult, error) {
		revoked = loaded.Storage.MasterKey.Mode == config.MasterKeyModeKeySlots && options.SlotID == "slot_aws_primary_old" &&
			options.ConfirmSlotID == options.SlotID && options.ExpectedDescriptorRevision == 4 && options.ExpectedSlotRevision == 3
		return app.KMSRevokeResult{SlotID: options.SlotID, State: masterkey.KeySlotRevoked}, nil
	}
	inspected := false
	inspectKMSSlotsCommand = func(_ context.Context, loaded config.Config) (app.KMSKeySlotStatusResult, error) {
		inspected = loaded.Storage.MasterKey.Mode == config.MasterKeyModeKeySlots
		return app.KMSKeySlotStatusResult{DescriptorRevision: 4, DescriptorReady: true}, nil
	}
	staticDoctor := false
	doctorCommand = func(_ context.Context, loaded config.Config, options app.DoctorOptions) (app.DoctorReport, error) {
		staticDoctor = loaded.Storage.MasterKey.Mode == config.MasterKeyModeKeySlots && options.NoKMS
		return app.DoctorReport{VaultStatus: "vault_unverified"}, nil
	}
	recoveryRestore := false
	restoreBackupCommand = func(_ context.Context, loaded config.Config, _ string, key []byte, backupID string, options app.RestoreOptions) (app.RestoreResult, error) {
		recoveryRestore = loaded.Storage.MasterKey.Mode == config.MasterKeyModeKeySlots && len(key) == 32 && backupID == "backup_test" &&
			options.UseRecoverySlot && options.ConfirmRecoverySlot == loaded.Storage.MasterKey.RecoverySlot
		return app.RestoreResult{BackupID: backupID, UnlockPath: "recovery", VaultVerified: true, RecoveryAudited: true}, nil
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := run([]string{"init", "--config", path}, logger); err != nil {
		t.Fatal(err)
	}
	if !initialized {
		t.Fatal("init CLI did not select the Key Slot initializer")
	}
	if err := run([]string{"key", "recover", "--config", path, "--confirm-recovery-slot", "slot_aws_recovery"}, logger); err != nil {
		t.Fatal(err)
	}
	if !recovered {
		t.Fatal("recovery CLI did not use the explicit configured Recovery Slot")
	}
	if strings.Index(recoveryNextStepMessage, "rewrap") > strings.Index(recoveryNextStepMessage, "revoke") {
		t.Fatal("Recovery CLI tells the operator to revoke authorization before Primary repair")
	}
	if err := run([]string{"key", "slot", "status", "--config", path}, logger); err != nil {
		t.Fatal(err)
	}
	if !inspected {
		t.Fatal("Slot status CLI did not inspect key_slots metadata")
	}
	if err := run([]string{"key", "rotate", "--config", path, "--operation-id", "rotation-001"}, logger); err != nil {
		t.Fatal(err)
	}
	if !rotated {
		t.Fatal("rotate CLI did not preserve the explicit KMS operation ID")
	}
	if err := run([]string{"key", "rewrap", "--config", path, "--purpose", "primary", "--slot-id", "slot_aws_primary", "--key-reference", cfg.Storage.MasterKey.AllowedKMSKeys[0].KeyID}, logger); err != nil {
		t.Fatal(err)
	}
	if !rewrapped {
		t.Fatal("rewrap CLI did not preserve explicit Slot and allowlist selection")
	}
	if err := run([]string{"key", "slot", "revoke", "--config", path, "--slot-id", "slot_aws_primary_old", "--expected-descriptor-revision", "4", "--expected-slot-revision", "3", "--confirm-slot-id", "slot_aws_primary_old"}, logger); err != nil {
		t.Fatal(err)
	}
	if !revoked {
		t.Fatal("revoke CLI did not preserve confirmation and optimistic revisions")
	}
	if err := run([]string{"doctor", "--config", path, "--no-kms"}, logger); err != nil {
		t.Fatal(err)
	}
	if !staticDoctor {
		t.Fatal("doctor CLI did not preserve --no-kms")
	}
	backupKeyPath := filepath.Join(t.TempDir(), "backup.key")
	if err := os.WriteFile(backupKeyPath, make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"backup", "restore", "--config", path, "--file", filepath.Join(t.TempDir(), "backup.hmbk"),
		"--key-file", backupKeyPath, "--confirm-backup-id", "backup_test", "--use-recovery-slot", "--confirm-recovery-slot", "slot_aws_recovery"}, logger); err != nil {
		t.Fatal(err)
	}
	if !recoveryRestore {
		t.Fatal("restore CLI did not preserve explicit Recovery selection")
	}
}

func TestInitDoesNotValidateListenersItNeverBinds(t *testing.T) {
	cfg := config.Default()
	cfg.Storage.DataDir = filepath.Join(t.TempDir(), "data")
	cfg.Server.GatewayListen = "0.0.0.0:8080"
	contents, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	previous := initializeCommand
	t.Cleanup(func() { initializeCommand = previous })
	called := false
	initializeCommand = func(loaded config.Config) error {
		called = loaded.Server.GatewayListen == "0.0.0.0:8080"
		return nil
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := run([]string{"init", "--config", path}, logger); err != nil {
		t.Fatalf("offline init rejected a listener it does not bind: %v", err)
	}
	if !called {
		t.Fatal("init did not reach storage initialization")
	}
	if _, err := config.Load(path, config.LoadOptions{}); err == nil || !strings.Contains(err.Error(), "gateway_listen") {
		t.Fatalf("runtime validation no longer rejects public plaintext Gateway: %v", err)
	}
}

func TestRestoreStatusReportsSchemaMigration(t *testing.T) {
	var output strings.Builder
	writeRestoreStatus(&output, app.RestoreResult{SchemaVersionBefore: 23, SchemaVersionAfter: 27})
	for _, expected := range []string{"schema migrated from v23 to v27", "previous data directory was preserved"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("restore status %q does not contain %q", output.String(), expected)
		}
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
