package bolt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/akz142857/Halro/internal/domain"
	bbolt "go.etcd.io/bbolt"
)

func (s *Store) PutProject(ctx context.Context, project domain.Project, expectedRevision uint64, intent *domain.AdminAuditIntent) (domain.Project, error) {
	if err := project.Validate(); err != nil {
		return domain.Project{}, err
	}
	if err := ctx.Err(); err != nil {
		return domain.Project{}, err
	}
	err := s.db.Update(func(tx *bbolt.Tx) error {
		if err := putVersioned(tx.Bucket(bucketProjects), project.ID, expectedRevision, &project); err != nil {
			return err
		}
		return putAdminAuditIntentTx(tx, intent)
	})
	return project, err
}

// normalizeProject keeps slices non-nil so records written before normalization
// existed still marshal as JSON arrays rather than null.
func normalizeProject(project domain.Project) domain.Project {
	if project.AllowedRoutes == nil {
		project.AllowedRoutes = []string{}
	}
	return project
}

func (s *Store) GetProject(ctx context.Context, id string) (domain.Project, error) {
	var project domain.Project
	err := s.getJSON(ctx, bucketProjects, id, &project)
	return normalizeProject(project), err
}

func (s *Store) ListProjects(ctx context.Context) ([]domain.Project, error) {
	var projects []domain.Project
	err := s.listJSON(ctx, bucketProjects, func(raw []byte) error {
		var project domain.Project
		if err := json.Unmarshal(raw, &project); err != nil {
			return err
		}
		projects = append(projects, normalizeProject(project))
		return nil
	})
	sort.Slice(projects, func(i, j int) bool { return projects[i].ID < projects[j].ID })
	return projects, err
}

func (s *Store) PutGatewayKey(ctx context.Context, key domain.GatewayKey, expectedRevision uint64, intent *domain.AdminAuditIntent) (domain.GatewayKey, error) {
	if err := key.Validate(); err != nil {
		return domain.GatewayKey{}, err
	}
	if err := ctx.Err(); err != nil {
		return domain.GatewayKey{}, err
	}
	err := s.db.Update(func(tx *bbolt.Tx) error {
		if tx.Bucket(bucketProjects).Get([]byte(key.ProjectID)) == nil {
			return fmt.Errorf("project %q: %w", key.ProjectID, ErrNotFound)
		}
		keys := tx.Bucket(bucketGatewayKeys)
		index := tx.Bucket(bucketGatewayKeyHash)
		existingID := index.Get(key.KeyHash[:])
		if existingID != nil && !bytes.Equal(existingID, []byte(key.ID)) {
			return ErrKeyHashConflict
		}
		if err := putVersioned(keys, key.ID, expectedRevision, &key); err != nil {
			return err
		}
		if err := index.Put(key.KeyHash[:], []byte(key.ID)); err != nil {
			return err
		}
		return putAdminAuditIntentTx(tx, intent)
	})
	return key, err
}

func (s *Store) GetGatewayKey(ctx context.Context, id string) (domain.GatewayKey, error) {
	var key domain.GatewayKey
	err := s.getJSON(ctx, bucketGatewayKeys, id, &key)
	return key, err
}

func (s *Store) FindGatewayKeyByHash(ctx context.Context, hash [32]byte) (domain.GatewayKey, error) {
	if err := ctx.Err(); err != nil {
		return domain.GatewayKey{}, err
	}
	var key domain.GatewayKey
	err := s.db.View(func(tx *bbolt.Tx) error {
		id := tx.Bucket(bucketGatewayKeyHash).Get(hash[:])
		if id == nil {
			return ErrNotFound
		}
		raw := tx.Bucket(bucketGatewayKeys).Get(id)
		if raw == nil {
			return errors.New("gateway key hash index is inconsistent")
		}
		return json.Unmarshal(raw, &key)
	})
	return key, err
}

func (s *Store) ListGatewayKeys(ctx context.Context) ([]domain.GatewayKey, error) {
	var keys []domain.GatewayKey
	err := s.listJSON(ctx, bucketGatewayKeys, func(raw []byte) error {
		var key domain.GatewayKey
		if err := json.Unmarshal(raw, &key); err != nil {
			return err
		}
		keys = append(keys, key)
		return nil
	})
	sort.Slice(keys, func(i, j int) bool { return keys[i].ID < keys[j].ID })
	return keys, err
}
