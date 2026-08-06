package app

import (
	"net/http"
	"time"

	"github.com/akz142857/Heimdall/internal/budget"
	"github.com/akz142857/Heimdall/internal/domain"
	"github.com/akz142857/Heimdall/internal/timezone"
)

// Stable error codes for the accounting timezone. A rejected change should tell
// an administrator which rule it broke, not just that it failed.
//
// There is deliberately no code for "a change is already scheduled": a new
// submission supersedes the pending one, which is the useful behaviour and
// never an error.
const (
	errTimezoneInvalid       = "timezone_invalid"
	errTimezonePendingAbsent = "timezone_pending_absent"
	errTimezonePendingPassed = "timezone_pending_elapsed"
)

type accountingSettingsResponse struct {
	Timezone           string      `json:"timezone"`
	TimezoneVersion    uint64      `json:"timezone_version"`
	PendingTimezone    string      `json:"pending_timezone,omitempty"`
	PendingEffectiveAt *time.Time  `json:"pending_effective_at,omitempty"`
	CurrentPeriod      periodBody  `json:"current_period"`
	ConfigFileTimezone string      `json:"config_file_timezone"`
	ConfigFileInEffect bool        `json:"config_file_in_effect"`
	Tzdata             *tzdataBody `json:"tzdata,omitempty"`
	// SwitchPreview describes what a proposed change would do, when the caller
	// asked for one with ?preview_timezone=. Present only on that request.
	SwitchPreview *switchPreview `json:"switch_preview,omitempty"`
	UpdatedAt     time.Time      `json:"updated_at"`
	Revision      uint64         `json:"revision"`
}

// switchPreview answers the question the confirmation dialog has to ask on the
// administrator's behalf: when does the budget next reset, and how soon?
//
// A switch mints a period in the new zone that can begin before the switch
// instant, and that period is a fresh balance because the timezone version
// changed. The daily allowance therefore starts again NextResetIn after the
// change — which can be as little as an hour — giving close to two full days of
// budget inside one wall-clock day. The number is unintuitive enough that it
// has to be shown rather than derived.
type switchPreview struct {
	Timezone         string     `json:"timezone"`
	EffectiveAt      time.Time  `json:"effective_at"`
	FirstPeriod      periodBody `json:"first_period"`
	NextResetAt      time.Time  `json:"next_reset_at"`
	NextResetInHours float64    `json:"next_reset_in_hours"`
}

type periodBody struct {
	PeriodID    string    `json:"period_id"`
	PeriodStart time.Time `json:"period_start"`
	PeriodEnd   time.Time `json:"period_end"`
}

type tzdataBody struct {
	Source      string `json:"source"`
	Version     string `json:"version"`
	Fingerprint string `json:"fingerprint"`
}

func (r *Runtime) accountingSettingsBody(now time.Time) (accountingSettingsResponse, error) {
	return r.accountingSettingsBodyWithPreview(now, "")
}

// previewSwitch computes the first period the target zone would produce at the
// switch instant, and how long after the switch the budget next resets.
func (r *Runtime) previewSwitch(targetZone string, effectiveAt time.Time) (*switchPreview, error) {
	location, err := time.LoadLocation(targetZone)
	if err != nil {
		return nil, err
	}
	period, err := budget.PeriodAt(effectiveAt, location)
	if err != nil {
		return nil, err
	}
	return &switchPreview{
		Timezone:    targetZone,
		EffectiveAt: effectiveAt.UTC(),
		FirstPeriod: periodBody{
			PeriodID: period.ID, PeriodStart: period.Start, PeriodEnd: period.End,
		},
		NextResetAt:      period.End,
		NextResetInHours: period.End.Sub(effectiveAt).Hours(),
	}, nil
}

func (r *Runtime) accountingSettingsBodyWithPreview(now time.Time, previewTimezone string) (accountingSettingsResponse, error) {
	settings := r.periods.Settings()
	period, err := r.periods.PeriodAt(now)
	if err != nil {
		return accountingSettingsResponse{}, err
	}
	body := accountingSettingsResponse{
		Timezone:        settings.Timezone,
		TimezoneVersion: settings.TimezoneVersion,
		PendingTimezone: settings.PendingTimezone,
		CurrentPeriod: periodBody{
			PeriodID: period.ID, PeriodStart: period.Start, PeriodEnd: period.End,
		},
		// Reported so an operator who edited config.yaml can see at a glance
		// that the file is no longer what decides the boundary.
		ConfigFileTimezone: r.config.Usage.Timezone,
		ConfigFileInEffect: r.config.Usage.Timezone == settings.Timezone,
		UpdatedAt:          settings.UpdatedAt,
		Revision:           settings.Revision,
	}
	if settings.PendingEffectiveAt != nil {
		effective := *settings.PendingEffectiveAt
		body.PendingEffectiveAt = &effective
	}
	if database, err := timezone.Describe(settings.Timezone); err == nil {
		body.Tzdata = &tzdataBody{Source: database.Source, Version: database.Version, Fingerprint: database.Fingerprint}
	}
	if previewTimezone != "" && previewTimezone != settings.Timezone {
		preview, err := r.previewSwitch(previewTimezone, period.End)
		if err != nil {
			return accountingSettingsResponse{}, err
		}
		body.SwitchPreview = preview
	}
	return body, nil
}

// timezoneChangeMetadata records what a timezone change actually moved.
//
// The action name alone cannot answer the question an auditor asks — which
// boundary applied to a given charge — so the payload carries both zones, both
// versions, and the instant the switch takes effect.
func timezoneChangeMetadata(before, after domain.InstanceAccountingSettings) map[string]any {
	metadata := map[string]any{
		"from_timezone":    before.Timezone,
		"from_version":     before.TimezoneVersion,
		"to_timezone":      after.Timezone,
		"to_version":       after.TimezoneVersion,
		"pending_timezone": after.PendingTimezone,
	}
	if after.PendingEffectiveAt != nil {
		metadata["effective_at"] = after.PendingEffectiveAt.UTC().Format(time.RFC3339)
	}
	if before.PendingTimezone != "" {
		metadata["superseded_pending_timezone"] = before.PendingTimezone
	}
	return metadata
}

func (r *Runtime) auditTimezoneChange(request *http.Request, action string, before, after domain.InstanceAccountingSettings) error {
	admin := request.Context().Value(adminContextKey{}).(adminRequestContext)
	return r.appendAdminAuditWithMetadata("admin_user", admin.session.Username, action,
		"instance_accounting", "accounting_timezone", "success", "", timezoneChangeMetadata(before, after))
}

func (r *Runtime) getAdminAccountingSettings(writer http.ResponseWriter, request *http.Request) {
	preview := request.URL.Query().Get("preview_timezone")
	if preview != "" {
		if err := domain.ValidateAccountingTimezone(preview); err != nil {
			adminBadRequestCode(writer, errTimezoneInvalid, err.Error())
			return
		}
	}
	body, err := r.accountingSettingsBodyWithPreview(time.Now(), preview)
	if err != nil {
		adminStoreError(writer)
		return
	}
	writer.Header().Set("ETag", revisionETag(body.Revision))
	writeJSON(writer, http.StatusOK, body)
}

// updateAdminAccountingSettings schedules a timezone change for the end of the
// period in progress.
//
// It never takes effect immediately. Moving the boundary under a day that is
// already accumulating would change what "today" means halfway through it,
// after budgets had been enforced against the old one.
func (r *Runtime) updateAdminAccountingSettings(writer http.ResponseWriter, request *http.Request) {
	expected, ok := requireRevision(writer, request)
	if !ok {
		return
	}
	var input struct {
		Timezone string `json:"timezone"`
	}
	if err := decodeAdminJSON(request, &input); err != nil {
		adminBadRequestCode(writer, errTimezoneInvalid, "invalid request")
		return
	}
	if err := domain.ValidateAccountingTimezone(input.Timezone); err != nil {
		adminBadRequestCode(writer, errTimezoneInvalid, err.Error())
		return
	}
	r.adminSettingsMu.Lock()
	defer r.adminSettingsMu.Unlock()

	now := time.Now()
	current := r.periods.Settings()
	period, err := r.periods.PeriodAt(now)
	if err != nil {
		adminStoreError(writer)
		return
	}
	// Asking for the zone already in force is not an error and must not burn a
	// version: the version is a ledger key, and a no-op that advanced it would
	// split a day's balance in two for nothing.
	if input.Timezone == current.Timezone && !current.HasPendingChange() {
		body, err := r.accountingSettingsBody(now)
		if err != nil {
			adminStoreError(writer)
			return
		}
		writer.Header().Set("ETag", revisionETag(body.Revision))
		writeJSON(writer, http.StatusOK, body)
		return
	}

	updated := current
	updated.PendingTimezone = input.Timezone
	effective := period.End
	updated.PendingEffectiveAt = &effective
	updated.UpdatedAt = now.UTC()
	if input.Timezone == current.Timezone {
		// Reverting to the zone already in force: drop the scheduled change
		// rather than scheduling a move to where we already are.
		updated.PendingTimezone = ""
		updated.PendingEffectiveAt = nil
	}
	stored, err := r.store.PutInstanceAccountingSettings(updated, expected)
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	if err := r.periods.Update(stored); err != nil {
		adminStoreError(writer)
		return
	}
	action := "accounting.timezone.change_scheduled"
	if updated.PendingTimezone == "" {
		action = "accounting.timezone.change_cancelled"
	}
	if err := r.auditTimezoneChange(request, action, current, stored); err != nil {
		adminAuditError(writer)
		return
	}
	body, err := r.accountingSettingsBody(now)
	if err != nil {
		adminStoreError(writer)
		return
	}
	writer.Header().Set("ETag", revisionETag(body.Revision))
	writeJSON(writer, http.StatusOK, body)
}

// cancelAdminAccountingTimezoneChange withdraws a scheduled change that has not
// taken effect. Once the moment has passed the change is part of the ledger's
// history and cannot be withdrawn, only superseded by another change.
func (r *Runtime) cancelAdminAccountingTimezoneChange(writer http.ResponseWriter, request *http.Request) {
	expected, ok := requireRevision(writer, request)
	if !ok {
		return
	}
	r.adminSettingsMu.Lock()
	defer r.adminSettingsMu.Unlock()

	now := time.Now()
	current := r.periods.Settings()
	if !current.HasPendingChange() {
		adminBadRequestCode(writer, errTimezonePendingAbsent, "no scheduled timezone change")
		return
	}
	if !now.Before(*current.PendingEffectiveAt) {
		adminBadRequestCode(writer, errTimezonePendingPassed, "the scheduled change has already taken effect")
		return
	}
	updated := current
	updated.PendingTimezone = ""
	updated.PendingEffectiveAt = nil
	updated.UpdatedAt = now.UTC()
	stored, err := r.store.PutInstanceAccountingSettings(updated, expected)
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	if err := r.periods.Update(stored); err != nil {
		adminStoreError(writer)
		return
	}
	if err := r.auditTimezoneChange(request, "accounting.timezone.change_cancelled", current, stored); err != nil {
		adminAuditError(writer)
		return
	}
	body, err := r.accountingSettingsBody(now)
	if err != nil {
		adminStoreError(writer)
		return
	}
	writer.Header().Set("ETag", revisionETag(body.Revision))
	writeJSON(writer, http.StatusOK, body)
}

// applyDueAccountingTimezoneChange promotes a scheduled change once its moment
// has passed, so the stored setting stops describing it as pending.
//
// Correctness does not depend on this running: the resolver already reports the
// new zone from the effective instant onward, whether or not any node was
// awake to promote it. This only settles the record and emits the audit event.
func (r *Runtime) applyDueAccountingTimezoneChange(now time.Time) error {
	r.adminSettingsMu.Lock()
	defer r.adminSettingsMu.Unlock()
	current := r.periods.Settings()
	if !current.HasPendingChange() || now.Before(*current.PendingEffectiveAt) {
		return nil
	}
	applied := current
	applied.Timezone = current.PendingTimezone
	applied.TimezoneVersion = current.TimezoneVersion + 1
	applied.PendingTimezone = ""
	applied.PendingEffectiveAt = nil
	applied.UpdatedAt = now.UTC()
	stored, err := r.store.PutInstanceAccountingSettings(applied, current.Revision)
	if err != nil {
		return err
	}
	if err := r.periods.Update(stored); err != nil {
		return err
	}
	return r.appendAdminAuditWithMetadata("system", "", "accounting.timezone.change_applied",
		"instance_accounting", "accounting_timezone", "success", "", timezoneChangeMetadata(current, stored))
}
