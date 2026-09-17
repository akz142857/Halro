package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/safetransport"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/vault"
)

const providerEgressCredentialType = domain.ProviderEgressCredentialType

func internalCredentialType(value domain.ProviderType) bool {
	return string(value) == webhookCredentialType || value == providerEgressCredentialType
}

type providerEgressAuth struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type providerEgressDescription struct {
	ID                      string `json:"id"`
	Name                    string `json:"name"`
	Kind                    string `json:"kind"`
	Endpoint                string `json:"endpoint"`
	EndpointScheme          string `json:"endpoint_scheme"`
	EndpointHost            string `json:"endpoint_host"`
	EndpointPort            int    `json:"endpoint_port"`
	AllowPrivateEndpoint    bool   `json:"allow_private_endpoint"`
	AllowLoopbackEndpoint   bool   `json:"allow_loopback_endpoint"`
	AllowCleartextBasicAuth bool   `json:"allow_cleartext_basic_auth"`
	Authenticated           bool   `json:"authenticated"`
	Revision                uint64 `json:"revision"`
	ActivationPending       bool   `json:"activation_pending,omitempty"`
	Fingerprint             string `json:"-"`
}

type providerEgressConnector struct {
	description providerEgressDescription
	dialer      *safetransport.HTTPConnectDialer
}

// providerEgressRegistry is one immutable activation snapshot. A topology
// activation builds the next snapshot from bbolt and swaps it together with the
// Provider registry; the old connectors close only after old adapters drain.
type providerEgressRegistry struct {
	runtimeID string
	entries   map[string]providerEgressConnector
	closeOnce sync.Once
}

func newProviderEgressRegistry(ctx context.Context, store *boltstore.Store, secretVault *vault.Vault) (*providerEgressRegistry, error) {
	definitions, err := store.ListProviderEgressProxies(ctx)
	if err != nil {
		return nil, fmt.Errorf("list Provider egress proxies: %w", err)
	}
	registry := &providerEgressRegistry{entries: make(map[string]providerEgressConnector, len(definitions))}
	fail := func(err error) (*providerEgressRegistry, error) {
		registry.Close()
		return nil, err
	}
	for _, definition := range definitions {
		if definition.DeletedAt != nil {
			continue
		}
		if err := definition.Validate(); err != nil {
			return fail(fmt.Errorf("build Provider egress proxy %q: %w", definition.ID, err))
		}
		endpoint, err := url.Parse(definition.Endpoint)
		if err != nil {
			return fail(fmt.Errorf("build Provider egress proxy %q: invalid endpoint", definition.ID))
		}
		var auth *safetransport.BasicAuth
		credentialVersion := uint16(0)
		if definition.BasicAuthCredentialID != "" {
			credential, credentialErr := store.GetCredential(ctx, definition.BasicAuthCredentialID)
			if credentialErr != nil || credential.Type != providerEgressCredentialType || credential.Audience != definition.Endpoint {
				return fail(fmt.Errorf("build Provider egress proxy %q: authentication is unavailable", definition.ID))
			}
			plaintext, decryptErr := secretVault.DecryptCredential(
				credential.ID, string(credential.Type), credential.Audience, credential.Ciphertext,
			)
			if decryptErr != nil {
				return fail(fmt.Errorf("build Provider egress proxy %q: authentication is unavailable", definition.ID))
			}
			var document providerEgressAuth
			decodeErr := json.Unmarshal(plaintext, &document)
			clear(plaintext)
			if decodeErr != nil || document.Username == "" || strings.Contains(document.Username, ":") ||
				len(document.Username) > 1024 || len(document.Password) == 0 || len(document.Password) > 4096 {
				return fail(fmt.Errorf("build Provider egress proxy %q: authentication is invalid", definition.ID))
			}
			auth = &safetransport.BasicAuth{Username: document.Username, Password: []byte(document.Password)}
			credentialVersion = credential.KeyVersion
		}
		dialer, err := safetransport.NewHTTPConnectDialer(safetransport.HTTPConnectOptions{
			ID: definition.ID, Endpoint: endpoint,
			EndpointPolicy: safetransport.ProxyEndpointPolicy{
				AllowPrivate: definition.AllowPrivateEndpoint, AllowLoopback: definition.AllowLoopbackEndpoint,
			},
			BasicAuth: auth,
		})
		if auth != nil {
			clear(auth.Password)
		}
		if err != nil {
			return fail(fmt.Errorf("build Provider egress proxy %q: %w", definition.ID, err))
		}
		port, _ := strconv.Atoi(endpoint.Port())
		description := providerEgressDescription{
			ID: definition.ID, Name: definition.Name, Kind: definition.Kind, Endpoint: definition.Endpoint,
			EndpointScheme: endpoint.Scheme, EndpointHost: endpoint.Hostname(), EndpointPort: port,
			AllowPrivateEndpoint: definition.AllowPrivateEndpoint, AllowLoopbackEndpoint: definition.AllowLoopbackEndpoint,
			AllowCleartextBasicAuth: definition.AllowCleartextBasicAuth,
			Authenticated:           definition.BasicAuthCredentialID != "", Revision: definition.Revision,
		}
		description.Fingerprint = providerEgressFingerprint(description, credentialVersion)
		registry.entries[definition.ID] = providerEgressConnector{description: description, dialer: dialer}
	}
	registry.runtimeID = providerEgressRegistryID(registry.descriptions())
	return registry, nil
}

func providerEgressFingerprint(description providerEgressDescription, credentialVersion uint16) string {
	// Fingerprint only fields that can change the connector's network
	// behaviour. Revision also changes for a display-name edit; including it
	// would invalidate every proxy-backed Provider connection test even though
	// no outbound path changed.
	payload := fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%t\x00%t\x00%t\x00%d",
		description.ID, description.Kind, description.Endpoint, description.EndpointPort,
		description.Authenticated, description.AllowPrivateEndpoint, description.AllowLoopbackEndpoint,
		credentialVersion)
	digest := sha256.Sum256([]byte(payload))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func providerEgressRegistryID(descriptions []providerEgressDescription) string {
	hash := sha256.New()
	for _, description := range descriptions {
		_, _ = hash.Write([]byte(description.ID))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(description.Fingerprint))
		_, _ = hash.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func (r *providerEgressRegistry) connector(proxyID string) (safetransport.Dialer, bool) {
	if proxyID == "" {
		return nil, true
	}
	if r == nil {
		return nil, false
	}
	entry, ok := r.entries[proxyID]
	if !ok {
		return nil, false
	}
	return entry.dialer, true
}

// testRuntimeID identifies the exact outbound path a connection probe used.
// It is deliberately per proxy: changing an unrelated proxy must not invalidate
// evidence for this one. Direct is a stable explicit path rather than an empty
// value so a concurrent direct-to-proxy switch is detectable.
func (r *providerEgressRegistry) testRuntimeID(proxyID string) string {
	if proxyID == "" {
		return "direct"
	}
	if r == nil {
		return ""
	}
	entry, ok := r.entries[proxyID]
	if !ok {
		return ""
	}
	return entry.description.Fingerprint
}

func (r *Runtime) providerEgressTestRuntimeID(proxyID string) string {
	if r == nil || r.providerEgress == nil {
		if proxyID == "" {
			return "direct"
		}
		return ""
	}
	return r.providerEgress.Current().testRuntimeID(proxyID)
}

func (r *Runtime) egressTestRuntimeIsCurrent(proxyID, testedRuntimeID string) bool {
	if proxyID == "" {
		// Empty is the representation written before direct egress gained an
		// explicit runtime marker; both mean the same stable path.
		return testedRuntimeID == "" || testedRuntimeID == "direct"
	}
	current := r.providerEgressTestRuntimeID(proxyID)
	return testedRuntimeID != "" && current != "" && testedRuntimeID == current
}

func (r *Runtime) deploymentTestIsCurrent(ctx context.Context, deployment domain.Deployment) bool {
	if deployment.LastTestRevision != deployment.Revision {
		return false
	}
	instance, err := r.store.GetProvider(ctx, deployment.ProviderID)
	return err == nil && instance.DeletedAt == nil && r.egressTestRuntimeIsCurrent(instance.EgressProxyID, deployment.LastTestRuntimeID)
}

func (r *Runtime) routeTestIsCurrent(ctx context.Context, route domain.Route) bool {
	if route.LastTestRevision != route.Revision {
		return false
	}
	deployment, err := r.store.GetDeployment(ctx, route.DeploymentID)
	if err != nil || deployment.DeletedAt != nil {
		return false
	}
	instance, err := r.store.GetProvider(ctx, deployment.ProviderID)
	return err == nil && instance.DeletedAt == nil && r.egressTestRuntimeIsCurrent(instance.EgressProxyID, route.LastTestRuntimeID)
}

func (r *providerEgressRegistry) descriptions() []providerEgressDescription {
	if r == nil {
		return nil
	}
	items := make([]providerEgressDescription, 0, len(r.entries))
	for _, entry := range r.entries {
		items = append(items, entry.description)
	}
	slices.SortFunc(items, func(left, right providerEgressDescription) int {
		return strings.Compare(left.ID, right.ID)
	})
	return items
}

func (r *providerEgressRegistry) Close() {
	if r == nil {
		return
	}
	r.closeOnce.Do(func() {
		for _, entry := range r.entries {
			entry.dialer.Close()
		}
	})
}

type providerEgressManager struct {
	mu      sync.RWMutex
	current *providerEgressRegistry
}

func newProviderEgressManager(current *providerEgressRegistry) *providerEgressManager {
	return &providerEgressManager{current: current}
}

func (m *providerEgressManager) Current() *providerEgressRegistry {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.current
}

func (m *providerEgressManager) Replace(next *providerEgressRegistry) *providerEgressRegistry {
	if m == nil || next == nil {
		return nil
	}
	m.mu.Lock()
	old := m.current
	m.current = next
	m.mu.Unlock()
	return old
}

func (m *providerEgressManager) Close() {
	if current := m.Current(); current != nil {
		current.Close()
	}
}

func (r *Runtime) listAdminProviderEgressProxies(writer http.ResponseWriter, _ *http.Request) {
	current := r.providerEgress.Current()
	if current == nil {
		writeJSON(writer, http.StatusOK, map[string]any{"runtime_id": "", "items": []providerEgressDescription{}})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"runtime_id": current.runtimeID,
		"items":      current.descriptions(),
	})
}

func (r *Runtime) auditProviderEgressRegistry(registry *providerEgressRegistry) error {
	if registry == nil {
		return errors.New("Provider egress registry is unavailable")
	}
	for _, proxy := range registry.descriptions() {
		if err := r.appendAdminAuditWithMetadata(
			"system", "halro", "provider_egress_registry.loaded", "provider_egress_proxy", proxy.ID,
			"success", "", map[string]any{
				"runtime_id": registry.runtimeID, "fingerprint": proxy.Fingerprint,
				"authenticated": proxy.Authenticated,
			},
		); err != nil {
			return err
		}
	}
	return nil
}
