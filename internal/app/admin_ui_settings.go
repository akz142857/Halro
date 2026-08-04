package app

import (
	"net/http"
	"time"

	"github.com/akz142857/Heimdall/internal/domain"
)

type adminPreferencesResponse struct {
	Locale     string `json:"locale"`
	Appearance string `json:"appearance"`
	Revision   uint64 `json:"revision"`
}

func (r *Runtime) getAdminUIBootstrap(writer http.ResponseWriter, _ *http.Request) {
	settings := r.uiSettings.Load()
	if settings == nil {
		adminStoreError(writer)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"default_locale":    settings.DefaultLocale,
		"supported_locales": domain.SupportedLocales,
	})
}

func (r *Runtime) getAdminUISettings(writer http.ResponseWriter, _ *http.Request) {
	settings := r.uiSettings.Load()
	if settings == nil {
		adminStoreError(writer)
		return
	}
	writer.Header().Set("ETag", revisionETag(settings.Revision))
	writeJSON(writer, http.StatusOK, settings)
}

func (r *Runtime) updateAdminUISettings(writer http.ResponseWriter, request *http.Request) {
	expected, ok := requireRevision(writer, request)
	if !ok {
		return
	}
	var input struct {
		DefaultLocale string `json:"default_locale"`
	}
	if err := decodeAdminJSON(request, &input); err != nil {
		adminBadRequest(writer, "invalid request")
		return
	}
	settings := domain.InstanceUISettings{DefaultLocale: input.DefaultLocale, UpdatedAt: time.Now().UTC()}
	if err := settings.Validate(); err != nil {
		adminBadRequest(writer, err.Error())
		return
	}
	r.adminSettingsMu.Lock()
	defer r.adminSettingsMu.Unlock()
	settings, err := r.store.PutInstanceUISettings(settings, expected)
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	if err := r.auditAdminMutation(request, "settings.ui.update", "settings", "ui"); err != nil {
		adminAuditError(writer)
		return
	}
	r.uiSettings.Store(&settings)
	writer.Header().Set("ETag", revisionETag(settings.Revision))
	writeJSON(writer, http.StatusOK, settings)
}

func (r *Runtime) getAdminPreferences(writer http.ResponseWriter, request *http.Request) {
	admin := request.Context().Value(adminContextKey{}).(adminRequestContext)
	user, err := r.store.GetAdminUser(request.Context(), admin.session.Username)
	if err != nil {
		adminStoreError(writer)
		return
	}
	writer.Header().Set("ETag", revisionETag(user.Revision))
	writeJSON(writer, http.StatusOK, adminPreferencesResponse{
		Locale:     domain.NormalizeLocalePreference(user.Locale),
		Appearance: domain.NormalizeAppearance(user.Appearance),
		Revision:   user.Revision,
	})
}

func (r *Runtime) updateAdminPreferences(writer http.ResponseWriter, request *http.Request) {
	expected, ok := requireRevision(writer, request)
	if !ok {
		return
	}
	// The client must submit the complete writable preference resource so that
	// updating one field never silently clears another (PRD §4.4, §9.2).
	var input struct {
		Locale     string `json:"locale"`
		Appearance string `json:"appearance"`
	}
	if err := decodeAdminJSON(request, &input); err != nil || !domain.IsSupportedLocalePreference(input.Locale) {
		adminBadRequest(writer, "locale preference is not supported")
		return
	}
	if !domain.IsSupportedAppearance(input.Appearance) {
		adminBadRequest(writer, "appearance preference is not supported")
		return
	}
	admin := request.Context().Value(adminContextKey{}).(adminRequestContext)
	r.adminSettingsMu.Lock()
	defer r.adminSettingsMu.Unlock()
	user, err := r.store.GetAdminUser(request.Context(), admin.session.Username)
	if err != nil {
		adminStoreError(writer)
		return
	}
	user.Locale = domain.NormalizeLocalePreference(input.Locale)
	user.Appearance = domain.NormalizeAppearance(input.Appearance)
	user.UpdatedAt = time.Now().UTC()
	user, err = r.store.PutAdminUser(request.Context(), user, expected)
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	if err := r.auditAdminMutation(request, "admin.preferences.update", "admin_user", user.Username); err != nil {
		adminAuditError(writer)
		return
	}
	writer.Header().Set("ETag", revisionETag(user.Revision))
	writeJSON(writer, http.StatusOK, adminPreferencesResponse{
		Locale:     user.Locale,
		Appearance: user.Appearance,
		Revision:   user.Revision,
	})
}
