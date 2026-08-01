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

var SupportedLocales = []string{LocaleZhCN, LocaleEnUS}

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
