package app

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/akz142857/Heimdall/internal/alert"
	"github.com/akz142857/Heimdall/internal/domain"
	"github.com/akz142857/Heimdall/internal/id"
	"github.com/akz142857/Heimdall/internal/safetransport"
	"github.com/go-chi/chi/v5"
)

type alertWebhookInput struct {
	Name       string  `json:"name"`
	URL        string  `json:"url"`
	HeaderName string  `json:"header_name"`
	Secret     *string `json:"secret,omitempty"`
	Enabled    bool    `json:"enabled"`
}

func (r *Runtime) getAdminAlert(writer http.ResponseWriter, request *http.Request) {
	webhook, err := r.store.GetAlertWebhook(request.Context(), chi.URLParam(request, "id"))
	if err != nil || webhook.DeletedAt != nil {
		adminNotFound(writer)
		return
	}
	writer.Header().Set("ETag", revisionETag(webhook.Revision))
	writeJSON(writer, http.StatusOK, alertWebhookSafeView(webhook))
}

func (r *Runtime) createAdminAlert(writer http.ResponseWriter, request *http.Request) {
	var input alertWebhookInput
	if err := decodeAdminJSON(request, &input); err != nil {
		adminBadRequest(writer, "invalid request")
		return
	}
	webhookID, err := id.New("whk")
	if err != nil {
		adminStoreError(writer)
		return
	}
	r.adminTopologyMu.Lock()
	defer r.adminTopologyMu.Unlock()
	r.adminAlertMu.Lock()
	defer r.adminAlertMu.Unlock()
	now := time.Now().UTC()
	webhook, credential, credentialRevision, err := r.prepareAlertWebhook(
		request.Context(), webhookID, input, nil, now, now,
	)
	if err != nil {
		adminBadRequest(writer, err.Error())
		return
	}
	deleteCredentialID := ""
	webhook, err = r.store.PutAlertWebhookBundle(
		request.Context(), webhook, 0, credential, credentialRevision, deleteCredentialID,
	)
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	// Audited before the dispatcher reload: the change is already durable at this point,
	// and a reload failure caused by some *other* webhook must not erase the record that
	// this one was written.
	if err := r.auditAdminMutation(request, "alert_webhook.create", "alert_webhook", webhook.ID); err != nil {
		adminAuditError(writer)
		return
	}
	if !r.reloadAlerts(writer, request) {
		return
	}
	writer.Header().Set("ETag", revisionETag(webhook.Revision))
	writeJSON(writer, http.StatusCreated, alertWebhookSafeView(webhook))
}

func (r *Runtime) updateAdminAlert(writer http.ResponseWriter, request *http.Request) {
	expected, ok := requireRevision(writer, request)
	if !ok {
		return
	}
	var input alertWebhookInput
	if err := decodeAdminJSON(request, &input); err != nil {
		adminBadRequest(writer, "invalid request")
		return
	}
	r.adminTopologyMu.Lock()
	defer r.adminTopologyMu.Unlock()
	r.adminAlertMu.Lock()
	defer r.adminAlertMu.Unlock()
	current, err := r.store.GetAlertWebhook(request.Context(), chi.URLParam(request, "id"))
	if err != nil || current.DeletedAt != nil {
		adminNotFound(writer)
		return
	}
	if current.Revision != expected {
		adminPreconditionFailed(writer)
		return
	}
	webhook, credential, credentialRevision, err := r.prepareAlertWebhook(
		request.Context(), current.ID, input, &current, current.CreatedAt, time.Now().UTC(),
	)
	if err != nil {
		adminBadRequest(writer, err.Error())
		return
	}
	deleteCredentialID := ""
	if current.CredentialID != "" && webhook.CredentialID == "" {
		deleteCredentialID = current.CredentialID
	}
	webhook, err = r.store.PutAlertWebhookBundle(
		request.Context(), webhook, expected, credential, credentialRevision, deleteCredentialID,
	)
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	// Audited before the dispatcher reload: the change is already durable at this point,
	// and a reload failure caused by some *other* webhook must not erase the record that
	// this one was written.
	if err := r.auditAdminMutation(request, "alert_webhook.update", "alert_webhook", webhook.ID); err != nil {
		adminAuditError(writer)
		return
	}
	if !r.reloadAlerts(writer, request) {
		return
	}
	writer.Header().Set("ETag", revisionETag(webhook.Revision))
	writeJSON(writer, http.StatusOK, alertWebhookSafeView(webhook))
}

func (r *Runtime) deleteAdminAlert(writer http.ResponseWriter, request *http.Request) {
	expected, ok := requireRevision(writer, request)
	if !ok {
		return
	}
	// Deleting is not undoable and a stolen session should not be enough to do
	// it. The revision precondition is checked first: it costs a header parse,
	// while the step-up costs an Argon2id verification, and a request that
	// cannot succeed anyway should not buy one.
	if !r.requireDestructiveStepUp(writer, request) {
		return
	}
	r.adminTopologyMu.Lock()
	defer r.adminTopologyMu.Unlock()
	r.adminAlertMu.Lock()
	defer r.adminAlertMu.Unlock()
	webhook, err := r.store.GetAlertWebhook(request.Context(), chi.URLParam(request, "id"))
	if err != nil || webhook.DeletedAt != nil {
		adminNotFound(writer)
		return
	}
	if webhook.Revision != expected {
		adminPreconditionFailed(writer)
		return
	}
	now := time.Now().UTC()
	deleteCredentialID := webhook.CredentialID
	webhook.Enabled = false
	webhook.HeaderName = ""
	webhook.CredentialID = ""
	webhook.UpdatedAt = now
	webhook.DeletedAt = &now
	webhook, err = r.store.PutAlertWebhookBundle(
		request.Context(), webhook, expected, nil, 0, deleteCredentialID,
	)
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	// Audited before the dispatcher reload: the change is already durable at this point,
	// and a reload failure caused by some *other* webhook must not erase the record that
	// this one was written.
	if err := r.auditAdminMutation(request, "alert_webhook.delete", "alert_webhook", webhook.ID); err != nil {
		adminAuditError(writer)
		return
	}
	if !r.reloadAlerts(writer, request) {
		return
	}
	writer.Header().Set("ETag", revisionETag(webhook.Revision))
	writer.WriteHeader(http.StatusNoContent)
}

func (r *Runtime) testAdminAlert(writer http.ResponseWriter, request *http.Request) {
	r.testAdminAlertID(writer, request, chi.URLParam(request, "id"))
}

func (r *Runtime) testAdminAlertSelection(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		ID string `json:"id"`
	}
	if err := decodeAdminJSON(request, &input); err != nil || strings.TrimSpace(input.ID) == "" {
		adminBadRequest(writer, "alert id is required")
		return
	}
	r.testAdminAlertID(writer, request, input.ID)
}

func (r *Runtime) testAdminAlertID(writer http.ResponseWriter, request *http.Request, webhookID string) {
	webhook, err := r.store.GetAlertWebhook(request.Context(), webhookID)
	if err != nil || webhook.DeletedAt != nil {
		adminNotFound(writer)
		return
	}
	if !webhook.Enabled {
		writeJSON(writer, http.StatusConflict, map[string]string{"error": "alert webhook is disabled"})
		return
	}
	eventID, err := id.New("alt")
	if err != nil {
		adminStoreError(writer)
		return
	}
	result, err := r.alerts.TestEndpoint(webhook.ID, alert.Event{
		ID: eventID, Type: "admin_test", Severity: "info",
		DedupKey: "", Summary: "Heimdall alert connection test",
		Timestamp: time.Now().UTC(), Details: map[string]any{"source": "admin"},
	})
	outcome := "success"
	reason := ""
	if err != nil {
		outcome = "failure"
		// The dispatcher already classified the failure. Collapsing every cause into one
		// string leaves the operator unable to tell a bad credential from a dead host.
		reason = result.Reason
		if reason == "" {
			reason = "delivery_failed"
		}
	}
	if auditErr := r.appendAdminAudit(
		"admin_user",
		request.Context().Value(adminContextKey{}).(adminRequestContext).session.Username,
		"alert_webhook.test", "alert_webhook", webhook.ID, outcome, reason,
	); auditErr != nil {
		adminAuditError(writer)
		return
	}
	if err != nil {
		writeJSON(writer, http.StatusBadGateway, map[string]any{
			"error": "alert delivery test failed", "code": reason,
			"status_code": result.StatusCode, "response": result.ResponseSnippet,
		})
		return
	}
	// The endpoint's own reply travels back with the success. Chat platforms routinely
	// answer 200 and reject the payload in the body; without this the console reports a
	// delivery nobody received.
	writeJSON(writer, http.StatusOK, map[string]any{
		"status": "delivered", "latency_ms": result.LatencyMillis,
		"status_code": result.StatusCode, "response": result.ResponseSnippet,
	})
}

func (r *Runtime) prepareAlertWebhook(
	ctx context.Context,
	webhookID string,
	input alertWebhookInput,
	current *domain.AlertWebhook,
	createdAt time.Time,
	updatedAt time.Time,
) (domain.AlertWebhook, *domain.Credential, uint64, error) {
	endpoint, err := safetransport.ValidateURL(input.URL, webhookPolicy(r.config, nil))
	if err != nil {
		return domain.AlertWebhook{}, nil, 0, err
	}
	headerName := strings.ToLower(strings.TrimSpace(input.HeaderName))
	webhook := domain.AlertWebhook{
		ID: webhookID, Name: input.Name, URL: input.URL,
		AllowedHosts: []string{strings.ToLower(endpoint.Hostname())},
		HeaderName:   headerName, Enabled: input.Enabled,
		CreatedAt: createdAt, UpdatedAt: updatedAt,
	}
	var existingCredential *domain.Credential
	if current != nil && current.CredentialID != "" {
		value, err := r.store.GetCredential(ctx, current.CredentialID)
		if err != nil {
			return domain.AlertWebhook{}, nil, 0, errors.New("stored webhook credential is unavailable")
		}
		existingCredential = &value
	}
	if input.Secret != nil && *input.Secret == "" {
		webhook.HeaderName = ""
		return webhook, nil, 0, webhook.Validate()
	}
	if input.Secret == nil && existingCredential == nil {
		webhook.HeaderName = ""
		return webhook, nil, 0, webhook.Validate()
	}
	if webhook.HeaderName == "" {
		return domain.AlertWebhook{}, nil, 0, errors.New("secret header name is required")
	}
	// The console never reveals a stored secret, so silently re-binding it to a new
	// destination would let an operator who has never seen the plaintext post it to a host
	// they control. Re-pointing a webhook means re-entering the secret.
	if input.Secret == nil && current != nil &&
		(!strings.EqualFold(current.URL, input.URL) || current.HeaderName != webhook.HeaderName) {
		return domain.AlertWebhook{}, nil, 0, errors.New(
			"changing the endpoint or header requires the secret to be entered again")
	}
	audience, err := safetransport.AudienceWithPolicy(
		input.URL,
		webhookAudienceSubject(webhook),
		webhookPolicy(r.config, webhook.AllowedHosts),
	)
	if err != nil {
		return domain.AlertWebhook{}, nil, 0, err
	}
	var plaintext []byte
	if input.Secret != nil {
		plaintext = []byte(*input.Secret)
		if len(plaintext) > 16<<10 {
			clear(plaintext)
			return domain.AlertWebhook{}, nil, 0, errors.New("webhook secret is too large")
		}
	} else {
		plaintext, err = r.vault.DecryptCredential(
			existingCredential.ID, string(existingCredential.Type),
			existingCredential.Audience, existingCredential.Ciphertext,
		)
		if err != nil {
			return domain.AlertWebhook{}, nil, 0, errors.New("stored webhook credential could not be decrypted")
		}
	}
	defer clear(plaintext)
	credentialID := ""
	credentialRevision := uint64(0)
	keyVersion := uint16(1)
	credentialCreatedAt := updatedAt
	if existingCredential != nil {
		credentialID = existingCredential.ID
		credentialRevision = existingCredential.Revision
		if existingCredential.KeyVersion == ^uint16(0) {
			return domain.AlertWebhook{}, nil, 0, errors.New("webhook credential key version is exhausted")
		}
		keyVersion = existingCredential.KeyVersion + 1
		credentialCreatedAt = existingCredential.CreatedAt
	} else {
		credentialID, err = id.New("cred")
		if err != nil {
			return domain.AlertWebhook{}, nil, 0, err
		}
	}
	ciphertext, err := r.vault.EncryptCredential(
		credentialID, webhookCredentialType, audience, plaintext,
	)
	if err != nil {
		return domain.AlertWebhook{}, nil, 0, err
	}
	credential := &domain.Credential{
		ID: credentialID, Name: "Webhook: " + input.Name,
		Type: domain.ProviderType(webhookCredentialType), Audience: audience,
		Ciphertext: ciphertext, KeyVersion: keyVersion,
		CreatedAt: credentialCreatedAt, UpdatedAt: updatedAt,
	}
	webhook.CredentialID = credentialID
	return webhook, credential, credentialRevision, webhook.Validate()
}

func (r *Runtime) reloadAlerts(writer http.ResponseWriter, request *http.Request) bool {
	endpoints, err := loadAlertEndpoints(request.Context(), r.config, r.store, r.vault)
	if err != nil {
		r.logger.Error("reload alert endpoints", "error", err)
		adminConfigurationError(writer, err)
		return false
	}
	retired := r.alerts.ReplaceEndpoints(endpoints)
	if len(retired) == 0 {
		return true
	}
	grace := r.config.Alerts.Timeout.Value()*time.Duration(r.config.Alerts.MaxAttempts) +
		r.config.Alerts.MaxDelay.Value()*time.Duration(r.config.Alerts.MaxAttempts) + time.Second
	r.backgroundWait.Add(1)
	go func() {
		defer r.backgroundWait.Done()
		timer := time.NewTimer(grace)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-r.backgroundCtx.Done():
		}
		for index := range retired {
			retired[index].Close()
		}
	}()
	return true
}

func alertWebhookSafeView(item domain.AlertWebhook) alertWebhookView {
	return alertWebhookView{
		ID: item.ID, Name: item.Name, URL: item.URL, HeaderName: item.HeaderName,
		SecretConfigured: item.CredentialID != "",
		Enabled:          item.Enabled, Revision: item.Revision,
	}
}
