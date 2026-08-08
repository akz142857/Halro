package budget

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

// PeriodResolver is the one place a daily accounting period is derived.
//
// Deriving a period is not formatting a date. It needs the governed accounting
// timezone, the version that zone is on, and calendar arithmetic that survives
// summer time — and every caller must agree on all three, or two of them will
// file the same charge under different days. Nothing outside this type may
// format a period identifier.
type PeriodResolver struct {
	settings  atomic.Pointer[domain.InstanceAccountingSettings]
	locations sync.Map // zone name -> *time.Location
}

// NewPeriodResolver fails rather than falling back to UTC: a resolver that
// silently disagreed with the stored setting would misfile charges with no
// symptom.
func NewPeriodResolver(settings domain.InstanceAccountingSettings) (*PeriodResolver, error) {
	resolver := &PeriodResolver{}
	if err := resolver.Update(settings); err != nil {
		return nil, err
	}
	return resolver, nil
}

// NewFixedPeriodResolver builds a resolver pinned to one zone, for callers with
// no stored settings to read: offline diagnostics, and tests.
func NewFixedPeriodResolver(timezone string) (*PeriodResolver, error) {
	return NewPeriodResolver(domain.InstanceAccountingSettings{
		Timezone: timezone, TimezoneVersion: 1, UpdatedAt: time.Now().UTC(), Revision: 1,
	})
}

// Update adopts a changed setting. Callers apply this after a successful write
// so the running process stops reading the previous zone.
func (r *PeriodResolver) Update(settings domain.InstanceAccountingSettings) error {
	if err := settings.Validate(); err != nil {
		return err
	}
	if _, err := r.location(settings.Timezone); err != nil {
		return err
	}
	if settings.PendingTimezone != "" {
		if _, err := r.location(settings.PendingTimezone); err != nil {
			return err
		}
	}
	r.settings.Store(&settings)
	return nil
}

// Settings returns the accounting settings currently in force.
func (r *PeriodResolver) Settings() domain.InstanceAccountingSettings {
	if current := r.settings.Load(); current != nil {
		return *current
	}
	return domain.InstanceAccountingSettings{}
}

// LocationAt returns the zone that governs instant, honouring a scheduled
// change once its effective moment has passed.
func (r *PeriodResolver) LocationAt(instant time.Time) (*time.Location, error) {
	name, _ := r.Settings().EffectiveTimezoneAt(instant)
	return r.location(name)
}

// PeriodAt returns the accounting period containing instant, stamped with the
// timezone version that produced it.
func (r *PeriodResolver) PeriodAt(instant time.Time) (Period, error) {
	settings := r.Settings()
	if settings.Timezone == "" {
		return Period{}, errors.New("accounting settings are not loaded")
	}
	name, version := settings.EffectiveTimezoneAt(instant)
	location, err := r.location(name)
	if err != nil {
		return Period{}, err
	}
	period, err := PeriodAt(instant, location)
	if err != nil {
		return Period{}, err
	}
	period.TimezoneVersion = version
	return period, nil
}

func (r *PeriodResolver) location(name string) (*time.Location, error) {
	if cached, ok := r.locations.Load(name); ok {
		return cached.(*time.Location), nil
	}
	location, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("load accounting timezone %q: %w", name, err)
	}
	r.locations.Store(name, location)
	return location, nil
}
