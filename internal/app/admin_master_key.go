package app

import (
	"net/http"
	"time"

	"github.com/akz142857/Heimdall/internal/config"
	"github.com/akz142857/Heimdall/internal/masterkey"
)

type masterKeyCustodySlotView struct {
	Purpose    masterkey.KeySlotPurpose `json:"purpose"`
	State      masterkey.KeySlotState   `json:"state"`
	Provider   string                   `json:"provider"`
	VerifiedAt *time.Time               `json:"verified_at,omitempty"`
}

type masterKeyCustodyView struct {
	Mode               string                     `json:"mode"`
	ProductionReady    bool                       `json:"production_ready"`
	RotationIncomplete bool                       `json:"rotation_incomplete"`
	PendingSlots       int                        `json:"pending_slots"`
	RetiringSlots      int                        `json:"retiring_slots"`
	RecoveryVerifiedAt *time.Time                 `json:"recovery_verified_at,omitempty"`
	Slots              []masterKeyCustodySlotView `json:"slots"`
	LifecycleRunbook   string                     `json:"lifecycle_runbook"`
	RecoveryRunbook    string                     `json:"recovery_runbook"`
}

func (r *Runtime) adminMasterKeyCustody(writer http.ResponseWriter, request *http.Request) {
	view := masterKeyCustodyView{
		Mode: r.config.Storage.MasterKey.Mode, ProductionReady: true,
		Slots:            []masterKeyCustodySlotView{},
		LifecycleRunbook: "docs/runbooks/m11-kms-key-lifecycle.md",
		RecoveryRunbook:  "docs/runbooks/m11-kms-disaster-recovery.md",
	}
	if view.Mode == config.MasterKeyModeKeySlots {
		descriptor, err := r.store.KeySlotDescriptor(request.Context())
		if err != nil {
			adminStoreError(writer)
			return
		}
		view = buildMasterKeyCustodyView(view.Mode, descriptor)
	}
	writeJSON(writer, http.StatusOK, view)
}

func buildMasterKeyCustodyView(mode string, descriptor masterkey.KeySlotDescriptor) masterKeyCustodyView {
	view := masterKeyCustodyView{
		Mode: mode, ProductionReady: descriptor.ProductionReady(), Slots: make([]masterKeyCustodySlotView, 0, len(descriptor.Slots)),
		LifecycleRunbook: "docs/runbooks/m11-kms-key-lifecycle.md",
		RecoveryRunbook:  "docs/runbooks/m11-kms-disaster-recovery.md",
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
	view.RotationIncomplete = view.PendingSlots > 0 || view.RetiringSlots > 0
	return view
}
