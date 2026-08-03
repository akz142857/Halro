package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	goruntime "runtime"
	"runtime/pprof"
	"testing"

	"github.com/akz142857/Heimdall/internal/config"
	"github.com/akz142857/Heimdall/internal/masterkey"
	"github.com/akz142857/Heimdall/internal/safelog"
	boltstore "github.com/akz142857/Heimdall/internal/store/bolt"
)

const kmsMasterKeyCanary = "M11KMS_MASTER_KEY_CANARY_1234567"

func TestKMSMasterKeyCanaryNeverReachesPersistenceTelemetryErrorsOrHeapProfile(t *testing.T) {
	cfg := kmsAppTestConfig(t)
	cfg.Metrics.Enabled = true
	harness := newKMSAppHarness(t)
	previousFactory := defaultKMSWrapperFactory
	defaultKMSWrapperFactory = harness.factory
	t.Cleanup(func() { defaultKMSWrapperFactory = previousFactory })
	keyMaterial := []byte(kmsMasterKeyCanary)
	if len(keyMaterial) != 32 {
		t.Fatalf("invalid test key length=%d", len(keyMaterial))
	}
	if err := initializeKMS(context.Background(), cfg, kmsInitializationOptions{
		factory: harness.factory, random: bytes.NewReader(keyMaterial),
	}); err != nil {
		clear(keyMaterial)
		t.Fatal(err)
	}
	clear(keyMaterial)

	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := store.KeySlotDescriptor(context.Background())
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	primary := keySlotForAppTest(t, descriptor, cfg.Storage.MasterKey.PrimarySlot)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	tampered := primary
	tampered.ProviderParameters = cloneStringMapForAppTest(primary.ProviderParameters)
	tampered.ProviderParameters[slotParameterPayloadVersion] = "2"
	_, safeErr := (kmsSlotUnwrapper{masterKey: cfg.Storage.MasterKey, factory: harness.factory}).Unwrap(context.Background(), tampered)
	if safeErr == nil {
		t.Fatal("tampered KMS payload version was accepted")
	}

	var logs bytes.Buffer
	logger := safelog.New(slog.NewJSONHandler(&logs, nil))
	metricsToken, err := MetricsToken(cfg)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = runtime.Close()
		}
	}()
	request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	request.Header.Set("Authorization", "Bearer "+string(metricsToken))
	response := httptest.NewRecorder()
	runtime.metricsRouter().ServeHTTP(response, request)
	clear(metricsToken)
	if response.Code != http.StatusOK {
		t.Fatalf("metrics status=%d body=%s", response.Code, response.Body.String())
	}

	ciphertextText := base64.StdEncoding.EncodeToString(primary.WrappedKey)
	publicSurfaceCanaries := []string{kmsMasterKeyCanary, primaryKMSKeyARN, recoveryKMSKeyARN, ciphertextText}
	assertNoCanaries(t, "KMS logs", logs.Bytes(), publicSurfaceCanaries)
	assertNoCanaries(t, "KMS stable error", []byte(safeErr.Error()), publicSurfaceCanaries)
	assertNoCanaries(t, "KMS metrics", response.Body.Bytes(), publicSurfaceCanaries)
	auditBytes, err := os.ReadFile(cfg.AuditPath())
	if err != nil {
		t.Fatal(err)
	}
	assertNoCanaries(t, "KMS Audit", auditBytes, publicSurfaceCanaries)
	var heapProfile bytes.Buffer
	goruntime.GC()
	if err := pprof.Lookup("heap").WriteTo(&heapProfile, 0); err != nil {
		t.Fatal(err)
	}
	assertNoCanaries(t, "KMS heap profile", heapProfile.Bytes(), publicSurfaceCanaries)

	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	closed = true
	if err := filepath.WalkDir(cfg.Storage.DataDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		payload, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		assertNoCanaries(t, path, payload, []string{kmsMasterKeyCanary})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if primary.Purpose != masterkey.KeySlotPrimary || cfg.Storage.MasterKey.Mode != config.MasterKeyModeKeySlots {
		t.Fatal("test did not exercise the configured KMS Primary Slot")
	}
}
