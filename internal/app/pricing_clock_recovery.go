package app

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/replication"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/vault"
	"github.com/go-chi/chi/v5"
)

func (r *Runtime) reviewClockDeploymentPricing(w http.ResponseWriter, req *http.Request) {
	review, err := r.store.ReviewClockPricing(req.Context(), chi.URLParam(req, "id"), time.Now().UTC())
	if err != nil {
		writePriceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, review)
}

func (r *Runtime) confirmClockDeploymentPricing(w http.ResponseWriter, req *http.Request) {
	var input struct {
		Confirmation    domain.PricingClockConfirmation `json:"confirmation"`
		CurrentPassword string                          `json:"current_password"`
		TOTPCode        string                          `json:"totp_code,omitempty"`
	}
	if err := decodeAdminJSON(req, &input); err != nil {
		writePriceError(w, errors.New("invalid clock confirmation"))
		return
	}
	admin := req.Context().Value(adminContextKey{}).(adminRequestContext)
	if !r.verifyAdminStepUp(w, req, admin.session.Username, input.CurrentPassword, input.TOTPCode) {
		return
	}
	input.CurrentPassword, input.TOTPCode = "", ""
	id := chi.URLParam(req, "id")
	intent, err := newPricingAuditIntent("deployment_price.clock_confirm", id, admin.session.Username, req.Header.Get("X-Request-ID"), input.Confirmation, nil)
	if err != nil {
		adminStoreError(w)
		return
	}
	intent.DeploymentID = id
	r.adminTopologyMu.Lock()
	defer r.adminTopologyMu.Unlock()
	intent, err = r.store.RecoverClockPricingWithAuditIntent(req.Context(), id, input.Confirmation, time.Now().UTC(), intent)
	if err != nil {
		writePriceError(w, err)
		return
	}
	r.activateTopologyAfterCommit()
	if err := r.deliverPricingAuditIntent(req.Context(), intent); err != nil {
		adminAuditError(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deployment_id": id, "pricing_quarantine": false})
}

// ClockPricingOffline is the break-glass counterpart to the step-up protected
// Admin action. Like offline key and route maintenance, it requires exclusive
// data-directory access and possession of the local Master Key for writes.
// It starts no listeners, probes, model calls, or background workers.
func ClockPricingOffline(ctx context.Context, cfg config.Config, deploymentID string, expected *domain.PricingClockConfirmation) (domain.PricingClockReview, error) {
	var review domain.PricingClockReview
	if cfg.Replication != nil {
		return review, errors.New("offline clock pricing recovery supports Standalone only; use the authenticated Admin action on a Primary")
	}
	if err := replication.GuardStandaloneAccess(cfg.Storage.DataDir, false, "clock pricing recovery"); err != nil {
		return review, err
	}
	dataLock, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		return review, err
	}
	defer dataLock.Close()
	if expected == nil {
		store, err := boltstore.OpenReadOnly(cfg.MetadataPath())
		if err != nil {
			return review, err
		}
		defer store.Close()
		return store.ReviewClockPricing(ctx, deploymentID, time.Now().UTC())
	}
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		return review, err
	}
	defer store.Close()
	if err := attachMetadataJournalForCLI(ctx, cfg, store, "clock pricing recovery"); err != nil {
		return review, err
	}
	masterKey, err := unlockMasterKey(ctx, cfg, store)
	if err != nil {
		return review, err
	}
	defer clear(masterKey)
	secretVault, err := vault.New(masterKey)
	if err != nil {
		return review, err
	}
	defer secretVault.Close()
	if err := verifyVaultKeyCheck(store, secretVault); err != nil {
		return review, err
	}
	auditKey, err := loadAuditHMACKey(store, secretVault, masterKey)
	if err != nil {
		return review, err
	}
	defer clear(auditKey)
	auditLog, err := audit.Open(cfg.AuditPath(), auditKey)
	if err != nil {
		return review, err
	}
	defer auditLog.Close()
	if err := reconcileAuditCheckpoint(store, auditLog.Summary()); err != nil {
		return review, err
	}
	now := time.Now().UTC()
	review, err = store.ReviewClockPricing(ctx, deploymentID, now)
	if err != nil {
		return review, err
	}
	intent, err := newPricingAuditIntent("deployment_price.clock_confirm", deploymentID, "local_operator", "", *expected, nil)
	if err != nil {
		return review, err
	}
	intent.ActorType = "local_cli"
	intent.DeploymentID = deploymentID
	if _, err = store.RecoverClockPricingWithAuditIntent(ctx, deploymentID, *expected, now, intent); err != nil {
		return review, err
	}
	runtime := &Runtime{store: store, audit: auditLog}
	if err := runtime.drainPricingAuditIntents(ctx); err != nil {
		return review, err
	}
	return review, nil
}
