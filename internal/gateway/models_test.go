package gateway

import (
	"context"
	"errors"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/provider"
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
