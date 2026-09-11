package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/provider"
	provideropenai "github.com/akz142857/Halro/internal/provider/openai"
	"github.com/akz142857/Halro/internal/safelog"
)

func TestEchoedAuthorizerSecretNeverLeavesTheProviderBoundary(t *testing.T) {
	const credentialCanary = "opaque-provider-credential-canary-value"
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprintf(writer, `{"error":{"message":%q,"type":"authentication_error","code":"invalid_api_key"}}`,
			"upstream echoed "+request.Header.Get("Authorization"))
	}))
	defer upstream.Close()

	cfg := testConfig(t)
	cfg.Gateway.FailureCapture.Enabled = true
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	bootstrap, err := Bootstrap(context.Background(), cfg, BootstrapOptions{
		ProviderName: "Echoing Provider", ProviderType: domain.ProviderOpenAI,
		ProviderBaseURL: "https://api.openai.com", ProviderModel: "echo-model",
		PublicModel: "chat", ProjectName: "Canary Project",
		DailyBudgetMicrosUSD: 100_000_000, InputMicrosPerMillion: 1_000_000,
		OutputMicrosPerMillion: 2_000_000,
	}, []byte(credentialCanary))
	if err != nil {
		t.Fatal(err)
	}
	const adminPassword = "correct horse battery staple"
	if err := BootstrapAdmin(context.Background(), cfg, "admin", []byte(adminPassword)); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	runtime, err := Open(context.Background(), cfg, safelog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = runtime.Close()
		}
	}()

	endpoint, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	authorizer, err := provider.NewStaticHeaderAuthorizer(
		domain.CredentialBearerStatic, "Authorization", "Bearer ", []byte(credentialCanary), "api-key", "x-api-key",
	)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := provideropenai.NewWithOptions(provideropenai.Options{
		Endpoint: endpoint, Authorizer: authorizer, Client: upstream.Client(),
		ProviderType: string(domain.ProviderOpenAI), CredentialScheme: domain.CredentialBearerStatic,
		Capabilities: provider.Capabilities{Chat: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	target, found := runtime.providers.Resolve("chat")
	if !found {
		t.Fatal("bootstrapped target was not loaded")
	}
	manifest, found := provider.BuiltinProfile(target.ProfileID)
	if !found {
		t.Fatal("bootstrapped target profile was not found")
	}
	profiled, err := provider.NewLegacyAdapterBridge(adapter, manifest,
		domain.EvidenceForCapabilities(target.Capabilities, domain.EvidenceDeclared))
	if err != nil {
		t.Fatal(err)
	}
	target.Adapter = profiled
	replacement := provider.NewRegistry()
	if err := replacement.Register(target); err != nil {
		t.Fatal(err)
	}
	for _, retired := range runtime.providers.Replace(replacement) {
		retired.Close()
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"chat","messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("Authorization", "Bearer "+bootstrap.GatewayKey)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	runtime.gatewayRouter().ServeHTTP(response, request)
	if response.Code < http.StatusBadRequest {
		t.Fatalf("gateway status = %d, want a provider failure", response.Code)
	}
	assertBytesExcludeSecret(t, response.Body.Bytes(), credentialCanary)
	requestID := response.Header().Get("X-Request-ID")
	if requestID == "" {
		t.Fatal("gateway response omitted request ID")
	}

	drainCtx, cancelDrain := context.WithTimeout(context.Background(), 5*time.Second)
	if err := runtime.gatewayService.ShutdownFailureCapture(drainCtx); err != nil {
		cancelDrain()
		t.Fatal(err)
	}
	cancelDrain()
	record, found, err := runtime.failureCapture.Get(requestID, bootstrap.ProjectID)
	if err != nil || !found {
		t.Fatalf("captured record: found=%v err=%v", found, err)
	}
	encodedRecord, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	assertBytesExcludeSecret(t, encodedRecord, credentialCanary)

	owner := loginTestAdmin(t, runtime, "admin", adminPassword)
	const viewerPassword = "another correct horse battery staple"
	create := adminMutationRequest(t, http.MethodPost, "/admin/api/v1/admin-users", owner, map[string]string{
		"username": "viewer", "password": viewerPassword, "role": domain.AdminRoleReadOnly,
		"current_password": adminPassword,
	})
	created := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(created, create)
	if created.Code != http.StatusCreated {
		t.Fatalf("create read-only admin status = %d", created.Code)
	}
	viewer := loginTestAdmin(t, runtime, "viewer", viewerPassword)
	payload := authenticatedAdminGet(t, runtime, viewer.cookie,
		"/admin/api/v1/usage/failures/"+requestID+"/payload")
	if payload.Code != http.StatusOK {
		t.Fatalf("payload status = %d", payload.Code)
	}
	assertBytesExcludeSecret(t, payload.Body.Bytes(), credentialCanary)
	assertBytesExcludeSecret(t, logs.Bytes(), credentialCanary)

	var auditBytes bytes.Buffer
	if _, err := runtime.audit.Replay(func(entry audit.Record) error {
		encoded, err := json.Marshal(entry)
		if err == nil {
			auditBytes.Write(encoded)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	assertBytesExcludeSecret(t, auditBytes.Bytes(), credentialCanary)

	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	closed = true
	if err := filepath.WalkDir(filepath.Dir(cfg.Storage.DataDir), func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		payload, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(payload, []byte(credentialCanary)) {
			return fmt.Errorf("secret present in persisted file")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDisabledFailureCaptureOnlyIgnoresAMissingDirectory(t *testing.T) {
	cfg := testConfig(t)
	if err := os.MkdirAll(cfg.Storage.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(cfg.Storage.DataDir, "failures")
	if err := os.Symlink("failures", root); err != nil {
		t.Fatal(err)
	}
	if _, err := openFailureCapture(cfg, nil); err == nil {
		t.Fatal("a non-NotExist stat failure was ignored while capture was disabled")
	}
}

func assertBytesExcludeSecret(t *testing.T, payload []byte, secret string) {
	t.Helper()
	if bytes.Contains(payload, []byte(secret)) {
		t.Fatal("a provider credential crossed the provider boundary")
	}
}
