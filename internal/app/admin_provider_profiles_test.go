package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
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

// The connection-level sets are what a form actually renders, and they are the
// server's own answer rather than something a caller recomputes from the
// per-profile ones. Two properties matter, and both are checked against the
// domain functions the write path uses: the ceiling a form offers is exactly the
// one a save accepts, and the defaults a form starts with are inside it.
func TestAdminProviderProfilesServesTheConnectionLevelSets(t *testing.T) {
	runtime, cookie := providerProfilesFixture(t)
	view := fetchProviderProfiles(t, runtime, cookie)

	for _, providerType := range view.ProviderTypes {
		for _, profile := range providerType.Profiles {
			wantCeiling := domain.ConnectionCeiling(providerType.Type, profile.ID)
			if profile.ConnectionCeiling != providerCapabilityViewOf(wantCeiling) {
				t.Errorf("%s connection ceiling differs from the domain answer: %#v", profile.ID, profile.ConnectionCeiling)
			}
			wantDefaults := domain.ConnectionDefaults(providerType.Type, profile.ID)
			if profile.ConnectionDefaults != providerCapabilityViewOf(wantDefaults) {
				t.Errorf("%s connection defaults differ from the domain answer: %#v", profile.ID, profile.ConnectionDefaults)
			}
			// Everything a form may tick has to survive the split, or the form can
			// produce a save the server refuses — the failure this endpoint exists
			// to make impossible.
			assignment := domain.AssignConnectionCapabilities(providerType.Type, profile.ID, wantCeiling)
			if len(assignment.Unservable) != 0 || len(assignment.Ambiguous) != 0 {
				t.Errorf("%s offers capabilities the save refuses: %+v", profile.ID, assignment)
			}
			peers := domain.ConnectionProfiles(providerType.Type, profile.ID)
			if len(profile.CombinesWith) != len(peers)-1 {
				t.Errorf("%s combines with %d profiles, the table says %d", profile.ID, len(profile.CombinesWith), len(peers)-1)
			}
		}
	}
}

// A capability whose consequence is not visible in a checkbox has to arrive
// marked, or the console has to keep its own list of which ones those are — the
// second copy this endpoint exists to remove.
func TestAdminProviderProfilesMarksTheCapabilitiesThatNeedAWarning(t *testing.T) {
	runtime, cookie := providerProfilesFixture(t)
	view := fetchProviderProfiles(t, runtime, cookie)

	known := make(map[string]bool, len(view.CapabilityNames))
	for _, name := range view.CapabilityNames {
		known[name] = true
	}
	if len(view.CapabilityOptInWarnings) == 0 {
		t.Fatal("no capability was marked as needing a warning")
	}
	for _, name := range view.CapabilityOptInWarnings {
		if !known[name] {
			t.Errorf("%q is marked for a warning but is not a served capability", name)
		}
	}
	// Enabling it accepts upstream egress that never passes through
	// SafeTransport, which is not something a checkbox conveys on its own.
	if !slices.Contains(view.CapabilityOptInWarnings, "provider_executed_tools") {
		t.Errorf("provider_executed_tools was served unmarked: %v", view.CapabilityOptInWarnings)
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
		// Two profiles have no endpoint to offer, and an empty field is the honest
		// answer: an Azure OpenAI resource and a compatibility server both live
		// wherever the operator put them.
		domain.ProfileAzureChatEmbeddings: "",
		domain.ProfileOpenAICompatible:    "",
	}
	for _, providerType := range view.ProviderTypes {
		for _, profile := range providerType.Profiles {
			expected, checked := want[profile.ID]
			if checked && profile.DefaultBaseURL != expected {
				t.Errorf("%s endpoint is %q, want %q", profile.ID, profile.DefaultBaseURL, expected)
			}
			if !checked && profile.DefaultBaseURL == "" {
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
	if len(view.CapabilityDependencies) == 0 {
		t.Fatal("no capability dependencies were served")
	}
	for name, needs := range view.CapabilityDependencies {
		if !known[name] {
			t.Errorf("%q has dependencies but is not a served capability name", name)
		}
		for _, need := range needs {
			if !known[need] {
				t.Errorf("%q depends on %q, which is not a served capability name", name, need)
			}
		}
	}
	// The link a flat "requires chat" list could not carry: stream usage stands
	// on streaming, which stands on chat. A form that only knew the first hop
	// offered stream usage with chat and no streaming, and the deployment
	// refused it.
	if got := view.CapabilityDependencies["stream_usage"]; len(got) != 1 || got[0] != "streaming" {
		t.Errorf("stream_usage depends on %v, want streaming", got)
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
