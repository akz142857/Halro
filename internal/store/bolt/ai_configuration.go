package bolt

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/akz142857/Halro/internal/domain"
)

// PublishProjectAlias commits the new route, the additive Project change and its
// audit intent in the same journaled transaction. committed distinguishes a
// failed confirmation after local commit from a refused write.
func (s *Store) PublishProjectAlias(ctx context.Context, route domain.Route, projectID string, projectRevision, deploymentRevision uint64, intent *domain.AdminAuditIntent) (committed bool, err error) {
	if intent == nil {
		return false, errors.New("publishing a project alias requires an audit intent")
	}
	if err := route.Validate(); err != nil {
		return false, err
	}
	receipt, requiresConfirmation, err := s.updateAll([]func(*Tx) error{func(tx *Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var deployment domain.Deployment
		raw := tx.Bucket(bucketDeployments).Get([]byte(route.DeploymentID))
		if raw == nil {
			return ErrNotFound
		}
		if err := json.Unmarshal(raw, &deployment); err != nil {
			return err
		}
		if deployment.Revision != deploymentRevision || deployment.DeletedAt != nil || !deployment.Enabled {
			return ErrRevisionConflict
		}
		projects := tx.Bucket(bucketProjects)
		var project domain.Project
		raw = projects.Get([]byte(projectID))
		if raw == nil {
			return ErrNotFound
		}
		if err := json.Unmarshal(raw, &project); err != nil {
			return err
		}
		if project.Revision != projectRevision || project.DeletedAt != nil || !project.Enabled || slices.Contains(project.AllowedModels, route.PublicModel) {
			return ErrRevisionConflict
		}
		project.AllowedModels = append(project.AllowedModels, route.PublicModel)
		project.UpdatedAt = route.UpdatedAt
		if err := project.Validate(); err != nil {
			return err
		}
		if err := putVersioned(tx.Bucket(bucketRoutes), route.ID, 0, &route); err != nil {
			return err
		}
		if err := putVersioned(projects, project.ID, projectRevision, &project); err != nil {
			return err
		}
		return putAdminAuditIntentTx(tx, intent)
	}})
	if err != nil {
		return false, err
	}
	if requiresConfirmation && s.journal != nil {
		return true, s.journal.WaitConfirmed(ctx, receipt)
	}
	return true, nil
}
