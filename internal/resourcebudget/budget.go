// Package resourcebudget accounts for explicitly owned logical resources.
// It is separate from the monetary budget package. It does not intercept Go
// allocations, reserve filesystem blocks, or establish HA confirmation.
package resourcebudget

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"
)

var (
	ErrInvalid   = errors.New("invalid resource budget input or ownership")
	ErrExhausted = errors.New("resource budget exhausted")
	ErrPressure  = errors.New("new resource admission stopped by pressure")
	ErrClosed    = errors.New("resource admission closed")
	ErrReserved  = errors.New("completion credit has not been activated")
	ErrOwners    = errors.New("resource ownership reference limit reached")
)

// Cost uses logical bytes, including conservative copy/encoding estimates.
// Disk and Unconfirmed remain charged until the persistent owner deletes the
// bytes or confirmation retires them, respectively; sending is not release.
type Cost struct {
	RAM, Disk, Unconfirmed int64
}

func (c Cost) valid() bool { return c.RAM >= 0 && c.Disk >= 0 && c.Unconfirmed >= 0 }
func (c Cost) add(v Cost) (Cost, bool) {
	if !c.valid() || !v.valid() || v.RAM > math.MaxInt64-c.RAM || v.Disk > math.MaxInt64-c.Disk || v.Unconfirmed > math.MaxInt64-c.Unconfirmed {
		return Cost{}, false
	}
	return Cost{c.RAM + v.RAM, c.Disk + v.Disk, c.Unconfirmed + v.Unconfirmed}, true
}
func (c Cost) sub(v Cost) Cost {
	return Cost{c.RAM - v.RAM, c.Disk - v.Disk, c.Unconfirmed - v.Unconfirmed}
}
func fits(used, extra, limit Cost) bool {
	return used.valid() && extra.valid() && limit.valid() && used.RAM <= limit.RAM && extra.RAM <= limit.RAM-used.RAM &&
		used.Disk <= limit.Disk && extra.Disk <= limit.Disk-used.Disk &&
		used.Unconfirmed <= limit.Unconfirmed && extra.Unconfirmed <= limit.Unconfirmed-used.Unconfirmed
}

type Class uint8

const (
	Configuration Class = iota
	Active
	Cache
	IO
	Background
	Completion
	Recovery
	Control
	ClassCount
)

const (
	maxOwners = 16
	// Includes one claim, fixed owner slots and live handle values. This is a
	// logical estimate, not a proof of runtime/caller allocation amplification.
	ClaimRAMBytes int64 = 1024
)

// Limits are explicit, measured configuration supplied by future integration;
// this package deliberately provides no product capacity defaults.
type Limits struct {
	Total, RecoveryReserve Cost
	Classes                [ClassCount]Cost
	MaxClaims              int
	RecoveryClaims         int
	MaxOwners              int
	MaxPressureAge         time.Duration
}

type Pressure uint8

const (
	Unknown Pressure = iota // missing or stale measurements stop new normal work
	Healthy
	Pressured
)

type Stats struct {
	Used, Normal, Recovery, CompletionReserved Cost
	Classes                                    [ClassCount]Cost
	Acquired, Rejected                         [ClassCount]uint64
	Invalid                                    uint64
	NormalClaims, RecoveryClaims, Owners       int
	Pressure                                   Pressure
	Closed                                     bool
}

type Manager struct {
	mu                      sync.Mutex
	sharedCompletion        *SharedCompletableWorkspace
	limits                  Limits
	stats                   Stats
	now                     func() time.Time
	healthyAt, healthyUntil time.Time
	controller              *Controller
	pressureSequence        uint64
	monitorOwned            bool
	monitorContext          context.Context
}

func New(limits Limits) (*Manager, error) {
	if !limits.Total.valid() || limits.Total.RAM == 0 || !fits(Cost{}, limits.RecoveryReserve, limits.Total) ||
		limits.MaxClaims <= 0 || int64(limits.MaxClaims) > math.MaxInt64/ClaimRAMBytes || limits.MaxClaims > int(^uint(0)>>1)/maxOwners || limits.RecoveryClaims < 0 || limits.RecoveryClaims >= limits.MaxClaims ||
		limits.MaxOwners <= 0 || limits.MaxOwners > maxOwners || limits.MaxPressureAge <= 0 {
		return nil, ErrInvalid
	}
	for _, c := range limits.Classes {
		if !fits(Cost{}, c, limits.Total) {
			return nil, ErrInvalid
		}
	}
	control := limits.Classes[Control]
	if control.Disk != 0 || control.Unconfirmed != 0 || control.RAM < int64(limits.MaxClaims)*ClaimRAMBytes ||
		limits.RecoveryReserve.RAM < int64(limits.RecoveryClaims)*ClaimRAMBytes ||
		(limits.RecoveryClaims == 0 && limits.RecoveryReserve != (Cost{})) || !fits(Cost{}, limits.Classes[Recovery], limits.RecoveryReserve) {
		return nil, ErrInvalid
	}
	return &Manager{limits: limits, stats: Stats{Pressure: Unknown}, now: time.Now}, nil
}

// SetPressure is a trusted manual boundary for an unbound manager. Once a
// Controller is bound, only its ordered, timestamped decisions may set pressure.
func (m *Manager) SetPressure(p Pressure) error {
	if p > Pressured {
		return ErrInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.controller != nil {
		return ErrInvalid
	}
	m.stats.Pressure = p
	if p == Healthy {
		m.healthyAt = m.now()
		m.healthyUntil = m.healthyAt.Add(m.limits.MaxPressureAge)
	} else {
		m.healthyAt = time.Time{}
		m.healthyUntil = time.Time{}
	}
	return nil
}

func (m *Manager) refreshPressureLocked() {
	if m.monitorOwned && (m.monitorContext == nil || m.monitorContext.Err() != nil) {
		m.stats.Pressure = Unknown
		return
	}
	if m.stats.Pressure != Healthy {
		return
	}
	now := m.now()
	if now.Before(m.healthyAt) || !now.Before(m.healthyUntil) {
		m.stats.Pressure = Unknown
	}
}

func (m *Manager) CloseAdmission() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stats.Closed = true
}

func (m *Manager) Snapshot() Stats {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refreshPressureLocked()
	return m.stats // fixed-size value; no historical owner registry or map
}

func increment(v *uint64) {
	if *v != math.MaxUint64 {
		*v = *v + 1
	}
}
func normalClass(class Class) bool { return class <= Background }

// TryAcquire is fail-fast: there is no waiter queue or goroutine per claim.
// Consumers must acquire before allocating/reading, and clear their last
// references before Release. Released handle copies must also be discarded.
func (m *Manager) TryAcquire(class Class, cost Cost) (Permit, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !normalClass(class) || !cost.valid() || cost == (Cost{}) {
		increment(&m.stats.Invalid)
		return Permit{}, ErrInvalid
	}
	if err := m.check(class, cost, Cost{}, false, 1); err != nil {
		return Permit{}, err
	}
	return m.charge(class, cost, false, false), nil
}

// Work reserves completion at the same instant as active resources. It is a
// resource credit, not proof a request was accepted/settled in the ledger.
type Work struct{ Active, Completion Permit }

func (m *Manager) TryAcquireWork(class Class, active, completion Cost) (Work, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !normalClass(class) || !active.valid() || active == (Cost{}) || !completion.valid() || completion == (Cost{}) {
		increment(&m.stats.Invalid)
		return Work{}, ErrInvalid
	}
	if err := m.check(class, active, completion, false, 2); err != nil {
		return Work{}, err
	}
	return Work{m.charge(class, active, false, false), m.charge(Completion, completion, false, true)}, nil
}

// TryAcquireRecovery uses only the dedicated reserve and dedicated claim slots.
// Callers must be trusted recovery paths, not client-selected work classes.
func (m *Manager) TryAcquireRecovery(cost Cost) (Permit, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !cost.valid() || cost == (Cost{}) {
		increment(&m.stats.Invalid)
		return Permit{}, ErrInvalid
	}
	if err := m.check(Recovery, cost, Cost{}, true, 1); err != nil {
		return Permit{}, err
	}
	return m.charge(Recovery, cost, true, false), nil
}

// All checks happen before any state or owner becomes visible.
func (m *Manager) check(class Class, cost, finish Cost, recovery bool, claims int) error {
	err := m.fit(class, cost, finish, recovery, claims)
	if err != nil {
		increment(&m.stats.Rejected[class])
	}
	return err
}

// fit is check without the rejection count, for a probe whose failure is
// answered by another admission path rather than reported to the caller.
func (m *Manager) fit(class Class, cost, finish Cost, recovery bool, claims int) error {
	reject := func(err error) error { return err }
	if m.stats.Closed {
		return reject(ErrClosed)
	}
	m.refreshPressureLocked()
	if !recovery && m.stats.Pressure != Healthy {
		return reject(ErrPressure)
	}
	combined, ok := cost.add(finish)
	if !ok {
		return reject(ErrInvalid)
	}
	overhead := Cost{RAM: int64(claims) * ClaimRAMBytes}
	combined, ok = combined.add(overhead)
	if !ok {
		return reject(ErrInvalid)
	}
	if !fits(m.stats.Classes[class], cost, m.limits.Classes[class]) || !fits(m.stats.Classes[Completion], finish, m.limits.Classes[Completion]) ||
		!fits(m.stats.Classes[Control], overhead, m.limits.Classes[Control]) || !fits(m.stats.Used, combined, m.limits.Total) {
		return reject(ErrExhausted)
	}
	if recovery {
		if claims > m.limits.RecoveryClaims-m.stats.RecoveryClaims || !fits(m.stats.Recovery, combined, m.limits.RecoveryReserve) {
			return reject(ErrExhausted)
		}
	} else if claims > m.limits.MaxClaims-m.limits.RecoveryClaims-m.stats.NormalClaims || !fits(m.stats.Normal, combined, m.limits.Total.sub(m.limits.RecoveryReserve)) {
		return reject(ErrExhausted)
	}
	return nil
}

type owner struct {
	ordinaryWorkspace bool
	generation        uint64
	live              bool
}
type claim struct {
	manager            *Manager
	cost               Cost
	class              Class
	recovery, reserved bool
	refs               int
	owners             [maxOwners]owner
	scope              *recoveryScope
	workspace          *workspaceSlot
	scopeActive        bool
}

// Permit is a value handle. Copies alias one owner; Retain explicitly creates
// another consumer. Transfer revokes every copy of the old handle atomically.
type Permit struct {
	claim      *claim
	generation uint64
	slot       uint8
}

func (m *Manager) charge(class Class, cost Cost, recovery, reserved bool) Permit {
	overhead := Cost{RAM: ClaimRAMBytes}
	combined, _ := cost.add(overhead) // checked as part of the atomic admission
	m.stats.Used, _ = m.stats.Used.add(combined)
	m.stats.Classes[class], _ = m.stats.Classes[class].add(cost)
	m.stats.Classes[Control], _ = m.stats.Classes[Control].add(overhead)
	if recovery {
		m.stats.Recovery, _ = m.stats.Recovery.add(combined)
		m.stats.RecoveryClaims++
	} else {
		m.stats.Normal, _ = m.stats.Normal.add(combined)
		m.stats.NormalClaims++
	}
	if reserved {
		m.stats.CompletionReserved, _ = m.stats.CompletionReserved.add(cost)
	}
	m.stats.Owners++
	increment(&m.stats.Acquired[class])
	c := &claim{manager: m, cost: cost, class: class, recovery: recovery, reserved: reserved, refs: 1}
	c.owners[0] = owner{generation: 1, live: true}
	return Permit{c, 1, 0}
}

func (p Permit) validLocked() bool {
	return p.claim != nil && int(p.slot) < p.claim.manager.limits.MaxOwners && p.claim.refs > 0 &&
		p.claim.owners[p.slot].live && p.claim.owners[p.slot].generation == p.generation
}

func (p Permit) Cost() (Cost, error) {
	if p.claim == nil {
		return Cost{}, ErrInvalid
	}
	m := p.claim.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	if !p.validLocked() {
		return Cost{}, ErrInvalid
	}
	if p.claim.reserved && !p.claim.owners[p.slot].ordinaryWorkspace {
		return Cost{}, ErrReserved
	}
	return p.claim.cost, nil
}

func (p Permit) Transfer() (Permit, error) {
	if p.claim == nil {
		return Permit{}, ErrInvalid
	}
	m := p.claim.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	return p.transferLocked()
}
func (p Permit) transferLocked() (Permit, error) {
	if !p.validLocked() || p.generation == math.MaxUint64 {
		return Permit{}, ErrInvalid
	}
	p.claim.owners[p.slot].generation++
	return Permit{p.claim, p.generation + 1, p.slot}, nil
}

func (p Permit) Retain() (Permit, error) {
	if p.claim == nil {
		return Permit{}, ErrInvalid
	}
	m := p.claim.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	if !p.validLocked() {
		return Permit{}, ErrInvalid
	}
	if p.claim.reserved && !p.claim.owners[p.slot].ordinaryWorkspace {
		return Permit{}, ErrReserved
	}
	return p.retainLocked()
}

// RetainCovering keeps a live reservation in the expected manager, with the
// required cost, while another physical owner uses it. Validation and retain
// are atomic, so transfer/release cannot invalidate the checked lifetime.
func (p Permit) RetainCovering(m *Manager, minimum Cost) (Permit, error) {
	if p.claim == nil || m == nil || p.claim.manager != m || !minimum.valid() || minimum == (Cost{}) {
		return Permit{}, ErrInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !p.validLocked() {
		return Permit{}, ErrInvalid
	}
	if p.claim.reserved && !p.claim.owners[p.slot].ordinaryWorkspace {
		return Permit{}, ErrReserved
	}
	if !fits(Cost{}, minimum, p.claim.cost) {
		return Permit{}, ErrExhausted
	}
	return p.retainLocked()
}
func (p Permit) retainLocked() (Permit, error) {
	m := p.claim.manager
	for i := 0; i < m.limits.MaxOwners; i++ {
		slot := &p.claim.owners[i]
		if slot.live || slot.generation == math.MaxUint64 {
			continue
		}
		slot.generation++
		slot.live = true
		slot.ordinaryWorkspace = p.claim.owners[p.slot].ordinaryWorkspace
		p.claim.refs++
		m.stats.Owners++
		return Permit{p.claim, slot.generation, uint8(i)}, nil
	}
	return Permit{}, ErrOwners
}

// ActivateCompletion consumes this credit handle once and returns its new
// owner. It never performs new admission, so pressure/closed admission cannot
// strand already reserved completion work. Callers still honor ledger/HA gates.
func (p Permit) ActivateCompletion() (Permit, error) {
	if p.claim == nil {
		return Permit{}, ErrInvalid
	}
	m := p.claim.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	if !p.validLocked() || p.claim.class != Completion || !p.claim.reserved {
		return Permit{}, ErrInvalid
	}
	next, err := p.transferLocked()
	if err != nil {
		return Permit{}, err
	}
	p.claim.reserved = false
	m.stats.CompletionReserved = m.stats.CompletionReserved.sub(p.claim.cost)
	return next, nil
}

// ReleasePart retires independently lived resources without allocating another
// claim or reacquiring capacity. A sole owner may clear RAM, then transfer the
// remaining disk/confirmation charge to its persistent owner. Shared consumers
// must first relinquish their references; partial release cannot revoke theirs.
func (p Permit) ReleasePart(cost Cost) error {
	if p.claim == nil || !cost.valid() || cost == (Cost{}) {
		return ErrInvalid
	}
	m := p.claim.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	if !p.validLocked() || p.claim.refs != 1 || !fits(Cost{}, cost, p.claim.cost) {
		return ErrInvalid
	}
	if p.claim.reserved {
		return ErrReserved
	}
	c := p.claim
	c.cost = c.cost.sub(cost)
	m.stats.Used = m.stats.Used.sub(cost)
	m.stats.Classes[c.class] = m.stats.Classes[c.class].sub(cost)
	if c.recovery {
		m.stats.Recovery = m.stats.Recovery.sub(cost)
	} else {
		m.stats.Normal = m.stats.Normal.sub(cost)
	}
	return nil
}

// Reclassify moves an already admitted, sole-owner claim between resource
// classes without changing total usage. It is intended for lifecycle changes
// such as a recovery-built persistent owner becoming a Background owner after
// publication. The destination's class and normal/recovery capacity must fit;
// pressure is not rechecked because this operation admits no additional bytes.
func (p Permit) Reclassify(target Class) (Permit, error) {
	if p.claim == nil {
		return Permit{}, ErrInvalid
	}
	m := p.claim.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	if !p.validLocked() || p.claim.refs != 1 || p.claim.reserved || !normalClass(target) || target == p.claim.class || m.stats.Closed {
		return Permit{}, ErrInvalid
	}
	c := p.claim
	combined, ok := c.cost.add(Cost{RAM: ClaimRAMBytes})
	if !ok {
		return Permit{}, ErrInvalid
	}
	if !fits(m.stats.Classes[target], c.cost, m.limits.Classes[target]) {
		return Permit{}, ErrExhausted
	}
	if c.recovery {
		if m.stats.RecoveryClaims == 0 || m.limits.MaxClaims-m.limits.RecoveryClaims-m.stats.NormalClaims < 1 || !fits(m.stats.Normal, combined, m.limits.Total.sub(m.limits.RecoveryReserve)) {
			return Permit{}, ErrExhausted
		}
		m.stats.Classes[c.class] = m.stats.Classes[c.class].sub(c.cost)
		m.stats.Recovery = m.stats.Recovery.sub(combined)
		m.stats.Normal, _ = m.stats.Normal.add(combined)
		m.stats.RecoveryClaims--
		m.stats.NormalClaims++
		c.recovery = false
	} else {
		m.stats.Classes[c.class] = m.stats.Classes[c.class].sub(c.cost)
	}
	m.stats.Classes[target], _ = m.stats.Classes[target].add(c.cost)
	c.class = target
	return p, nil
}

// Merge moves two sole-owner, active claims into one owner without increasing
// resource use. Both old handles are revoked. Persistent file/window owners can
// aggregate retired request charges instead of retaining a claim per event.
// Claims must have the same class/reserve pool and recovery scope; no
// class-budget or callback-lifetime escape occurs.
func (p Permit) Merge(other Permit) (Permit, error) {
	if p.claim == nil || other.claim == nil || p.claim == other.claim || p.claim.manager != other.claim.manager {
		return Permit{}, ErrInvalid
	}
	m := p.claim.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	if !p.validLocked() || !other.validLocked() || p.claim.refs != 1 || other.claim.refs != 1 || p.claim.class != other.claim.class || p.claim.recovery != other.claim.recovery || p.claim.scope != other.claim.scope || p.generation == math.MaxUint64 {
		return Permit{}, ErrInvalid
	}
	if p.claim.reserved || other.claim.reserved {
		return Permit{}, ErrReserved
	}
	combined, ok := p.claim.cost.add(other.claim.cost)
	if !ok {
		return Permit{}, ErrInvalid
	}
	next, err := p.transferLocked()
	if err != nil {
		return Permit{}, err
	}
	p.claim.cost = combined
	other.claim.owners[other.slot].live = false
	other.claim.refs = 0
	overhead := Cost{RAM: ClaimRAMBytes}
	m.stats.Used = m.stats.Used.sub(overhead)
	m.stats.Classes[Control] = m.stats.Classes[Control].sub(overhead)
	if p.claim.recovery {
		m.stats.Recovery = m.stats.Recovery.sub(overhead)
		m.stats.RecoveryClaims--
	} else {
		m.stats.Normal = m.stats.Normal.sub(overhead)
		m.stats.NormalClaims--
	}
	m.stats.Owners--
	if other.claim.scope != nil {
		other.claim.scope.releaseChild()
	}
	return next, nil
}

// Release requires the owner to have cleared references to RAM or retired the
// corresponding disk/unconfirmed bytes. It cannot verify those external facts.
// Unused completion credits may be released only when admission is cancelled or
// work reliably finishes; they must not be released merely on HTTP return.
func (p Permit) Release() error {
	if p.claim == nil {
		return ErrInvalid
	}
	m := p.claim.manager
	m.mu.Lock()
	var retiredWorkspace *workspaceSlot
	defer func() {
		m.mu.Unlock()
		if retiredWorkspace != nil {
			retiredWorkspace.retireChild()
		}
	}()
	if !p.validLocked() {
		return ErrInvalid
	}
	c := p.claim
	c.owners[p.slot].live = false
	c.refs--
	m.stats.Owners--
	if c.refs != 0 {
		if c.refs == 1 && c.workspace != nil {
			retiredWorkspace = c.workspace
		}
		return nil
	}
	combined, _ := c.cost.add(Cost{RAM: ClaimRAMBytes})
	m.stats.Used = m.stats.Used.sub(combined)
	m.stats.Classes[c.class] = m.stats.Classes[c.class].sub(c.cost)
	m.stats.Classes[Control] = m.stats.Classes[Control].sub(Cost{RAM: ClaimRAMBytes})
	if c.recovery {
		m.stats.Recovery = m.stats.Recovery.sub(combined)
		m.stats.RecoveryClaims--
	} else {
		m.stats.Normal = m.stats.Normal.sub(combined)
		m.stats.NormalClaims--
	}
	if c.reserved {
		m.stats.CompletionReserved = m.stats.CompletionReserved.sub(c.cost)
	}
	if c.scope != nil {
		c.scope.releaseChild()
	}
	return nil
}
