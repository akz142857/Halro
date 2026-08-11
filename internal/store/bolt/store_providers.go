package bolt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	bbolt "go.etcd.io/bbolt"
)

func migrateProviderProfileBindings(tx *bbolt.Tx, step func(string) error) error {
	if err := migrationStep(step, "before_migrate_provider_profile_bindings"); err != nil {
		return err
	}
	providers, deployments := tx.Bucket(bucketProviders), tx.Bucket(bucketDeployments)
	if providers == nil || deployments == nil {
		return errors.New("provider profile binding migration buckets are missing")
	}
	providerByID := make(map[string]domain.ProviderInstance)
	if err := rewriteBucket(providers, func(raw []byte) ([]byte, error) {
		var instance domain.ProviderInstance
		if err := json.Unmarshal(raw, &instance); err != nil {
			return nil, err
		}
		normalizeProviderBindings(&instance)
		providerByID[instance.ID] = instance
		return json.Marshal(instance)
	}); err != nil {
		return fmt.Errorf("migrate provider profile bindings: %w", err)
	}
	if err := rewriteBucket(deployments, func(raw []byte) ([]byte, error) {
		var deployment domain.Deployment
		if err := json.Unmarshal(raw, &deployment); err != nil {
			return nil, err
		}
		if instance, ok := providerByID[deployment.ProviderID]; ok {
			normalizeDeploymentBinding(&deployment, instance)
		}
		return json.Marshal(deployment)
	}); err != nil {
		return fmt.Errorf("migrate deployment profile bindings: %w", err)
	}
	return migrationStep(step, "after_migrate_provider_profile_bindings")
}

func migrateProviderProfiles(tx *bbolt.Tx, step func(string) error) error {
	if err := migrationStep(step, "before_migrate_provider_profiles"); err != nil {
		return err
	}
	credentials := tx.Bucket(bucketCredentials)
	providers := tx.Bucket(bucketProviders)
	deployments := tx.Bucket(bucketDeployments)
	if credentials == nil || providers == nil || deployments == nil {
		return errors.New("provider profile migration buckets are missing")
	}
	if err := rewriteBucket(credentials, func(raw []byte) ([]byte, error) {
		var credential domain.Credential
		if err := json.Unmarshal(raw, &credential); err != nil {
			return nil, err
		}
		normalizeCredentialProfile(&credential)
		return json.Marshal(credential)
	}); err != nil {
		return fmt.Errorf("migrate credential profiles: %w", err)
	}
	providerByID := make(map[string]domain.ProviderInstance)
	if err := rewriteBucket(providers, func(raw []byte) ([]byte, error) {
		var instance domain.ProviderInstance
		if err := json.Unmarshal(raw, &instance); err != nil {
			return nil, err
		}
		normalizeProviderProfile(&instance, legacyCapabilityEvidence)
		providerByID[instance.ID] = instance
		return json.Marshal(instance)
	}); err != nil {
		return fmt.Errorf("migrate provider profiles: %w", err)
	}
	if err := rewriteBucket(deployments, func(raw []byte) ([]byte, error) {
		var deployment domain.Deployment
		if err := json.Unmarshal(raw, &deployment); err != nil {
			return nil, err
		}
		instance, ok := providerByID[deployment.ProviderID]
		if !ok {
			// Earlier migration fixtures and damaged legacy databases can contain
			// orphan deployments. Profile migration must remain atomic and must not
			// invent an access surface without a provider. Runtime topology checks
			// continue to reject these records if they are used.
			return raw, nil
		}
		normalizeDeploymentProfile(&deployment, instance, legacyCapabilityEvidence)
		return json.Marshal(deployment)
	}); err != nil {
		return fmt.Errorf("migrate deployment profiles: %w", err)
	}
	return migrationStep(step, "after_migrate_provider_profiles")
}

func normalizeCredentialProfile(credential *domain.Credential) {
	profile, ok := domain.DefaultProviderProfile(credential.Type)
	if !ok {
		return
	}
	if credential.AccessSurface == "" {
		credential.AccessSurface = profile.AccessSurface
	}
	if credential.Scheme == "" {
		credential.Scheme = profile.CredentialScheme
	}
}

func normalizeProviderProfile(instance *domain.ProviderInstance, evidence domain.CapabilityEvidence) {
	legacyProfile := instance.AccessSurface == "" && instance.ProfileID == "" && instance.CredentialScheme == ""
	profile, ok := domain.DefaultProviderProfile(instance.Type)
	if !ok {
		return
	}
	if instance.AccessSurface == "" {
		instance.AccessSurface = profile.AccessSurface
	}
	if instance.ProfileID == "" {
		instance.ProfileID = profile.ProfileID
	}
	if instance.CredentialScheme == "" {
		instance.CredentialScheme = profile.CredentialScheme
	}
	if legacyProfile && !instance.Capabilities.Chat && !instance.Capabilities.Embeddings {
		instance.Capabilities = domain.DefaultProviderCapabilities(instance.Type)
	}
	if instance.ProfileID == domain.ProfileBedrockConverseText {
		instance.Capabilities.DeveloperRole = false
	}
	if len(instance.CapabilityEvidence) == 0 {
		instance.CapabilityEvidence = domain.EvidenceForCapabilities(instance.Capabilities, evidence)
	}
	normalizeProviderBindings(instance)
}

func normalizeProviderBindings(instance *domain.ProviderInstance) {
	if len(instance.Bindings) == 0 && instance.ProfileID != "" {
		instance.Bindings = []domain.ProviderProfileBinding{{
			ID: domain.DefaultProviderProfileBindingID(instance.ID, instance.ProfileID), ProviderID: instance.ID,
			ProfileID: instance.ProfileID, AccessSurface: instance.AccessSurface,
			CredentialScheme: instance.CredentialScheme, Capabilities: instance.Capabilities,
			CapabilityEvidence: instance.CapabilityEvidence.Clone(), Enabled: instance.Enabled,
		}}
	}
	if len(instance.Bindings) != 0 {
		instance.Capabilities, instance.CapabilityEvidence = domain.BindingsCapabilitiesSummary(instance.Bindings)
	}
}

func (s *Store) PutCredential(ctx context.Context, credential domain.Credential, expectedRevision uint64, intent *domain.AdminAuditIntent) (domain.Credential, error) {
	normalizeCredentialProfile(&credential)
	if err := credential.Validate(); err != nil {
		return domain.Credential{}, err
	}
	if err := ctx.Err(); err != nil {
		return domain.Credential{}, err
	}
	err := s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketCredentials)
		if err := putVersioned(bucket, credential.ID, expectedRevision, &credential); err != nil {
			return err
		}
		return putAdminAuditIntentTx(tx, intent)
	})
	return credential, err
}

func (s *Store) GetCredential(ctx context.Context, id string) (domain.Credential, error) {
	var credential domain.Credential
	err := s.getJSON(ctx, bucketCredentials, id, &credential)
	return credential, err
}

func (s *Store) ListCredentials(ctx context.Context) ([]domain.Credential, error) {
	var credentials []domain.Credential
	err := s.listJSON(ctx, bucketCredentials, func(raw []byte) error {
		var credential domain.Credential
		if err := json.Unmarshal(raw, &credential); err != nil {
			return err
		}
		credentials = append(credentials, credential)
		return nil
	})
	sort.Slice(credentials, func(i, j int) bool { return credentials[i].ID < credentials[j].ID })
	return credentials, err
}

func (s *Store) DeleteCredential(ctx context.Context, id string, expectedRevision uint64, intent *domain.AdminAuditIntent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if id == "" || expectedRevision == 0 {
		return errors.New("credential id and expected revision are required")
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketCredentials)
		raw := bucket.Get([]byte(id))
		if raw == nil {
			return ErrNotFound
		}
		var header struct {
			Revision uint64 `json:"revision"`
		}
		if err := json.Unmarshal(raw, &header); err != nil {
			return err
		}
		if header.Revision != expectedRevision {
			return ErrRevisionConflict
		}
		if err := ensureCredentialUnreferenced(tx, id); err != nil {
			return err
		}
		if err := bucket.Delete([]byte(id)); err != nil {
			return err
		}
		return putAdminAuditIntentTx(tx, intent)
	})
}

func (s *Store) PutProvider(ctx context.Context, provider domain.ProviderInstance, expectedRevision uint64, intent *domain.AdminAuditIntent) (domain.ProviderInstance, error) {
	if err := provider.Validate(); err != nil {
		return domain.ProviderInstance{}, err
	}
	if err := ctx.Err(); err != nil {
		return domain.ProviderInstance{}, err
	}
	err := s.db.Update(func(tx *bbolt.Tx) error {
		rawCredential := tx.Bucket(bucketCredentials).Get([]byte(provider.CredentialID))
		if rawCredential == nil {
			return fmt.Errorf("credential %q: %w", provider.CredentialID, ErrNotFound)
		}
		var credential domain.Credential
		if err := json.Unmarshal(rawCredential, &credential); err != nil {
			return fmt.Errorf("decode credential %q: %w", provider.CredentialID, err)
		}
		normalizeCredentialProfile(&credential)
		if err := validateProviderCredentialProfile(provider, credential); err != nil {
			return err
		}
		deployments := tx.Bucket(bucketDeployments)
		if err := deployments.ForEach(func(_, raw []byte) error {
			if raw == nil {
				return nil
			}
			var deployment domain.Deployment
			if err := json.Unmarshal(raw, &deployment); err != nil {
				return err
			}
			if deployment.ProviderID != provider.ID || deployment.DeletedAt != nil {
				return nil
			}
			if err := validateDeploymentProviderProfile(deployment, provider); err != nil {
				return fmt.Errorf("deployment %q would become incompatible: %w", deployment.ID, err)
			}
			return nil
		}); err != nil {
			return err
		}
		if err := putVersioned(tx.Bucket(bucketProviders), provider.ID, expectedRevision, &provider); err != nil {
			return err
		}
		return putAdminAuditIntentTx(tx, intent)
	})
	return provider, err
}

func (s *Store) GetProvider(ctx context.Context, id string) (domain.ProviderInstance, error) {
	var provider domain.ProviderInstance
	err := s.getJSON(ctx, bucketProviders, id, &provider)
	if err == nil {
		normalizeProviderBindings(&provider)
	}
	return provider, err
}

func (s *Store) ListProviders(ctx context.Context) ([]domain.ProviderInstance, error) {
	var providers []domain.ProviderInstance
	err := s.listJSON(ctx, bucketProviders, func(raw []byte) error {
		var provider domain.ProviderInstance
		if err := json.Unmarshal(raw, &provider); err != nil {
			return err
		}
		normalizeProviderBindings(&provider)
		providers = append(providers, provider)
		return nil
	})
	sort.Slice(providers, func(i, j int) bool { return providers[i].ID < providers[j].ID })
	return providers, err
}

func validateProviderCredentialProfile(instance domain.ProviderInstance, credential domain.Credential) error {
	if credential.Type != instance.Type || credential.AccessSurface != instance.AccessSurface ||
		credential.Scheme != instance.CredentialScheme {
		return errors.New("provider credential profile is incompatible")
	}
	return nil
}

func providerCapabilitySubset(candidate, available domain.ProviderCapabilities) bool {
	return domain.ProviderCapabilitiesSubset(candidate, available)
}

func providerCapabilityLimitSubset(candidate, available int64) bool {
	if available == 0 {
		return candidate >= 0
	}
	return candidate > 0 && candidate <= available
}

func (s *Store) PutRoute(ctx context.Context, route domain.Route, expectedRevision uint64, intent *domain.AdminAuditIntent) (domain.Route, error) {
	if err := route.Validate(); err != nil {
		return domain.Route{}, err
	}
	if err := ctx.Err(); err != nil {
		return domain.Route{}, err
	}
	err := s.db.Update(func(tx *bbolt.Tx) error {
		if tx.Bucket(bucketDeployments).Get([]byte(route.DeploymentID)) == nil {
			return fmt.Errorf("deployment %q: %w", route.DeploymentID, ErrNotFound)
		}
		if err := putVersioned(tx.Bucket(bucketRoutes), route.ID, expectedRevision, &route); err != nil {
			return err
		}
		return putAdminAuditIntentTx(tx, intent)
	})
	return route, err
}

func (s *Store) GetRoute(ctx context.Context, id string) (domain.Route, error) {
	var route domain.Route
	err := s.getJSON(ctx, bucketRoutes, id, &route)
	return route, err
}

func (s *Store) ListRoutes(ctx context.Context) ([]domain.Route, error) {
	var routes []domain.Route
	err := s.listJSON(ctx, bucketRoutes, func(raw []byte) error {
		var route domain.Route
		if err := json.Unmarshal(raw, &route); err != nil {
			return err
		}
		routes = append(routes, route)
		return nil
	})
	sort.Slice(routes, func(i, j int) bool { return routes[i].ID < routes[j].ID })
	return routes, err
}

func ensureCredentialUnreferenced(tx *bbolt.Tx, credentialID string) error {
	for _, bucketName := range [][]byte{bucketProviders, bucketAlertWebhooks} {
		err := tx.Bucket(bucketName).ForEach(func(_, raw []byte) error {
			if raw == nil {
				return nil
			}
			var reference struct {
				CredentialID string     `json:"credential_id"`
				DeletedAt    *time.Time `json:"deleted_at,omitempty"`
			}
			if err := json.Unmarshal(raw, &reference); err != nil {
				return err
			}
			if reference.CredentialID == credentialID && reference.DeletedAt == nil {
				return ErrCredentialInUse
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) PutProviderResource(ctx context.Context, resource domain.ProviderResource, expectedRevision uint64) (domain.ProviderResource, error) {
	if err := resource.Validate(); err != nil {
		return domain.ProviderResource{}, err
	}
	err := s.db.Update(func(tx *bbolt.Tx) error {
		if tx.Bucket(bucketProjects).Get([]byte(resource.ProjectID)) == nil {
			return errors.New("resource project does not exist")
		}
		if tx.Bucket(bucketProviders).Get([]byte(resource.ProviderID)) == nil {
			return errors.New("resource provider does not exist")
		}
		if tx.Bucket(bucketDeployments).Get([]byte(resource.DeploymentID)) == nil {
			return errors.New("resource deployment does not exist")
		}
		bucket := tx.Bucket(bucketProviderResources)
		if expectedRevision == 0 && resource.IdempotencyKeyHash != ([32]byte{}) {
			if err := bucket.ForEach(func(key, raw []byte) error {
				var existing domain.ProviderResource
				if err := json.Unmarshal(raw, &existing); err != nil {
					return err
				}
				if existing.ProjectID == resource.ProjectID && existing.Kind == resource.Kind && existing.IdempotencyKeyHash == resource.IdempotencyKeyHash {
					return ErrAlreadyExists
				}
				return nil
			}); err != nil {
				return err
			}
		}
		return putVersioned(bucket, resource.ID, expectedRevision, &resource)
	})
	return resource, err
}

func (s *Store) ProviderResource(ctx context.Context, projectID, id string) (domain.ProviderResource, error) {
	var resource domain.ProviderResource
	if err := s.getJSON(ctx, bucketProviderResources, id, &resource); err != nil {
		return resource, err
	}
	if resource.ProjectID != projectID {
		return domain.ProviderResource{}, ErrNotFound
	}
	return resource, nil
}

// ListProviderResources returns every persisted resource owner mapping. Backup
// uses this while holding the data-directory lock so it can include exactly
// the local objects referenced by the metadata snapshot and reject a missing
// object instead of publishing an incomplete archive.
func (s *Store) ListProviderResources(ctx context.Context) ([]domain.ProviderResource, error) {
	var resources []domain.ProviderResource
	err := s.listJSON(ctx, bucketProviderResources, func(raw []byte) error {
		var resource domain.ProviderResource
		if err := json.Unmarshal(raw, &resource); err != nil {
			return err
		}
		resources = append(resources, resource)
		return nil
	})
	return resources, err
}

func (s *Store) ProviderResourceByIdempotency(ctx context.Context, projectID string, kind domain.ProviderResourceKind, keyHash [32]byte) (domain.ProviderResource, error) {
	var found domain.ProviderResource
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketProviderResources).ForEach(func(_, raw []byte) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			var resource domain.ProviderResource
			if err := json.Unmarshal(raw, &resource); err != nil {
				return err
			}
			if resource.ProjectID == projectID && resource.Kind == kind && resource.IdempotencyKeyHash == keyHash {
				found = resource
				return errStopIteration
			}
			return nil
		})
	})
	if errors.Is(err, errStopIteration) {
		return found, nil
	}
	if err != nil {
		return found, err
	}
	return found, ErrNotFound
}

func (s *Store) DeleteProviderResource(ctx context.Context, projectID, id string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketProviderResources)
		raw := bucket.Get([]byte(id))
		if raw == nil {
			return ErrNotFound
		}
		var resource domain.ProviderResource
		if err := json.Unmarshal(raw, &resource); err != nil {
			return err
		}
		if resource.ProjectID != projectID {
			return ErrNotFound
		}
		return bucket.Delete([]byte(id))
	})
}

func (s *Store) ExpiredProviderResources(ctx context.Context, now time.Time) ([]domain.ProviderResource, error) {
	var expired []domain.ProviderResource
	err := s.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketProviderResources)
		cursor := bucket.Cursor()
		for key, raw := cursor.First(); key != nil; key, raw = cursor.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			var resource domain.ProviderResource
			if err := json.Unmarshal(raw, &resource); err != nil {
				return err
			}
			if !resource.ExpiresAt.After(now) && resource.ExpiryReapable() {
				expired = append(expired, resource)
			}
		}
		return nil
	})
	return expired, err
}
