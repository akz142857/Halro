package app

import (
	"context"
	"sort"

	"github.com/akz142857/Halro/internal/advisor"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

// doctorAdvisorInput assembles what the rules read from an offline data
// directory.
//
// Two of the five rules have no answer here and say so rather than reporting
// "ok": the admission gate and the refusal classifier both live in the running
// process, and doctor is a read-only view of a directory that may have no
// process behind it at all.
func doctorAdvisorInput(ctx context.Context, cfg config.Config, store *boltstore.Store) advisor.Input {
	input := advisorConfigInput(cfg)
	if store == nil {
		return input
	}
	routes, routeErr := store.ListRoutes(ctx)
	deployments, deploymentErr := store.ListDeployments(ctx)
	providers, providerErr := store.ListProviders(ctx)
	if routeErr != nil || deploymentErr != nil || providerErr != nil {
		return input
	}
	input.TopologyKnown = true
	input.WidestFanOut = widestFanOut(routes, deployments, providers)
	return input
}

// advisorConfigInput is the half of the input every caller has, online or off.
func advisorConfigInput(cfg config.Config) advisor.Input {
	return advisor.Input{
		AttemptResponseHeaderTimeout: cfg.Gateway.AttemptResponseHeaderTimeout.Value(),
		RouteTotalTimeout:            cfg.Gateway.RouteTotalTimeout.Value(),
		MaxTotalAttempts:             cfg.Gateway.MaxTotalAttempts,
		MaxAttemptsPerTarget:         cfg.Retry.MaxAttemptsPerTarget,
	}
}

// widestFanOut counts the candidates behind each public model and returns the
// largest.
//
// Candidates are walked per alias, so this — not the total number of routes —
// is what an attempt budget has to cover. A route whose deployment or provider
// is switched off produces no target and is not counted: it is already not
// reachable, and counting it would report a budget problem that disabling the
// deployment did not cause.
//
// Ties are broken by public model so two reads of an unchanged route table
// render identically.
func widestFanOut(routes []domain.Route, deployments []domain.Deployment, providers []domain.ProviderInstance) advisor.FanOut {
	providerEnabled := make(map[string]bool, len(providers))
	for _, item := range providers {
		providerEnabled[item.ID] = item.Enabled && item.DeletedAt == nil
	}
	deploymentEnabled := make(map[string]bool, len(deployments))
	for _, item := range deployments {
		deploymentEnabled[item.ID] = item.Enabled && item.DeletedAt == nil && providerEnabled[item.ProviderID]
	}
	counts := make(map[string]int, len(routes))
	for _, route := range routes {
		if !route.Enabled || route.DeletedAt != nil || !deploymentEnabled[route.DeploymentID] {
			continue
		}
		counts[route.PublicModel]++
	}
	aliases := make([]string, 0, len(counts))
	for alias := range counts {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	widest := advisor.FanOut{}
	for _, alias := range aliases {
		if counts[alias] > widest.Candidates {
			widest = advisor.FanOut{PublicModel: alias, Candidates: counts[alias]}
		}
	}
	return widest
}
