package bolt

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

// tombstoneChain builds credential → provider → deployment → project so each
// test can tombstone one link and probe the writes that reference it.
func tombstoneChain(t *testing.T) (*Store, domain.ProviderInstance, domain.Deployment, domain.Project) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	now := time.Now().UTC()
	profile, _ := domain.DefaultProviderProfile(domain.ProviderOpenAI)
	credential, err := store.PutCredential(ctx, domain.Credential{
		ID: "cred_tomb", Name: "OpenAI", Type: domain.ProviderOpenAI,
		AccessSurface: profile.AccessSurface, Scheme: profile.CredentialScheme,
		Audience: "audience", Ciphertext: []byte("ciphertext"), KeyVersion: 1,
		CreatedAt: now, UpdatedAt: now,
	}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	capabilities := domain.ProviderCapabilities{Chat: true, Streaming: true}
	instance, err := store.PutProvider(ctx, domain.ProviderInstance{
		ID: "provider_tomb", Name: "OpenAI", Type: domain.ProviderOpenAI,
		AccessSurface: profile.AccessSurface, ProfileID: profile.ProfileID, CredentialScheme: profile.CredentialScheme,
		BaseURL: "https://api.openai.com", CredentialID: credential.ID, AllowedHosts: []string{"api.openai.com"},
		Capabilities:       capabilities,
		CapabilityEvidence: domain.EvidenceForCapabilities(capabilities, domain.EvidenceDeclared),
		CreatedAt:          now, UpdatedAt: now,
	}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	deployment, err := store.PutDeployment(ctx, domain.Deployment{
		ID: "deployment_tomb", Name: "Valid", ProviderID: instance.ID, ProviderModel: "model",
		AccessSurface: instance.AccessSurface, ProfileID: instance.ProfileID, Capabilities: capabilities,
		CapabilityEvidence:      domain.EvidenceForCapabilities(capabilities, domain.EvidenceDeclared),
		ModelCapabilitySnapshot: domain.DeclaredCapabilitySnapshot("model", "sha256:test", capabilities, now),
		CreatedAt:               now, UpdatedAt: now,
	}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.PutProject(ctx, domain.Project{
		ID: "project_tomb", Name: "Owner", Enabled: true, AllowedModels: []string{"model"},
		CreatedAt: now, UpdatedAt: now,
	}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	return store, instance, deployment, project
}

func tombstoneDeployment(t *testing.T, store *Store, deployment domain.Deployment) domain.Deployment {
	t.Helper()
	now := time.Now().UTC()
	deployment.Enabled = false
	deployment.DeletedAt = &now
	deployment.UpdatedAt = now
	updated, err := store.PutDeployment(context.Background(), deployment, deployment.Revision, nil)
	if err != nil {
		t.Fatal(err)
	}
	return updated
}

func TestPutRouteRefusesLiveRouteOnTombstonedDeployment(t *testing.T) {
	store, _, deployment, _ := tombstoneChain(t)
	ctx := context.Background()
	now := time.Now().UTC()
	deployment = tombstoneDeployment(t, store, deployment)

	_, err := store.PutRoute(ctx, domain.Route{
		ID: "route_tomb", PublicModel: "model", DeploymentID: deployment.ID,
		Enabled: true, CreatedAt: now, UpdatedAt: now,
	}, 0, nil)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("live route on tombstoned deployment: err=%v", err)
	}
}

func TestPutRouteTombstoneWriteSurvivesTombstonedDeployment(t *testing.T) {
	store, _, deployment, _ := tombstoneChain(t)
	ctx := context.Background()
	now := time.Now().UTC()
	route, err := store.PutRoute(ctx, domain.Route{
		ID: "route_tomb", PublicModel: "model", DeploymentID: deployment.ID,
		Enabled: false, CreatedAt: now, UpdatedAt: now,
	}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	tombstoneDeployment(t, store, deployment)

	// The deployment went first; the route must still be deletable.
	route.Enabled = false
	route.DeletedAt = &now
	if _, err := store.PutRoute(ctx, route, route.Revision, nil); err != nil {
		t.Fatalf("route tombstone write refused after its deployment was removed: %v", err)
	}
}

func TestPutGatewayKeyRefusesLiveKeyOnTombstonedProject(t *testing.T) {
	store, _, _, project := tombstoneChain(t)
	ctx := context.Background()
	now := time.Now().UTC()
	project.Enabled = false
	project.DeletedAt = &now
	project, err := store.PutProject(ctx, project, project.Revision, nil)
	if err != nil {
		t.Fatal(err)
	}

	_, err = store.PutGatewayKey(ctx, domain.GatewayKey{
		ID: "key_tomb", ProjectID: project.ID, Name: "orphan", HashVersion: 1,
		KeyHash: [32]byte{1}, Enabled: true, CreatedAt: now,
	}, 0, nil)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("live key on tombstoned project: err=%v", err)
	}
}

func TestPutGatewayKeyTombstoneWriteSurvivesTombstonedProject(t *testing.T) {
	store, _, _, project := tombstoneChain(t)
	ctx := context.Background()
	now := time.Now().UTC()
	key, err := store.PutGatewayKey(ctx, domain.GatewayKey{
		ID: "key_tomb", ProjectID: project.ID, Name: "key", HashVersion: 1,
		KeyHash: [32]byte{1}, Enabled: true, CreatedAt: now,
	}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	project.Enabled = false
	project.DeletedAt = &now
	if _, err := store.PutProject(ctx, project, project.Revision, nil); err != nil {
		t.Fatal(err)
	}

	key.Enabled = false
	key.DeletedAt = &now
	if _, err := store.PutGatewayKey(ctx, key, key.Revision, nil); err != nil {
		t.Fatalf("key tombstone write refused after its project was removed: %v", err)
	}
}

func TestPutProviderResourceCreationRequiresLiveOwners(t *testing.T) {
	store, instance, deployment, project := tombstoneChain(t)
	ctx := context.Background()
	now := time.Now().UTC()
	resource := domain.ProviderResource{
		ID: "res_tomb", Kind: domain.ResourceFile, ProjectID: project.ID,
		ProviderID: instance.ID, DeploymentID: deployment.ID, PublicModel: "model",
		ProfileID: instance.ProfileID, CreationStatus: "completed", Status: "uploaded",
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	// An update survives a tombstoned owner: the batch is still being settled.
	created, err := store.PutProviderResource(ctx, resource, 0)
	if err != nil {
		t.Fatal(err)
	}
	tombstoneDeployment(t, store, deployment)
	created.Status = "processed"
	created.UpdatedAt = now
	if _, err := store.PutProviderResource(ctx, created, created.Revision); err != nil {
		t.Fatalf("resource update refused after its deployment was removed: %v", err)
	}
	// A new resource against the tombstoned owner does not.
	fresh := resource
	fresh.ID = "res_tomb_2"
	if _, err := store.PutProviderResource(ctx, fresh, 0); err == nil {
		t.Fatal("resource created against a tombstoned deployment")
	}
}
