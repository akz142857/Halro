package gateway

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/requestmeta"
	"github.com/akz142857/Halro/internal/safelog"
)

// A gateway 502 answers the caller with a fixed sentence and no upstream detail,
// which is right — they are on the other side of a trust boundary — and used to
// be the only trace the failure left anywhere. Neither this package nor
// gatewayapi held a logger at all, so an operator holding a `provider_error`
// had no way to learn whether the dial was refused, the response would not
// decode, or the upstream returned 500.
func TestProviderAttemptFailureIsLoggedWithItsRouteAndClass(t *testing.T) {
	f := newFixture(t, 10_000)
	defer f.close()
	logs := &bytes.Buffer{}
	f.service.logger = safelog.New(slog.NewJSONHandler(logs, nil))
	f.adapter.err = &provider.Error{
		Class:   provider.ErrorConnect,
		Message: "provider request failed",
		Cause:   errors.New("dial: host is not on the provider allowlist: api.example.internal"),
	}

	ctx := requestmeta.WithRequestID(context.Background(), "req_logged")
	if _, err := f.service.Chat(ctx, f.plaintext, chatRequest()); err == nil {
		t.Fatal("the provider failure did not reach the caller")
	}

	logged := logs.String()
	for _, want := range []string{
		"provider attempt failed",
		`"request_id":"req_logged"`,
		`"public_model":"chat"`,
		`"deployment_id":"dep_target_1"`,
		`"error_class":"connect"`,
		"api.example.internal",
	} {
		if !strings.Contains(logged, want) {
			t.Fatalf("log is missing %s: %s", want, logged)
		}
	}
}

// The upstream's own sentence is a provider response body. It is the one place a
// credential is most likely to be quoted back, and a pattern denylist only knows
// the formats it was told about — so a failure the upstream classified is logged
// by class and status, never by body.
func TestAnUpstreamRefusalIsLoggedWithoutItsResponseBody(t *testing.T) {
	f := newFixture(t, 10_000)
	defer f.close()
	logs := &bytes.Buffer{}
	f.service.logger = safelog.New(slog.NewJSONHandler(logs, nil))
	f.adapter.err = &provider.Error{
		Class:             provider.ErrorProvider5xx,
		StatusCode:        503,
		ProviderRequestID: "req_upstream_9",
		Message:           "provider error (503): capacity exceeded for key ABSKQmVkcm9ja0FQSUtleUNhbmFyeQ==",
	}

	if _, err := f.service.Chat(context.Background(), f.plaintext, chatRequest()); err == nil {
		t.Fatal("the provider failure did not reach the caller")
	}

	logged := logs.String()
	if !strings.Contains(logged, `"provider_status":503`) || !strings.Contains(logged, "req_upstream_9") {
		t.Fatalf("the upstream's own classification was not logged: %s", logged)
	}
	if strings.Contains(logged, "capacity exceeded") || strings.Contains(logged, "ABSKQmVkcm9ja0FQSUtleUNhbmFyeQ==") {
		t.Fatalf("an upstream response body was written to the log: %s", logged)
	}
}

// The adapter goes out of its way to keep the upstream's identifier apart from
// its prose — the code, and the parameter it names, are the parts an operator can
// act on. They were extracted and then dropped: the log carried status and
// nothing else, so a 400 read as "the provider refused it" with no way to learn
// which field it refused.
func TestAnUpstreamRefusalIsLoggedWithTheCodeItNamed(t *testing.T) {
	f := newFixture(t, 10_000)
	defer f.close()
	logs := &bytes.Buffer{}
	f.service.logger = safelog.New(slog.NewJSONHandler(logs, nil))
	f.adapter.err = &provider.Error{
		Class:        provider.ErrorBadRequest,
		StatusCode:   400,
		ProviderCode: "invalid_image_url:messages[0].content[1].image_url",
		Message:      "provider error (400): Error while downloading https://example.test/photo.png",
	}

	if _, err := f.service.Chat(context.Background(), f.plaintext, chatRequest()); err == nil {
		t.Fatal("the provider failure did not reach the caller")
	}

	logged := logs.String()
	if !strings.Contains(logged, `"provider_code":"invalid_image_url:messages[0].content[1].image_url"`) {
		t.Fatalf("the identifier the upstream named was not logged: %s", logged)
	}
	// The sentence beside it is still a response body.
	if strings.Contains(logged, "Error while downloading") {
		t.Fatalf("an upstream response body was written to the log: %s", logged)
	}
}

// A service built without a logger must not panic on the first failure. Tests
// and embedders construct one that way, and a nil logger reached only on the
// error path is the shape that survives every green run.
func TestAServiceWithoutALoggerStillFailsQuietly(t *testing.T) {
	f := newFixture(t, 10_000)
	defer f.close()
	f.adapter.err = &provider.Error{Class: provider.ErrorConnect, Message: "provider request failed"}
	if _, err := f.service.Chat(context.Background(), f.plaintext, chatRequest()); err == nil {
		t.Fatal("the provider failure did not reach the caller")
	}
}
