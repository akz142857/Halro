package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/requestmeta"
	"github.com/akz142857/Halro/internal/routegate"
)

// The list is what a request may name: the Project's allowed aliases, kept to
// the ones the route table serves. An alias granted to the Project but carried
// by no route would answer model_not_found on every other endpoint, so it is not
// advertised; an alias served by a route but not granted is another Project's
// business and never appears.
func TestModelsListsAllowedAliasesTheRouteTableServes(t *testing.T) {
	f := newFixture(t, 0)
	defer f.close()
	for _, alias := range []string{"embed", "chat-elsewhere"} {
		if err := f.registry.Register(provider.Target{ID: "target_" + alias, DeploymentID: "dep_" + alias, PublicModel: alias, ProviderModel: "provider-model", Adapter: f.adapter}); err != nil {
			t.Fatal(err)
		}
	}
	// "unrouted" is granted and unserved; "chat-elsewhere" is served and not
	// granted; "chat" is listed twice to show the answer is a set.
	f.project.AllowedModels = []string{"unrouted", "embed", "chat", "chat"}
	if err := f.service.auth.Refresh(context.Background(), source{keys: []domain.GatewayKey{f.key}, projects: []domain.Project{f.project}}); err != nil {
		t.Fatal(err)
	}

	models, err := f.service.Models(context.Background(), f.plaintext)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.ID)
		if model.Object != "model" || model.OwnedBy != "halro" || model.Created != 0 {
			t.Fatalf("model %+v discloses more than the alias", model)
		}
	}
	if len(ids) != 2 || ids[0] != "chat" || ids[1] != "embed" {
		t.Fatalf("listed %v, want [chat embed]", ids)
	}
	if f.adapter.calls != 0 {
		t.Fatalf("listing made %d provider calls", f.adapter.calls)
	}
}

// Retrieval follows the same rule, and refuses with the same status whether the
// alias is unserved, ungranted, or unknown: a 403 for the ungranted case would
// confirm that another Project has it.
func TestModelAnswersNotFoundForEveryAliasTheCallerMayNotName(t *testing.T) {
	f := newFixture(t, 0)
	defer f.close()
	if err := f.registry.Register(provider.Target{ID: "target_elsewhere", DeploymentID: "dep_elsewhere", PublicModel: "chat-elsewhere", ProviderModel: "provider-model", Adapter: f.adapter}); err != nil {
		t.Fatal(err)
	}
	f.project.AllowedModels = []string{"chat", "unrouted"}
	if err := f.service.auth.Refresh(context.Background(), source{keys: []domain.GatewayKey{f.key}, projects: []domain.Project{f.project}}); err != nil {
		t.Fatal(err)
	}

	model, err := f.service.Model(context.Background(), f.plaintext, "chat")
	if err != nil || model.ID != "chat" || model.Object != "model" {
		t.Fatalf("model=%+v err=%v", model, err)
	}
	for _, alias := range []string{"unrouted", "chat-elsewhere", "never-heard-of"} {
		_, err := f.service.Model(context.Background(), f.plaintext, alias)
		var failure *Error
		if !errors.As(err, &failure) || failure.HTTPStatus != 404 || failure.Code != "model_not_found" {
			t.Fatalf("alias %q: err=%v, want 404 model_not_found", alias, err)
		}
	}
}

func TestModelsRefusesAKeyThatCannotAuthenticate(t *testing.T) {
	f := newFixture(t, 0)
	defer f.close()
	_, err := f.service.Models(context.Background(), "gw_not_a_real_key")
	var failure *Error
	if !errors.As(err, &failure) || failure.HTTPStatus != 401 || failure.Code != "invalid_api_key" {
		t.Fatalf("err=%v, want 401 invalid_api_key", err)
	}
}

// The load-bearing deviation: an alias every deployment of which the gate has
// suspended stays listed, and the call is where the caller learns it is out.
//
// Without this, swapping ResolveAll for ResolveCandidates in Models — which
// inverts the published contract — is a green change: the fixture installs no
// eligibility gate, so nothing else in this package can tell the two apart.
func TestASuspendedAliasIsStillListedAndSaysSoOnlyWhenCalled(t *testing.T) {
	f := newFixture(t, 10_000)
	defer f.close()
	gate := routegate.New(routegate.Config{
		AvailabilityThreshold: 1, AvailabilityWindow: time.Minute, MaxAvailabilityWindow: time.Minute,
	})
	f.registry.SetEligibility(gate)
	service, err := NewServiceWithOptions(f.service.auth, f.registry, f.accounting, ServiceOptions{
		MaxAttempts: 1, RetryBaseDelay: time.Millisecond, RetryMaxDelay: time.Millisecond, RouteGate: gate,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Suspend every target behind "chat" by failing the one deployment it has.
	f.adapter.err = &provider.Error{Class: provider.ErrorProvider5xx, Retryable: true, Message: "down"}
	if _, err := service.Chat(context.Background(), f.plaintext, chatRequest()); err == nil {
		t.Fatal("the provider failure did not reach the caller")
	}
	if len(f.registry.ResolveCandidates("chat")) != 0 {
		t.Fatal("the gate did not suspend the alias, so this test proves nothing")
	}

	models, err := service.Models(context.Background(), f.plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "chat" {
		t.Fatalf("a fully suspended alias was hidden from the list: %+v", models)
	}
	if _, err := service.Model(context.Background(), f.plaintext, "chat"); err != nil {
		t.Fatalf("a fully suspended alias was not retrievable: %v", err)
	}

	// And the call is where it is reported, which is the half that makes
	// listing it honest rather than misleading.
	_, err = service.Chat(context.Background(), f.plaintext, chatRequest())
	var failure *Error
	if !errors.As(err, &failure) || failure.HTTPStatus != 503 {
		t.Fatalf("calling the listed-but-suspended alias gave %v, want a 503", err)
	}
}

// A key an operator scoped away from inference does not get handed the
// Project's alias menu. EffectiveGatewayScopes treats an empty list as
// inference, so the only keys this turns away are ones scoped deliberately.
func TestModelsRefusesAKeyScopedAwayFromInference(t *testing.T) {
	f := newFixture(t, 0)
	defer f.close()
	key := f.key
	key.Scopes = []domain.GatewayScope{domain.GatewayScopeGovernanceRead}
	if err := f.service.auth.Refresh(context.Background(), source{
		keys: []domain.GatewayKey{key}, projects: []domain.Project{f.project},
	}); err != nil {
		t.Fatal(err)
	}
	for _, call := range []func() error{
		func() error { _, err := f.service.Models(context.Background(), f.plaintext); return err },
		func() error { _, err := f.service.Model(context.Background(), f.plaintext, "chat"); return err },
	} {
		var failure *Error
		if err := call(); !errors.As(err, &failure) || failure.HTTPStatus != 403 || failure.Code != "gateway_key_scope_denied" {
			t.Fatalf("err=%v, want 403 gateway_key_scope_denied", err)
		}
	}
}

// The Project's CIDR allow-list governs this read as it governs an inference
// call: a Project that admits only one network does not answer its alias list
// to a request arriving from outside it, or from a request carrying no source
// at all.
func TestModelsHonoursTheProjectSourcePolicy(t *testing.T) {
	f := newFixtureShaped(t, 0, ledger.Options{}, nil, func(project *domain.Project) {
		project.AllowedCIDRs = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	}, nil)
	defer f.close()

	var failure *Error
	if _, err := f.service.Models(context.Background(), f.plaintext); !errors.As(err, &failure) ||
		failure.HTTPStatus != 403 || failure.Code != "source_not_allowed" {
		t.Fatalf("a request with no source was answered: %v", err)
	}
	outside := requestmeta.WithSourceIP(context.Background(), netip.MustParseAddr("203.0.113.9"))
	if _, err := f.service.Models(outside, f.plaintext); !errors.As(err, &failure) ||
		failure.Code != "source_not_allowed" {
		t.Fatalf("a request from outside the allow-list was answered: %v", err)
	}
	inside := requestmeta.WithSourceIP(context.Background(), netip.MustParseAddr("10.1.2.3"))
	models, err := f.service.Models(inside, f.plaintext)
	if err != nil || len(models) != 1 {
		t.Fatalf("a request from inside the allow-list was refused: models=%+v err=%v", models, err)
	}
}

// A Project whose every alias is unrouted lists nothing — and the nothing has
// to be an empty list rather than a nil one, because the OpenAI SDKs type
// `data` as a required array and a null is a deserialization failure at the
// client, not an empty page.
func TestModelsAnswersAnEmptyListRatherThanNothing(t *testing.T) {
	f := newFixture(t, 0)
	defer f.close()
	f.project.AllowedModels = []string{"unrouted"}
	if err := f.service.auth.Refresh(context.Background(), source{
		keys: []domain.GatewayKey{f.key}, projects: []domain.Project{f.project},
	}); err != nil {
		t.Fatal(err)
	}
	models, err := f.service.Models(context.Background(), f.plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if models == nil || len(models) != 0 {
		t.Fatalf("models=%#v, want a non-nil empty slice", models)
	}
	body, err := json.Marshal(openaiapi.ModelList{Object: "list", Data: models})
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != `{"object":"list","data":[]}` {
		t.Fatalf("serialized as %s", body)
	}
}

// Enumeration is its own grant. Before this endpoint existed an application
// knew only the alias its operator handed it, and a leaked key had to guess to
// reach anything else the Project allows; listing hands over the whole menu at
// once. That is a change to what a stolen key reveals rather than to what it
// may do — the budget, the rate limits and Token Guard all still bound it — so
// the operator decides per key, and a key that may call "chat" is not thereby
// told that "reasoning-expensive" exists.
func TestListingRequiresTheDiscoveryScopeSeparatelyFromInference(t *testing.T) {
	f := newFixture(t, 0)
	defer f.close()
	f.key.Scopes = []domain.GatewayScope{domain.GatewayScopeInference}
	if err := f.service.auth.Refresh(context.Background(), source{keys: []domain.GatewayKey{f.key}, projects: []domain.Project{f.project}}); err != nil {
		t.Fatal(err)
	}

	for name, call := range map[string]func() error{
		"list":     func() error { _, err := f.service.Models(context.Background(), f.plaintext); return err },
		"retrieve": func() error { _, err := f.service.Model(context.Background(), f.plaintext, "chat"); return err },
	} {
		var failure *Error
		if err := call(); !errors.As(err, &failure) || failure.HTTPStatus != 403 || failure.Code != "gateway_key_scope_denied" {
			t.Fatalf("%s: err = %v, want 403 gateway_key_scope_denied", name, err)
		}
	}
	// The key it refuses is a working inference key: the scope withholds the
	// menu, not the call.
	if _, err := f.service.Chat(context.Background(), f.plaintext, chatRequest()); err != nil {
		t.Fatalf("the inference the key does hold was refused: %v", err)
	}
}

// A key with no scopes at all is the narrowest key there is, and the unset
// default must not quietly widen to include enumeration.
func TestAKeyWithNoScopesCannotEnumerate(t *testing.T) {
	f := newFixture(t, 0)
	defer f.close()
	f.key.Scopes = nil
	if err := f.service.auth.Refresh(context.Background(), source{keys: []domain.GatewayKey{f.key}, projects: []domain.Project{f.project}}); err != nil {
		t.Fatal(err)
	}
	var failure *Error
	if _, err := f.service.Models(context.Background(), f.plaintext); !errors.As(err, &failure) || failure.HTTPStatus != 403 {
		t.Fatalf("err = %v, want 403: the unset default is inference alone", err)
	}
}
