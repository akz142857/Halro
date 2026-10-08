package resourcebudget

import (
	"context"
	"errors"
	"sync"

	"github.com/akz142857/Halro/internal/ownedfile"
)

const (
	MaxSharedWorkMembers                 = 8
	SharedWorkspaceControlRAMBytes int64 = 16 << 10
)

// SharedWorkProfile selects exact resource shapes, never domain/source rights.
// Installation is explicit on one Manager owned by the runtime foundation.
type SharedWorkProfile struct {
	Class        Class
	Active       Cost
	Requirements []WorkspaceRequirement
}

type sharedWorkRule struct {
	class  Class
	active Cost
	count  int
	slots  [MaxWorkspaceSlots]WorkspaceRequirement
}

type sharedWorkMember struct {
	work  *CompletableWork
	loans int
}

type sharedWorkWaiter struct {
	work    *CompletableWork
	ready   chan struct{}
	granted bool
}

type sharedRAMLoanKey struct{}
type sharedRAMLoan struct {
	owner      *SharedCompletableWorkspace
	work       *CompletableWork
	parent     *sharedRAMLoan
	generation uint64
	active     bool // protected by owner.mu
	terminal   bool // inherited from the actual entered phase, never a masked context
	child      *sharedRAMLoan
	jobs       sync.WaitGroup
	childErr   error // protected by owner.mu; joined before a parent returns
}

// SharedCompletableWorkspace keeps one original pool until every member and
// physical child retires. Each member pays its own Active claim before native
// acceptance. At most one root call per member may wait; the fixed FIFO has no
// goroutine, payload, historical identity registry or unbounded waiter list.
// Close withdraws new membership, preserving already owned completion loans.
type SharedCompletableWorkspace struct {
	mu              sync.Mutex
	manager         *Manager
	base            *CompletableWork
	rules           [MaxSharedWorkMembers]sharedWorkRule
	ruleCount       int
	members         [MaxSharedWorkMembers]sharedWorkMember
	queue           [MaxSharedWorkMembers]*sharedWorkWaiter
	queueCount      int
	root            *CompletableWork
	closing, closed bool
}

// SharedWorkspaceStats reports bounded ownership, without lending any credit.
type SharedWorkspaceStats struct {
	Members, Waiting       int
	InUse, Closing, Closed bool
}

func (s *SharedCompletableWorkspace) Snapshot() SharedWorkspaceStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	stats := SharedWorkspaceStats{Waiting: s.queueCount, InUse: s.root != nil, Closing: s.closing, Closed: s.closed}
	for _, member := range s.members {
		if member.work != nil {
			stats.Members++
		}
	}
	return stats
}

// SharedWorkspaceRequirements computes the exact class/size multiset union.
// Its sufficiency is enforced on every real child allocation; it does not
// establish native ordering or prove that callers can safely hold locks while
// waiting for this lane. Such callers must be restructured before installation.
func SharedWorkspaceRequirements(profiles []SharedWorkProfile) ([]WorkspaceRequirement, error) {
	if len(profiles) == 0 || len(profiles) > MaxSharedWorkMembers {
		return nil, ErrInvalid
	}
	var union []WorkspaceRequirement
	for _, p := range profiles {
		if !normalClass(p.Class) || !p.Active.valid() || p.Active == (Cost{}) || len(p.Requirements) == 0 || len(p.Requirements) > MaxWorkspaceSlots {
			return nil, ErrInvalid
		}
		for i, r := range p.Requirements {
			if (r.Class != IO && r.Class != Background) || r.RAM <= 0 {
				return nil, ErrInvalid
			}
			n, have := 0, 0
			for _, prior := range p.Requirements[:i+1] {
				if prior == r {
					n++
				}
			}
			for _, prior := range union {
				if prior == r {
					have++
				}
			}
			if n > have {
				union = append(union, r)
			}
		}
	}
	if len(union) > MaxWorkspaceSlots {
		return nil, ErrInvalid
	}
	return union, nil
}

func (m *Manager) InstallSharedCompletableWorkspace(profiles []SharedWorkProfile) (*SharedCompletableWorkspace, error) {
	if m == nil {
		return nil, ErrInvalid
	}
	requirements, err := SharedWorkspaceRequirements(profiles)
	if err != nil {
		return nil, err
	}
	var ram int64
	for _, r := range requirements {
		if r.RAM > int64(^uint64(0)>>1)-ram {
			return nil, ErrInvalid
		}
		ram += r.RAM
	}
	base, err := m.tryAcquirePrivateCompletableWork(IO, Cost{RAM: SharedWorkspaceControlRAMBytes}, Cost{RAM: ram}, requirements)
	if err != nil {
		return nil, err
	}
	s := &SharedCompletableWorkspace{manager: m, base: base, ruleCount: len(profiles)}
	for i, p := range profiles {
		s.rules[i] = sharedWorkRule{class: p.Class, active: p.Active, count: len(p.Requirements)}
		copy(s.rules[i].slots[:], p.Requirements)
	}
	m.mu.Lock()
	if m.sharedCompletion != nil {
		m.mu.Unlock()
		return nil, errors.Join(ErrInvalid, base.Close(), base.Active.Release())
	}
	m.sharedCompletion = s
	m.mu.Unlock()
	return s, nil
}

func (s *SharedCompletableWorkspace) matches(class Class, active, completion Cost, requirements []WorkspaceRequirement) bool {
	if completion.Disk != 0 || completion.Unconfirmed != 0 {
		return false
	}
	for _, r := range s.rules[:s.ruleCount] {
		if class != r.class || active != r.active || len(requirements) != r.count {
			continue
		}
		var ram int64
		equal := true
		for i, slot := range requirements {
			if slot != r.slots[i] || slot.RAM > completion.RAM-ram {
				equal = false
				break
			}
			ram += slot.RAM
		}
		if equal && ram == completion.RAM {
			return true
		}
	}
	return false
}

func (s *SharedCompletableWorkspace) acquire(class Class, active Cost) (*CompletableWork, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing || s.closed {
		return nil, ErrClosed
	}
	index := -1
	for i, m := range s.members {
		if m.work == nil {
			index = i
			break
		}
	}
	if index < 0 {
		return nil, ErrExhausted
	}
	s.base.pool.mu.Lock()
	reusable := s.base.pool.reusable
	s.base.pool.mu.Unlock()
	if !reusable {
		return nil, ErrClosed
	}
	p, err := s.manager.TryAcquire(class, active)
	if err != nil {
		return nil, err
	}
	w := &CompletableWork{Active: p, pool: s.base.pool, shared: s}
	s.members[index] = sharedWorkMember{work: w}
	return w, nil
}

func (s *SharedCompletableWorkspace) member(w *CompletableWork) *sharedWorkMember {
	for i := range s.members {
		if s.members[i].work == w {
			return &s.members[i]
		}
	}
	return nil
}

func (s *SharedCompletableWorkspace) grantNextLocked() {
	s.root = nil
	if s.queueCount == 0 {
		return
	}
	w := s.queue[0]
	copy(s.queue[:], s.queue[1:s.queueCount])
	s.queueCount--
	s.queue[s.queueCount] = nil
	s.root = w.work
	w.granted = true
	close(w.ready)
}

func (s *SharedCompletableWorkspace) wait(ctx context.Context, w *CompletableWork) error {
	waitCtx, stop := WorkQueueContext(ctx)
	defer stop()
	s.mu.Lock()
	member := s.member(w)
	if member == nil || s.closed {
		s.mu.Unlock()
		return ErrClosed
	}
	if member.loans != 0 {
		s.mu.Unlock()
		return ErrClosed
	}
	member.loans++
	waiter := &sharedWorkWaiter{work: w, ready: make(chan struct{})}
	if s.root == nil {
		s.root = w
		waiter.granted = true
		close(waiter.ready)
	} else {
		s.queue[s.queueCount] = waiter
		s.queueCount++
	}
	s.mu.Unlock()
	select {
	case <-waiter.ready:
	case <-waitCtx.Done():
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := waitCtx.Err(); err != nil {
		if waiter.granted {
			s.grantNextLocked()
		} else {
			for i, q := range s.queue[:s.queueCount] {
				if q == waiter {
					copy(s.queue[i:], s.queue[i+1:s.queueCount])
					s.queueCount--
					s.queue[s.queueCount] = nil
					break
				}
			}
		}
		member.loans--
		return workQueueWaitError(ctx, waitCtx)
	}
	return nil
}

func (s *SharedCompletableWorkspace) validLoanLocked(n *sharedRAMLoan) bool {
	if n == nil || !n.active {
		return false
	}
	for depth := 0; n != nil; depth++ {
		if depth >= MaxSharedWorkMembers || n.owner != s || s.root == nil {
			return false
		}
		if n.parent == nil && n.work != s.root {
			return false
		}
		n = n.parent
	}
	return true
}

func (s *SharedCompletableWorkspace) visit(ctx context.Context, w *CompletableWork, parent *sharedRAMLoan, generation uint64, visit func(context.Context) error) (resultErr error) {
	s.mu.Lock()
	member := s.member(w)
	if member == nil || s.closed || parent != nil && (!s.validLoanLocked(parent) || parent.child != nil) {
		s.mu.Unlock()
		return ErrClosed
	}
	if parent != nil {
		depth := 1
		contains := false
		for p := parent; p != nil; p = p.parent {
			depth++
			contains = contains || p.work == w
		}
		if depth > MaxSharedWorkMembers || member.loans != 0 && !contains {
			s.mu.Unlock()
			return ErrClosed
		}
		member.loans++
		parent.jobs.Add(1)
	}
	n := &sharedRAMLoan{owner: s, work: w, parent: parent, generation: generation, active: true, terminal: InCompletionContinuation(ctx)}
	if parent != nil {
		parent.child = n
	}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		n.active = false
		s.mu.Unlock()
		n.jobs.Wait()
		s.mu.Lock()
		resultErr = errors.Join(resultErr, n.childErr)
		if parent != nil {
			member.loans--
			parent.child = nil
			if parent.childErr == nil {
				parent.childErr = resultErr
			}
		}
		s.mu.Unlock()
		if parent != nil {
			parent.jobs.Done()
		}
	}()
	ctx = context.WithValue(ctx, sharedRAMLoanKey{}, n)
	ctx = context.WithValue(ctx, workspaceKey{}, workspaceCapability{owner: s.base.pool, generation: generation, loan: n})
	return visit(ctx)
}

func (s *SharedCompletableWorkspace) withLoan(ctx context.Context, w *CompletableWork, activate bool, visit func(context.Context) error) (resultErr error) {
	if ctx == nil || visit == nil {
		return ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Every entered member retains its own fee, including genuine nested jobs.
	fee, err := w.Active.Retain()
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, fee.Release()) }()
	if parent, found := ctx.Value(sharedRAMLoanKey{}).(*sharedRAMLoan); found {
		if parent == nil || parent.owner != s {
			return ErrInvalid
		}
		if pool, _, scoped := workspaceFromContext(ctx); scoped && pool != s.base.pool {
			return ErrInvalid
		}
		// A nested borrower cannot upgrade an ordinary root into completion.
		// WithoutRAMWorkspace masks the public phase; the genuine parent keeps
		// its actual phase so an explicit child can continue that same phase.
		ctx = context.WithValue(ctx, completionContinuationKey{}, activate && parent.terminal)
		return s.visit(ctx, w, parent, parent.generation, visit)
	}
	if _, _, foreign := workspaceFromContext(ctx); foreign {
		return ErrInvalid
	}
	if err := s.wait(ctx, w); err != nil {
		return err
	}
	defer func() { s.mu.Lock(); s.member(w).loans--; s.grantNextLocked(); s.mu.Unlock() }()
	ctx = context.WithValue(ctx, completionContinuationKey{}, activate)
	return withRAMWorkspaceLoan(ctx, s.base.pool, activate, func(ctx context.Context) error {
		_, generation, _ := workspaceFromContext(ctx)
		return s.visit(ctx, w, nil, generation, visit)
	})
}

func (s *SharedCompletableWorkspace) closeMember(w *CompletableWork) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	member := s.member(w)
	if member == nil {
		return nil
	}
	if member.loans != 0 {
		return ErrExhausted
	}
	p := s.base.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.children != 0 && !p.reusable {
		return newWorkspaceRetirementLocked(ownedfile.ErrRetirement, p)
	}
	*member = sharedWorkMember{}
	return nil
}

func (s *SharedCompletableWorkspace) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closing = true
	if s.closed {
		return nil
	}
	for _, m := range s.members {
		if m.work != nil {
			return ErrExhausted
		}
	}
	if err := s.base.Close(); err != nil {
		return err
	}
	if err := s.base.Active.Release(); err != nil {
		return err
	}
	s.closed = true
	// Keep the deny-only closed binding installed. Reusing this Manager for a
	// new foundation requires a fresh Manager, not an implicit private fallback.
	return nil
}
