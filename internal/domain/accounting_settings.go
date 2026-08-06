package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// InstanceAccountingSettings is the accounting time zone: the one setting that
// decides where a day ends.
//
// It lives here rather than in config.yaml because of what depends on it. A
// project's daily budget resets at midnight in this zone and every charge is
// filed under that zone's calendar date, so a change to it moves money between
// days. That needs a revision, an audit trail, and a controlled moment of
// effect — none of which a stateless configuration file can carry.
//
// Display of timestamps in the Admin Console is a separate concern and is not
// governed here; see the per-administrator preference.
type InstanceAccountingSettings struct {
	Timezone string `json:"timezone"`
	// TimezoneVersion increments on each applied change. It is part of the
	// ledger balance key, so periods either side of a change can never merge.
	TimezoneVersion uint64 `json:"timezone_version"`
	// PendingTimezone and PendingEffectiveAt hold a change that has been
	// accepted but has not reached its moment of effect. Both are set or
	// neither is.
	PendingTimezone    string     `json:"pending_timezone,omitempty"`
	PendingEffectiveAt *time.Time `json:"pending_effective_at,omitempty"`
	UpdatedAt          time.Time  `json:"updated_at"`
	Revision           uint64     `json:"revision"`
}

func (s *InstanceAccountingSettings) GetRevision() uint64      { return s.Revision }
func (s *InstanceAccountingSettings) SetRevision(value uint64) { s.Revision = value }

func (s InstanceAccountingSettings) Validate() error {
	if err := ValidateAccountingTimezone(s.Timezone); err != nil {
		return err
	}
	if (s.PendingTimezone == "") != (s.PendingEffectiveAt == nil) {
		return errors.New("pending timezone and its effective instant must be set together")
	}
	if s.PendingTimezone != "" {
		if err := ValidateAccountingTimezone(s.PendingTimezone); err != nil {
			return fmt.Errorf("pending %w", err)
		}
	}
	return nil
}

// HasPendingChange reports whether a change is waiting for its moment of effect.
func (s InstanceAccountingSettings) HasPendingChange() bool {
	return s.PendingTimezone != "" && s.PendingEffectiveAt != nil
}

// EffectiveTimezoneAt reports the zone and version in force at instant, taking
// a pending change into account once its effective instant has passed.
//
// Reading it this way rather than mutating on a timer means every node reaches
// the same answer for the same instant, whether or not it was running when the
// change was scheduled.
func (s InstanceAccountingSettings) EffectiveTimezoneAt(instant time.Time) (string, uint64) {
	if s.HasPendingChange() && !instant.Before(*s.PendingEffectiveAt) {
		return s.PendingTimezone, s.TimezoneVersion + 1
	}
	return s.Timezone, s.TimezoneVersion
}

// ValidateAccountingTimezone accepts an IANA zone name and nothing else.
//
// A fixed offset such as "UTC+08:00" cannot express summer time, so the days on
// either side of a transition would come out the wrong length. "Local" resolves
// against whatever the host is set to, which would make the boundary a property
// of the machine rather than of the instance.
func ValidateAccountingTimezone(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("accounting timezone is required")
	}
	if value != strings.TrimSpace(value) {
		return errors.New("accounting timezone must not be padded with whitespace")
	}
	if value == "Local" {
		return errors.New("accounting timezone must not be Local; it would depend on the host")
	}
	if isFixedOffsetZone(value) {
		return errors.New("accounting timezone must be an IANA name, not a fixed offset")
	}
	if _, err := time.LoadLocation(value); err != nil {
		return fmt.Errorf("accounting timezone %q is not a known IANA name", value)
	}
	return nil
}

// isFixedOffsetZone catches the shapes people reach for instead of a zone name:
// "UTC+8", "GMT-05:00", "+08:00". time.LoadLocation rejects most of them, but
// naming the reason is more useful than "unknown zone", and some do parse.
func isFixedOffsetZone(value string) bool {
	trimmed := strings.TrimPrefix(strings.TrimPrefix(value, "GMT"), "UTC")
	if trimmed == value {
		return strings.HasPrefix(value, "+") || strings.HasPrefix(value, "-")
	}
	return strings.HasPrefix(trimmed, "+") || strings.HasPrefix(trimmed, "-")
}
