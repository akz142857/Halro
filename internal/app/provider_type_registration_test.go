package app

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
)

// Every type the console is told about has to be a type the server will accept.
//
// This existed as two lists that could not see each other: the profile table,
// which the metadata endpoint enumerates for the connection form, and a switch
// in the Admin write path. MiniMax was added to the first and not the second,
// so the console offered "MiniMax (Beta)", the operator picked it, and the save
// came back "provider type is not implemented". Nothing in the suite could see
// it, because the wiring test that covered MiniMax called the adapter
// constructor directly and never crossed the Admin API.
func TestEveryOfferedProviderTypeIsAcceptedOnSave(t *testing.T) {
	for _, providerType := range domain.AllProviderTypes() {
		if !implementedProviderType(providerType) {
			t.Errorf("%s is offered by the provider-profiles endpoint and refused by the write path", providerType)
		}
	}
}

// The same thing end to end, on the one path an operator actually takes: save a
// credential, then a connection on it. A unit check that both lists agree would
// still pass if the agreement were on a value the rest of the save rejects.
func TestMiniMaxCredentialAndConnectionSaveThroughTheAdminAPI(t *testing.T) {
	cfg := testConfig(t)
	runtime, _ := openRuntimeWithPolicyForTest(t, cfg)
	cookie, csrf := loginAdminForTest(t, runtime)
	const endpoint = "https://api.minimax.io"

	credentialResponse := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/credentials", "", map[string]any{
		"name": "minimax-io", "type": "minimax", "base_url": endpoint, "secret": "minimax-key",
		"access_surface": domain.SurfaceMiniMax, "scheme": domain.CredentialBearerStatic,
	})
	if credentialResponse.Code != http.StatusCreated {
		t.Fatalf("credential: status=%d body=%s", credentialResponse.Code, credentialResponse.Body.String())
	}
	var credential struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(credentialResponse.Body.Bytes(), &credential); err != nil {
		t.Fatal(err)
	}

	// All three faces share one key and one surface, so each has to be creatable
	// against the same credential.
	for _, profileID := range []domain.ProviderProfileID{
		domain.ProfileMiniMaxAnthropicMessages,
		domain.ProfileMiniMaxChat,
		domain.ProfileMiniMaxResponses,
	} {
		response := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/providers", "", map[string]any{
			"name": "minimax " + string(profileID), "type": "minimax", "base_url": endpoint,
			"credential_id": credential.ID, "enabled": true,
			"access_surface": domain.SurfaceMiniMax, "profile_id": profileID,
			"credential_scheme": domain.CredentialBearerStatic,
		})
		if response.Code != http.StatusCreated {
			t.Fatalf("connection on %s: status=%d body=%s", profileID, response.Code, response.Body.String())
		}
	}

	// The mainland host is the same contract at a different address, and the
	// credential is bound to the host it was saved against. An operator with a
	// mainland key has to be able to save one.
	mainland := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/credentials", "", map[string]any{
		"name": "minimax-cn", "type": "minimax", "base_url": "https://api.minimaxi.com", "secret": "minimax-cn-key",
		"access_surface": domain.SurfaceMiniMax, "scheme": domain.CredentialBearerStatic,
	})
	if mainland.Code != http.StatusCreated {
		t.Fatalf("mainland credential: status=%d body=%s", mainland.Code, mainland.Body.String())
	}
}
