package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Heimdall/internal/config"
	"github.com/akz142857/Heimdall/internal/metricsauth"
	boltstore "github.com/akz142857/Heimdall/internal/store/bolt"
)

// mode:file has no witness of its own for the audit chain outside anchoring
// (ADR 0015). The operator who can turn anchoring on has to be told the gap
// exists at startup, the same way an exposed developer workbench is.
func TestWarnAboutMissingAnchorSink(t *testing.T) {
	warned := func(mode string, anchorEnabled bool) bool {
		var buffer strings.Builder
		runtime := &Runtime{
			logger: slog.New(slog.NewTextHandler(&buffer, &slog.HandlerOptions{Level: slog.LevelWarn})),
			config: config.Config{
				Storage: config.Storage{MasterKey: config.MasterKey{Mode: mode}},
				Audit:   config.Audit{Anchor: config.AuditAnchor{Enabled: anchorEnabled}},
			},
		}
		runtime.warnAboutMissingAnchorSink()
		return strings.Contains(buffer.String(), "audit chain")
	}
	if !warned(config.MasterKeyModeFile, false) {
		t.Fatal("mode:file with anchoring off was not reported")
	}
	if warned(config.MasterKeyModeFile, true) {
		t.Fatal("warned even though anchoring is enabled")
	}
	if warned(config.MasterKeyModeKeySlots, false) {
		t.Fatal("warned for mode:key_slots, which has non-repudiation independent of anchoring")
	}
}

func anchorTestConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := testConfig(t)
	cfg.Metrics.Enabled = true
	cfg.Audit.Anchor = config.AuditAnchor{
		Enabled: true, Sink: config.AuditAnchorSinkDeadManPull,
		Interval: config.Duration(5 * time.Minute), RecordDelta: 500,
		CredentialFile: filepath.Join(filepath.Dir(cfg.Storage.MasterKey.File), "anchor-credentials.json"),
	}
	return cfg
}

func TestAuditAnchorsEndpointRequiresItsOwnCredential(t *testing.T) {
	cfg := anchorTestConfig(t)
	now := time.Now().UTC()
	rotation, err := metricsauth.Rotate(cfg.Audit.Anchor.CredentialFile, time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(rotation.Token)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	unauthenticated := httptest.NewRequest(http.MethodGet, "/audit/anchors", nil)
	response := httptest.NewRecorder()
	runtime.metricsRouter().ServeHTTP(response, unauthenticated)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", response.Code)
	}

	// A metrics-scoped token must not authorize the anchor endpoint — the two
	// credential domains are independent on purpose.
	metricsToken, err := MetricsToken(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(metricsToken)
	crossScoped := httptest.NewRequest(http.MethodGet, "/audit/anchors", nil)
	crossScoped.Header.Set("Authorization", "Bearer "+string(metricsToken))
	response = httptest.NewRecorder()
	runtime.metricsRouter().ServeHTTP(response, crossScoped)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("metrics token authorized the anchor endpoint: status=%d", response.Code)
	}

	authenticated := httptest.NewRequest(http.MethodGet, "/audit/anchors", nil)
	authenticated.Header.Set("Authorization", "Bearer "+string(rotation.Token))
	response = httptest.NewRecorder()
	runtime.metricsRouter().ServeHTTP(response, authenticated)
	if response.Code != http.StatusOK {
		t.Fatalf("authenticated status=%d body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Anchors []map[string]any `json:"anchors"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Anchors) != 0 {
		t.Fatalf("fresh instance already has anchors: %#v", payload.Anchors)
	}
}

func TestAuditAnchorsEndpointReturnsOnlyNewerThanSince(t *testing.T) {
	cfg := anchorTestConfig(t)
	now := time.Now().UTC()
	rotation, err := metricsauth.Rotate(cfg.Audit.Anchor.CredentialFile, time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(rotation.Token)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	for sequence := uint64(1); sequence <= 3; sequence++ {
		anchor := boltstore.AuditAnchor{
			Sequence: sequence, Records: sequence * 10, InstanceID: runtime.instanceID,
			ObservedAt: now.Add(time.Duration(sequence) * time.Minute),
		}
		if err := runtime.store.AppendAuditAnchor(anchor); err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequest(http.MethodGet, "/audit/anchors?since=1", nil)
	request.Header.Set("Authorization", "Bearer "+string(rotation.Token))
	response := httptest.NewRecorder()
	runtime.metricsRouter().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Anchors []struct {
			Sequence uint64 `json:"sequence"`
		} `json:"anchors"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Anchors) != 2 || payload.Anchors[0].Sequence != 2 || payload.Anchors[1].Sequence != 3 {
		t.Fatalf("anchors=%#v", payload.Anchors)
	}
}

// Open already starts exactly one runAuditAnchorMaintenance goroutine per
// Runtime (gated inside the function body on Audit.Anchor.Enabled, not at
// the call site) — a test must not call it a second time itself, or the two
// concurrent invocations race on the same store and audit log.
func TestRunAuditAnchorMaintenanceEmitsOnRecordDelta(t *testing.T) {
	original := auditAnchorPollInterval
	auditAnchorPollInterval = 10 * time.Millisecond
	defer func() { auditAnchorPollInterval = original }()

	cfg := anchorTestConfig(t)
	cfg.Audit.Anchor.RecordDelta = 2
	cfg.Audit.Anchor.Interval = config.Duration(time.Hour)
	now := time.Now().UTC()
	rotation, err := metricsauth.Rotate(cfg.Audit.Anchor.CredentialFile, time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(rotation.Token)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	for index := 0; index < 3; index++ {
		if err := appendSystemAudit(runtime.audit, runtime.store, "audit.anchor.maintenance.probe"); err != nil {
			t.Fatal(err)
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	var latest boltstore.AuditAnchor
	for {
		latest, err = runtime.store.LatestAuditAnchor()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no anchor emitted after crossing record_delta within the deadline: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if latest.Records < 2 {
		t.Fatalf("emitted anchor records=%d, want at least 2", latest.Records)
	}
}
