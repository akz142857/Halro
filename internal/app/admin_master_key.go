package app

import (
	"net/http"
	"time"

	"github.com/akz142857/Heimdall/internal/config"
	"github.com/akz142857/Heimdall/internal/masterkey"
	boltstore "github.com/akz142857/Heimdall/internal/store/bolt"
)

const (
	recoveryVerificationMaxAge    = 90 * 24 * time.Hour
	recoveryVerificationClockSkew = 5 * time.Minute

	recoveryVerificationNotApplicable = "not_applicable"
	recoveryVerificationMissing       = "missing"
	recoveryVerificationCurrent       = "current"
	recoveryVerificationExpired       = "expired"
	recoveryVerificationInvalidFuture = "invalid_future"
)

type masterKeyCustodySlotView struct {
	Purpose    masterkey.KeySlotPurpose `json:"purpose"`
	State      masterkey.KeySlotState   `json:"state"`
	Provider   string                   `json:"provider"`
	VerifiedAt *time.Time               `json:"verified_at,omitempty"`
}

type masterKeyCustodyView struct {
	Mode                       string                     `json:"mode"`
	LocalCustodyReady          bool                       `json:"local_custody_ready"`
	CustodyState               string                     `json:"custody_state"`
	ProductionAdmission        string                     `json:"production_admission"`
	RotationIncomplete         bool                       `json:"rotation_incomplete"`
	LifecycleOperation         string                     `json:"lifecycle_operation"`
	PendingSlots               int                        `json:"pending_slots"`
	RetiringSlots              int                        `json:"retiring_slots"`
	RecoveryVerifiedAt         *time.Time                 `json:"recovery_verified_at,omitempty"`
	RecoveryVerificationStatus string                     `json:"recovery_verification_status"`
	DegradedReasons            []string                   `json:"degraded_reasons"`
	Slots                      []masterKeyCustodySlotView `json:"slots"`
	LifecycleRunbookURL        string                     `json:"lifecycle_runbook_url,omitempty"`
	RecoveryRunbookURL         string                     `json:"recovery_runbook_url,omitempty"`
}

func (r *Runtime) adminMasterKeyCustody(writer http.ResponseWriter, request *http.Request) {
	view := buildFileMasterKeyCustodyView()
	if view.Mode != r.config.Storage.MasterKey.Mode {
		view.Mode = r.config.Storage.MasterKey.Mode
	}
	if view.Mode == config.MasterKeyModeKeySlots {
		descriptor, err := r.store.KeySlotDescriptor(request.Context())
		if err != nil {
			adminStoreError(writer)
			return
		}
		keyring, err := r.store.VaultKeyring()
		if err != nil {
			adminStoreError(writer)
			return
		}
		view = buildMasterKeyCustodyView(view.Mode, descriptor, keyring, r.kmsRecoveryLastUsed, time.Now().UTC())
	}
	writeJSON(writer, http.StatusOK, view)
}

func buildFileMasterKeyCustodyView() masterKeyCustodyView {
	return masterKeyCustodyView{
		Mode: config.MasterKeyModeFile, LocalCustodyReady: true, CustodyState: "healthy",
		ProductionAdmission: "not_applicable", LifecycleOperation: "none", RecoveryVerificationStatus: recoveryVerificationNotApplicable,
		Slots: []masterKeyCustodySlotView{}, DegradedReasons: []string{},
	}
}

func buildMasterKeyCustodyView(mode string, descriptor masterkey.KeySlotDescriptor, keyring boltstore.VaultKeyring, recoveryLastUsed, now time.Time) masterKeyCustodyView {
	view := masterKeyCustodyView{
		Mode: mode, LocalCustodyReady: descriptor.ProductionReady(), CustodyState: "healthy",
		ProductionAdmission: "external_evidence_required", LifecycleOperation: "none", RecoveryVerificationStatus: recoveryVerificationMissing,
		Slots: make([]masterKeyCustodySlotView, 0, len(descriptor.Slots)), DegradedReasons: []string{},
		LifecycleRunbookURL: "/admin/api/v1/master-key/runbooks/lifecycle",
		RecoveryRunbookURL:  "/admin/api/v1/master-key/runbooks/recovery",
	}
	for _, slot := range descriptor.Slots {
		view.Slots = append(view.Slots, masterKeyCustodySlotView{
			Purpose: slot.Purpose, State: slot.State, Provider: slot.Provider, VerifiedAt: slot.VerifiedAt,
		})
		switch slot.State {
		case masterkey.KeySlotPending:
			view.PendingSlots++
		case masterkey.KeySlotRetiring:
			view.RetiringSlots++
		}
		if slot.Purpose == masterkey.KeySlotRecovery && slot.State == masterkey.KeySlotActive && slot.VerifiedAt != nil {
			if view.RecoveryVerifiedAt == nil || slot.VerifiedAt.After(*view.RecoveryVerifiedAt) {
				verified := slot.VerifiedAt.UTC()
				view.RecoveryVerifiedAt = &verified
			}
		}
	}
	if !recoveryLastUsed.IsZero() && (view.RecoveryVerifiedAt == nil || recoveryLastUsed.After(*view.RecoveryVerifiedAt)) {
		verified := recoveryLastUsed.UTC()
		view.RecoveryVerifiedAt = &verified
	}
	view.RotationIncomplete = view.PendingSlots > 0 || view.RetiringSlots > 0
	if len(keyring.RecoveryEnvelope) > 0 {
		view.RotationIncomplete = true
		view.LifecycleOperation = "dek_rotate"
		view.DegradedReasons = append(view.DegradedReasons, "dek_rotation_incomplete")
	} else if view.RotationIncomplete {
		view.LifecycleOperation = "kek_rewrap"
	}
	if !view.LocalCustodyReady {
		view.DegradedReasons = append(view.DegradedReasons, "descriptor_not_ready")
	}
	if view.PendingSlots > 0 {
		view.DegradedReasons = append(view.DegradedReasons, "pending_slots")
	}
	if view.RetiringSlots > 0 {
		view.DegradedReasons = append(view.DegradedReasons, "retiring_slots")
	}
	if view.RecoveryVerifiedAt == nil {
		view.DegradedReasons = append(view.DegradedReasons, "recovery_verification_missing")
	} else if view.RecoveryVerifiedAt.After(now.Add(recoveryVerificationClockSkew)) {
		view.RecoveryVerificationStatus = recoveryVerificationInvalidFuture
		view.DegradedReasons = append(view.DegradedReasons, "recovery_verification_invalid_future")
	} else if now.Sub(*view.RecoveryVerifiedAt) >= recoveryVerificationMaxAge {
		view.RecoveryVerificationStatus = recoveryVerificationExpired
		view.DegradedReasons = append(view.DegradedReasons, "recovery_verification_expired")
	} else {
		view.RecoveryVerificationStatus = recoveryVerificationCurrent
	}
	if len(view.DegradedReasons) > 0 {
		view.CustodyState = "degraded"
	}
	return view
}
