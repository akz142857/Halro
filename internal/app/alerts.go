package app

import (
	"context"
	"fmt"

	"github.com/akz142857/Heimdall/internal/alert"
	"github.com/akz142857/Heimdall/internal/config"
	"github.com/akz142857/Heimdall/internal/id"
	"github.com/akz142857/Heimdall/internal/safetransport"
	boltstore "github.com/akz142857/Heimdall/internal/store/bolt"
	"github.com/akz142857/Heimdall/internal/tokenguard"
	"github.com/akz142857/Heimdall/internal/vault"
)

const webhookCredentialType = "webhook"

func loadAlertDispatcher(
	ctx context.Context,
	cfg config.Config,
	store *boltstore.Store,
	secretVault *vault.Vault,
) (*alert.Dispatcher, error) {
	endpoints, err := loadAlertEndpoints(ctx, cfg, store, secretVault)
	if err != nil {
		return nil, err
	}
	dispatcher, err := alert.New(alert.Config{
		QueueCapacity: cfg.Alerts.QueueCapacity,
		Workers:       cfg.Alerts.Workers,
		Timeout:       cfg.Alerts.Timeout.Value(),
		MaxAttempts:   cfg.Alerts.MaxAttempts,
		BaseDelay:     cfg.Alerts.BaseDelay.Value(),
		MaxDelay:      cfg.Alerts.MaxDelay.Value(),
		DedupCooldown: cfg.Alerts.DedupCooldown.Value(),
	}, endpoints)
	if err != nil {
		for index := range endpoints {
			endpoints[index].Close()
		}
		return nil, err
	}
	return dispatcher, nil
}

func loadAlertEndpoints(
	ctx context.Context,
	cfg config.Config,
	store *boltstore.Store,
	secretVault *vault.Vault,
) ([]alert.Endpoint, error) {
	webhooks, err := store.ListAlertWebhooks(ctx)
	if err != nil {
		return nil, fmt.Errorf("list alert webhooks: %w", err)
	}
	var endpoints []alert.Endpoint
	fail := func(err error) ([]alert.Endpoint, error) {
		for index := range endpoints {
			endpoints[index].Close()
		}
		return nil, err
	}
	for _, webhook := range webhooks {
		if !webhook.Enabled || webhook.DeletedAt != nil {
			continue
		}
		policy := safetransport.Policy{
			RequireHTTPS: true,
			AllowPrivate: cfg.Security.AllowPrivateWebhooks,
			AllowedHosts: webhook.AllowedHosts,
		}
		endpointURL, err := safetransport.ValidateURL(webhook.URL, policy)
		if err != nil {
			return fail(fmt.Errorf("alert webhook %q URL: %w", webhook.ID, err))
		}
		client, err := safetransport.NewClient(safetransport.Options{
			Policy: policy, ConnectTimeout: cfg.Gateway.AttemptConnectTimeout.Value(),
			ResponseHeaderTimeout: cfg.Alerts.Timeout.Value(),
		})
		if err != nil {
			return fail(fmt.Errorf("alert webhook %q transport: %w", webhook.ID, err))
		}
		var secret []byte
		if webhook.CredentialID != "" {
			credential, err := store.GetCredential(ctx, webhook.CredentialID)
			if err != nil {
				client.CloseIdleConnections()
				return fail(fmt.Errorf("alert webhook %q credential: %w", webhook.ID, err))
			}
			if string(credential.Type) != webhookCredentialType {
				client.CloseIdleConnections()
				return fail(fmt.Errorf("alert webhook %q credential type mismatch", webhook.ID))
			}
			audience, err := safetransport.AudienceWithPolicy(
				webhook.URL, webhookCredentialType+":"+webhook.HeaderName, policy,
			)
			if err != nil || credential.Audience != audience {
				client.CloseIdleConnections()
				return fail(fmt.Errorf("alert webhook %q credential audience mismatch", webhook.ID))
			}
			secret, err = secretVault.DecryptCredential(
				credential.ID, string(credential.Type), credential.Audience, credential.Ciphertext,
			)
			if err != nil {
				client.CloseIdleConnections()
				return fail(fmt.Errorf("alert webhook %q decrypt credential: %w", webhook.ID, err))
			}
		}
		endpoint, err := alert.NewEndpoint(webhook.ID, endpointURL, webhook.HeaderName, secret, client)
		clear(secret)
		if err != nil {
			client.CloseIdleConnections()
			return fail(fmt.Errorf("alert webhook %q: %w", webhook.ID, err))
		}
		endpoints = append(endpoints, endpoint)
	}
	return endpoints, nil
}

func forwardTokenGuardAlerts(
	ctx context.Context,
	events <-chan tokenguard.Event,
	dispatcher *alert.Dispatcher,
	auditSubmission func(alert.Event, alert.SubmissionResult),
) {
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-events:
			if !ok {
				return
			}
			alertID, err := id.New("alt")
			if err != nil {
				continue
			}
			alertEvent := alert.Event{
				ID:   alertID,
				Type: event.Type, Severity: event.Severity,
				DedupKey: event.ProjectID + ":" + event.Type + ":" + event.Reason,
				Summary:  "Token Guard state changed", ProjectID: event.ProjectID,
				Timestamp: event.Timestamp,
				Details: map[string]any{
					"reason": event.Reason, "status": event.Status,
					"expires_at": event.ExpiresAt,
				},
			}
			result := dispatcher.SubmitWithResult(alertEvent)
			if auditSubmission != nil {
				auditSubmission(alertEvent, result)
			}
		}
	}
}

func (r *Runtime) auditAlertSubmission(event alert.Event, result alert.SubmissionResult) {
	if err := r.appendAdminAudit(
		"project", event.ProjectID, "alert.generated", "alert", event.ID,
		"success", alertAuditType(event.Type),
	); err != nil {
		r.logger.Error("alert generation audit failed", "error", err)
	}
	outcome := "success"
	if !result.Accepted {
		outcome = "failure"
	}
	if err := r.appendAdminAudit(
		"project", event.ProjectID, "alert.submission", "alert", event.ID,
		outcome, string(result.Status),
	); err != nil {
		r.logger.Error("alert submission audit failed", "error", err)
	}
}

func alertAuditType(value string) string {
	switch value {
	case "token_guard_recovered", "token_guard_blocked", "token_guard_suspicious",
		"token_guard_alert", "token_guard_observed", "token_guard_ewma_detected", "admin_test":
		return value
	default:
		return "other"
	}
}

func (r *Runtime) auditAlertDelivery(result alert.DeliveryResult) {
	if err := r.appendAdminAudit(
		"alert", result.EventID, "alert.delivery", "alert_webhook", result.EndpointID,
		result.Outcome, result.Reason,
	); err != nil {
		r.logger.Error("alert delivery audit failed", "error", err)
	}
}
