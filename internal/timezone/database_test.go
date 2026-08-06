package timezone

import (
	"strings"
	"testing"
	"time"
	_ "time/tzdata"
)

func TestFingerprintIsStableAndZoneSensitive(t *testing.T) {
	zones := []string{"UTC", "Europe/Berlin"}
	first, err := Fingerprint(zones)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	second, err := Fingerprint(zones)
	if err != nil {
		t.Fatalf("Fingerprint again: %v", err)
	}
	if first != second {
		t.Fatalf("fingerprint is not deterministic: %s vs %s", first, second)
	}
	if !strings.HasPrefix(first, "sha256:") {
		t.Fatalf("fingerprint %q is not tagged with its digest algorithm", first)
	}
	// A zone with transitions must contribute to the digest; otherwise the
	// fingerprint could not detect the rule changes it exists to detect.
	utcOnly, err := Fingerprint([]string{"UTC"})
	if err != nil {
		t.Fatalf("Fingerprint UTC: %v", err)
	}
	if utcOnly == first {
		t.Fatal("adding a zone with DST transitions did not change the fingerprint")
	}
}

func TestFingerprintRejectsUnknownZone(t *testing.T) {
	if _, err := Fingerprint([]string{"Mars/Olympus_Mons"}); err == nil {
		t.Fatal("unknown zone accepted")
	}
}

func TestTransitionsFindKnownBoundaries(t *testing.T) {
	location, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatalf("load zone: %v", err)
	}
	found := transitions(location)
	// Berlin has observed summer time continuously since 1980, two transitions
	// a year, so the scan window must yield well over a hundred.
	if len(found) < 100 {
		t.Fatalf("found %d transitions, want at least 100", len(found))
	}
	want := time.Date(2026, time.March, 29, 1, 0, 0, 0, time.UTC)
	for _, change := range found {
		if change.at.Equal(want) {
			if change.offset != 2*60*60 {
				t.Fatalf("offset after %s = %d, want 7200", want, change.offset)
			}
			return
		}
	}
	t.Fatalf("2026 summer time transition at %s not found", want)
}

func TestDescribeReportsAResolvableSource(t *testing.T) {
	database, err := Describe("Asia/Shanghai")
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	switch database.Source {
	case SourceEnv, SourceSystem, SourceEmbedded:
	default:
		t.Fatalf("unexpected source %q", database.Source)
	}
	if database.Fingerprint == "" {
		t.Fatal("fingerprint is empty")
	}
	if database.Version == "" {
		t.Fatalf("version is empty; expected a value or %q", UnknownVersion)
	}
	var covered bool
	for _, zone := range database.Zones {
		if zone == "Asia/Shanghai" {
			covered = true
		}
	}
	if !covered {
		t.Fatalf("requested zone missing from %v", database.Zones)
	}
	if len(database.Zones) != len(mergeZones([]string{"Asia/Shanghai"})) {
		t.Fatalf("zones %v are not deduplicated against the baseline set", database.Zones)
	}
}
