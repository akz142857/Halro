package app

import (
	"context"
	"net/http"
	"sort"

	"github.com/go-chi/chi/v5"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/modelcatalog"
)

// adminDeploymentView is a deployment as the console reads it: the stored record
// plus the review comparison, which is derived per read because both sides of it
// move without the record being rewritten.
type adminDeploymentView struct {
	domain.Deployment
	CapabilityReview capabilityReview `json:"capability_review"`
}

// deploymentBinding resolves the profile binding a deployment runs against,
// falling back to the instance ceiling when the binding is gone. Registry load
// does the same thing (internal/app/providers.go); both go through here so a
// state the console shows cannot disagree with the state that admits traffic.
func deploymentBinding(instance domain.ProviderInstance, deployment domain.Deployment) domain.ProviderProfileBinding {
	if binding, bound := instance.ProfileBinding(deployment.BindingID); bound {
		return binding
	}
	return domain.ProviderProfileBinding{ProfileID: deployment.ProfileID, Capabilities: instance.Capabilities}
}

// reviewForDeployment derives the review against the deployment's own provider.
// A deployment whose provider has disappeared is reported as drifted rather than
// as current: nothing establishes its claims any more, and the fail-closed
// reading is the one that does not invent evidence.
func reviewForDeployment(instances map[string]domain.ProviderInstance, deployment domain.Deployment) capabilityReview {
	instance, ok := instances[deployment.ProviderID]
	if !ok {
		return capabilityReview{
			State:         domain.CapabilityReviewDrifted,
			Source:        deployment.ModelCapabilitySnapshot.Source,
			Status:        deployment.ModelCapabilitySnapshot.Status,
			ModelRevision: deployment.ModelCapabilitySnapshot.ModelRevision,
			Reason:        reviewReasonProfileNarrowed,
		}
	}
	return reviewCapabilities(deployment, deploymentBinding(instance, deployment), instance.Type)
}

func (r *Runtime) providerInstancesByID(ctx context.Context) (map[string]domain.ProviderInstance, error) {
	instances, err := r.store.ListProviders(ctx)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]domain.ProviderInstance, len(instances))
	for _, instance := range instances {
		if instance.DeletedAt == nil {
			byID[instance.ID] = instance
		}
	}
	return byID, nil
}

// routeCapabilityImpact is one route that a narrowing would affect.
type routeCapabilityImpact struct {
	RouteID     string `json:"route_id"`
	PublicModel string `json:"public_model"`
	Capability  string `json:"capability"`
	// SoleCandidate means no other enabled route on this public model is served
	// by a deployment that still has the capability. Requests to the public model
	// that need it will be rejected with unsupported_feature once this is saved.
	SoleCandidate bool `json:"sole_candidate"`
}

// capabilityPreflightResponse answers "what breaks if I save this" before the
// mutation, because the failure it predicts is otherwise only visible as
// 400 unsupported_feature on live traffic.
type capabilityPreflightResponse struct {
	Removed []string                `json:"removed_capabilities"`
	Added   []string                `json:"added_capabilities"`
	Routes  []routeCapabilityImpact `json:"affected_routes"`
	// Blocking is true when at least one affected route loses its only
	// candidate. The console requires an explicit confirmation for those.
	Blocking bool `json:"blocking"`
}

// preflightAdminDeploymentCapabilities reports which routes a proposed
// capability set would strand, without writing anything.
//
// Narrowing a deployment is allowed in place (§7.2 of the model-aware capability
// design), which is exactly why it needs a preflight: filterSemanticCapabilities
// drops candidates that lack a required capability, and a public model whose
// last capable candidate goes away starts answering 400. That is discoverable
// here for the cost of a read.
func (r *Runtime) preflightAdminDeploymentCapabilities(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		Capabilities domain.ProviderCapabilities `json:"capabilities"`
	}
	if err := decodeAdminJSON(request, &input); err != nil {
		adminBadRequest(writer, "invalid request")
		return
	}
	current, err := r.store.GetDeployment(request.Context(), chi.URLParam(request, "id"))
	if err != nil || current.DeletedAt != nil {
		adminNotFound(writer)
		return
	}
	routes, err := r.store.ListRoutes(request.Context())
	if err != nil {
		adminStoreError(writer)
		return
	}
	deployments, err := r.store.ListDeployments(request.Context())
	if err != nil {
		adminStoreError(writer)
		return
	}
	response := capabilityPreflight(current, input.Capabilities, routes, deployments)
	writeJSON(writer, http.StatusOK, response)
}

// capabilityPreflight is the whole decision, kept free of HTTP and storage so it
// can be tested against constructed topologies directly.
func capabilityPreflight(target domain.Deployment, proposed domain.ProviderCapabilities,
	routes []domain.Route, deployments []domain.Deployment) capabilityPreflightResponse {
	response := capabilityPreflightResponse{
		Removed: modelcatalog.LostCapabilities(target.Capabilities, proposed),
		Added:   modelcatalog.GainedCapabilities(target.Capabilities, proposed),
		Routes:  []routeCapabilityImpact{},
	}
	if len(response.Removed) == 0 {
		return response
	}

	deploymentByID := make(map[string]domain.Deployment, len(deployments))
	for _, deployment := range deployments {
		if deployment.DeletedAt == nil {
			deploymentByID[deployment.ID] = deployment
		}
	}
	// Candidates for a public model are the other enabled routes carrying it.
	// Routes served by this deployment are excluded from the alternatives on
	// purpose: a second route onto the same deployment is not an alternative,
	// because the narrowing removes the capability from both.
	siblingsByModel := make(map[string][]domain.Deployment)
	var affected []domain.Route
	for _, route := range routes {
		if route.DeletedAt != nil || !route.Enabled {
			continue
		}
		if route.DeploymentID == target.ID {
			affected = append(affected, route)
			continue
		}
		deployment, ok := deploymentByID[route.DeploymentID]
		if !ok || !deployment.Enabled {
			continue
		}
		siblingsByModel[route.PublicModel] = append(siblingsByModel[route.PublicModel], deployment)
	}

	for _, route := range affected {
		for _, capability := range response.Removed {
			sole := true
			for _, sibling := range siblingsByModel[route.PublicModel] {
				if modelcatalog.HasCapability(sibling.Capabilities, capability) {
					sole = false
					break
				}
			}
			if sole {
				response.Blocking = true
			}
			response.Routes = append(response.Routes, routeCapabilityImpact{
				RouteID: route.ID, PublicModel: route.PublicModel,
				Capability: capability, SoleCandidate: sole,
			})
		}
	}
	sort.SliceStable(response.Routes, func(i, j int) bool {
		if response.Routes[i].RouteID != response.Routes[j].RouteID {
			return response.Routes[i].RouteID < response.Routes[j].RouteID
		}
		return response.Routes[i].Capability < response.Routes[j].Capability
	})
	return response
}
