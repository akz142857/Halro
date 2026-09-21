package gateway

import (
	"context"
	"slices"

	"github.com/akz142857/Halro/internal/auth"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
)

// modelOwner is what every listed alias reports as owned_by. The provider and
// model behind an alias are the operator's business; an application sees the
// alias alone, which is the whole point of having one.
const modelOwner = "halro"

// Models answers which public aliases the caller may put in a request's model
// field: the Project's allowed aliases, kept to the ones the route table serves.
//
// The guarantee is exactly this: a listed alias will not answer 404
// model_not_found. It is deliberately weaker than "a listed alias is callable",
// because resolveRequest's route check is ResolveCandidatesFor — it drops
// probe-unhealthy targets and then filters by operation — while this asks only
// whether the alias is carried at all. The list is therefore a superset of what
// is callable now, and the manifest's deviations say what an integrator should
// make of that.
//
// The one direction that cannot happen is the dangerous one: candidates are
// filtered out of the same slice this reads, so nothing callable is unlisted.
//
// No provider is consulted and nothing is accounted. The registry is the live
// one, so a route the operator added is listed as soon as it is served.
func (s *Service) Models(ctx context.Context, plaintextKey string) ([]openaiapi.Model, error) {
	principal, err := s.authenticateForDiscovery(ctx, plaintextKey)
	if err != nil {
		return nil, err
	}
	aliases := slices.Clone(principal.Project.AllowedModels)
	slices.Sort(aliases)
	aliases = slices.Compact(aliases)
	models := make([]openaiapi.Model, 0, len(aliases))
	for _, alias := range aliases {
		if !s.registry.Serves(alias) {
			continue
		}
		models = append(models, openaiapi.Model{ID: alias, Object: "model", Created: 0, OwnedBy: modelOwner})
	}
	return models, nil
}

// Model retrieves one alias by the same rule Models lists them. An alias the
// caller may not name answers 404 whether or not it exists on another Project:
// 403 would confirm the name, and this endpoint must not be a way to enumerate
// what other Projects were given.
func (s *Service) Model(ctx context.Context, plaintextKey, alias string) (openaiapi.Model, error) {
	principal, err := s.authenticateForDiscovery(ctx, plaintextKey)
	if err != nil {
		return openaiapi.Model{}, err
	}
	if !slices.Contains(principal.Project.AllowedModels, alias) || !s.registry.Serves(alias) {
		return openaiapi.Model{}, gatewayError("model_not_found", "model is not available to this project", 404, nil)
	}
	return openaiapi.Model{ID: alias, Object: "model", Created: 0, OwnedBy: modelOwner}, nil
}

// authenticateForDiscovery is the front of resolveRequest without the alias:
// the key must authenticate, carry the inference scope the listed aliases are
// for, and arrive from a source the Project admits. The policy-snapshot check is
// not applied — it guards the redaction and Token Guard a generation runs under,
// and no generation runs here.
func (s *Service) authenticateForDiscovery(ctx context.Context, plaintextKey string) (auth.AuthResult, error) {
	principal, err := s.auth.Authenticate(plaintextKey, s.now())
	if err != nil {
		return auth.AuthResult{}, gatewayError("invalid_api_key", "invalid API key", 401, err)
	}
	if !domain.HasGatewayScope(principal.Key.Scopes, domain.GatewayScopeInference) {
		return auth.AuthResult{}, gatewayError("gateway_key_scope_denied", "gateway key does not allow inference", 403, nil)
	}
	if err := authorizeSource(ctx, principal.Project); err != nil {
		return auth.AuthResult{}, err
	}
	return principal, nil
}
