package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/provider"
)

// An AWS credential declares its own region, and the signer pins the host to
// that region. The write path used to check only that the secret was 1–16KB of
// something, so a document naming us-east-1 saved cleanly against an endpoint in
// us-east-2 — and then the registry load dropped the binding, the console said
// "provider binding adapter is unavailable", and nothing on the screen named the
// field to change. The refusal belongs at the moment the operator is still
// looking at the value.
func TestCredentialRefusesAWSMaterialItsEndpointCannotSign(t *testing.T) {
	// The check is on the SigV4 credential constructor, which only the Runtime
	// surfaces use. Withholding them takes away the way in, not the rule.
	if domain.IsWithheldProfile(domain.ProfileBedrockConverseText) {
		t.Skip("Bedrock Runtime is withheld from this build, so no credential can be created on it")
	}
	cfg := testConfig(t)
	runtime, _ := openRuntimeWithPolicyForTest(t, cfg)
	cookie, csrf := loginAdminForTest(t, runtime)

	const endpoint = "https://bedrock-runtime.us-east-2.amazonaws.com"
	create := func(name, secret string) *httptest.ResponseRecorder {
		t.Helper()
		return performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/credentials", "", map[string]any{
			"name": name, "type": "bedrock", "base_url": endpoint, "secret": secret,
			"access_surface": domain.SurfaceBedrockRuntime, "scheme": domain.CredentialAWSSigV4Explicit,
		})
	}

	mismatched := create("Bedrock in the wrong region", awsCredentialForTest("us-east-1"))
	if mismatched.Code != http.StatusBadRequest {
		t.Fatalf("a credential that can never sign for its endpoint was stored: status=%d body=%s",
			mismatched.Code, mismatched.Body.String())
	}
	var refusal map[string]string
	if err := json.Unmarshal(mismatched.Body.Bytes(), &refusal); err != nil {
		t.Fatal(err)
	}
	// The reader has to be told which value disagreed. The message names the
	// region and the endpoint relationship; it must not carry the material.
	if refusal["error"] == "" {
		t.Fatalf("the refusal said nothing: %s", mismatched.Body.String())
	}
	for _, secret := range []string{"AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI"} {
		if body := mismatched.Body.String(); strings.Contains(body, secret) {
			t.Fatalf("the refusal echoed credential material: %s", body)
		}
	}

	// Same document shape, region the endpoint can actually sign for.
	matching := create("Bedrock", awsCredentialForTest("us-east-2"))
	if matching.Code != http.StatusCreated {
		t.Fatalf("a credential whose region matches its endpoint was refused: status=%d body=%s",
			matching.Code, matching.Body.String())
	}
	var credential struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(matching.Body.Bytes(), &credential); err != nil {
		t.Fatal(err)
	}

	// The other way into the same disagreement: keep the material and move the
	// endpoint. A rotation carries the stored secret when it sends none, so the
	// check has to run against what is stored rather than only against what the
	// request carried.
	moved := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPut, "/admin/api/v1/credentials/"+credential.ID, `"1"`,
		map[string]any{
			"name": "Bedrock", "type": "bedrock",
			"base_url":       "https://bedrock-runtime.eu-west-1.amazonaws.com",
			"access_surface": domain.SurfaceBedrockRuntime, "scheme": domain.CredentialAWSSigV4Explicit,
			"current_password": stepUpTestPassword,
		},
	)
	if moved.Code != http.StatusBadRequest {
		t.Fatalf("the endpoint moved out from under the material: status=%d body=%s",
			moved.Code, moved.Body.String())
	}

	// A scheme whose material has no shape keeps saving: the static-header
	// schemes carry an opaque token, and the only requirement — non-empty — is
	// already checked. This is what stops the new check from becoming a guess
	// about what an API key looks like.
	opaque := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/credentials", "", map[string]any{
		"name": "OpenAI", "type": "openai", "base_url": "https://api.openai.com", "secret": "sk-not-json-at-all",
	})
	if opaque.Code != http.StatusCreated {
		t.Fatalf("an opaque API key was refused: status=%d body=%s", opaque.Code, opaque.Body.String())
	}
}

// A probe that finds no adapter used to answer with the symptom alone, in
// English, on a row whose record looked healthy. The load already decided why
// the binding was left out, so the refusal carries that class and the console
// can say which record to open.
func TestProviderTestNamesWhyTheBindingHasNoAdapter(t *testing.T) {
	if domain.IsWithheldProfile(domain.ProfileBedrockConverseText) {
		t.Skip("Bedrock Runtime is withheld from this build, so no connection can be created on it")
	}
	cfg := testConfig(t)
	runtime, _ := openRuntimeWithPolicyForTest(t, cfg)
	cookie, csrf := loginAdminForTest(t, runtime)

	const endpoint = "https://bedrock-runtime.us-east-1.amazonaws.com"
	credentialResponse := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/credentials", "", map[string]any{
		"name": "Bedrock Runtime", "type": "bedrock", "base_url": endpoint,
		"secret":         awsCredentialForTest("us-east-1"),
		"access_surface": domain.SurfaceBedrockRuntime, "scheme": domain.CredentialAWSSigV4Explicit,
	})
	var credential struct {
		ID string `json:"id"`
	}
	if credentialResponse.Code != http.StatusCreated || json.Unmarshal(credentialResponse.Body.Bytes(), &credential) != nil {
		t.Fatalf("credential status=%d body=%s", credentialResponse.Code, credentialResponse.Body.String())
	}
	providerResponse := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/providers", "", map[string]any{
		"name": "Bedrock Converse", "type": "bedrock", "base_url": endpoint,
		"credential_id": credential.ID, "enabled": true,
	})
	var instance struct {
		ID       string `json:"id"`
		Bindings []struct {
			ID      string `json:"id"`
			Enabled bool   `json:"enabled"`
		} `json:"bindings"`
	}
	if providerResponse.Code != http.StatusCreated || json.Unmarshal(providerResponse.Body.Bytes(), &instance) != nil {
		t.Fatalf("provider status=%d body=%s", providerResponse.Code, providerResponse.Body.String())
	}
	bindingID := ""
	for _, binding := range instance.Bindings {
		if binding.Enabled {
			bindingID = binding.ID
		}
	}
	if bindingID == "" {
		t.Fatal("the provider came up with no enabled binding")
	}

	// Before any registry is swapped in: the provider loaded, its adapter built,
	// and the connection has nothing deployed on it. The Bedrock probe addresses
	// a model path, so there is nothing to probe with — and the answer has to say
	// that rather than call an id that was never supplied invalid.
	noDeployment := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/providers/"+instance.ID+"/test", "", nil)
	if noDeployment.Code != http.StatusBadRequest {
		t.Fatalf("probe status=%d body=%s", noDeployment.Code, noDeployment.Body.String())
	}
	var missingDeployment map[string]string
	if err := json.Unmarshal(noDeployment.Body.Bytes(), &missingDeployment); err != nil {
		t.Fatal(err)
	}
	if missingDeployment["code"] != probeRequiresDeployment {
		t.Fatalf("code=%q want %q: %s", missingDeployment["code"], probeRequiresDeployment, noDeployment.Body.String())
	}
	if strings.Contains(noDeployment.Body.String(), "model id") {
		t.Fatalf("the refusal still blamed the model id: %s", noDeployment.Body.String())
	}

	for _, test := range []struct {
		name     string
		recorded string
		want     string
	}{
		{
			name:     "the load excluded the binding",
			recorded: excludedAdapterUnavailable,
			want:     excludedAdapterUnavailable,
		},
		{
			// Nothing in the live registry accounts for the missing adapter. The
			// refusal still has to be fail-closed and still has to carry a class,
			// or the console is back to explaining a bare English sentence.
			name:     "nothing explains the missing adapter",
			recorded: "",
			want:     excludedBindingAdapterMissing,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			registry := provider.NewRegistry()
			if test.recorded != "" {
				registry.RecordUnavailable(instance.ID, bindingID, test.recorded)
			}
			runtime.providers.Replace(registry)

			response := performAdminMutation(t, runtime, cookie, csrf,
				http.MethodPost, "/admin/api/v1/providers/"+instance.ID+"/test", "", nil)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("probe status=%d body=%s", response.Code, response.Body.String())
			}
			var refusal map[string]string
			if err := json.Unmarshal(response.Body.Bytes(), &refusal); err != nil {
				t.Fatal(err)
			}
			if refusal["code"] != test.want {
				t.Fatalf("code=%q want %q: %s", refusal["code"], test.want, response.Body.String())
			}
		})
	}
}

// Anthropic serves two products on one host, so nothing about an endpoint says
// which one a secret is for — only the secret itself does. Both directions are
// refused by name, because getting it wrong in either one is silent: a
// subscription token on the metered product is answered `401 API key is
// invalid.`, and a Console key on the subscription product would spend the wrong
// balance rather than fail at all.
func TestAnthropicCredentialRefusesTheOtherProductsSecret(t *testing.T) {
	cfg := testConfig(t)
	// The subscription product is off in the shipped default; this instance is
	// one where an operator has turned it on, which is the only way to exercise
	// both directions.
	cfg.ProviderSubscriptions.AnthropicClaude = true
	runtime, _ := openRuntimeWithPolicyForTest(t, cfg)
	cookie, csrf := loginAdminForTest(t, runtime)

	const anthropicEndpoint = "https://api.anthropic.com"
	create := func(name, secret string, surface domain.AccessSurface, scheme domain.CredentialScheme) *httptest.ResponseRecorder {
		t.Helper()
		return performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/credentials", "", map[string]any{
			"name": name, "type": "anthropic", "base_url": anthropicEndpoint, "secret": secret,
			"access_surface": surface, "scheme": scheme,
		})
	}
	refusalCode := func(response *httptest.ResponseRecorder) string {
		t.Helper()
		var refusal map[string]string
		if err := json.Unmarshal(response.Body.Bytes(), &refusal); err != nil {
			t.Fatal(err)
		}
		return refusal["code"]
	}

	// The two shapes a Claude sign-in produces, read first-hand from a live
	// credential store on 2026-09-22. The version digits are carried here and
	// matched nowhere, so a future `oat02` is still recognised.
	for _, test := range []struct{ name, secret string }{
		{"an OAuth access token", "sk-ant-oat01-" + strings.Repeat("A", 95)},
		{"an OAuth refresh token", "sk-ant-ort01-" + strings.Repeat("A", 95)},
	} {
		t.Run(test.name+" on the metered product", func(t *testing.T) {
			response := create("Claude Max", test.secret, domain.SurfaceAnthropic, domain.CredentialAnthropicAPIKey)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("a subscription token was stored as an API key: status=%d body=%s",
					response.Code, response.Body.String())
			}
			if got := refusalCode(response); got != "anthropic_subscription_token_refused" {
				t.Fatalf("refused with code %q: %s", got, response.Body.String())
			}
			if strings.Contains(response.Body.String(), test.secret) {
				t.Fatalf("the refusal echoed credential material: %s", response.Body.String())
			}
		})
	}

	// The mirror. A Console key here is the dangerous direction: the upstream
	// would serve it, against the wrong balance.
	consoleOnSubscription := create(
		"Console key in the wrong place",
		`{"access_token":"sk-ant-api03-`+strings.Repeat("A", 93)+`AA"}`,
		domain.SurfaceAnthropicSubscription, domain.CredentialAnthropicOAuth,
	)
	if consoleOnSubscription.Code != http.StatusBadRequest {
		t.Fatalf("a Console key was stored as a subscription credential: status=%d body=%s",
			consoleOnSubscription.Code, consoleOnSubscription.Body.String())
	}
	if got := refusalCode(consoleOnSubscription); got != "anthropic_console_key_refused" {
		t.Fatalf("refused with code %q: %s", got, consoleOnSubscription.Body.String())
	}

	// Both products save what belongs to them.
	consoleKey := create("Claude Console", "sk-ant-api03-"+strings.Repeat("A", 93)+"AA",
		domain.SurfaceAnthropic, domain.CredentialAnthropicAPIKey)
	if consoleKey.Code != http.StatusCreated {
		t.Fatalf("a Console API key was refused: status=%d body=%s", consoleKey.Code, consoleKey.Body.String())
	}
	// A subscription product carries a usage warning, so creating one is refused
	// until the operator acknowledges the upstream's own statement of what the
	// credential may be used for. That acknowledgement is recorded against the
	// Offering in the audit trail, which is where "I read this and accepted it"
	// belongs — not in a checkbox the browser forgets.
	subscription := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/credentials", "", map[string]any{
		"name": "Claude subscription", "type": "anthropic", "base_url": anthropicEndpoint,
		"secret":         `{"access_token":"sk-ant-oat01-` + strings.Repeat("A", 95) + `","refresh_token":"sk-ant-ort01-` + strings.Repeat("A", 95) + `"}`,
		"access_surface": domain.SurfaceAnthropicSubscription, "scheme": domain.CredentialAnthropicOAuth,
		"acknowledged_policy_revision": "anthropic-claude-subscription-2026-09-22",
	})
	if subscription.Code != http.StatusCreated {
		t.Fatalf("a subscription credential was refused on its own product: status=%d body=%s",
			subscription.Code, subscription.Body.String())
	}

	var credential struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(consoleKey.Body.Bytes(), &credential); err != nil {
		t.Fatal(err)
	}
	// Rotation reaches the same check: a credential that starts legitimate must
	// not be walked onto the other product's secret by an edit.
	rotated := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPut, "/admin/api/v1/credentials/"+credential.ID, `"1"`,
		map[string]any{
			"name": "Claude Console", "type": "anthropic", "base_url": anthropicEndpoint,
			"secret":           "sk-ant-oat01-" + strings.Repeat("A", 95),
			"current_password": stepUpTestPassword,
		},
	)
	if rotated.Code != http.StatusBadRequest {
		t.Fatalf("a rotation walked the credential onto a subscription token: status=%d body=%s",
			rotated.Code, rotated.Body.String())
	}

	// Narrowness, which is the whole design of the check: it is bound to the two
	// Anthropic schemes and says nothing about anyone else's secrets.
	elsewhere := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/credentials", "", map[string]any{
		"name": "DeepSeek", "type": "deepseek", "base_url": "https://api.deepseek.com",
		"secret": "sk-ant-oat01-not-anthropics-token",
	})
	if elsewhere.Code != http.StatusCreated {
		t.Fatalf("the check leaked onto another provider's secret: status=%d body=%s",
			elsewhere.Code, elsewhere.Body.String())
	}
}

// The shipped default offers no subscription product at all, and that default is
// what makes the distributed artifact comply with an upstream's terms without
// the operator having to know the question exists. The refusal has to name the
// switch: an operator who meant to turn it on is looking at a form, not at the
// configuration file.
func TestClaudeSubscriptionIsRefusedUntilTheOperatorEnablesIt(t *testing.T) {
	runtime, _ := openRuntimeWithPolicyForTest(t, testConfig(t))
	cookie, csrf := loginAdminForTest(t, runtime)

	response := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/credentials", "", map[string]any{
		"name": "Claude subscription", "type": "anthropic", "base_url": "https://api.anthropic.com",
		"secret":         `{"access_token":"sk-ant-oat01-` + strings.Repeat("A", 95) + `"}`,
		"access_surface": domain.SurfaceAnthropicSubscription, "scheme": domain.CredentialAnthropicOAuth,
	})
	if response.Code != http.StatusBadRequest {
		t.Fatalf("the default build offered a subscription product: status=%d body=%s",
			response.Code, response.Body.String())
	}
	var refusal map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &refusal); err != nil {
		t.Fatal(err)
	}
	if refusal["code"] != "provider_subscription_disabled" {
		t.Fatalf("refused with code %q: %s", refusal["code"], response.Body.String())
	}

	// And it is absent from the served matrix, not merely refused on save: an
	// Offering the console can select and the server then rejects is the drift
	// that endpoint exists to prevent.
	for _, profile := range buildProviderProfilesView(testConfig(t).ProviderSubscriptions).ProviderTypes {
		for _, row := range profile.Profiles {
			if row.ID == domain.ProfileAnthropicSubscriptionMessages {
				t.Fatal("the subscription profile was served while switched off")
			}
		}
	}
}
