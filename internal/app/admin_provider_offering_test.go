package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

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

// BigModel is the type that carries two products: a metered general API and a
// Coding Plan subscription, each with mainland and international surfaces. This is what the
// console renders as a product choice and then a region choice.
func TestBigModelIsServedAsTwoProducts(t *testing.T) {
	view := buildProviderProfilesView("us-east-1")
	var bigmodel providerTypeView
	for _, providerType := range view.ProviderTypes {
		if providerType.Type == domain.ProviderBigModel {
			bigmodel = providerType
		}
	}
	kinds := map[domain.ProviderOfferingID]domain.ProviderOfferingKind{}
	regions := map[domain.ProviderOfferingID][]domain.ProviderRegionID{}
	for _, offering := range bigmodel.Offerings {
		kinds[offering.ID] = offering.Kind
		regions[offering.ID] = offering.Regions
		if offering.RegionScope != domain.RegionScopeFixed {
			t.Fatalf("offering %q has region scope %q, want fixed", offering.ID, offering.RegionScope)
		}
		if offering.ID == domain.OfferingBigModelCodingPlan {
			if !offering.RequiresUsageWarning {
				t.Fatal("the Coding Plan is served without its required usage warning")
			}
			if len(offering.Documentation) != 2 ||
				offering.Documentation[0].Region != domain.RegionCN ||
				offering.Documentation[0].URL != "https://docs.bigmodel.cn/cn/coding-plan/usage-notes" ||
				offering.Documentation[1].Region != domain.RegionGlobal ||
				offering.Documentation[1].URL != "https://docs.z.ai/devpack/usage-policy" {
				t.Fatalf("the Coding Plan usage terms are missing or attached to the wrong region: %#v", offering.Documentation)
			}
		}
	}
	if kinds[domain.OfferingBigModelGeneral] != domain.OfferingKindMeteredAPI {
		t.Fatalf("the general API is served as %q", kinds[domain.OfferingBigModelGeneral])
	}
	// The first subscription this build offers. The console reads this to tell a
	// plan apart from a metered API.
	if kinds[domain.OfferingBigModelCodingPlan] != domain.OfferingKindSubscription {
		t.Fatalf("the Coding Plan is served as %q, want a subscription", kinds[domain.OfferingBigModelCodingPlan])
	}
	if len(regions[domain.OfferingBigModelGeneral]) != 2 {
		t.Fatalf("the general API is served with regions %v, want two", regions[domain.OfferingBigModelGeneral])
	}
	if got := regions[domain.OfferingBigModelCodingPlan]; len(got) != 2 ||
		got[0] != domain.RegionCN || got[1] != domain.RegionGlobal {
		t.Fatalf("the Coding Plan is served with regions %v, want mainland and international", got)
	}
	profiles := map[domain.ProviderProfileID]domain.ProviderOfferingID{}
	for _, profile := range bigmodel.Profiles {
		profiles[profile.ID] = profile.OfferingID
	}
	if profiles[domain.ProfileBigModelCNCodingChat] != domain.OfferingBigModelCodingPlan {
		t.Fatalf("the Coding Plan profile resolves to offering %q", profiles[domain.ProfileBigModelCNCodingChat])
	}
	if profiles[domain.ProfileBigModelGlobalCodingChat] != domain.OfferingBigModelCodingPlan {
		t.Fatalf("the international Coding Plan profile resolves to offering %q", profiles[domain.ProfileBigModelGlobalCodingChat])
	}
}

func TestCodeSubscriptionOfferingsExposeOnlyValidatedProducts(t *testing.T) {
	view := buildProviderProfilesView("us-east-1")
	for _, providerType := range view.ProviderTypes {
		switch providerType.Type {
		case domain.ProviderKimi:
			for _, offering := range providerType.Offerings {
				if offering.ID == domain.OfferingKimiCode {
					t.Fatal("Kimi Code was served before its identity and protocol admission gates passed")
				}
			}
			for _, profile := range providerType.Profiles {
				if profile.ID == domain.ProfileKimiCodeOpenAIChat || profile.ID == domain.ProfileKimiCodeAnthropicMessages {
					t.Fatalf("withheld Kimi Code profile %q reached Admin metadata", profile.ID)
				}
			}
		case domain.ProviderMiniMax:
			var subscription *providerOfferingView
			for index := range providerType.Offerings {
				if providerType.Offerings[index].ID == domain.OfferingMiniMaxSubscriptionAccess {
					subscription = &providerType.Offerings[index]
				}
			}
			if subscription == nil || subscription.Kind != domain.OfferingKindEntitlement || !subscription.RequiresUsageWarning {
				t.Fatalf("MiniMax Subscription Access metadata=%#v", subscription)
			}
			if got := subscription.Regions; len(got) != 2 || got[0] != domain.RegionCN || got[1] != domain.RegionGlobal {
				t.Fatalf("MiniMax subscription regions=%v", got)
			}
			profiles := map[domain.ProviderProfileID]bool{}
			for _, profile := range providerType.Profiles {
				profiles[profile.ID] = true
			}
			if !profiles[domain.ProfileMiniMaxCNSubscriptionOpenAIChat] || !profiles[domain.ProfileMiniMaxGlobalSubscriptionOpenAIChat] {
				t.Fatalf("validated MiniMax subscription profiles are missing: %#v", profiles)
			}
			if profiles[domain.ProfileMiniMaxCNSubscriptionAnthropicMessages] || profiles[domain.ProfileMiniMaxGlobalSubscriptionAnthropicMessages] {
				t.Fatalf("unvalidated MiniMax Anthropic subscription profiles were served: %#v", profiles)
			}
		}
	}
}

func TestSubscriptionUsageWarningIsRequiredAndAuditedForCredentialAndConnection(t *testing.T) {
	cfg := testConfig(t)
	runtime, _ := openRuntimeWithPolicyForTest(t, cfg)
	cookie, csrf := loginAdminForTest(t, runtime)

	credentialInput := map[string]any{
		"name": "Coding Plan", "type": "bigmodel", "base_url": "https://open.bigmodel.cn",
		"secret": "provider-secret", "access_surface": domain.SurfaceBigModelCNCoding,
		"scheme": domain.CredentialBigModelCodingPlanKey,
	}
	refusedCredential := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost,
		"/admin/api/v1/credentials", "", credentialInput)
	if refusedCredential.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(refusedCredential.Body.String(), "usage_policy_acknowledgement_required") {
		t.Fatalf("unacknowledged subscription credential status=%d body=%s",
			refusedCredential.Code, refusedCredential.Body.String())
	}
	credentialInput["acknowledged_policy_revision"] = "bigmodel-coding-plan-cn-2026-09-10"
	createdCredential := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost,
		"/admin/api/v1/credentials", "", credentialInput)
	if createdCredential.Code != http.StatusCreated {
		t.Fatalf("acknowledged subscription credential status=%d body=%s",
			createdCredential.Code, createdCredential.Body.String())
	}
	var credential credentialView
	if err := json.Unmarshal(createdCredential.Body.Bytes(), &credential); err != nil {
		t.Fatal(err)
	}

	providerInput := map[string]any{
		"name": "Coding Plan", "type": "bigmodel", "base_url": "https://open.bigmodel.cn",
		"credential_id": credential.ID, "profile_id": domain.ProfileBigModelCNCodingChat,
		"access_surface":    domain.SurfaceBigModelCNCoding,
		"credential_scheme": domain.CredentialBigModelCodingPlanKey,
		"capabilities":      map[string]any{"chat": true, "streaming": true}, "enabled": true,
	}
	refusedProvider := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost,
		"/admin/api/v1/providers", "", providerInput)
	if refusedProvider.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(refusedProvider.Body.String(), "usage_policy_acknowledgement_required") {
		t.Fatalf("unacknowledged subscription connection status=%d body=%s",
			refusedProvider.Code, refusedProvider.Body.String())
	}
	providerInput["acknowledged_policy_revision"] = "bigmodel-coding-plan-cn-2026-09-10"
	createdProvider := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost,
		"/admin/api/v1/providers", "", providerInput)
	if createdProvider.Code != http.StatusCreated {
		t.Fatalf("acknowledged subscription connection status=%d body=%s",
			createdProvider.Code, createdProvider.Body.String())
	}

	auditResponse := authenticatedAdminGet(t, runtime, cookie, "/admin/api/v1/audit?limit=20")
	if auditResponse.Code != http.StatusOK {
		t.Fatalf("audit status=%d body=%s", auditResponse.Code, auditResponse.Body.String())
	}
	var page struct {
		Items []auditRecordView `json:"items"`
	}
	if err := json.Unmarshal(auditResponse.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	acknowledgements := 0
	for _, record := range page.Items {
		if record.Metadata["usage_warning_acknowledged"] != "true" {
			continue
		}
		acknowledgements++
		if record.Metadata["offering_id"] != string(domain.OfferingBigModelCodingPlan) ||
			record.Metadata["usage_policy_documentation"] != "https://docs.bigmodel.cn/cn/coding-plan/usage-notes" ||
			record.Metadata["usage_policy_revision"] != "bigmodel-coding-plan-cn-2026-09-10" {
			t.Fatalf("usage warning audit metadata is not bound to the exact product/profile: %#v", record.Metadata)
		}
		if record.Action == "provider.create" && record.Metadata["profile_id"] != string(domain.ProfileBigModelCNCodingChat) {
			t.Fatalf("connection acknowledgement is not bound to its profile: %#v", record.Metadata)
		}
		if _, exists := record.Metadata["profile_id"]; record.Action == "credential.create" && exists {
			t.Fatalf("credential acknowledgement invented a profile: %#v", record.Metadata)
		}
	}
	if acknowledgements != 2 {
		t.Fatalf("got %d usage warning acknowledgements, want credential and connection", acknowledgements)
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

	changeProduct := performAdminMutation(t, runtime, cookie, csrf, http.MethodPut,
		"/admin/api/v1/credentials/"+created.ID, fmt.Sprintf("%q", strconv.FormatUint(created.Revision+1, 10)), map[string]any{
			"name": "Z.AI Coding", "type": "bigmodel", "base_url": "https://api.z.ai", "secret": "coding-secret",
			"access_surface": domain.SurfaceBigModelGlobalCoding, "scheme": domain.CredentialBigModelCodingPlanKey,
			"current_password": stepUpTestPassword,
		})
	if changeProduct.Code != http.StatusBadRequest ||
		!strings.Contains(changeProduct.Body.String(), "credential_product_immutable") {
		t.Fatalf("product-changing rotation status=%d body=%s", changeProduct.Code, changeProduct.Body.String())
	}
}

func TestByEndpointCredentialRotationKeepsTheAccountRegion(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		current  string
		next     string
		wantFail bool
	}{
		{"same recognised region", "https://api.moonshot.cn:443:kimi", "https://api.moonshot.cn", false},
		{"recognised cross-region", "https://api.moonshot.cn:443:kimi", "https://api.moonshot.ai", true},
		{"recognised to custom", "https://api.moonshot.cn:443:kimi", "https://kimi.internal.example", true},
		{"custom to recognised", "https://kimi.internal.example:443:kimi", "https://api.moonshot.cn", true},
		{"same custom endpoint", "https://old.internal.example:kimi", "https://old.internal.example", false},
		{"custom to another custom endpoint", "https://old.internal.example:443:kimi", "https://new.internal.example", true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			current := domain.Credential{
				Type: domain.ProviderKimi, AccessSurface: domain.SurfaceKimi, Audience: testCase.current,
			}
			err := validateCredentialRotationRegion(current, testCase.next)
			if (err != nil) != testCase.wantFail {
				t.Fatalf("validateCredentialRotationRegion() error=%v, wantFail=%v", err, testCase.wantFail)
			}
		})
	}
}

func TestCredentialCreationRejectsAHostPublishedByAnotherProductSurface(t *testing.T) {
	for _, testCase := range []struct {
		name         string
		surface      domain.AccessSurface
		providerType domain.ProviderType
		endpoint     string
	}{
		{"MiniMax subscription on general API", domain.SurfaceMiniMaxCNSubscription, domain.ProviderMiniMax, "https://api.minimaxi.com"},
		{"Kimi Code on Open Platform", domain.SurfaceKimiCode, domain.ProviderKimi, "https://api.moonshot.ai"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if err := validateCredentialProductRegion(testCase.surface, testCase.providerType, testCase.endpoint); err == nil {
				t.Fatal("credential product accepted an endpoint published by another surface")
			}
		})
	}
}

func TestCredentialCreationRejectsAKnownHostFromAnotherFixedRegion(t *testing.T) {
	cfg := testConfig(t)
	runtime, _ := openRuntimeWithPolicyForTest(t, cfg)
	cookie, csrf := loginAdminForTest(t, runtime)

	wrongRegion := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost,
		"/admin/api/v1/credentials", "", map[string]any{
			"name": "misbound", "type": "bigmodel", "base_url": "https://api.z.ai", "secret": "provider-secret",
			"access_surface": domain.SurfaceBigModelCNGeneral, "scheme": domain.CredentialBigModelAPIKey,
		})
	if wrongRegion.Code != http.StatusBadRequest || !strings.Contains(wrongRegion.Body.String(), "region") {
		t.Fatalf("known cross-region host status=%d body=%s", wrongRegion.Code, wrongRegion.Body.String())
	}

	fronted := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost,
		"/admin/api/v1/credentials", "", map[string]any{
			"name": "fronted", "type": "bigmodel", "base_url": "https://bigmodel.internal.example", "secret": "provider-secret",
			"access_surface": domain.SurfaceBigModelCNGeneral, "scheme": domain.CredentialBigModelAPIKey,
		})
	if fronted.Code != http.StatusCreated {
		t.Fatalf("unknown proxy host status=%d body=%s", fronted.Code, fronted.Body.String())
	}

	// Existing installations may already contain a contradictory credential
	// written before the create guard existed. Creating a connection must not
	// make that legacy defect reachable; doctor remains the discovery path.
	now := time.Now().UTC()
	if _, err := runtime.store.PutCredential(context.Background(), domain.Credential{
		ID: "cred_legacy_cross_region", Name: "Legacy cross-region", Type: domain.ProviderBigModel,
		AccessSurface: domain.SurfaceBigModelCNGeneral, Scheme: domain.CredentialBigModelAPIKey,
		Audience: "https://api.z.ai:443:bigmodel", Ciphertext: []byte("sealed"), KeyVersion: 1,
		CreatedAt: now, UpdatedAt: now,
	}, 0, nil); err != nil {
		t.Fatal(err)
	}
	connection := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost,
		"/admin/api/v1/providers", "", map[string]any{
			"name": "Legacy cross-region", "type": "bigmodel", "base_url": "https://api.z.ai",
			"credential_id": "cred_legacy_cross_region", "profile_id": domain.ProfileBigModelCNChatEmbeddings,
			"access_surface": domain.SurfaceBigModelCNGeneral, "credential_scheme": domain.CredentialBigModelAPIKey,
			"capabilities": map[string]any{"chat": true}, "enabled": true,
		})
	if connection.Code != http.StatusBadRequest ||
		!strings.Contains(connection.Body.String(), "credential_region_mismatch") {
		t.Fatalf("legacy cross-region connection status=%d body=%s", connection.Code, connection.Body.String())
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
