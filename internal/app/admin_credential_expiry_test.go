package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

// A provider secret with a declared lifetime — a Bedrock API key, an STS
// session, a key under a rotation policy — expires whether or not anything
// records it, and the first sign used to be a 401 in production. The expiry is
// operator-declared and advisory: it is stored, returned and survives a
// restart, and nothing in the request path reads it.
func TestAdminCredentialExpiryIsOptionalAndRoundTrips(t *testing.T) {
	cfg := testConfig(t)
	runtime, _ := openRuntimeWithPolicyForTest(t, cfg)
	cookie, csrf := loginAdminForTest(t, runtime)
	expiry := time.Date(2027, 3, 1, 12, 0, 0, 0, time.UTC)

	create := func(name string, payload map[string]any) credentialView {
		t.Helper()
		body := map[string]any{
			"name": name, "type": "openai", "base_url": "https://api.openai.com", "secret": "provider-secret",
		}
		for key, value := range payload {
			body[key] = value
		}
		response := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/credentials", "", body)
		if response.Code != http.StatusCreated {
			t.Fatalf("create %s: status=%d body=%s", name, response.Code, response.Body.String())
		}
		var view credentialView
		if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
			t.Fatal(err)
		}
		return view
	}

	// Optional: the field an operator has nothing to put in stays absent from
	// the response rather than coming back as the year 1.
	undated := create("No declared expiry", nil)
	if undated.ExpiresAt != nil {
		t.Fatalf("an omitted expiry became %v", undated.ExpiresAt)
	}
	var undatedBody map[string]any
	if err := json.Unmarshal(mustGetCredential(t, runtime, cookie, csrf, undated.ID), &undatedBody); err != nil {
		t.Fatal(err)
	}
	if _, present := undatedBody["expires_at"]; present {
		t.Fatalf("an absent expiry was serialised anyway: %v", undatedBody["expires_at"])
	}

	dated := create("Bedrock API key", map[string]any{"expires_at": expiry})
	if dated.ExpiresAt == nil || !dated.ExpiresAt.Equal(expiry) {
		t.Fatalf("expiry did not round-trip: %v", dated.ExpiresAt)
	}

	// A zero instant is what an unparsed value decays to; stored, it would read
	// as a credential that expired two millennia ago.
	zero := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/credentials", "", map[string]any{
		"name": "Zero", "type": "openai", "base_url": "https://api.openai.com", "secret": "provider-secret",
		"expires_at": time.Time{},
	})
	if zero.Code != http.StatusBadRequest {
		t.Fatalf("a zero expiry was accepted: status=%d body=%s", zero.Code, zero.Body.String())
	}

	// Persisted before anything is rotated: reading it back through the API and
	// out of the store is what shows the value reached bbolt rather than only the
	// response it was echoed into.
	var datedBody map[string]any
	if err := json.Unmarshal(mustGetCredential(t, runtime, cookie, csrf, dated.ID), &datedBody); err != nil {
		t.Fatal(err)
	}
	if datedBody["expires_at"] != expiry.Format(time.RFC3339Nano) {
		t.Fatalf("the stored expiry did not come back: %v", datedBody["expires_at"])
	}
	storedDated, err := runtime.store.GetCredential(context.Background(), dated.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedDated.ExpiresAt == nil || !storedDated.ExpiresAt.Equal(expiry) {
		t.Fatalf("the store holds %v, not the expiry that was saved", storedDated.ExpiresAt)
	}

	// The other direction: a credential saved without one takes an expiry on
	// rotation.
	adopted := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPut, "/admin/api/v1/credentials/"+undated.ID, `"1"`,
		map[string]any{
			"name": "No declared expiry", "type": "openai", "base_url": "https://api.openai.com",
			"secret": "rotated-secret", "expires_at": expiry, "current_password": stepUpTestPassword,
		},
	)
	var adoptedView credentialView
	if adopted.Code != http.StatusOK || json.Unmarshal(adopted.Body.Bytes(), &adoptedView) != nil {
		t.Fatalf("rotation: status=%d body=%s", adopted.Code, adopted.Body.String())
	}
	if adoptedView.ExpiresAt == nil || !adoptedView.ExpiresAt.Equal(expiry) {
		t.Fatalf("a rotation did not adopt the expiry it stated: %v", adoptedView.ExpiresAt)
	}

	// A rotation states the expiry it wants. Clearing the field clears the
	// stored value rather than silently carrying the old date forward.
	rotated := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPut, "/admin/api/v1/credentials/"+dated.ID, `"1"`,
		map[string]any{
			"name": "Bedrock API key", "type": "openai", "base_url": "https://api.openai.com",
			"secret": "rotated-secret", "expires_at": nil, "current_password": stepUpTestPassword,
		},
	)
	if rotated.Code != http.StatusOK {
		t.Fatalf("rotation: status=%d body=%s", rotated.Code, rotated.Body.String())
	}
	var rotatedView credentialView
	if err := json.Unmarshal(rotated.Body.Bytes(), &rotatedView); err != nil {
		t.Fatal(err)
	}
	if rotatedView.ExpiresAt != nil {
		t.Fatalf("a cleared expiry survived the rotation: %v", rotatedView.ExpiresAt)
	}

	// Persisted, not merely echoed: an expiry that only lives in the response is
	// no use for planning a rotation next month.
	runtime.Close()
	reopened, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	stored, err := reopened.store.ListCredentials(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]*time.Time{}
	for _, credential := range stored {
		found[credential.Name] = credential.ExpiresAt
	}
	if kept, ok := found["No declared expiry"]; !ok || kept == nil || !kept.Equal(expiry) {
		t.Fatalf("the expiry adopted by the rotation did not survive the restart: %v", kept)
	}
	if kept, ok := found["Bedrock API key"]; !ok || kept != nil {
		t.Fatalf("the cleared expiry did not survive the restart cleared: %v", kept)
	}

	// The stored shape is what the request path ignores, so a credential with a
	// past expiry still validates and is still usable — refusing traffic on a
	// typed-in date is a decision this field deliberately does not make.
	past := expiry.AddDate(-10, 0, 0)
	if err := (domain.Credential{
		ID: "cred_past", Name: "Past", Type: domain.ProviderOpenAI,
		AccessSurface: domain.SurfaceOpenAI, Scheme: domain.CredentialBearerStatic,
		Audience: "https://api.openai.com:443:openai", Ciphertext: []byte{1},
		ExpiresAt: &past,
	}).Validate(); err != nil {
		t.Fatalf("a credential that already expired was refused: %v", err)
	}
}

func mustGetCredential(t *testing.T, runtime *Runtime, cookie *http.Cookie, csrf, id string) []byte {
	t.Helper()
	response := performAdminMutation(t, runtime, cookie, csrf, http.MethodGet, "/admin/api/v1/credentials/"+id, "", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("get credential: status=%d body=%s", response.Code, response.Body.String())
	}
	return response.Body.Bytes()
}
