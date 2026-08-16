package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
)

// The point of serving the matrix is that a caller stops keeping its own copy,
// which only holds if the response is the table rather than a transcription of
// it. This walks the domain table and demands a row in the response for each
// entry, so a profile added there and forgotten here fails rather than silently
// disappearing from every connection form.
func TestAdminProviderProfilesServesEveryTableRow(t *testing.T) {
	runtime, cookie := providerProfilesFixture(t)
	view := fetchProviderProfiles(t, runtime, cookie)

	served := make(map[domain.ProviderProfileID]providerProfileView)
	for _, providerType := range view.ProviderTypes {
		for _, profile := range providerType.Profiles {
			if existing, duplicate := served[profile.ID]; duplicate {
				t.Fatalf("%s served twice: %#v and %#v", profile.ID, existing, profile)
			}
			served[profile.ID] = profile
		}
	}

	table := domain.AllProviderProfiles()
	if len(served) != len(table) {
		t.Fatalf("served %d profiles, the table has %d", len(served), len(table))
	}
	for _, want := range table {
		got, ok := served[want.ID]
		if !ok {
			t.Errorf("%s is in the table but not in the response", want.ID)
			continue
		}
		if got.AccessSurface != want.AccessSurface || got.CredentialScheme != want.CredentialScheme ||
			got.Immutable != want.Immutable {
			t.Errorf("%s identity differs from the table: %#v", want.ID, got)
		}
		if got.Defaults != providerCapabilityViewOf(want.Defaults) {
			t.Errorf("%s defaults differ from the table: %#v", want.ID, got.Defaults)
		}
		if got.Ceiling != providerCapabilityViewOf(want.Ceiling) {
			t.Errorf("%s ceiling differs from the table: %#v", want.ID, got.Ceiling)
		}
	}

	for _, providerType := range view.ProviderTypes {
		if len(providerType.Profiles) == 0 {
			t.Errorf("%s was served with no profiles", providerType.Type)
		}
		if providerType.DefaultProfileID == "" {
			t.Errorf("%s was served with no default profile", providerType.Type)
		}
		if _, ok := served[providerType.DefaultProfileID]; !ok {
			t.Errorf("%s defaults to %q, which was not served", providerType.Type, providerType.DefaultProfileID)
		}
	}
	if len(view.ProviderTypes) != len(domain.AllProviderTypes()) {
		t.Errorf("served %d provider types, the table has %d", len(view.ProviderTypes), len(domain.AllProviderTypes()))
	}
}

// The numeric limits decide whether a save is accepted — Titan Embed refuses a
// context above 8192 and refuses zero — so they have to survive the round trip.
// A boolean-only projection would leave a caller unable to fill the field at all.
func TestAdminProviderProfilesCarriesNumericLimits(t *testing.T) {
	runtime, cookie := providerProfilesFixture(t)
	view := fetchProviderProfiles(t, runtime, cookie)

	for _, providerType := range view.ProviderTypes {
		for _, profile := range providerType.Profiles {
			if profile.ID != domain.ProfileBedrockInvokeTitanEmbedV2 {
				continue
			}
			if profile.Ceiling.MaxContextTokens != 8192 {
				t.Fatalf("titan embed context limit is %d, want 8192", profile.Ceiling.MaxContextTokens)
			}
			return
		}
	}
	t.Fatal("titan embed was not served")
}

// The endpoint exists to fill a form, so the endpoint it offers must be the one
// this deployment would use — region already substituted, no placeholder left
// for a caller to interpret.
func TestAdminProviderProfilesResolvesTheConfiguredRegion(t *testing.T) {
	cfg := testConfig(t)
	cfg.Providers.Bedrock.Region = "eu-central-1"
	runtime, cookie := providerProfilesFixtureWithConfig(t, cfg)
	view := fetchProviderProfiles(t, runtime, cookie)

	want := map[domain.ProviderProfileID]string{
		domain.ProfileBedrockConverseText:     "https://bedrock-runtime.eu-central-1.amazonaws.com",
		domain.ProfileBedrockMantleOpenAIChat: "https://bedrock-mantle.eu-central-1.api.aws",
		domain.ProfileOpenAIChatEmbeddings:    "https://api.openai.com",
	}
	for _, providerType := range view.ProviderTypes {
		for _, profile := range providerType.Profiles {
			if expected, checked := want[profile.ID]; checked && profile.DefaultBaseURL != expected {
				t.Errorf("%s endpoint is %q, want %q", profile.ID, profile.DefaultBaseURL, expected)
			}
			if profile.DefaultBaseURL == "" {
				t.Errorf("%s was served with no endpoint", profile.ID)
			}
		}
	}
}

// A form built from capability_requires_chat has to offer what the save accepts.
func TestAdminProviderProfilesCarriesTheChatDependency(t *testing.T) {
	runtime, cookie := providerProfilesFixture(t)
	view := fetchProviderProfiles(t, runtime, cookie)

	if len(view.CapabilityNames) != len(domain.CapabilityNames()) {
		t.Fatalf("served %d capability names, domain has %d", len(view.CapabilityNames), len(domain.CapabilityNames()))
	}
	known := make(map[string]bool, len(view.CapabilityNames))
	for _, name := range view.CapabilityNames {
		known[name] = true
	}
	if len(view.CapabilityRequiresChat) == 0 {
		t.Fatal("no chat dependency was served")
	}
	for _, name := range view.CapabilityRequiresChat {
		if !known[name] {
			t.Errorf("%q depends on chat but is not a served capability name", name)
		}
	}
}

// It is metadata about the build, not about anyone's data, so a read_only
// session reads it — the same bar as the other Admin reads a form needs. An
// unauthenticated caller still gets nothing.
func TestAdminProviderProfilesRequiresASession(t *testing.T) {
	runtime, _ := providerProfilesFixture(t)
	request := httptest.NewRequest(http.MethodGet, "/admin/api/v1/provider-profiles", nil)
	recorder := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(recorder, request)
	if recorder.Code == http.StatusOK {
		t.Fatalf("an unauthenticated caller was served the matrix: %s", recorder.Body.String())
	}
}

// Nothing here may carry a secret, and the response is compiled-in metadata, so
// this is a canary rather than a suspicion: it fails if the shape ever grows a
// field that reaches into stored credentials.
func TestAdminProviderProfilesCarriesNoStoredMaterial(t *testing.T) {
	runtime, cookie := providerProfilesFixture(t)
	body := fetchProviderProfilesBody(t, runtime, cookie)
	for _, forbidden := range []string{"secret", "ciphertext", "api_key", "credential_id", "gw_"} {
		if containsFold(body, forbidden) {
			t.Errorf("response contains %q: %s", forbidden, body)
		}
	}
}

func containsFold(haystack, needle string) bool {
	for index := 0; index+len(needle) <= len(haystack); index++ {
		matched := true
		for offset := 0; offset < len(needle); offset++ {
			a, b := haystack[index+offset], needle[offset]
			if 'A' <= a && a <= 'Z' {
				a += 'a' - 'A'
			}
			if 'A' <= b && b <= 'Z' {
				b += 'a' - 'A'
			}
			if a != b {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func providerProfilesFixture(t *testing.T) (*Runtime, *http.Cookie) {
	t.Helper()
	return providerProfilesFixtureWithConfig(t, testConfig(t))
}

func providerProfilesFixtureWithConfig(t *testing.T, cfg config.Config) (*Runtime, *http.Cookie) {
	t.Helper()
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapAdmin(
		context.Background(), cfg, "admin", []byte("correct horse battery staple"),
	); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runtime.Close() })
	cookie, _ := loginAdminForTest(t, runtime)
	return runtime, cookie
}

func fetchProviderProfiles(t *testing.T, runtime *Runtime, cookie *http.Cookie) providerProfilesView {
	t.Helper()
	var view providerProfilesView
	if err := json.Unmarshal([]byte(fetchProviderProfilesBody(t, runtime, cookie)), &view); err != nil {
		t.Fatal(err)
	}
	return view
}

func fetchProviderProfilesBody(t *testing.T, runtime *Runtime, cookie *http.Cookie) string {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/admin/api/v1/provider-profiles", nil)
	request.AddCookie(cookie)
	recorder := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("provider-profiles status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	return recorder.Body.String()
}
