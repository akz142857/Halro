package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	boltstore "github.com/akz142857/Heimdall/internal/store/bolt"
)

func TestAdminAlertWebhookStoresSecretEncryptedAndNeverReturnsIt(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
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
	cookie, csrf := loginAdminForTest(t, runtime)
	secret := "webhook-secret-canary"
	created := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/alerts", "",
		map[string]any{
			"name": "Security operations", "url": "https://hooks.example.com/heimdall",
			"header_name": "Authorization", "secret": secret, "enabled": false,
		})
	if created.Code != http.StatusCreated || strings.Contains(created.Body.String(), secret) ||
		strings.Contains(created.Body.String(), "credential_id") {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	var view alertWebhookView
	if err := json.Unmarshal(created.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if !view.SecretConfigured || view.HeaderName != "authorization" {
		t.Fatalf("unexpected view: %#v", view)
	}
	testSelection := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/alerts/test", "", map[string]string{"id": view.ID})
	if testSelection.Code != http.StatusConflict {
		t.Fatalf("disabled alert selection test status=%d body=%s", testSelection.Code, testSelection.Body.String())
	}
	stored, err := runtime.store.GetAlertWebhook(context.Background(), view.ID)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := runtime.store.GetCredential(context.Background(), stored.CredentialID)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := runtime.vault.DecryptCredential(
		credential.ID, string(credential.Type), credential.Audience, credential.Ciphertext,
	)
	if err != nil {
		t.Fatal(err)
	}
	if string(plaintext) != secret || string(credential.Type) != webhookCredentialType {
		clear(plaintext)
		t.Fatal("stored webhook credential is invalid")
	}
	clear(plaintext)

	list := adminRequest(t, http.MethodGet, "/admin/api/v1/credentials", nil)
	list.AddCookie(cookie)
	listResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(listResponse, list)
	if strings.Contains(listResponse.Body.String(), credential.ID) {
		t.Fatalf("webhook credential leaked into provider credential list: %s", listResponse.Body.String())
	}
	removed := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPut, "/admin/api/v1/alerts/"+view.ID, `"1"`,
		map[string]any{
			"name": "Security operations", "url": "https://hooks.example.com/heimdall",
			"header_name": "authorization", "secret": "", "enabled": false,
		})
	if removed.Code != http.StatusOK || strings.Contains(removed.Body.String(), secret) {
		t.Fatalf("remove secret status=%d body=%s", removed.Code, removed.Body.String())
	}
	if err := json.Unmarshal(removed.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.SecretConfigured {
		t.Fatal("secret remained attached after explicit removal")
	}
	if _, err := runtime.store.GetCredential(context.Background(), credential.ID); !errors.Is(err, boltstore.ErrNotFound) {
		t.Fatalf("detached webhook credential was not deleted: %v", err)
	}
}
