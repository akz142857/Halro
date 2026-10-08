package resourcebudget

import (
	"fmt"
	"math"
	"sync"
	"time"
)

// A valid authority/sequence can arrive after its lease expired. Keep the
// public ErrInvalid classification while allowing the monitor to retry fresh
// observations instead of permanently losing its single sampling owner.
var errPressureLease = fmt.Errorf("pressure decision lease unavailable: %w", ErrInvalid)

const MaxCgroupDepth = 32

type SignalIssue uint32

const (
	RuntimeUnavailable SignalIssue = 1 << iota
	CgroupUnavailable
	DiskUnavailable
	SampleCancelled
	SignalInvalid
	SampleStale
	ClockInvalid
	SampleRepeated
	SampleGap
	SourceChanged
	RuntimeHigh
	CgroupHigh
	CgroupHeadroomLow
	DiskLow
	InodesLow
	Cooling
	MonitorInactive
)

type GroupMemory struct {
	Current, Max, High      int64
	MaxLimited, HighLimited bool
}

// Sample describes independent scopes; runtime and cgroup values must not be
// added. Groups[0] is the leaf, followed by visible ancestors (which may include
// siblings). At is the beginning of collection, not the end of slow reads.
type Sample struct {
	At                                          time.Time
	RuntimeUsed, DiskAvailable, InodesAvailable int64
	Groups                                      [MaxCgroupDepth]GroupMemory
	GroupCount                                  int
	CgroupID                                    [32]byte
	Issues                                      SignalIssue
}

// All thresholds are explicit calibration inputs, not product defaults.
// Used-byte Exit is below Enter; available-space Exit is above Enter.
type PressurePolicy struct {
	RuntimeEnter, RuntimeExit   int64
	CgroupEnter, CgroupExit     int64
	HeadroomEnter, HeadroomExit int64
	DiskEnter, DiskExit         int64
	InodesEnter, InodesExit     int64
	MaxAge, Cooldown            time.Duration
}

type Decision struct {
	state                  Pressure
	issues                 SignalIssue
	observedAt, validUntil time.Time
	controller             *Controller
	sequence               uint64
}

// Decision exposes a read-only view; callers cannot relabel pressure or renew
// the original observation lease while retaining its authority and sequence.
func (d Decision) State() Pressure       { return d.state }
func (d Decision) Issues() SignalIssue   { return d.issues }
func (d Decision) ObservedAt() time.Time { return d.observedAt }
func (d Decision) ValidUntil() time.Time { return d.validUntil }

type Controller struct {
	mu                        sync.Mutex
	policy                    PressurePolicy
	state                     Pressure
	lastAt, lastNow, lowSince time.Time
	source                    [32]byte
	sequence                  uint64
}

func (c *Controller) resetRecovery() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = Unknown
	c.lowSince = time.Time{}
}

func NewController(p PressurePolicy) (*Controller, error) {
	if p.RuntimeExit <= 0 || p.RuntimeEnter <= p.RuntimeExit || p.CgroupExit <= 0 || p.CgroupEnter <= p.CgroupExit ||
		p.HeadroomEnter <= 0 || p.HeadroomExit <= p.HeadroomEnter || p.DiskEnter <= 0 || p.DiskExit <= p.DiskEnter ||
		p.InodesEnter <= 0 || p.InodesExit <= p.InodesEnter || p.MaxAge <= 0 || p.Cooldown <= 0 {
		return nil, ErrInvalid
	}
	return &Controller{policy: p, state: Unknown}, nil
}

func (c *Controller) Observe(s Sample, now time.Time) Decision {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sequence < math.MaxUint64 {
		c.sequence++
	}
	d := Decision{state: Unknown, observedAt: s.At, validUntil: s.At.Add(c.policy.MaxAge), issues: s.Issues, controller: c, sequence: c.sequence}
	if c.sequence == math.MaxUint64 {
		d.issues |= SignalInvalid
	}
	if s.At.IsZero() || now.IsZero() || s.At.After(now) || (!c.lastNow.IsZero() && now.Before(c.lastNow)) {
		d.issues |= ClockInvalid
	}
	if !s.At.IsZero() && !now.Before(d.validUntil) {
		d.issues |= SampleStale
	}
	if !c.lastAt.IsZero() && !s.At.After(c.lastAt) {
		d.issues |= SampleRepeated
	}
	if s.RuntimeUsed < 0 || s.DiskAvailable < 0 || s.InodesAvailable < 0 || s.GroupCount <= 0 || s.GroupCount > MaxCgroupDepth || s.CgroupID == ([32]byte{}) {
		d.issues |= SignalInvalid
	}
	if d.issues != 0 {
		c.state = Unknown
		c.lowSince = time.Time{}
		if s.At.After(c.lastAt) && !s.At.After(now) {
			c.lastAt = s.At
		}
		if now.After(c.lastNow) {
			c.lastNow = now
		}
		return d
	}
	if !c.lastAt.IsZero() && s.At.Sub(c.lastAt) >= c.policy.MaxAge {
		d.issues |= SampleGap
		c.state = Unknown
		c.lowSince = time.Time{}
	}
	if c.source != ([32]byte{}) && c.source != s.CgroupID {
		d.issues |= SourceChanged
		c.state = Unknown
		c.lowSince = time.Time{}
	}
	c.lastAt = s.At
	c.lastNow = now
	c.source = s.CgroupID
	low := s.RuntimeUsed <= c.policy.RuntimeExit && s.Groups[0].Current <= c.policy.CgroupExit && s.DiskAvailable >= c.policy.DiskExit && s.InodesAvailable >= c.policy.InodesExit
	if s.RuntimeUsed >= c.policy.RuntimeEnter {
		d.issues |= RuntimeHigh
	}
	if s.Groups[0].Current >= c.policy.CgroupEnter {
		d.issues |= CgroupHigh
	}
	if s.DiskAvailable <= c.policy.DiskEnter {
		d.issues |= DiskLow
	}
	if s.InodesAvailable <= c.policy.InodesEnter {
		d.issues |= InodesLow
	}
	for _, g := range s.Groups[:s.GroupCount] {
		if g.Current < 0 || g.Max < 0 || g.High < 0 {
			c.state = Unknown
			c.lowSince = time.Time{}
			d.issues |= SignalInvalid
			return d
		}
		for _, limit := range [2]struct {
			value   int64
			limited bool
		}{{g.Max, g.MaxLimited}, {g.High, g.HighLimited}} {
			if !limit.limited {
				continue
			}
			// Current may temporarily exceed a limit; this is pressure, not zero
			// usage or a parse failure. Nonnegative operands cannot overflow.
			remaining := limit.value - g.Current
			if remaining <= c.policy.HeadroomEnter {
				d.issues |= CgroupHeadroomLow
			}
			if remaining < c.policy.HeadroomExit {
				low = false
			}
		}
	}
	high := d.issues&(RuntimeHigh|CgroupHigh|CgroupHeadroomLow|DiskLow|InodesLow) != 0
	if high {
		c.state = Pressured
		c.lowSince = time.Time{}
	} else if c.state != Healthy {
		if !low {
			c.lowSince = time.Time{}
		} else {
			if c.lowSince.IsZero() {
				c.lowSince = s.At
			}
			if s.At.Sub(c.lowSince) >= c.policy.Cooldown {
				c.state = Healthy
			}
		}
		if c.state != Healthy {
			d.issues |= Cooling
		}
	}
	d.state = c.state
	return d
}

// BindController assigns one immutable decision authority, before its sampling
// loop starts. Rebinding requires a separately designed replacement protocol.
func (m *Manager) BindController(c *Controller) error {
	if c == nil {
		return ErrInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.controller != nil {
		return ErrInvalid
	}
	m.controller = c
	m.stats.Pressure = Unknown
	m.healthyAt, m.healthyUntil = time.Time{}, time.Time{}
	return nil
}

// ApplyPressure binds admission freshness to the original sample time. Calling
// this late cannot turn an old healthy sample into a new full-duration lease.
// Controller locks are released before calling it; no cross-lock callback.
func (m *Manager) ApplyPressure(d Decision) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.controller == nil || d.controller != m.controller || d.sequence <= m.pressureSequence {
		return ErrInvalid
	}
	// Consume the sequence even when the new decision is invalid. An older
	// healthy decision must never undo a later pressure/unknown decision.
	m.pressureSequence = d.sequence
	if m.monitorOwned && (m.monitorContext == nil || m.monitorContext.Err() != nil) {
		m.stats.Pressure = Unknown
		m.healthyAt, m.healthyUntil = time.Time{}, time.Time{}
		return nil
	}
	if d.state > Pressured {
		m.stats.Pressure = Unknown
		m.healthyAt, m.healthyUntil = time.Time{}, time.Time{}
		return ErrInvalid
	}
	if d.state != Healthy {
		m.stats.Pressure = d.state
		m.healthyAt = time.Time{}
		m.healthyUntil = time.Time{}
		return nil
	}
	now := m.now()
	until := d.validUntil
	if cap := d.observedAt.Add(m.limits.MaxPressureAge); until.After(cap) {
		until = cap
	}
	if d.issues != 0 || d.observedAt.IsZero() || d.observedAt.After(now) || !now.Before(until) || (!m.healthyAt.IsZero() && d.observedAt.Before(m.healthyAt)) {
		m.stats.Pressure = Unknown
		m.healthyAt = time.Time{}
		m.healthyUntil = time.Time{}
		return errPressureLease
	}
	m.stats.Pressure = Healthy
	m.healthyAt = d.observedAt
	m.healthyUntil = until
	return nil
}
