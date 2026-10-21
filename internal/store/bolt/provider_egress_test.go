package bolt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

func TestProviderEgressProxyAndCredentialLifecycleIsAtomic(t *testing.T) {
	store, err := openForTest(t, filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	credential := domain.Credential{
		ID: "cred_proxy", Name: "Proxy authentication", Type: "provider_egress_proxy",
		Audience: "https://proxy.example.com:8443", Ciphertext: []byte("ciphertext"), KeyVersion: 1,
		CreatedAt: now, UpdatedAt: now,
	}
	proxy := domain.ProviderEgressProxy{
		ID: "corp-egress", Name: "Corporate egress", Kind: domain.ProviderEgressProxyKindHTTPConnect,
		Endpoint: credential.Audience, BasicAuthCredentialID: credential.ID, CreatedAt: now, UpdatedAt: now,
	}
	stored, err := store.PutProviderEgressProxy(ctx, proxy, 0, &credential, 0, "", nil)
	if err != nil || stored.Revision != 1 {
		t.Fatalf("stored=%#v err=%v", stored, err)
	}
	if _, err := store.GetCredential(ctx, credential.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteCredential(ctx, credential.ID, 1, nil); !errors.Is(err, ErrCredentialInUse) {
		t.Fatalf("proxy credential deletion err=%v", err)
	}

	providerJSON, err := json.Marshal(domain.ProviderInstance{ID: "provider_proxy", EgressProxyID: proxy.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.update(func(tx *Tx) error {
		return tx.Bucket(bucketProviders).Put([]byte("provider_proxy"), providerJSON)
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteProviderEgressProxy(ctx, proxy.ID, stored.Revision, nil); !errors.Is(err, ErrProviderEgressProxyInUse) {
		t.Fatalf("referenced proxy deletion err=%v", err)
	}
	if err := store.update(func(tx *Tx) error {
		return tx.Bucket(bucketProviders).Delete([]byte("provider_proxy"))
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteProviderEgressProxy(ctx, proxy.ID, stored.Revision, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetProviderEgressProxy(ctx, proxy.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted proxy lookup err=%v", err)
	}
	if _, err := store.GetCredential(ctx, credential.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("proxy credential survived proxy deletion: %v", err)
	}
}

func TestProviderEgressProxyStoreEnforcesAuthenticationOwnership(t *testing.T) {
	store, err := openForTest(t, filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	proxy := domain.ProviderEgressProxy{
		ID: "corp-egress", Name: "Corporate egress", Kind: domain.ProviderEgressProxyKindHTTPConnect,
		Endpoint: "https://proxy.example.com:8443", BasicAuthCredentialID: "cred_proxy",
		CreatedAt: now, UpdatedAt: now,
	}
	wrongType := domain.Credential{
		ID: "cred_proxy", Name: "Wrong type", Type: "internal_other",
		Audience: proxy.Endpoint, Ciphertext: []byte("ciphertext"), KeyVersion: 1,
		CreatedAt: now, UpdatedAt: now,
	}
	if _, err := store.PutProviderEgressProxy(ctx, proxy, 0, &wrongType, 0, "", nil); err == nil {
		t.Fatal("proxy accepted a credential of another type")
	}
	if _, err := store.GetCredential(ctx, wrongType.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rejected credential write was not rolled back: %v", err)
	}

	auth := wrongType
	auth.Name = "Proxy authentication"
	auth.Type = domain.ProviderEgressCredentialType
	stored, err := store.PutProviderEgressProxy(ctx, proxy, 0, &auth, 0, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	unrelated, err := store.PutCredential(ctx, domain.Credential{
		ID: "cred_unrelated", Name: "Unrelated", Type: "internal_other", Audience: "internal",
		Ciphertext: []byte("ciphertext"), KeyVersion: 1, CreatedAt: now, UpdatedAt: now,
	}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	proxy.Name = "Renamed"
	if _, err := store.PutProviderEgressProxy(ctx, proxy, stored.Revision, nil, 0, unrelated.ID, nil); err == nil {
		t.Fatal("proxy deleted authentication it did not own")
	}
	if _, err := store.GetCredential(ctx, unrelated.ID); err != nil {
		t.Fatalf("unrelated credential was deleted: %v", err)
	}
	unchanged, err := store.GetProviderEgressProxy(ctx, proxy.ID)
	if err != nil || unchanged.Revision != stored.Revision || unchanged.Name != stored.Name {
		t.Fatalf("rejected mutation was not atomic: proxy=%#v err=%v", unchanged, err)
	}
}

func TestProviderEgressProxyLimitIsAtomic(t *testing.T) {
	store, err := openForTest(t, filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	for index := 0; index < domain.MaxProviderEgressProxies; index++ {
		proxy := domain.ProviderEgressProxy{
			ID: fmt.Sprintf("proxy-%02d", index), Name: "Proxy", Kind: domain.ProviderEgressProxyKindHTTPConnect,
			Endpoint: fmt.Sprintf("https://proxy-%02d.example.com:8443", index), CreatedAt: now, UpdatedAt: now,
		}
		if _, err := store.PutProviderEgressProxy(ctx, proxy, 0, nil, 0, "", nil); err != nil {
			t.Fatalf("proxy %d: %v", index, err)
		}
	}
	overflow := domain.ProviderEgressProxy{
		ID: "proxy-overflow", Name: "Overflow", Kind: domain.ProviderEgressProxyKindHTTPConnect,
		Endpoint: "https://overflow.example.com:8443", CreatedAt: now, UpdatedAt: now,
	}
	if _, err := store.PutProviderEgressProxy(ctx, overflow, 0, nil, 0, "", nil); !errors.Is(err, ErrProviderEgressProxyLimit) {
		t.Fatalf("overflow err=%v", err)
	}
	items, err := store.ListProviderEgressProxies(ctx)
	if err != nil || len(items) != domain.MaxProviderEgressProxies {
		t.Fatalf("limit mutation was not atomic: count=%d err=%v", len(items), err)
	}
}

func TestProviderStoreRequiresExistingEgressProxy(t *testing.T) {
	store, err := openForTest(t, filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	profile, _ := domain.DefaultProviderProfile(domain.ProviderOpenAI)
	credential, err := store.PutCredential(ctx, domain.Credential{
		ID: "cred_openai", Name: "OpenAI", Type: domain.ProviderOpenAI,
		AccessSurface: profile.AccessSurface, Scheme: profile.CredentialScheme,
		Audience: "https://api.openai.com:443", Ciphertext: []byte("ciphertext"), KeyVersion: 1,
		CreatedAt: now, UpdatedAt: now,
	}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	capabilities := domain.DefaultProviderCapabilities(domain.ProviderOpenAI)
	instance := domain.ProviderInstance{
		ID: "provider_proxy", Name: "OpenAI", Type: domain.ProviderOpenAI,
		BaseURL: "https://api.openai.com", CredentialID: credential.ID,
		AccessSurface: profile.AccessSurface, ProfileID: profile.ProfileID, CredentialScheme: profile.CredentialScheme,
		EgressProxyID: "corp-egress", AllowedHosts: []string{"api.openai.com"}, Capabilities: capabilities,
		CapabilityEvidence: domain.EvidenceForCapabilities(capabilities, domain.EvidenceDeclared),
		CreatedAt:          now, UpdatedAt: now,
	}
	if _, err := store.PutProvider(ctx, instance, 0, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing proxy reference err=%v", err)
	}
	if _, err := store.PutProviderEgressProxy(ctx, domain.ProviderEgressProxy{
		ID: instance.EgressProxyID, Name: "Corporate egress", Kind: domain.ProviderEgressProxyKindHTTPConnect,
		Endpoint: "https://proxy.example.com:8443", CreatedAt: now, UpdatedAt: now,
	}, 0, nil, 0, "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutProvider(ctx, instance, 0, nil); err != nil {
		t.Fatalf("provider with existing proxy: %v", err)
	}
}
