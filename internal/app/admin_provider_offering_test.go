package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
)

// What the console is told about products, and what happens when a request does
// not say which product it means.

func TestServedMatrixGivesEveryProfileAProductAndEveryProductAProfile(t *testing.T) {
	view := buildProviderProfilesView("us-east-1")
	for _, providerType := range view.ProviderTypes {
		offerings := make(map[domain.ProviderOfferingID]bool, len(providerType.Offerings))
		for _, offering := range providerType.Offerings {
			if offerings[offering.ID] {
				t.Fatalf("provider type %q lists offering %q twice", providerType.Type, offering.ID)
			}
			offerings[offering.ID] = true
		}
		used := make(map[domain.ProviderOfferingID]bool, len(offerings))
		for _, profile := range providerType.Profiles {
			if profile.OfferingID == "" {
				t.Fatalf("profile %s is served with no offering", profile.ID)
			}
			if !offerings[profile.OfferingID] {
				t.Fatalf("profile %s belongs to offering %q, which its type does not list",
					profile.ID, profile.OfferingID)
			}
			used[profile.OfferingID] = true
		}
		for id := range offerings {
			if !used[id] {
				t.Fatalf("offering %q is offered with no profile behind it; a form would present a"+
					" product it could not then save", id)
			}
		}
	}
}

// Withholding scopes what an operator may reach, and a product reachable only
// through withheld profiles is not reachable. Bedrock is the case in this build:
// the Runtime and Agent Runtime profiles are withheld, Mantle is not.
func TestWithheldOnlyOfferingIsNotServed(t *testing.T) {
	if !domain.IsWithheldProfile(domain.ProfileBedrockConverseText) {
		t.Skip("Bedrock Runtime is offered by this build, so it is expected in the served matrix")
	}
	view := buildProviderProfilesView("us-east-1")
	for _, providerType := range view.ProviderTypes {
		for _, offering := range providerType.Offerings {
			if offering.ID == domain.OfferingBedrockRuntime {
				t.Fatalf("the Bedrock Runtime product is served while every one of its profiles is withheld")
			}
		}
	}
}

// The one product axis this build actually has: BigModel's two regional
// products, which the console has to be able to tell apart because their
// capability sets differ.
func TestBigModelIsServedAsOneProductWithTwoRegions(t *testing.T) {
	view := buildProviderProfilesView("us-east-1")
	var bigmodel providerTypeView
	for _, providerType := range view.ProviderTypes {
		if providerType.Type == domain.ProviderBigModel {
			bigmodel = providerType
		}
	}
	if len(bigmodel.Offerings) != 1 {
		t.Fatalf("BigModel is served with %d offerings, want 1", len(bigmodel.Offerings))
	}
	offering := bigmodel.Offerings[0]
	if offering.RegionScope != domain.RegionScopeFixed {
		t.Fatalf("BigModel's region scope is %q, want fixed", offering.RegionScope)
	}
	if len(offering.Regions) != 2 {
		t.Fatalf("BigModel is served with regions %v, want two", offering.Regions)
	}
	regions := map[domain.ProviderRegionID]domain.ProviderProfileID{}
	for _, profile := range bigmodel.Profiles {
		regions[profile.RegionID] = profile.ID
	}
	if regions[domain.RegionCN] != domain.ProfileBigModelCNChatEmbeddings ||
		regions[domain.RegionGlobal] != domain.ProfileBigModelGlobalChat {
		t.Fatalf("BigModel regions resolve to %v", regions)
	}
	// The two endpoints are what an operator recognises the products by, and the
	// console shows them; a product spanning two surfaces has to carry both.
	hosts := make([]string, 0, len(offering.RegionHosts))
	for _, host := range offering.RegionHosts {
		hosts = append(hosts, host.Host)
	}
	if len(hosts) != 2 {
		t.Fatalf("BigModel is served with hosts %v, want both regional addresses", hosts)
	}
}

// One surface, several account hosts. The console renders this as a region
// choice that writes the endpoint field, which is only possible if the hosts
// arrive with it.
func TestByEndpointProductsCarryTheirHosts(t *testing.T) {
	view := buildProviderProfilesView("us-east-1")
	seen := 0
	for _, providerType := range view.ProviderTypes {
		for _, offering := range providerType.Offerings {
			if offering.RegionScope != domain.RegionScopeByEndpoint {
				continue
			}
			seen++
			if len(offering.RegionHosts) < 2 || len(offering.Regions) < 2 {
				t.Fatalf("offering %q reads its region from the endpoint but is served with %d host(s)"+
					" and %d region(s)", offering.ID, len(offering.RegionHosts), len(offering.Regions))
			}
		}
	}
	if seen == 0 {
		t.Fatal("no by-endpoint product is served; Kimi and MiniMax are both this shape")
	}
}

// The defect the Offering model exists to close. Until it landed the console
// sent no access surface and the server filled one in from the provider type's
// default profile, so every BigModel credential was sealed to the mainland
// surface — including the ones bound to the global host, which then carried the
// mainland profile's capability set and its embeddings claim.
//
// Refused, not guessed. A type with one product still resolves, because a
// question with one answer is not asked.
func TestCredentialWithoutAProductIsRefusedOnlyWhereThereIsAChoice(t *testing.T) {
	cfg := testConfig(t)
	runtime, _ := openRuntimeWithPolicyForTest(t, cfg)
	cookie, csrf := loginAdminForTest(t, runtime)

	unstated := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/credentials", "", map[string]any{
		"name": "Z.AI", "type": "bigmodel", "base_url": "https://api.z.ai", "secret": "provider-secret",
	})
	if unstated.Code != http.StatusBadRequest {
		t.Fatalf("a BigModel credential that names no product was accepted: status=%d body=%s",
			unstated.Code, unstated.Body.String())
	}
	body := unstated.Body.String()
	for _, want := range []string{"bigmodel.general-api/cn", "bigmodel.general-api/global", "access_surface"} {
		if !strings.Contains(body, want) {
			t.Fatalf("the refusal does not name %q, so it does not say what to send: %s", want, body)
		}
	}

	// The same request naming the global product is accepted and stored there.
	global := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/credentials", "", map[string]any{
		"name": "Z.AI", "type": "bigmodel", "base_url": "https://api.z.ai", "secret": "provider-secret",
		"access_surface": domain.SurfaceBigModelGlobalGeneral, "scheme": domain.CredentialBigModelAPIKey,
	})
	if global.Code != http.StatusCreated {
		t.Fatalf("create global BigModel credential: status=%d body=%s", global.Code, global.Body.String())
	}
	var created struct {
		AccessSurface domain.AccessSurface `json:"access_surface"`
	}
	if err := json.Unmarshal(global.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.AccessSurface != domain.SurfaceBigModelGlobalGeneral {
		t.Fatalf("a credential created for the global product was stored on %q", created.AccessSurface)
	}

	// A type with one product is unchanged: nothing to choose, nothing to ask.
	single := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/credentials", "", map[string]any{
		"name": "OpenAI", "type": "openai", "base_url": "https://api.openai.com", "secret": "provider-secret",
	})
	if single.Code != http.StatusCreated {
		t.Fatalf("an OpenAI credential that names no product was refused: status=%d body=%s",
			single.Code, single.Body.String())
	}
}

// A saved credential reads back as the product it was sealed to, so the console
// can show what an operator bought rather than only the internal surface. The
// region is derived, not stored: a by-endpoint product answers from the endpoint
// and a fronted one answers with nothing.
func TestCredentialViewReportsItsProductAndRegion(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		stored   domain.Credential
		offering domain.ProviderOfferingID
		region   domain.ProviderRegionID
	}{
		{
			name:     "a fixed-region product answers from its surface",
			stored:   domain.Credential{Type: domain.ProviderBigModel, AccessSurface: domain.SurfaceBigModelGlobalGeneral, Audience: "https://api.z.ai:443:bigmodel"},
			offering: domain.OfferingBigModelGeneral, region: domain.RegionGlobal,
		},
		{
			name:     "a by-endpoint product answers from the endpoint",
			stored:   domain.Credential{Type: domain.ProviderKimi, AccessSurface: domain.SurfaceKimi, Audience: "https://api.moonshot.cn:443:kimi"},
			offering: domain.OfferingKimiOpenPlatform, region: domain.RegionCN,
		},
		{
			name:     "a fronted endpoint reports no region rather than a guess",
			stored:   domain.Credential{Type: domain.ProviderKimi, AccessSurface: domain.SurfaceKimi, Audience: "https://gateway.internal:443:kimi"},
			offering: domain.OfferingKimiOpenPlatform, region: domain.RegionNone,
		},
		{
			name:     "a product with no region axis reports none",
			stored:   domain.Credential{Type: domain.ProviderOpenAI, AccessSurface: domain.SurfaceOpenAI, Audience: "https://api.openai.com:443:openai"},
			offering: domain.OfferingOpenAIAPI, region: domain.RegionNone,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			view := credentialViewFrom(testCase.stored)
			if view.OfferingID != testCase.offering || view.RegionID != testCase.region {
				t.Fatalf("got offering %q region %q, want %q and %q",
					view.OfferingID, view.RegionID, testCase.offering, testCase.region)
			}
		})
	}
}

// Rotation keeps the product the credential was sealed to. Changing product is
// a new credential, not a rotation, because the surface decides which endpoint
// and which capability set every connection built on it carries.
func TestRotationKeepsTheStoredProduct(t *testing.T) {
	cfg := testConfig(t)
	runtime, _ := openRuntimeWithPolicyForTest(t, cfg)
	cookie, csrf := loginAdminForTest(t, runtime)

	create := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/credentials", "", map[string]any{
		"name": "Z.AI", "type": "bigmodel", "base_url": "https://api.z.ai", "secret": "provider-secret",
		"access_surface": domain.SurfaceBigModelGlobalGeneral, "scheme": domain.CredentialBigModelAPIKey,
	})
	if create.Code != http.StatusCreated {
		t.Fatalf("create: status=%d body=%s", create.Code, create.Body.String())
	}
	var created struct {
		ID       string `json:"id"`
		Revision uint64 `json:"revision"`
	}
	if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	rotate := performAdminMutation(t, runtime, cookie, csrf, http.MethodPut,
		"/admin/api/v1/credentials/"+created.ID, fmt.Sprintf("%q", strconv.FormatUint(created.Revision, 10)), map[string]any{
			"name": "Z.AI", "type": "bigmodel", "base_url": "https://api.z.ai", "secret": "rotated-secret",
			// Replacing stored material is a trust-boundary change, so the
			// server asks again; the product identity being carried forward is
			// what this test is about, not the gate.
			"current_password": stepUpTestPassword,
		})
	if rotate.Code != http.StatusOK {
		t.Fatalf("rotate: status=%d body=%s", rotate.Code, rotate.Body.String())
	}
	var rotated struct {
		AccessSurface domain.AccessSurface `json:"access_surface"`
	}
	if err := json.Unmarshal(rotate.Body.Bytes(), &rotated); err != nil {
		t.Fatal(err)
	}
	if rotated.AccessSurface != domain.SurfaceBigModelGlobalGeneral {
		t.Fatalf("rotation moved the credential to %q", rotated.AccessSurface)
	}
}

// doctor is the only thing that can find the credentials the old guess already
// produced: both endpoints are legal, so nothing else in the build can tell a
// mis-sealed one from a deliberate one.
func TestDoctorReportsACredentialSealedToTheWrongProduct(t *testing.T) {
	checks := map[string]DoctorCheck{}
	add := func(name, status, detail string) { checks[name] = DoctorCheck{name, status, detail} }

	checkDoctorCredentialProducts([]domain.Credential{
		{Type: domain.ProviderBigModel, AccessSurface: domain.SurfaceBigModelCNGeneral, Audience: "https://api.z.ai:443:bigmodel"},
		{Type: domain.ProviderBigModel, AccessSurface: domain.SurfaceBigModelCNGeneral, Audience: "https://open.bigmodel.cn:443:bigmodel"},
		// A fronted endpoint is unknown, not wrong: an operator may put a proxy
		// in front of any upstream, and BaseURLTemplate is a prefill rather than
		// a bound.
		{Type: domain.ProviderBigModel, AccessSurface: domain.SurfaceBigModelCNGeneral, Audience: "https://gateway.internal:443:bigmodel"},
	}, add)

	check, reported := checks["credential_product"]
	if !reported {
		t.Fatal("doctor produced no credential_product check")
	}
	if check.Status != "fail" {
		t.Fatalf("doctor did not fail on a mis-sealed credential: %+v", check)
	}
	if !strings.Contains(check.Detail, "1 credential(s)") {
		t.Fatalf("doctor counted more or fewer than the one real mismatch: %q", check.Detail)
	}

	clear(checks)
	checkDoctorCredentialProducts([]domain.Credential{
		{Type: domain.ProviderBigModel, AccessSurface: domain.SurfaceBigModelGlobalGeneral, Audience: "https://api.z.ai:443:bigmodel"},
		{Type: domain.ProviderKimi, AccessSurface: domain.SurfaceKimi, Audience: "https://api.moonshot.cn:443:kimi"},
	}, add)
	if checks["credential_product"].Status != "pass" {
		t.Fatalf("doctor failed on credentials that agree with their product: %+v", checks["credential_product"])
	}
}
