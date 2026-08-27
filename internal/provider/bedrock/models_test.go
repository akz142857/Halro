package bedrock

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/provider"
)

func refuseEveryRequest(t *testing.T) *http.Client {
	t.Helper()
	return &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Fatalf("unexpected outbound request to %s", request.URL)
		return nil, nil
	})}
}

// A profile that accepts one model has nothing to ask upstream. Listing the
// whole account would offer the operator models this profile rejects, and the
// pinned model is the one the builtin catalog actually establishes — so this is
// also what makes a catalog-covered model reachable in the console.
func TestListInvocationTargetsAnswersPinnedProfilesWithoutCallingUpstream(t *testing.T) {
	for profile, pinned := range pinnedProfileModels {
		adapter := newProfileTestAdapter(t, refuseEveryRequest(t), profile)
		models, err := adapter.ListInvocationTargets(context.Background(), domain.TargetQuery{})
		adapter.Close()
		if err != nil {
			t.Fatalf("profile %q: %v", profile, err)
		}
		if len(models) != 1 || models[0].TargetID != pinned.Model {
			t.Fatalf("profile %q listed %#v, want only %q", profile, models, pinned.Model)
		}
	}
}

func TestPinnedInvocationTargetUsesQueryRegionWithoutControlPlane(t *testing.T) {
	adapter := newProfileTestAdapter(t, refuseEveryRequest(t), domain.ProfileBedrockAgentRerankCohere35)
	defer adapter.Close()
	targets, err := adapter.ListInvocationTargets(context.Background(), domain.TargetQuery{Region: "eu-west-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].Region != "eu-west-1" {
		t.Fatalf("targets=%#v", targets)
	}
}

func TestPinnedInvocationTargetFallsBackToValidatedAgentRuntimeRegion(t *testing.T) {
	adapter := newProfileTestAdapter(t, refuseEveryRequest(t), domain.ProfileBedrockAgentRerankCohere35)
	defer adapter.Close()
	targets, err := adapter.ListInvocationTargets(context.Background(), domain.TargetQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].Region != "us-east-1" {
		t.Fatalf("targets=%#v", targets)
	}
}

// Discovery lives on the control plane, which is a different host from every
// request that serves traffic. The signature has to name that host, not the
// runtime one the connection is bound to.
func TestListInvocationTargetsSignsTheControlPlaneHost(t *testing.T) {
	var seen *http.Request
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		seen = request
		return jsonResponse(http.StatusOK, `{"modelSummaries":[
			{"modelId":"amazon.nova-pro-v1:0","providerName":"Amazon","outputModalities":["TEXT"],
			 "inferenceTypesSupported":["ON_DEMAND"],"modelLifecycle":{"status":"ACTIVE"}}]}`), nil
	})}
	adapter := newTestAdapter(t, client)
	defer adapter.Close()

	models, err := adapter.ListInvocationTargets(context.Background(), domain.TargetQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].TargetID != "amazon.nova-pro-v1:0" || models[0].OwnedBy != "Amazon" {
		t.Fatalf("models=%#v", models)
	}
	if seen.URL.Host != "bedrock.us-east-1.amazonaws.com" || seen.URL.Path != "/foundation-models" {
		t.Fatalf("requested %s", seen.URL)
	}
	if seen.Method != http.MethodGet {
		t.Fatalf("method=%s", seen.Method)
	}
	authorization := seen.Header.Get("Authorization")
	if !strings.Contains(authorization, "/20260731/us-east-1/bedrock/aws4_request") {
		t.Fatalf("credential scope does not name the control plane: %q", authorization)
	}
	if !strings.Contains(authorization, "host") || strings.Contains(authorization, "test-secret") {
		t.Fatalf("signature is malformed or leaks the secret: %q", authorization)
	}
}

// Everything filtered out here is filtered because creating a deployment for it
// would fail later, not because the model is uninteresting.
func TestListInvocationTargetsOffersOnlyModelsADeploymentCanBePointedAt(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"modelSummaries":[
			{"modelId":"amazon.nova-pro-v1:0","providerName":"Amazon","outputModalities":["TEXT"],
			 "inferenceTypesSupported":["ON_DEMAND"],"modelLifecycle":{"status":"ACTIVE"}},
			{"modelId":"vendor.retired-v1:0","providerName":"Vendor","outputModalities":["TEXT"],
			 "inferenceTypesSupported":["ON_DEMAND"],"modelLifecycle":{"status":"LEGACY"}},
			{"modelId":"vendor.image-only-v1:0","providerName":"Vendor","outputModalities":["IMAGE"],
			 "inferenceTypesSupported":["ON_DEMAND"],"modelLifecycle":{"status":"ACTIVE"}},
			{"modelId":"vendor.provisioned-only-v1:0","providerName":"Vendor","outputModalities":["TEXT"],
			 "inferenceTypesSupported":["PROVISIONED"],"modelLifecycle":{"status":"ACTIVE"}},
			{"modelId":"vendor.via-profile-v1:0","providerName":"Vendor","outputModalities":["TEXT"],
			 "inferenceTypesSupported":["INFERENCE_PROFILE"],"modelLifecycle":{"status":"ACTIVE"}}]}`), nil
	})}
	adapter := newTestAdapter(t, client)
	defer adapter.Close()

	models, err := adapter.ListInvocationTargets(context.Background(), domain.TargetQuery{})
	if err != nil {
		t.Fatal(err)
	}
	var offered []string
	for _, model := range models {
		offered = append(offered, model.TargetID)
	}
	want := []string{"amazon.nova-pro-v1:0", "vendor.via-profile-v1:0"}
	if len(offered) != len(want) {
		t.Fatalf("offered %v, want %v", offered, want)
	}
	for index, id := range want {
		if offered[index] != id {
			t.Fatalf("offered %v, want %v", offered, want)
		}
	}
}

func TestBedrockMetadataMapsOnlyReviewedModalities(t *testing.T) {
	adapter := newTestAdapter(t, refuseEveryRequest(t))
	defer adapter.Close()
	target := domain.InvocationTargetDescriptor{
		TargetID: "vendor.future-v1:0",
		Metadata: domain.NormalizedModelMetadata{
			InputModalities: []string{"TEXT", "IMAGE", "AUDIO"}, OutputModalities: []string{"TEXT"},
		},
	}
	scope := domain.InvocationTargetScopeKey{ProviderID: "provider", TargetKind: domain.TargetBedrockFoundationModel, TargetID: target.TargetID, BindingID: "binding", ProfileID: domain.ProfileBedrockConverseText}
	claims := adapter.MapCapabilityClaims(target, scope, time.Now().UTC())
	if len(claims) != 2 || claims[0].CapabilityID != "chat" || claims[1].CapabilityID != "vision" {
		t.Fatalf("claims=%#v", claims)
	}
}

// The control plane states streaming outright, so this is declared evidence for
// free — but only when it says yes. Absent means the catalog has not spoken, and
// an explicit false is the upstream declining rather than Halro guessing;
// neither is evidence for the capability, and this adapter does not produce
// claims against one.
func TestBedrockReadsStreamingOnlyWhenTheCatalogueClaimsIt(t *testing.T) {
	adapter := newTestAdapter(t, refuseEveryRequest(t))
	defer adapter.Close()
	scope := domain.InvocationTargetScopeKey{
		ProviderID: "provider", TargetKind: domain.TargetBedrockFoundationModel,
		TargetID: "vendor.future-v1:0", BindingID: "binding", ProfileID: domain.ProfileBedrockConverseText,
	}
	claimed := func(streaming *bool) []string {
		target := domain.InvocationTargetDescriptor{
			TargetID: scope.TargetID,
			Metadata: domain.NormalizedModelMetadata{
				InputModalities: []string{"TEXT"}, OutputModalities: []string{"TEXT"}, ResponseStreaming: streaming,
			},
		}
		names := make([]string, 0, 2)
		for _, claim := range adapter.MapCapabilityClaims(target, scope, time.Now().UTC()) {
			names = append(names, claim.CapabilityID)
		}
		return names
	}
	yes, no := true, false
	if got := claimed(&yes); !slices.Equal(got, []string{"chat", "streaming"}) {
		t.Fatalf("an explicit yes did not become a claim: %v", got)
	}
	if got := claimed(&no); !slices.Equal(got, []string{"chat"}) {
		t.Fatalf("an explicit no produced a claim: %v", got)
	}
	if got := claimed(nil); !slices.Equal(got, []string{"chat"}) {
		t.Fatalf("silence produced a claim: %v", got)
	}
}

// An operator who approved a PrivateLink runtime endpoint said which endpoint
// this connection reaches. There is no control-plane host that stays inside
// that, so discovery reports itself unavailable and the console falls back to
// manual model entry — rather than the adapter reaching a public AWS host the
// operator never approved.
func TestListInvocationTargetsRefusesWhenNoControlPlaneStaysInsideTheApprovedEndpoint(t *testing.T) {
	endpoint, _ := url.Parse("https://vpce-0abc.bedrock-runtime.us-east-1.vpce.amazonaws.com")
	adapter, err := New(Options{
		Endpoint: endpoint, CredentialJSON: []byte(testCredential), Client: refuseEveryRequest(t),
		Now: func() time.Time { return time.Date(2026, 7, 31, 12, 34, 56, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()

	if adapter.controlPlaneEndpoint != nil {
		t.Fatalf("derived a control plane for a PrivateLink endpoint: %s", adapter.controlPlaneEndpoint)
	}
	_, err = adapter.ListInvocationTargets(context.Background(), domain.TargetQuery{})
	var classified *provider.Error
	if err == nil {
		t.Fatal("discovery reached upstream from a PrivateLink endpoint")
	}
	if !errors.As(err, &classified) || classified.Class != provider.ErrorBadRequest {
		t.Fatalf("classification=%#v err=%v", classified, err)
	}
}

func TestControlPlaneEndpointStaysInThePartitionAndRegion(t *testing.T) {
	for runtime, want := range map[string]string{
		"https://bedrock-runtime.us-east-1.amazonaws.com":       "bedrock.us-east-1.amazonaws.com",
		"https://bedrock-runtime-fips.us-east-1.amazonaws.com":  "bedrock-fips.us-east-1.amazonaws.com",
		"https://bedrock-runtime.us-east-1.api.aws":             "bedrock.us-east-1.api.aws",
		"https://bedrock-runtime.us-east-1.amazonaws.com.cn":    "bedrock.us-east-1.amazonaws.com.cn",
		"https://bedrock-agent-runtime.us-east-1.amazonaws.com": "",
	} {
		endpoint, _ := url.Parse(runtime)
		derived, ok := controlPlaneEndpointFor(endpoint, "us-east-1")
		if want == "" {
			if ok {
				t.Fatalf("%s derived %s, want no control plane", runtime, derived)
			}
			continue
		}
		if !ok || derived.Host != want {
			t.Fatalf("%s derived %v, want %s", runtime, derived, want)
		}
	}
	// A credential for another region cannot borrow this endpoint's control
	// plane: the derivation is keyed on the region that signs.
	endpoint, _ := url.Parse("https://bedrock-runtime.us-east-1.amazonaws.com")
	if _, ok := controlPlaneEndpointFor(endpoint, "eu-west-1"); ok {
		t.Fatal("derived a control plane across regions")
	}
}

func newProfileTestAdapter(t *testing.T, client *http.Client, profile domain.ProviderProfileID) *Adapter {
	t.Helper()
	host := "https://bedrock-runtime.us-east-1.amazonaws.com"
	if profile == domain.ProfileBedrockAgentRerankCohere35 {
		host = "https://bedrock-agent-runtime.us-east-1.amazonaws.com"
	}
	endpoint, _ := url.Parse(host)
	adapter, err := New(Options{
		Endpoint: endpoint, CredentialJSON: []byte(testCredential), Client: client, ProfileID: profile,
		Now: func() time.Time { return time.Date(2026, 7, 31, 12, 34, 56, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}
