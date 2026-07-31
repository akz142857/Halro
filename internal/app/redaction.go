package app

import (
	"context"
	"fmt"

	"github.com/akz142857/Heimdall/internal/redaction"
	boltstore "github.com/akz142857/Heimdall/internal/store/bolt"
)

func loadRedaction(
	ctx context.Context,
	store *boltstore.Store,
) (*redaction.Engine, error) {
	policies, err := store.ListRedactionPolicies(ctx)
	if err != nil {
		return nil, fmt.Errorf("list redaction policies: %w", err)
	}
	engine, err := redaction.New(policies)
	if err != nil {
		return nil, fmt.Errorf("load redaction policies: %w", err)
	}
	projects, err := store.ListProjects(ctx)
	if err != nil {
		return nil, fmt.Errorf("list projects for redaction: %w", err)
	}
	for _, project := range projects {
		if project.DeletedAt == nil && project.Enabled && project.RedactionPolicyID != "" &&
			!engine.HasPolicy(project.RedactionPolicyID) {
			return nil, fmt.Errorf(
				"project %q references unavailable redaction policy %q",
				project.ID, project.RedactionPolicyID,
			)
		}
	}
	return engine, nil
}
