package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Heimdall/internal/audit"
	backuppkg "github.com/akz142857/Heimdall/internal/backup"
	"github.com/akz142857/Heimdall/internal/config"
	"github.com/akz142857/Heimdall/internal/domain"
	corekms "github.com/akz142857/Heimdall/internal/kms"
	"github.com/akz142857/Heimdall/internal/kms/awskms"
	"github.com/akz142857/Heimdall/internal/kms/fakekms"
	"github.com/akz142857/Heimdall/internal/masterkey"
	boltstore "github.com/akz142857/Heimdall/internal/store/bolt"
	"github.com/akz142857/Heimdall/internal/vault"
)

type awsAliasWrapper struct{ delegate *fakekms.Wrapper }

func (w awsAliasWrapper) Provider() string { return awskms.Provider }
func (w awsAliasWrapper) Wrap(ctx context.Context, request corekms.WrapRequest) (corekms.WrapResult, error) {
	return w.delegate.Wrap(ctx, request)
}
func (w awsAliasWrapper) Unwrap(ctx context.Context, request corekms.UnwrapRequest) (corekms.UnwrapResult, error) {
	return w.delegate.Unwrap(ctx, request)
}

type kmsAppHarness struct {
	wrappers map[string]*fakekms.Wrapper
}

func newKMSAppHarness(t *testing.T) *kmsAppHarness {
	t.Helper()
	harness := &kmsAppHarness{wrappers: make(map[string]*fakekms.Wrapper)}
	for index, keyARN := range []string{primaryKMSKeyARN, recoveryKMSKeyARN} {
		wrapper, err := fakekms.New(bytes.Repeat([]byte{byte(index + 1)}, 32))
		if err != nil {
			t.Fatal(err)
		}
		harness.wrappers[keyARN] = wrapper
	}
	return harness
}

func (h *kmsAppHarness) factory(_ context.Context, allowed config.AllowedKMSKey) (corekms.Wrapper, error) {
	wrapper := h.wrappers[allowed.KeyID]
	if wrapper == nil {
		return nil, errors.New("test KMS key is not configured")
	}
	return awsAliasWrapper{delegate: wrapper}, nil
}

func (h *kmsAppHarness) callCount() int {
	total := 0
	for _, wrapper := range h.wrappers {
		total += len(wrapper.Calls())
	}
	return total
}

const (
	primaryKMSKeyARN  = "arn:aws:kms:ap-southeast-1:123456789012:key/11111111-1111-4111-8111-111111111111"
	recoveryKMSKeyARN = "arn:aws:kms:ap-southeast-2:210987654321:key/22222222-2222-4222-8222-222222222222"
)

func kmsAppTestConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := testConfig(t)
	cfg.Storage.MasterKey = config.MasterKey{
		Mode: config.MasterKeyModeKeySlots, PrimarySlot: "slot_aws_primary", RecoverySlot: "slot_aws_recovery",
		StartupDeadline: config.Duration(2 * time.Second), CallTimeout: config.Duration(250 * time.Millisecond),
		AllowedKMSKeys: []config.AllowedKMSKey{
			{Purpose: "primary", Provider: awskms.Provider, Region: "ap-southeast-1", Account: "123456789012", KeyID: primaryKMSKeyARN, Algorithm: awskms.SymmetricDefaultAlgorithm},
			{Purpose: "recovery", Provider: awskms.Provider, Region: "ap-southeast-2", Account: "210987654321", KeyID: recoveryKMSKeyARN, Algorithm: awskms.SymmetricDefaultAlgorithm},
		},
	}
	if err := cfg.Validate(config.LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestKMSInitializationPublishesIndependentVerifiedSlotsWithoutPlaintextKey(t *testing.T) {
	cfg := kmsAppTestConfig(t)
	harness := newKMSAppHarness(t)
	masterKey := bytes.Repeat([]byte{0x6d}, 32)
	fixedNow := time.Date(2026, 8, 3, 19, 0, 0, 0, time.UTC)
	if err := initializeKMS(context.Background(), cfg, kmsInitializationOptions{
		factory: harness.factory, random: bytes.NewReader(masterKey), now: func() time.Time { return fixedNow },
	}); err != nil {
		t.Fatal(err)
	}
	if state, err := InspectInitialization(cfg); err != nil || state != InitializationSystemReady {
		t.Fatalf("state=%q err=%v", state, err)
	}
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	descriptor, err := store.KeySlotDescriptor(context.Background())
	if err != nil || !descriptor.ProductionReady() || len(descriptor.Slots) != 2 {
		t.Fatalf("descriptor=%#v err=%v", descriptor, err)
	}
	primaryKey, err := unlockKMSMasterKey(context.Background(), cfg, store, masterkey.KeySlotPrimary, harness.factory)
	if err != nil {
		t.Fatal(err)
	}
	recoveryKey, err := unlockKMSMasterKey(context.Background(), cfg, store, masterkey.KeySlotRecovery, harness.factory)
	if err != nil {
		clear(primaryKey)
		t.Fatal(err)
	}
	if !bytes.Equal(primaryKey, masterKey) || !bytes.Equal(recoveryKey, masterKey) {
		t.Fatal("Primary and Recovery did not unlock the initialized Master Key")
	}
	clear(primaryKey)
	clear(recoveryKey)

	primary := keySlotForAppTest(t, descriptor, cfg.Storage.MasterKey.PrimarySlot)
	recovery := keySlotForAppTest(t, descriptor, cfg.Storage.MasterKey.RecoverySlot)
	if primary.KeyReference == recovery.KeyReference || bytes.Equal(primary.WrappedKey, recovery.WrappedKey) {
		t.Fatal("Primary and Recovery do not have independent KMS roots")
	}
	swapped := primary
	swapped.WrappedKey = bytes.Clone(recovery.WrappedKey)
	if _, err := (kmsSlotUnwrapper{masterKey: cfg.Storage.MasterKey, factory: harness.factory}).Unwrap(context.Background(), swapped); corekms.Classify(err) != corekms.ErrorCiphertextInvalid {
		t.Fatalf("cross-KMS ciphertext substitution class=%q err=%v", corekms.Classify(err), err)
	}
	tampered := recovery
	tampered.ProviderParameters = cloneStringMapForAppTest(recovery.ProviderParameters)
	tampered.ProviderParameters[slotParameterInstanceID] = "other-instance"
	if _, err := (kmsSlotUnwrapper{masterKey: cfg.Storage.MasterKey, factory: harness.factory}).Unwrap(context.Background(), tampered); corekms.Classify(err) != corekms.ErrorCiphertextInvalid {
		t.Fatalf("Context substitution class=%q err=%v", corekms.Classify(err), err)
	}
	unsafeConfig := cfg
	unsafeConfig.Storage.MasterKey.AllowedKMSKeys = append([]config.AllowedKMSKey(nil), cfg.Storage.MasterKey.AllowedKMSKeys...)
	unsafeConfig.Storage.MasterKey.AllowedKMSKeys[0].KeyID = "arn:aws:kms:ap-southeast-1:123456789012:key/99999999-9999-4999-8999-999999999999"
	callsBeforeReject := harness.callCount()
	if _, err := unlockKMSMasterKey(context.Background(), unsafeConfig, store, masterkey.KeySlotPrimary, harness.factory); corekms.Classify(err) != corekms.ErrorConfigInvalid {
		t.Fatalf("wrong allowlist class=%q err=%v", corekms.Classify(err), err)
	}
	if harness.callCount() != callsBeforeReject {
		t.Fatal("wrong allowlist reached KMS")
	}

	if err := filepath.Walk(cfg.Storage.DataDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil || info.IsDir() {
			return walkErr
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(contents, masterKey) {
			t.Fatalf("plaintext Master Key persisted in %s", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestKMSInitializationFailureNeverPublishesPartialInstance(t *testing.T) {
	for _, point := range []string{
		"after_empty_check", "after_primary_verified", "after_recovery_verified",
		"after_stage_persisted", "after_persisted_primary_verified",
	} {
		t.Run(point, func(t *testing.T) {
			cfg := kmsAppTestConfig(t)
			harness := newKMSAppHarness(t)
			injected := errors.New("injected initialization failure")
			err := initializeKMS(context.Background(), cfg, kmsInitializationOptions{
				factory: harness.factory, random: bytes.NewReader(bytes.Repeat([]byte{7}, 32)),
				now: func() time.Time { return time.Date(2026, 8, 3, 20, 0, 0, 0, time.UTC) },
				hook: func(current string) error {
					if current == point {
						return injected
					}
					return nil
				},
			})
			if !errors.Is(err, injected) {
				t.Fatalf("point=%s err=%v", point, err)
			}
			if _, err := os.Lstat(cfg.Storage.DataDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("point=%s published partial live state: %v", point, err)
			}
			stages, err := filepath.Glob(filepath.Join(filepath.Dir(cfg.Storage.DataDir), ".heimdall-init-stage-*"))
			if err != nil || len(stages) != 0 {
				t.Fatalf("point=%s retained staging state=%v err=%v", point, stages, err)
			}
		})
	}
	t.Run("recovery KMS unavailable", func(t *testing.T) {
		cfg := kmsAppTestConfig(t)
		harness := newKMSAppHarness(t)
		factory := func(ctx context.Context, allowed config.AllowedKMSKey) (corekms.Wrapper, error) {
			if allowed.Purpose == "recovery" {
				return nil, corekms.NewError(corekms.ErrorPermissionDenied, awskms.Provider, corekms.OperationWrap, 0, errors.New("recovery identity denied"))
			}
			return harness.factory(ctx, allowed)
		}
		err := initializeKMS(context.Background(), cfg, kmsInitializationOptions{
			factory: factory, random: bytes.NewReader(bytes.Repeat([]byte{7}, 32)),
		})
		if corekms.Classify(err) != corekms.ErrorPermissionDenied {
			t.Fatalf("class=%q err=%v", corekms.Classify(err), err)
		}
		if _, err := os.Lstat(cfg.Storage.DataDir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("single-Slot failure published live state: %v", err)
		}
	})
}

func TestKeySlotStartDoesNotAutoInitializeOrTrustFilePresence(t *testing.T) {
	cfg := kmsAppTestConfig(t)
	harness := newKMSAppHarness(t)
	previousFactory := defaultKMSWrapperFactory
	defaultKMSWrapperFactory = harness.factory
	t.Cleanup(func() { defaultKMSWrapperFactory = previousFactory })
	created, err := InitializeIfNeeded(cfg)
	if err == nil || created {
		t.Fatalf("empty key_slots auto-initialization created=%v err=%v", created, err)
	}
	if harness.callCount() != 0 {
		t.Fatal("Runtime auto-initialization path called KMS")
	}
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{cfg.LedgerPath(), cfg.AuditPath()} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	state, err := InspectInitialization(cfg)
	if err != nil || state != InitializationInconsistent {
		t.Fatalf("presence-only key_slots state=%q err=%v", state, err)
	}
	if harness.callCount() != 0 {
		t.Fatal("static initialization inspection called KMS")
	}
}

func TestKMSBootstrapAndRuntimeUsePrimaryOnlyOutsideRequestPath(t *testing.T) {
	cfg := kmsAppTestConfig(t)
	harness := newKMSAppHarness(t)
	previousFactory := defaultKMSWrapperFactory
	defaultKMSWrapperFactory = harness.factory
	t.Cleanup(func() { defaultKMSWrapperFactory = previousFactory })
	if err := initializeKMS(context.Background(), cfg, kmsInitializationOptions{
		factory: harness.factory, random: bytes.NewReader(bytes.Repeat([]byte{8}, 32)),
	}); err != nil {
		t.Fatal(err)
	}
	recoveryUnwrapsAfterInitialization := 0
	for _, call := range harness.wrappers[recoveryKMSKeyARN].Calls() {
		if call.Operation == corekms.OperationUnwrap {
			recoveryUnwrapsAfterInitialization++
		}
	}
	result, err := Bootstrap(context.Background(), cfg, BootstrapOptions{
		ProviderName: "OpenAI", ProviderType: domain.ProviderOpenAI,
		ProviderBaseURL: "https://api.openai.com", ProviderModel: "gpt-test", PublicModel: "chat", ProjectName: "Default",
	}, []byte("provider-secret"))
	if err != nil {
		t.Fatal(err)
	}
	if err := BootstrapAdmin(context.Background(), cfg, "admin", []byte("correct horse battery staple")); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	callsAfterStartup := harness.callCount()
	if _, err := runtime.auth.Authenticate(result.GatewayKey, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, ok := runtime.providers.Resolve("chat"); !ok {
		t.Fatal("bootstrap route was not loaded")
	}
	if harness.callCount() != callsAfterStartup {
		t.Fatal("Gateway request-path operation called KMS")
	}
	recoveryUnwrapsAfterStartup := 0
	for _, call := range harness.wrappers[recoveryKMSKeyARN].Calls() {
		if call.Operation == corekms.OperationUnwrap {
			recoveryUnwrapsAfterStartup++
		}
	}
	if recoveryUnwrapsAfterStartup != recoveryUnwrapsAfterInitialization {
		t.Fatalf("normal Bootstrap/Runtime reused Recovery Slot: %#v", harness.wrappers[recoveryKMSKeyARN].Calls())
	}
}

func TestRecoverySlotRequiresExactConfirmationAndWritesBreakGlassAudit(t *testing.T) {
	cfg := kmsAppTestConfig(t)
	harness := newKMSAppHarness(t)
	masterKey := bytes.Repeat([]byte{0x3c}, 32)
	previousFactory := defaultKMSWrapperFactory
	defaultKMSWrapperFactory = harness.factory
	t.Cleanup(func() { defaultKMSWrapperFactory = previousFactory })
	if err := initializeKMS(context.Background(), cfg, kmsInitializationOptions{
		factory: harness.factory, random: bytes.NewReader(masterKey),
	}); err != nil {
		t.Fatal(err)
	}
	before := len(harness.wrappers[recoveryKMSKeyARN].Calls())
	if _, err := VerifyRecoverySlot(context.Background(), cfg, "wrong-slot"); err == nil {
		t.Fatal("Recovery Slot was used without exact confirmation")
	}
	if len(harness.wrappers[recoveryKMSKeyARN].Calls()) != before {
		t.Fatal("failed confirmation reached Recovery KMS")
	}
	result, err := VerifyRecoverySlot(context.Background(), cfg, cfg.Storage.MasterKey.RecoverySlot)
	if err != nil {
		t.Fatal(err)
	}
	if result.SlotID != cfg.Storage.MasterKey.RecoverySlot || result.VerifiedAt.IsZero() {
		t.Fatalf("result=%#v", result)
	}
	auditKey, err := vault.DeriveAuditHMACKey(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(auditKey)
	log, err := audit.Open(cfg.AuditPath(), auditKey)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	found := false
	if _, err := log.Replay(func(record audit.Record) error {
		if record.Event.Action == "security.master_key.recovery_used" {
			found = record.Event.TargetID == cfg.Storage.MasterKey.RecoverySlot &&
				record.Event.ReasonCode == "break_glass_recovery" && record.Event.ActorType == "local_cli"
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("successful Recovery use did not produce high-severity break-glass Audit evidence")
	}
	rawAudit, err := os.ReadFile(cfg.AuditPath())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(rawAudit, []byte(primaryKMSKeyARN)) || bytes.Contains(rawAudit, []byte(recoveryKMSKeyARN)) || bytes.Contains(rawAudit, masterKey) {
		t.Fatal("Recovery Audit persisted a Key ARN or plaintext Master Key")
	}
}

func TestKMSBackupContainsDescriptorsButNoPlaintextMasterKey(t *testing.T) {
	cfg := kmsAppTestConfig(t)
	harness := newKMSAppHarness(t)
	masterKey := bytes.Repeat([]byte{0x57}, 32)
	previousFactory := defaultKMSWrapperFactory
	defaultKMSWrapperFactory = harness.factory
	t.Cleanup(func() { defaultKMSWrapperFactory = previousFactory })
	if err := initializeKMS(context.Background(), cfg, kmsInitializationOptions{
		factory: harness.factory, random: bytes.NewReader(masterKey),
	}); err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(cfg.Storage.DataDir)
	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(configPath, []byte("version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(root, "kms-backup.hmbk")
	backupKey := bytes.Repeat([]byte{0x22}, 32)
	if _, err := CreateBackup(context.Background(), cfg, configPath, archivePath, backupKey); err != nil {
		t.Fatal(err)
	}
	extracted := filepath.Join(root, "extracted")
	if _, err := backuppkg.Extract(archivePath, backupKey, extracted); err != nil {
		t.Fatal(err)
	}
	metadataFound := false
	if err := filepath.Walk(extracted, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil || info.IsDir() {
			return walkErr
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(contents, masterKey) {
			t.Fatalf("plaintext Master Key appeared in extracted backup file %s", path)
		}
		if filepath.Base(path) == "metadata.db" {
			metadataFound = true
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !metadataFound {
		t.Fatal("KMS backup omitted descriptor-bearing metadata")
	}
}

func keySlotForAppTest(t *testing.T, descriptor masterkey.KeySlotDescriptor, id string) masterkey.KeySlot {
	t.Helper()
	for _, slot := range descriptor.Slots {
		if slot.ID == id {
			return slot
		}
	}
	t.Fatalf("slot %q not found", id)
	return masterkey.KeySlot{}
}

func cloneStringMapForAppTest(source map[string]string) map[string]string {
	cloned := make(map[string]string, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}
