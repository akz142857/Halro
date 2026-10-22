package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/anthropicapi"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/safetransport"
	"github.com/akz142857/Halro/internal/semantic"
)

// Every streaming profile reads its stream through the binding client, and
// every one of them, met by an upstream that answers 200 and then goes quiet,
// must end the attempt as an ambiguous timeout within the idle limit rather
// than wait out the stream's whole duration — and must say so in the shape the
// gateway reads, not as its own malformed-stream error, which on some paths is
// not ambiguous before the first event and would send a stalled attempt to a
// fallback.
//
// It walks the profile table rather than naming adapters, so a profile added
// later is held to the same thing without anyone remembering to add it here.
func TestEveryStreamingProfileEndsAStalledStreamAsAnAmbiguousTimeout(t *testing.T) {
	const idle = 50 * time.Millisecond
	checked := 0
	for _, profile := range domain.AllProviderProfiles() {
		capabilities := domain.DefaultProviderCapabilitiesForProfile(profile.Type, profile.ID)
		// Withheld profiles are refused by every write path, so no connection
		// can exist on one; the Bedrock Runtime stream is covered in its package.
		if !capabilities.Streaming || profile.Withheld {
			continue
		}
		t.Run(string(profile.ID), func(t *testing.T) {
			base := domain.ResolveBaseURL(profile.ID, "us-east-1")
			if base == "" || strings.Contains(base, "{") {
				base = "https://provider.invalid"
			}
			endpoint, err := url.Parse(base)
			if err != nil {
				t.Fatal(err)
			}
			var streamed bool
			stalled := make(chan struct{})
			t.Cleanup(func() { close(stalled) })
			client := &http.Client{Transport: provider.StreamIdleTransport{Idle: idle, Next: recordingTransport(func(request *http.Request) (*http.Response, error) {
				accept := request.Header.Get("Accept")
				streamed = streamed || strings.Contains(accept, "event-stream") || strings.Contains(accept, "eventstream")
				reader, writer := io.Pipe()
				go func() {
					// Headers, then nothing, until the test is over.
					<-stalled
					_ = writer.Close()
				}()
				return &http.Response{
					StatusCode: http.StatusOK, Request: request,
					Header: http.Header{"Content-Type": []string{strings.TrimSpace(strings.Split(accept, ",")[0])}},
					Body:   reader,
				}, nil
			})}}
			instance := domain.ProviderInstance{
				ID: "prov_1", Name: "stalling", Type: profile.Type, BaseURL: endpoint.String(), CredentialID: "cred_1",
				AccessSurface: profile.AccessSurface, ProfileID: profile.ID, CredentialScheme: profile.CredentialScheme,
				BedrockProjectID: "proj_1",
			}
			if profile.Type == domain.ProviderAzureOpenAI {
				instance.APIVersion = "2024-10-21"
			}
			secret := []byte("stall-key")
			if profile.CredentialScheme == domain.CredentialAnthropicOAuth {
				secret = []byte(`{"access_token":"sk-ant-oat01-test"}`)
			}
			binding := domain.ProviderProfileBinding{
				ID: domain.DefaultProviderProfileBindingID(instance.ID, profile.ID), ProviderID: instance.ID,
				ProfileID: profile.ID, AccessSurface: profile.AccessSurface, CredentialScheme: profile.CredentialScheme,
				Enabled: true, Capabilities: capabilities,
			}
			adapter, err := newProviderBindingAdapterWithClient(instance, binding, endpoint, secret, client)
			if err != nil {
				t.Fatal(err)
			}
			defer adapter.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			started := time.Now()
			if native, ok := adapter.(provider.NativeMessagesAdapter); ok {
				_, err = native.MessagesNativeStream(ctx, provider.NativeMessageCall{
					RequestID: "req_1", ProviderModel: "model", Version: "2023-06-01",
					Payload: []byte(`{"model":"model","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`),
				}, func(anthropicapi.RawStreamEvent) error { return nil })
			} else {
				_, err = adapter.ChatStream(ctx, provider.ChatCall{
					RequestID: "req_1", ProviderModel: "model",
					Request: openaiapi.ChatCompletionRequest{
						Model: "model", Stream: true,
						Messages: []openaiapi.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
					},
				}, func(semantic.Event) error { return nil })
			}
			if !streamed {
				t.Fatalf("no request asked for a stream, so the idle limit was never applied (err=%v)", err)
			}
			var classified *provider.Error
			if !errors.As(err, &classified) || classified.Class != provider.ErrorTimeout || !classified.Ambiguous ||
				classified.Retryable || !errors.Is(err, provider.ErrStreamIdle) {
				t.Fatalf("stalled stream ended with %#v (%v), want an ambiguous timeout from the idle limit", classified, err)
			}
			if waited := time.Since(started); waited > 5*time.Second {
				t.Fatalf("stalled stream took %s to end under a %s limit", waited, idle)
			}
		})
		checked++
	}
	if checked == 0 {
		t.Fatal("no streaming profile was found, so nothing was checked")
	}
}

// The walk above builds its own transport; this asserts the one a binding
// actually runs on carries the configured limit, which is where leaving the
// wrapper out would go unnoticed.
func TestBindingClientBoundsStreamsByTheConfiguredIdleLimit(t *testing.T) {
	cfg := testConfig(t)
	cfg.Gateway.UpstreamStreamIdleTimeout = config.Duration(42 * time.Second)
	endpoint, _ := url.Parse("https://api.openai.com")
	client, err := newBindingClient(cfg, domain.ProviderProfileBinding{AccessSurface: domain.SurfaceOpenAI}, endpoint,
		safetransport.Policy{RequireHTTPS: true, AllowedHosts: []string{endpoint.Hostname()}})
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := client.Transport.(provider.StreamIdleTransport)
	if !ok {
		t.Fatalf("binding client transport is %T, not bounded by the idle limit", client.Transport)
	}
	if transport.Idle != 42*time.Second {
		t.Fatalf("idle limit = %s, want the configured 42s", transport.Idle)
	}
	if _, ok := safetransport.PolicyOf(client); !ok {
		t.Fatal("wrapping the transport hid the SafeTransport policy")
	}
}
