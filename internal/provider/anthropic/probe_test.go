package anthropic

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/anthropicapi"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/provider"
)

// A provider tested before any Deployment names a model has nothing to send to
// /v1/messages, and Halro's own validation refuses the empty model before the
// request reaches the network — reporting a local refusal as an upstream one.
// The catalog read is what makes the test answer the question it was asked.
func TestProbeWithoutDeploymentModelReadsModelCatalog(t *testing.T) {
	seen := 0
	adapter := newTestAdapter(t, func(writer http.ResponseWriter, request *http.Request) {
		seen++
		if request.Method != http.MethodGet || request.URL.Path != "/v1/models" {
			t.Errorf("probe did not read the model catalog: %s %s", request.Method, request.URL)
		}
		if request.URL.Query().Get("limit") != "1" {
			t.Errorf("probe asked for more than it needs: %s", request.URL.RawQuery)
		}
		if request.Header.Get("x-api-key") != "provider-secret" || request.Header.Get("anthropic-version") != anthropicapi.SupportedVersion {
			t.Errorf("unexpected probe headers: %v", request.Header)
		}
		writer.Header().Set("content-type", "application/json")
		_, _ = writer.Write([]byte(`{"data":[{"id":"claude-opus-4-6","display_name":"Claude Opus 4.6","type":"model"}],"has_more":true,"last_id":"claude-opus-4-6"}`))
	})
	if err := adapter.Probe(context.Background(), "   "); err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	if seen != 1 {
		t.Fatalf("catalog requests=%d", seen)
	}
}

// An account entitled to no models still has a working credential; the probe
// answers reachability, not entitlement.
func TestProbeWithoutDeploymentModelAcceptsEmptyCatalog(t *testing.T) {
	adapter := newTestAdapter(t, func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{"data":[],"has_more":false}`))
	})
	if err := adapter.Probe(context.Background(), ""); err != nil {
		t.Fatalf("empty catalog was treated as a failure: %v", err)
	}
}

// A 200 that is not the Models API's answer — a captive portal or proxy login
// page — must not read as a healthy provider.
func TestProbeWithoutDeploymentModelRejectsNonCatalogSuccess(t *testing.T) {
	adapter := newTestAdapter(t, func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{"message":"sign in to continue"}`))
	})
	err := adapter.Probe(context.Background(), "")
	var classified *provider.Error
	if !errors.As(err, &classified) || classified.Class != provider.ErrorMalformed {
		t.Fatalf("err=%v classified=%#v", err, classified)
	}
}

// The upstream's refusal of the catalog read is the operator's answer: a revoked
// key has to surface as authentication, not as a generic bad request.
func TestProbeWithoutDeploymentModelClassifiesUpstreamRefusal(t *testing.T) {
	adapter := newTestAdapter(t, func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusUnauthorized)
		_, _ = writer.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`))
	})
	err := adapter.Probe(context.Background(), "")
	var classified *provider.Error
	if !errors.As(err, &classified) || classified.Class != provider.ErrorAuthentication || classified.StatusCode != http.StatusUnauthorized {
		t.Fatalf("err=%v classified=%#v", err, classified)
	}
}

// A profile with no catalog to read — an Anthropic-compatible upstream reached
// through an explicit Messages path — cannot answer a credential-only test, and
// says which step is missing instead of failing on an empty model.
func TestProbeWithoutDeploymentModelOnCompatibleProfileNamesTheMissingDeployment(t *testing.T) {
	var reached int
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached++ }))
	t.Cleanup(server.Close)
	authorizer, err := provider.NewStaticHeaderAuthorizer(domain.CredentialAnthropicAPIKey, "x-api-key", "", []byte("provider-secret"), "Authorization")
	if err != nil {
		t.Fatal(err)
	}
	endpoint, _ := url.Parse(server.URL)
	adapter, err := New(Options{Endpoint: endpoint, Authorizer: authorizer, Client: server.Client(), MessagesPath: "/openai/v1/messages", ProviderType: "anthropic_compatible"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(adapter.Close)
	probeErr := adapter.Probe(context.Background(), "")
	var classified *provider.Error
	if !errors.As(probeErr, &classified) || classified.Class != provider.ErrorBadRequest || !strings.Contains(classified.Message, "deployment") {
		t.Fatalf("err=%v classified=%#v", probeErr, classified)
	}
	if reached != 0 {
		t.Fatalf("probe reached the upstream %d times", reached)
	}
}
