package app

import (
	"net/http"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

// The console window as a governed setting: how many days the attempt log and
// the failed-request list show by default. Changing it discards nothing — the
// usage history index keeps every attempt whatever the window says.

type usageSettingsResponse struct {
	ConsoleWindowDays int   `json:"console_window_days"`
	Presets           []int `json:"presets"`
	MinDays           int   `json:"min_days"`
	// ConfigFileDays and ConfigFileInEffect are reported for the same reason the
	// accounting settings report them: an operator who edited config.yaml should
	// be able to see at a glance that the file is no longer what decides this.
	ConfigFileDays     int       `json:"config_file_days"`
	ConfigFileInEffect bool      `json:"config_file_in_effect"`
	UpdatedAt          time.Time `json:"updated_at"`
	Revision           uint64    `json:"revision"`
}

// consoleWindowDays is the live window every read of it goes through.
//
// Falling back to config.yaml is not a second source of truth: the pointer is
// only nil before Open finishes seeding it.
func (r *Runtime) consoleWindowDays() int {
	if settings := r.usageSettings.Load(); settings != nil {
		return settings.ConsoleWindowDays
	}
	return r.config.Usage.ConsoleWindowDays
}

func (r *Runtime) usageSettingsBody(settings domain.InstanceUsageSettings) usageSettingsResponse {
	return usageSettingsResponse{
		ConsoleWindowDays:  settings.ConsoleWindowDays,
		Presets:            domain.ConsoleWindowPresets,
		MinDays:            domain.MinInstanceConsoleWindowDays,
		ConfigFileDays:     r.config.Usage.ConsoleWindowDays,
		ConfigFileInEffect: r.config.Usage.ConsoleWindowDays == settings.ConsoleWindowDays,
		UpdatedAt:          settings.UpdatedAt,
		Revision:           settings.Revision,
	}
}

func (r *Runtime) getAdminUsageSettings(writer http.ResponseWriter, _ *http.Request) {
	settings := r.usageSettings.Load()
	if settings == nil {
		adminStoreError(writer)
		return
	}
	writer.Header().Set("ETag", revisionETag(settings.Revision))
	writeJSON(writer, http.StatusOK, r.usageSettingsBody(*settings))
}

func (r *Runtime) updateAdminUsageSettings(writer http.ResponseWriter, request *http.Request) {
	expected, ok := requireRevision(writer, request)
	if !ok {
		return
	}
	var input struct {
		ConsoleWindowDays *int `json:"console_window_days"`
	}
	if err := decodeAdminJSON(request, &input); err != nil {
		adminBadRequestCode(writer, "invalid_request", "invalid request")
		return
	}
	if input.ConsoleWindowDays == nil {
		adminBadRequestCode(writer, "invalid_console_window", "console window is required")
		return
	}
	settings := domain.InstanceUsageSettings{
		ConsoleWindowDays: *input.ConsoleWindowDays, UpdatedAt: time.Now().UTC(),
	}
	if err := settings.Validate(); err != nil {
		adminBadRequestCode(writer, "invalid_console_window", err.Error())
		return
	}
	r.adminSettingsMu.Lock()
	defer r.adminSettingsMu.Unlock()
	current := r.usageSettings.Load()
	if current == nil {
		adminStoreError(writer)
		return
	}
	stored, err := r.store.PutInstanceUsageSettings(settings, expected)
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	if err := r.auditAdminMutation(request, "settings.usage.update", "settings", "usage"); err != nil {
		adminAuditError(writer)
		return
	}
	r.usageSettings.Store(&stored)
	writer.Header().Set("ETag", revisionETag(stored.Revision))
	writeJSON(writer, http.StatusOK, r.usageSettingsBody(stored))
}
