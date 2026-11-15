package app

import "errors"

type configurationImportSelection struct {
	Mode               string   `json:"mode"`
	ProjectIDs         []string `json:"project_ids,omitempty"`
	RouteAliases       []string `json:"route_aliases,omitempty"`
	IncludeGatewayKeys bool     `json:"include_gateway_keys"`
}

type configurationSelectionOptions struct {
	Projects     []configurationObject `json:"projects"`
	RouteAliases []string              `json:"route_aliases"`
	GatewayKeys  int                   `json:"gateway_keys"`
}

func (b configurationBundle) selectionOptions() *configurationSelectionOptions {
	options := &configurationSelectionOptions{
		Projects:     make([]configurationObject, 0, len(b.Projects)),
		RouteAliases: make([]string, 0, len(b.Routes)),
		GatewayKeys:  len(b.GatewayKeys),
	}
	for _, project := range b.Projects {
		options.Projects = append(options.Projects, configurationObject{ID: project.ID, Name: project.Name})
	}
	seen := make(map[string]bool, len(b.Routes))
	for _, route := range b.Routes {
		if !seen[route.PublicModel] {
			options.RouteAliases = append(options.RouteAliases, route.PublicModel)
			seen[route.PublicModel] = true
		}
	}
	return options
}

func (b configurationBundle) counts() map[string]int {
	return map[string]int{
		"providers": len(b.Providers), "deployments": len(b.Deployments), "routes": len(b.Routes),
		"redaction_policies": len(b.RedactionPolicies), "token_guard_policies": len(b.TokenGuardPolicies),
		"projects": len(b.Projects), "gateway_keys": len(b.GatewayKeys),
	}
}

func (p *configurationImportPreview) selectConfiguration(b configurationBundle, selection configurationImportSelection) (configurationBundle, bool) {
	rootCount := len(selection.ProjectIDs) + len(selection.RouteAliases)
	switch {
	case selection.Mode == "all" && rootCount == 0:
		if !selection.IncludeGatewayKeys {
			b.GatewayKeys = nil
		}
		if b.recordCount() != 0 {
			return b, true
		}
	case selection.Mode == "selected" && rootCount > 0 && rootCount <= configurationRecordLimit:
		return p.selectConfigurationDependencies(b, selection)
	case selection.Mode != "selected" || rootCount != 0:
		p.issue("bundle", "", "invalid_selection", errors.New("invalid configuration import selection"))
		return configurationBundle{}, false
	}
	p.issue("bundle", "", "empty_selection", errors.New("select at least one project or model alias to import"))
	return configurationBundle{}, false
}

func (p *configurationImportPreview) selectConfigurationDependencies(b configurationBundle, selection configurationImportSelection) (configurationBundle, bool) {
	projects := make(map[string]bool, len(selection.ProjectIDs))
	for _, id := range selection.ProjectIDs {
		projects[id] = true
	}
	aliases := make(map[string]bool, len(selection.RouteAliases))
	for _, alias := range selection.RouteAliases {
		aliases[alias] = true
	}
	knownProjects := make(map[string]bool, len(b.Projects))
	for _, project := range b.Projects {
		knownProjects[project.ID] = true
	}
	knownAliases := make(map[string]bool, len(b.Routes))
	for _, route := range b.Routes {
		knownAliases[route.PublicModel] = true
	}
	for _, id := range selection.ProjectIDs {
		if !knownProjects[id] {
			p.issue("projects", id, "unknown_selected_project", errors.New("selected project is outside the bundle"))
		}
	}
	for _, alias := range selection.RouteAliases {
		if !knownAliases[alias] {
			p.issue("routes", alias, "unknown_selected_alias", errors.New("selected model alias is outside the bundle"))
		}
	}
	if len(p.Issues) != 0 {
		return configurationBundle{}, false
	}
	redaction, tokenGuard := map[string]bool{}, map[string]bool{}
	b.Projects = configurationRecordsMatching(b.Projects, func(project configurationProject) bool {
		if !projects[project.ID] {
			return false
		}
		for _, alias := range project.AllowedModels {
			aliases[alias] = true
		}
		redaction[project.RedactionPolicyID] = project.RedactionPolicyID != ""
		tokenGuard[project.TokenGuardPolicyID] = project.TokenGuardPolicyID != ""
		return true
	})
	deployments := make(map[string]bool)
	b.Routes = configurationRecordsMatching(b.Routes, func(route configurationRoute) bool {
		if !aliases[route.PublicModel] {
			return false
		}
		deployments[route.DeploymentID] = true
		return true
	})
	providers := make(map[string]bool)
	b.Deployments = configurationRecordsMatching(b.Deployments, func(deployment configurationDeployment) bool {
		if !deployments[deployment.ID] {
			return false
		}
		providers[deployment.ProviderID] = true
		return true
	})
	b.Providers = configurationRecordsMatching(b.Providers, func(provider configurationProvider) bool { return providers[provider.ID] })
	b.RedactionPolicies = configurationRecordsMatching(b.RedactionPolicies, func(policy configurationRedaction) bool { return redaction[policy.ID] })
	b.TokenGuardPolicies = configurationRecordsMatching(b.TokenGuardPolicies, func(policy configurationTokenGuard) bool { return tokenGuard[policy.ID] })
	b.GatewayKeys = configurationRecordsMatching(b.GatewayKeys, func(key configurationKey) bool {
		return selection.IncludeGatewayKeys && projects[key.ProjectID]
	})
	return b, true
}

func configurationRecordsMatching[T any](items []T, include func(T) bool) []T {
	var selected []T
	for _, item := range items {
		if include(item) {
			selected = append(selected, item)
		}
	}
	return selected
}
