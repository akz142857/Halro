package bolt

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/akz142857/Halro/internal/domain"
)

func openAccountingStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// config.yaml seeds the zone once and then loses its say. An operator who edits
// the file later must not silently move a boundary that was changed
// deliberately through the audited path.
func TestSeedInstanceAccountingSettingsAppliesOnlyOnce(t *testing.T) {
	store := openAccountingStore(t)
	now := time.Date(2026, time.August, 6, 1, 0, 0, 0, time.UTC)

	seeded, err := store.SeedInstanceAccountingSettings("Asia/Shanghai", now)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if seeded.Timezone != "Asia/Shanghai" || seeded.TimezoneVersion != 1 {
		t.Fatalf("seeded = %#v", seeded)
	}

	changed := seeded
	changed.Timezone = "Europe/Berlin"
	changed.TimezoneVersion = 2
	if _, err := store.PutInstanceAccountingSettings(changed, seeded.Revision); err != nil {
		t.Fatalf("put: %v", err)
	}

	reseeded, err := store.SeedInstanceAccountingSettings("Asia/Shanghai", now)
	if err != nil {
		t.Fatalf("reseed: %v", err)
	}
	if reseeded.Timezone != "Europe/Berlin" || reseeded.TimezoneVersion != 2 {
		t.Fatalf("configuration overwrote a governed change: %#v", reseeded)
	}
}

func TestInstanceAccountingSettingsRevisionAndValidation(t *testing.T) {
	store := openAccountingStore(t)
	now := time.Date(2026, time.August, 6, 1, 0, 0, 0, time.UTC)

	if _, err := store.InstanceAccountingSettings(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("before seeding = %v, want ErrNotFound", err)
	}
	seeded, err := store.SeedInstanceAccountingSettings("UTC", now)
	if err != nil {
		t.Fatal(err)
	}

	stale := seeded
	stale.Timezone = "Europe/Berlin"
	if _, err := store.PutInstanceAccountingSettings(stale, seeded.Revision+7); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale revision = %v, want ErrRevisionConflict", err)
	}

	invalid := seeded
	invalid.Timezone = "UTC+08:00"
	if _, err := store.PutInstanceAccountingSettings(invalid, seeded.Revision); err == nil {
		t.Fatal("fixed offset was persisted")
	}

	// The version is part of the ledger balance key. Rewinding it would let two
	// periods that were deliberately kept apart merge into one balance.
	rewound := seeded
	rewound.TimezoneVersion = 0
	if _, err := store.PutInstanceAccountingSettings(rewound, seeded.Revision); err == nil {
		t.Fatal("timezone version was allowed to move backwards")
	}
}

func TestInstanceAccountingSettingsRoundTripsPendingChange(t *testing.T) {
	store := openAccountingStore(t)
	now := time.Date(2026, time.August, 6, 1, 0, 0, 0, time.UTC)
	seeded, err := store.SeedInstanceAccountingSettings("Asia/Shanghai", now)
	if err != nil {
		t.Fatal(err)
	}

	effective := time.Date(2026, time.August, 6, 16, 0, 0, 0, time.UTC)
	pending := seeded
	pending.PendingTimezone = "Europe/Berlin"
	pending.PendingEffectiveAt = &effective
	if _, err := store.PutInstanceAccountingSettings(pending, seeded.Revision); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.InstanceAccountingSettings()
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.HasPendingChange() || loaded.PendingTimezone != "Europe/Berlin" {
		t.Fatalf("loaded = %#v", loaded)
	}
	if !loaded.PendingEffectiveAt.Equal(effective) {
		t.Fatalf("effective instant = %s, want %s", loaded.PendingEffectiveAt, effective)
	}
	zone, version := loaded.EffectiveTimezoneAt(effective)
	if zone != "Europe/Berlin" || version != seeded.TimezoneVersion+1 {
		t.Fatalf("at the effective instant = (%s, %d)", zone, version)
	}
	var _ domain.InstanceAccountingSettings = loaded
}
