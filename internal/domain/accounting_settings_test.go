package domain

import (
	"testing"
	"time"
	_ "time/tzdata"
)

func TestValidateAccountingTimezone(t *testing.T) {
	for _, accepted := range []string{"UTC", "Asia/Shanghai", "Europe/Berlin", "America/Argentina/Buenos_Aires"} {
		if err := ValidateAccountingTimezone(accepted); err != nil {
			t.Fatalf("ValidateAccountingTimezone(%q) = %v, want nil", accepted, err)
		}
	}
	// Fixed offsets cannot express summer time, and Local is a property of the
	// host rather than of the instance.
	for _, rejected := range []string{"", "  ", " UTC", "Local", "UTC+08:00", "GMT-5", "+08:00", "Mars/Olympus_Mons"} {
		if err := ValidateAccountingTimezone(rejected); err == nil {
			t.Fatalf("ValidateAccountingTimezone(%q) = nil, want an error", rejected)
		}
	}
}

func TestAccountingSettingsValidate(t *testing.T) {
	effective := time.Date(2026, time.August, 6, 16, 0, 0, 0, time.UTC)
	valid := InstanceAccountingSettings{
		Timezone: "Asia/Shanghai", TimezoneVersion: 1,
		PendingTimezone: "Europe/Berlin", PendingEffectiveAt: &effective,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	// A pending zone without an instant would never take effect; an instant
	// without a zone has nothing to apply.
	halfPending := valid
	halfPending.PendingEffectiveAt = nil
	if err := halfPending.Validate(); err == nil {
		t.Fatal("pending timezone without an effective instant was accepted")
	}
	halfPending = valid
	halfPending.PendingTimezone = ""
	if err := halfPending.Validate(); err == nil {
		t.Fatal("effective instant without a pending timezone was accepted")
	}
	invalidPending := valid
	invalidPending.PendingTimezone = "UTC+02:00"
	if err := invalidPending.Validate(); err == nil {
		t.Fatal("pending fixed offset was accepted")
	}
}

// Resolving by instant rather than by a timer means a node that was down when
// the change was scheduled still agrees with one that was up.
func TestEffectiveTimezoneAt(t *testing.T) {
	effective := time.Date(2026, time.August, 6, 16, 0, 0, 0, time.UTC)
	settings := InstanceAccountingSettings{
		Timezone: "Asia/Shanghai", TimezoneVersion: 3,
		PendingTimezone: "Europe/Berlin", PendingEffectiveAt: &effective,
	}
	zone, version := settings.EffectiveTimezoneAt(effective.Add(-time.Nanosecond))
	if zone != "Asia/Shanghai" || version != 3 {
		t.Fatalf("before the effective instant = (%s, %d), want (Asia/Shanghai, 3)", zone, version)
	}
	zone, version = settings.EffectiveTimezoneAt(effective)
	if zone != "Europe/Berlin" || version != 4 {
		t.Fatalf("at the effective instant = (%s, %d), want (Europe/Berlin, 4)", zone, version)
	}
	settled := InstanceAccountingSettings{Timezone: "UTC", TimezoneVersion: 9}
	zone, version = settled.EffectiveTimezoneAt(effective)
	if zone != "UTC" || version != 9 {
		t.Fatalf("with no pending change = (%s, %d), want (UTC, 9)", zone, version)
	}
}
