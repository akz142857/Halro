package app

import (
	"net/http"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

type settingsInput struct {
	HealthProbeIntervalSeconds int64 `json:"health_probe_interval_seconds"`
}

func (r *Runtime) getAdminSettings(writer http.ResponseWriter, _ *http.Request) {
	settings := r.runtimeSettings.Load()
	if settings == nil {
		adminStoreError(writer)
		return
	}
	writer.Header().Set("ETag", revisionETag(settings.Revision))
	writeJSON(writer, http.StatusOK, settings)
}

func (r *Runtime) updateAdminSettings(writer http.ResponseWriter, request *http.Request) {
	expected, ok := requireRevision(writer, request)
	if !ok {
		return
	}
	var input settingsInput
	if err := decodeAdminJSON(request, &input); err != nil {
		adminBadRequestCode(writer, "invalid_request", "invalid request")
		return
	}
	settings := domain.RuntimeSettings{
		HealthProbeIntervalSeconds: input.HealthProbeIntervalSeconds,
		UpdatedAt:                  time.Now().UTC(),
	}
	if err := settings.Validate(); err != nil {
		adminBadRequest(writer, err.Error())
		return
	}
	r.adminSettingsMu.Lock()
	defer r.adminSettingsMu.Unlock()
	settings, err := r.store.PutRuntimeSettings(settings, expected)
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	if err := r.auditAdminMutation(request, "settings.update", "settings", "runtime"); err != nil {
		adminAuditError(writer)
		return
	}
	r.runtimeSettings.Store(&settings)
	writer.Header().Set("ETag", revisionETag(settings.Revision))
	writeJSON(writer, http.StatusOK, settings)
}
