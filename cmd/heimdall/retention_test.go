package main

import (
	"testing"
	"time"
	_ "time/tzdata"
)

// retention_days is a floor, not an exact age. The promise is read in the
// operator's local day while partitions are dated in UTC, so the cutoff has to
// leave room for the offset between them — otherwise an operator east of UTC
// loses a day they were told they still had.
func TestUsageRetentionCutoffKeepsAtLeastTheRetainedDays(t *testing.T) {
	// Noon UTC is the least forgiving hour to test from: an instance at UTC+14
	// is already on tomorrow's date, and one at UTC-12 is still on yesterday's.
	now := time.Date(2026, time.August, 6, 12, 0, 0, 0, time.UTC)
	const retentionDays = 90

	cutoff := usageRetentionCutoff(now, retentionDays)
	if got := now.Sub(cutoff); got != time.Duration(retentionDays+1)*24*time.Hour {
		t.Fatalf("cutoff is %s before now, want %d days", got, retentionDays+1)
	}

	for _, zone := range []string{"Pacific/Kiritimati", "Asia/Shanghai", "UTC", "America/New_York", "Etc/GMT+12"} {
		t.Run(zone, func(t *testing.T) {
			location, err := time.LoadLocation(zone)
			if err != nil {
				t.Fatal(err)
			}
			// The oldest local day the operator was promised, and the UTC
			// partition date that day's data is filed under.
			local := now.In(location)
			promised := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location).
				AddDate(0, 0, -(retentionDays - 1))
			partition := promised.UTC().Format("2006-01-02")
			if partition < cutoff.Format("2006-01-02") {
				t.Fatalf("partition %s for the operator's oldest promised day would be pruned at cutoff %s",
					partition, cutoff.Format("2006-01-02"))
			}
		})
	}
}

// The extra day is one partition, not an open-ended reprieve: retention has to
// stay bounded or the floor becomes a leak.
func TestUsageRetentionCutoffKeepsExactlyOneExtraDay(t *testing.T) {
	now := time.Date(2026, time.August, 6, 0, 0, 0, 0, time.UTC)
	for _, days := range []int{1, 7, 90, 365} {
		exact := now.UTC().AddDate(0, 0, -days)
		cutoff := usageRetentionCutoff(now, days)
		if got := exact.Sub(cutoff); got != 24*time.Hour {
			t.Fatalf("retention %d days keeps %s beyond the exact age, want 24h", days, got)
		}
	}
}
