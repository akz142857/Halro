package app

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
	_ "time/tzdata"
)

func doctorCheck(t *testing.T, report DoctorReport, name string) DoctorCheck {
	t.Helper()
	for _, check := range report.Checks {
		if check.Name == name {
			return check
		}
	}
	t.Fatalf("doctor produced no %q check", name)
	return DoctorCheck{}
}

// doctor is the only view an operator has of the accounting boundary on a
// stopped instance. Reporting the zone without the interval it produces would
// leave the interesting half unsaid, and the tzdata fingerprint is the only way
// to tell two instances apart when their version strings agree.
func TestDoctorReportsTheAccountingBoundary(t *testing.T) {
	cfg := testConfig(t)
	cfg.Usage.Timezone = "Asia/Shanghai"
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	// Initialize does not seed the stored setting; opening the runtime does.
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}

	report, err := Doctor(context.Background(), cfg)
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}

	accounting := doctorCheck(t, report, "accounting_timezone")
	if accounting.Status != "pass" {
		t.Fatalf("accounting_timezone status=%s detail=%s", accounting.Status, accounting.Detail)
	}
	for _, want := range []string{"stored=Asia/Shanghai", "version=1"} {
		if !strings.Contains(accounting.Detail, want) {
			t.Fatalf("accounting_timezone detail %q is missing %q", accounting.Detail, want)
		}
	}

	clock := doctorCheck(t, report, "clock")
	if clock.Status != "pass" {
		t.Fatalf("clock status=%s detail=%s", clock.Status, clock.Detail)
	}
	// The zone alone does not tell an operator which interval is in force.
	for _, want := range []string{"accounting timezone=Asia/Shanghai", "current period"} {
		if !strings.Contains(clock.Detail, want) {
			t.Fatalf("clock detail %q is missing %q", clock.Detail, want)
		}
	}

	tzdata := doctorCheck(t, report, "tzdata")
	if tzdata.Status != "pass" {
		t.Fatalf("tzdata status=%s detail=%s", tzdata.Status, tzdata.Detail)
	}
	for _, want := range []string{"source=", "version=", "fingerprint=sha256:"} {
		if !strings.Contains(tzdata.Detail, want) {
			t.Fatalf("tzdata detail %q is missing %q", tzdata.Detail, want)
		}
	}
}

// config.yaml seeds the zone once and then loses its say. An operator who edits
// the file and restarts has no other way to discover the edit did nothing.
func TestDoctorWarnsWhenTheConfigFileNoLongerApplies(t *testing.T) {
	cfg := testConfig(t)
	cfg.Usage.Timezone = "UTC"
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	settings := runtime.periods.Settings()
	settings.Timezone = "Europe/Berlin"
	settings.TimezoneVersion++
	settings.UpdatedAt = time.Now().UTC()
	if _, err := runtime.store.PutInstanceAccountingSettings(settings, settings.Revision); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}

	report, err := Doctor(context.Background(), cfg)
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	accounting := doctorCheck(t, report, "accounting_timezone")
	if accounting.Status != "warn" {
		t.Fatalf("status=%s, want warn; detail=%s", accounting.Status, accounting.Detail)
	}
	if !strings.Contains(accounting.Detail, "no longer applied") {
		t.Fatalf("detail %q does not say the configuration file is ignored", accounting.Detail)
	}
	// The boundary reported must be the stored one, not the file's.
	clock := doctorCheck(t, report, "clock")
	if !strings.Contains(clock.Detail, "accounting timezone=Europe/Berlin") {
		t.Fatalf("clock detail %q did not follow the stored zone", clock.Detail)
	}
}
