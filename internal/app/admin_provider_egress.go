package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/go-chi/chi/v5"
)

type providerEgressProxyInput struct {
	stepUpMaterial
	Name                    string  `json:"name"`
	Kind                    string  `json:"kind"`
	Endpoint                string  `json:"endpoint"`
	AllowPrivateEndpoint    bool    `json:"allow_private_endpoint"`
	AllowLoopbackEndpoint   bool    `json:"allow_loopback_endpoint"`
	Username                *string `json:"username,omitempty"`
	Password                *string `json:"password,omitempty"`
	ClearBasicAuth          bool    `json:"clear_basic_auth,omitempty"`
	AllowCleartextBasicAuth bool    `json:"allow_cleartext_basic_auth"`
}

func (r *Runtime) createAdminProviderEgressProxy(writer http.ResponseWriter, request *http.Request) {
	var input providerEgressProxyInput
	if err := decodeAdminJSON(request, &input); err != nil {
		adminBadRequestCode(writer, "invalid_request", "invalid request")
		return
	}
	if !r.requireStepUpMaterial(writer, request, input.stepUpMaterial) {
		return
	}
	idempotencyKey, ok := adminCreateIdempotencyKey(writer, request)
	if !ok {
		return
	}
	admin := request.Context().Value(adminContextKey{}).(adminRequestContext)
	proxyID := adminCreateID("egp", "provider-egress-proxy", admin.session.Username, idempotencyKey)
	now := time.Now().UTC()
	proxy := domain.ProviderEgressProxy{
		ID: proxyID, Name: strings.TrimSpace(input.Name), Kind: strings.TrimSpace(input.Kind),
		Endpoint:             strings.TrimSpace(input.Endpoint),
		AllowPrivateEndpoint: input.AllowPrivateEndpoint, AllowLoopbackEndpoint: input.AllowLoopbackEndpoint,
		AllowCleartextBasicAuth: input.AllowCleartextBasicAuth,
		CreatedAt:               now, UpdatedAt: now,
	}
	if proxy.Kind == "" {
		proxy.Kind = domain.ProviderEgressProxyKindHTTPConnect
	}
	if input.ClearBasicAuth {
		adminBadRequestCode(writer, "provider_egress_auth_invalid", "a new proxy cannot clear authentication")
		return
	}
	authCredential, err := r.providerEgressAuthCredential(proxy, input, nil)
	if err != nil {
		adminBadRequestCode(writer, "provider_egress_auth_invalid", err.Error())
		return
	}
	if authCredential != nil {
		proxy.BasicAuthCredentialID = authCredential.ID
	}
	if endpoint, parseErr := url.Parse(proxy.Endpoint); parseErr == nil && endpoint.Scheme != "http" {
		proxy.AllowCleartextBasicAuth = false
	}
	if err := proxy.Validate(); err != nil {
		adminBadRequest(writer, err.Error())
		return
	}

	r.adminTopologyMu.Lock()
	defer r.adminTopologyMu.Unlock()
	intent, intentErr := r.newAdminAuditIntentWithMetadata(request, "provider_egress_proxy.create", "provider_egress_proxy", proxy.ID, map[string]string{
		"authenticated": strconv.FormatBool(authCredential != nil),
	})
	if intentErr != nil {
		adminStoreError(writer)
		return
	}
	proxy, err = r.store.PutProviderEgressProxy(request.Context(), proxy, 0, authCredential, 0, "", intent)
	if err != nil {
		if writeAdminCreateReplay(writer, err, "provider_egress_proxy", proxy.ID) {
			return
		}
		if errors.Is(err, boltstore.ErrProviderEgressProxyLimit) {
			adminBadRequestCode(writer, "provider_egress_proxy_limit", err.Error())
			return
		}
		adminMutationError(writer, err)
		return
	}
	activationCurrent := r.activateTopologyAfterCommit()
	r.completeAdminMutation(writer, request, *intent)
	writer.Header().Set("ETag", revisionETag(proxy.Revision))
	response := providerEgressDescriptionFrom(proxy)
	response.ActivationPending = !activationCurrent
	writeJSON(writer, http.StatusCreated, response)
}

func (r *Runtime) updateAdminProviderEgressProxy(writer http.ResponseWriter, request *http.Request) {
	expected, ok := requireRevision(writer, request)
	if !ok {
		return
	}
	var input providerEgressProxyInput
	if err := decodeAdminJSON(request, &input); err != nil {
		adminBadRequestCode(writer, "invalid_request", "invalid request")
		return
	}
	if !r.requireStepUpMaterial(writer, request, input.stepUpMaterial) {
		return
	}

	r.adminTopologyMu.Lock()
	defer r.adminTopologyMu.Unlock()
	current, err := r.store.GetProviderEgressProxy(request.Context(), chi.URLParam(request, "id"))
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	if current.Revision != expected {
		adminPreconditionFailed(writer)
		return
	}
	next := current
	next.Name = strings.TrimSpace(input.Name)
	next.Kind = strings.TrimSpace(input.Kind)
	if next.Kind == "" {
		next.Kind = domain.ProviderEgressProxyKindHTTPConnect
	}
	next.Endpoint = strings.TrimSpace(input.Endpoint)
	next.AllowPrivateEndpoint = input.AllowPrivateEndpoint
	next.AllowLoopbackEndpoint = input.AllowLoopbackEndpoint
	next.AllowCleartextBasicAuth = input.AllowCleartextBasicAuth
	next.UpdatedAt = time.Now().UTC()

	runtimeChange := current.Endpoint != next.Endpoint || current.AllowPrivateEndpoint != next.AllowPrivateEndpoint ||
		current.AllowLoopbackEndpoint != next.AllowLoopbackEndpoint || current.AllowCleartextBasicAuth != next.AllowCleartextBasicAuth ||
		input.ClearBasicAuth || input.Username != nil || input.Password != nil
	if runtimeChange {
		blocked, blockErr := r.providerEgressProxyHasEnabledDeployments(request.Context(), current.ID)
		if blockErr != nil {
			adminStoreError(writer)
			return
		}
		if blocked {
			adminBadRequestCode(writer, "provider_egress_proxy_change_locked_by_deployments", "disable the proxy's active deployments before changing it")
			return
		}
	}

	var authCredential *domain.Credential
	var expectedCredentialRevision uint64
	deleteCredentialID := ""
	if input.ClearBasicAuth {
		if input.Username != nil || input.Password != nil {
			adminBadRequestCode(writer, "provider_egress_auth_invalid", "authentication cannot be replaced and cleared together")
			return
		}
		deleteCredentialID = current.BasicAuthCredentialID
		next.BasicAuthCredentialID = ""
		next.AllowCleartextBasicAuth = false
	} else if input.Username != nil || input.Password != nil || current.BasicAuthCredentialID != "" && current.Endpoint != next.Endpoint {
		var existing *domain.Credential
		if current.BasicAuthCredentialID != "" {
			value, credentialErr := r.store.GetCredential(request.Context(), current.BasicAuthCredentialID)
			if credentialErr != nil {
				adminStoreError(writer)
				return
			}
			existing = &value
			expectedCredentialRevision = value.Revision
		}
		authCredential, err = r.providerEgressAuthCredential(next, input, existing)
		if err != nil {
			adminBadRequestCode(writer, "provider_egress_auth_invalid", err.Error())
			return
		}
		if authCredential != nil {
			next.BasicAuthCredentialID = authCredential.ID
		}
	}
	if endpoint, parseErr := url.Parse(next.Endpoint); parseErr == nil && endpoint.Scheme != "http" {
		next.AllowCleartextBasicAuth = false
	}
	if err := next.Validate(); err != nil {
		adminBadRequest(writer, err.Error())
		return
	}
	intent, intentErr := r.newAdminAuditIntentWithMetadata(request, "provider_egress_proxy.update", "provider_egress_proxy", next.ID, map[string]string{
		"runtime_change": strconv.FormatBool(runtimeChange),
		"authenticated":  strconv.FormatBool(next.BasicAuthCredentialID != ""),
	})
	if intentErr != nil {
		adminStoreError(writer)
		return
	}
	next, err = r.store.PutProviderEgressProxy(request.Context(), next, expected, authCredential, expectedCredentialRevision, deleteCredentialID, intent)
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	activationCurrent := r.activateTopologyAfterCommit()
	r.completeAdminMutation(writer, request, *intent)
	writer.Header().Set("ETag", revisionETag(next.Revision))
	response := providerEgressDescriptionFrom(next)
	response.ActivationPending = !activationCurrent
	writeJSON(writer, http.StatusOK, response)
}

func (r *Runtime) deleteAdminProviderEgressProxy(writer http.ResponseWriter, request *http.Request) {
	expected, ok := requireRevision(writer, request)
	if !ok {
		return
	}
	if !r.requireDestructiveStepUp(writer, request) {
		return
	}
	id := chi.URLParam(request, "id")
	r.adminTopologyMu.Lock()
	defer r.adminTopologyMu.Unlock()
	intent, intentErr := r.newAdminAuditIntent(request, "provider_egress_proxy.delete", "provider_egress_proxy", id)
	if intentErr != nil {
		adminStoreError(writer)
		return
	}
	if err := r.store.DeleteProviderEgressProxy(request.Context(), id, expected, intent); err != nil {
		if errors.Is(err, boltstore.ErrProviderEgressProxyInUse) {
			writeJSON(writer, http.StatusConflict, codedErrorBody("provider_egress_proxy_in_use", "proxy is still referenced by a Provider", nil))
			return
		}
		adminMutationError(writer, err)
		return
	}
	r.activateTopologyAfterCommit()
	r.completeAdminMutation(writer, request, *intent)
	writer.WriteHeader(http.StatusNoContent)
}

func (r *Runtime) providerEgressAuthCredential(proxy domain.ProviderEgressProxy, input providerEgressProxyInput, current *domain.Credential) (*domain.Credential, error) {
	var document providerEgressAuth
	if input.Username != nil || input.Password != nil {
		if input.Username == nil || input.Password == nil {
			return nil, errors.New("username and password must be provided together")
		}
		document.Username = *input.Username
		document.Password = *input.Password
	} else if current != nil {
		plaintext, err := r.vault.DecryptCredential(current.ID, string(current.Type), current.Audience, current.Ciphertext)
		if err != nil {
			return nil, errors.New("stored proxy authentication could not be decrypted")
		}
		defer clear(plaintext)
		if err := json.Unmarshal(plaintext, &document); err != nil {
			return nil, errors.New("stored proxy authentication is invalid")
		}
	} else {
		return nil, nil
	}
	if document.Username == "" || strings.Contains(document.Username, ":") || len(document.Username) > 1024 ||
		len(document.Password) == 0 || len(document.Password) > 4096 {
		return nil, errors.New("proxy Basic authentication fields are invalid")
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return nil, err
	}
	defer clear(encoded)
	credentialID := ""
	createdAt := time.Now().UTC()
	keyVersion := uint16(1)
	expectedType := providerEgressCredentialType
	if current != nil {
		if current.Type != expectedType {
			return nil, errors.New("stored proxy authentication type is invalid")
		}
		if current.KeyVersion == ^uint16(0) {
			return nil, errors.New("proxy authentication key version is exhausted")
		}
		credentialID = current.ID
		createdAt = current.CreatedAt
		keyVersion = current.KeyVersion + 1
	} else {
		credentialID = "cred_" + strings.TrimPrefix(proxy.ID, "egp_")
	}
	ciphertext, err := r.vault.EncryptCredential(credentialID, string(expectedType), proxy.Endpoint, encoded)
	if err != nil {
		return nil, err
	}
	credential := &domain.Credential{
		ID: credentialID, Name: "Provider egress proxy authentication", Type: expectedType,
		Audience: proxy.Endpoint, Ciphertext: ciphertext, KeyVersion: keyVersion,
		CreatedAt: createdAt, UpdatedAt: time.Now().UTC(),
	}
	return credential, credential.Validate()
}

func (r *Runtime) providerEgressProxyHasEnabledDeployments(ctx context.Context, proxyID string) (bool, error) {
	providers, err := r.store.ListProviders(ctx)
	if err != nil {
		return false, err
	}
	bound := make(map[string]struct{})
	for _, instance := range providers {
		if instance.DeletedAt == nil && instance.EgressProxyID == proxyID {
			bound[instance.ID] = struct{}{}
		}
	}
	if len(bound) == 0 {
		return false, nil
	}
	deployments, err := r.store.ListDeployments(ctx)
	if err != nil {
		return false, err
	}
	for _, deployment := range deployments {
		if _, ok := bound[deployment.ProviderID]; ok && deployment.DeletedAt == nil && deployment.Enabled {
			return true, nil
		}
	}
	return false, nil
}

func providerEgressDescriptionFrom(proxy domain.ProviderEgressProxy) providerEgressDescription {
	endpoint, _ := url.Parse(proxy.Endpoint)
	port, _ := strconv.Atoi(endpoint.Port())
	return providerEgressDescription{
		ID: proxy.ID, Name: proxy.Name, Kind: proxy.Kind, Endpoint: proxy.Endpoint,
		EndpointScheme: endpoint.Scheme, EndpointHost: endpoint.Hostname(), EndpointPort: port,
		AllowPrivateEndpoint: proxy.AllowPrivateEndpoint, AllowLoopbackEndpoint: proxy.AllowLoopbackEndpoint,
		AllowCleartextBasicAuth: proxy.AllowCleartextBasicAuth,
		Authenticated:           proxy.BasicAuthCredentialID != "", Revision: proxy.Revision,
	}
}

func (r *Runtime) getAdminProviderEgressProxy(writer http.ResponseWriter, request *http.Request) {
	proxy, err := r.store.GetProviderEgressProxy(request.Context(), chi.URLParam(request, "id"))
	if err != nil || proxy.DeletedAt != nil {
		adminNotFound(writer)
		return
	}
	writer.Header().Set("ETag", revisionETag(proxy.Revision))
	writeJSON(writer, http.StatusOK, providerEgressDescriptionFrom(proxy))
}
