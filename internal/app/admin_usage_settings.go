package app

import (
	"net/http"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

// The console window as a governed setting.
//
// It is the one setting in this file whose change is destructive: shortening
// the window trims the attempt history the moment the next export tick runs,
// and what is trimmed is gone from memory — it lives on in the Parquet archive,
// but the console cannot page into it. That is why the write requires the
// caller to say so rather than inferring consent from the number.

type usageSettingsResponse struct {
	ConsoleWindowDays int   `json:"console_window_days"`
	Presets           []int `json:"presets"`
	MinDays           int   `json:"min_days"`
	// MaxDays is the archive's retention: the console must not offer to page
	// back further than the archive keeps, or the screen promises history
	// nothing holds.
	MaxDays int `json:"max_days"`
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
// only nil before Open finishes seeding it, and answering zero there would let
// a trim run with no window at all.
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
		MaxDays:            r.config.Usage.RetentionDays,
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
		// AcknowledgeTrim is the caller stating that they know a shorter window
		// discards history. It is required only when the window shrinks, so the
		// console can ask once, at the moment the answer matters, rather than
		// putting a checkbox beside a setting that is usually harmless.
		AcknowledgeTrim bool `json:"acknowledge_trim"`
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
	// The archive's retention is the ceiling. It lives in config.yaml rather
	// than here because it governs files on disk rather than a screen, and a
	// window longer than it would page into partitions that have been pruned.
	if retention := r.config.Usage.RetentionDays; retention >= 1 && settings.ConsoleWindowDays > retention {
		adminBadRequestCode(writer, "console_window_exceeds_retention",
			"console window cannot exceed the archive's retention")
		return
	}
	r.adminSettingsMu.Lock()
	defer r.adminSettingsMu.Unlock()
	current := r.usageSettings.Load()
	if current == nil {
		adminStoreError(writer)
		return
	}
	if settings.ConsoleWindowDays < current.ConsoleWindowDays && !input.AcknowledgeTrim {
		adminBadRequestCode(writer, "console_window_trim_unacknowledged",
			"shortening the console window discards attempt history and must be acknowledged")
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
