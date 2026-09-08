package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/provider"
)

func TestBigModelWiringKeepsEachRegionOnItsOwnHostAndSurface(t *testing.T) {
	for _, test := range []struct {
		profile domain.ProviderProfileID
		surface domain.AccessSurface
		host    string
	}{
		{domain.ProfileBigModelCNChatEmbeddings, domain.SurfaceBigModelCNGeneral, "https://open.bigmodel.cn"},
		{domain.ProfileBigModelGlobalChat, domain.SurfaceBigModelGlobalGeneral, "https://api.z.ai"},
	} {
		t.Run(string(test.profile), func(t *testing.T) {
			endpoint, _ := url.Parse(test.host)
			var seen *http.Request
			var sent map[string]json.RawMessage
			client := &http.Client{Transport: recordingTransport(func(request *http.Request) (*http.Response, error) {
				seen = request
				body, _ := io.ReadAll(request.Body)
				if err := json.Unmarshal(body, &sent); err != nil {
					t.Fatalf("request body: %v: %s", err, body)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"chat_1","object":"chat.completion","created":1,"model":"glm-5.2","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))}, nil
			})}
			instance := domain.ProviderInstance{
				ID: "prov_1", Name: "bigmodel", Type: domain.ProviderBigModel, BaseURL: endpoint.String(), CredentialID: "cred_1",
				AccessSurface: test.surface, ProfileID: test.profile, CredentialScheme: domain.CredentialBigModelAPIKey,
			}
			binding := domain.ProviderProfileBinding{
				ID: domain.DefaultProviderProfileBindingID(instance.ID, test.profile), ProviderID: instance.ID,
				ProfileID: test.profile, AccessSurface: test.surface, CredentialScheme: domain.CredentialBigModelAPIKey,
				Enabled: true, Capabilities: domain.DefaultProviderCapabilitiesForProfile(domain.ProviderBigModel, test.profile),
			}
			adapter, err := newProviderBindingAdapterWithClient(instance, binding, endpoint, []byte("regional-key"), client)
			if err != nil {
				t.Fatal(err)
			}
			defer adapter.Close()
			_, err = adapter.Chat(context.Background(), provider.ChatCall{
				RequestID: "req_1", ProviderModel: "glm-5.2",
				Request: openaiapi.ChatCompletionRequest{Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("hi")}}, User: "halro-user"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if seen == nil || seen.URL.Scheme+"://"+seen.URL.Host != test.host || seen.URL.Path != "/api/paas/v4/chat/completions" {
				t.Fatalf("request=%v", seen)
			}
			if got := seen.Header.Get("Authorization"); got != "Bearer regional-key" {
				t.Fatalf("authorization=%q", got)
			}
			if string(sent["request_id"]) != `"req_1"` || string(sent["user_id"]) != `"halro-user"` {
				t.Fatalf("BigModel dialect was not selected: %#v", sent)
			}
		})
	}
}

func TestBigModelRegionalCredentialsAndConnectionsSaveThroughTheAdminAPI(t *testing.T) {
	cfg := testConfig(t)
	runtime, _ := openRuntimeWithPolicyForTest(t, cfg)
	cookie, csrf := loginAdminForTest(t, runtime)
	for _, test := range []struct {
		name, endpoint string
		surface        domain.AccessSurface
		profile        domain.ProviderProfileID
	}{
		{"bigmodel-cn", "https://open.bigmodel.cn", domain.SurfaceBigModelCNGeneral, domain.ProfileBigModelCNChatEmbeddings},
		{"bigmodel-global", "https://api.z.ai", domain.SurfaceBigModelGlobalGeneral, domain.ProfileBigModelGlobalChat},
	} {
		credentialResponse := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/credentials", "", map[string]any{
			"name": test.name, "type": "bigmodel", "base_url": test.endpoint, "secret": "regional-key",
			"access_surface": test.surface, "scheme": domain.CredentialBigModelAPIKey,
		})
		if credentialResponse.Code != http.StatusCreated {
			t.Fatalf("%s credential: status=%d body=%s", test.name, credentialResponse.Code, credentialResponse.Body.String())
		}
		var credential struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(credentialResponse.Body.Bytes(), &credential); err != nil {
			t.Fatal(err)
		}
		connectionResponse := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/providers", "", map[string]any{
			"name": test.name, "type": "bigmodel", "base_url": test.endpoint, "credential_id": credential.ID, "enabled": true,
			"access_surface": test.surface, "profile_id": test.profile, "credential_scheme": domain.CredentialBigModelAPIKey,
		})
		if connectionResponse.Code != http.StatusCreated {
			t.Fatalf("%s connection: status=%d body=%s", test.name, connectionResponse.Code, connectionResponse.Body.String())
		}
	}
}
