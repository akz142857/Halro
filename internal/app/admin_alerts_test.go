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

// The console never shows a stored webhook secret. Re-pointing the webhook while reusing
// that secret would post it to a host the operator chose but never had the plaintext for,
// so the endpoint has to refuse and demand the secret again.
func TestAdminAlertWebhookRefusesToReuseSecretForANewDestination(t *testing.T) {
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

	created := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/alerts", "",
		map[string]any{
			"name": "Security operations", "url": "https://hooks.example.com/heimdall",
			"header_name": "Authorization", "secret": "webhook-secret-canary", "enabled": false,
		})
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	var view alertWebhookView
	if err := json.Unmarshal(created.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}

	redirected := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPut, "/admin/api/v1/alerts/"+view.ID, `"1"`,
		map[string]any{
			"name": "Security operations", "url": "https://attacker.example.com/collect",
			"header_name": "Authorization", "enabled": false,
		})
	if redirected.Code != http.StatusBadRequest ||
		!strings.Contains(redirected.Body.String(), "requires the secret to be entered again") {
		t.Fatalf("redirect without secret status=%d body=%s", redirected.Code, redirected.Body.String())
	}

	// Switching the header carries the same risk and is refused the same way.
	rehomedHeader := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPut, "/admin/api/v1/alerts/"+view.ID, `"1"`,
		map[string]any{
			"name": "Security operations", "url": "https://hooks.example.com/heimdall",
			"header_name": "X-Webhook-Token", "enabled": false,
		})
	if rehomedHeader.Code != http.StatusBadRequest {
		t.Fatalf("header switch without secret status=%d body=%s",
			rehomedHeader.Code, rehomedHeader.Body.String())
	}

	// Supplying the secret again is the supported path, and editing anything other than
	// the destination keeps working without it.
	rename := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPut, "/admin/api/v1/alerts/"+view.ID, `"1"`,
		map[string]any{
			"name": "Renamed", "url": "https://hooks.example.com/heimdall",
			"header_name": "Authorization", "enabled": false,
		})
	if rename.Code != http.StatusOK {
		t.Fatalf("rename status=%d body=%s", rename.Code, rename.Body.String())
	}
	moved := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPut, "/admin/api/v1/alerts/"+view.ID, `"2"`,
		map[string]any{
			"name": "Renamed", "url": "https://hooks2.example.com/heimdall",
			"header_name": "Authorization", "secret": "new-secret-canary", "enabled": false,
		})
	if moved.Code != http.StatusOK {
		t.Fatalf("move with secret status=%d body=%s", moved.Code, moved.Body.String())
	}
}

// The stored audit record nests everything under `event`. The console reads the fields
// directly, so the list endpoint has to serve them flat or the timeline renders blanks.
func TestAdminAuditListIsServedFlat(t *testing.T) {
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
	cookie, _ := loginAdminForTest(t, runtime)

	response := authenticatedAdminGet(t, runtime, cookie, "/admin/api/v1/audit")
	if response.Code != http.StatusOK {
		t.Fatalf("audit list status=%d body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), `"event":{`) {
		t.Fatalf("audit list still nests the event: %s", response.Body.String())
	}
	var page struct {
		Items []auditRecordView `json:"items"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) == 0 {
		t.Fatal("audit list returned no records")
	}
	for _, record := range page.Items {
		if record.Sequence == 0 || record.Action == "" || record.ActorType == "" ||
			record.Outcome == "" || record.OccurredAt.IsZero() || record.EventID == "" {
			t.Fatalf("audit record is missing flattened fields: %#v", record)
		}
	}
	// The per-frame hash is verified server-side on replay; publishing it invites a reader
	// to treat an unverifiable value as proof.
	if strings.Contains(response.Body.String(), `"hash"`) {
		t.Fatalf("audit list exposed the frame hash: %s", response.Body.String())
	}
}
