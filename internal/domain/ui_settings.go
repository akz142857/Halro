package domain

import (
	"errors"
	"slices"
	"time"
)

const (
	LocaleSystem = "system"
	LocaleZhCN   = "zh-CN"
	LocaleEnUS   = "en-US"
)

const (
	AppearanceDark  = "dark"
	AppearanceLight = "light"
)

var SupportedLocales = []string{LocaleZhCN, LocaleEnUS}

// SupportedAppearances enumerates the Admin Console appearance modes shipped in
// this release. System / Auto is intentionally excluded (see PRD §3, §4.2).
var SupportedAppearances = []string{AppearanceLight, AppearanceDark}

type InstanceUISettings struct {
	DefaultLocale string    `json:"default_locale"`
	UpdatedAt     time.Time `json:"updated_at"`
	Revision      uint64    `json:"revision"`
}

func (s *InstanceUISettings) GetRevision() uint64      { return s.Revision }
func (s *InstanceUISettings) SetRevision(value uint64) { s.Revision = value }

func (s InstanceUISettings) Validate() error {
	if !IsSupportedLocale(s.DefaultLocale) {
		return errors.New("default locale is not supported")
	}
	return nil
}

func IsSupportedLocale(locale string) bool {
	return slices.Contains(SupportedLocales, locale)
}

func IsSupportedLocalePreference(locale string) bool {
	return locale == "" || locale == LocaleSystem || IsSupportedLocale(locale)
}

func NormalizeLocalePreference(locale string) string {
	if locale == "" {
		return LocaleSystem
	}
	return locale
}

// IsSupportedAppearance reports whether value is an appearance we accept on
// write. Empty is intentionally rejected here; use NormalizeAppearance for read
// paths where legacy records may carry no appearance.
func IsSupportedAppearance(value string) bool {
	return slices.Contains(SupportedAppearances, value)
}

// NormalizeAppearance maps missing or unknown appearance values to the default
// dark theme (PRD §4.3, §5.4). It is safe for lazy migration of existing admins.
func NormalizeAppearance(value string) string {
	if IsSupportedAppearance(value) {
		return value
	}
	return AppearanceDark
}
