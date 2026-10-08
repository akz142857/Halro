package resourcebudget

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/akz142857/Halro/internal/ownedfile"
)

type workspaceKey struct{}

type workspaceCapability struct {
	owner      *ramWorkspace
	generation uint64
	loan       *sharedRAMLoan
}

func workspaceFromContext(ctx context.Context) (*ramWorkspace, uint64, bool) {
	capability, found := ctx.Value(workspaceKey{}).(workspaceCapability)
	return capability.owner, capability.generation, found
}

// WithoutRAMWorkspace keeps cancellation, deadlines and domain/source values,
// but withdraws RAM-loan capabilities. Independently retained maintenance work
// must pay its own ordinary admission rather than extend a parent's fixed pool.
// An explicit shared member may reenter only through its separate live parent
// loan. This function does not itself lend any shared RAM to ordinary callers.
func WithoutRAMWorkspace(ctx context.Context) context.Context {
	ctx = context.WithValue(ctx, admittedWorkspaceKey{}, struct{}{})
	ctx = context.WithValue(ctx, workspaceKey{}, struct{}{})
	ctx = context.WithValue(ctx, completionWorkspaceKey{}, struct{}{})
	return context.WithValue(ctx, completionContinuationKey{}, false)
}

// WorkspaceRequirement describes one simultaneous, temporary RAM consumer.
// All slots are admitted before the callback. Disk/unconfirmed ownership cannot
// borrow these credits. A slot's retained/transferred children must all retire
// before that slot can be reused.
type WorkspaceRequirement struct {
	Class Class
	RAM   int64
}

// Seven-root publication overlaps its seven root freezes, native source pins
// and publisher work. This fixed maximum also bounds the selection bitmask.
const MaxWorkspaceSlots = 32

type ramWorkspace struct {
	mu         sync.Mutex
	manager    *Manager
	active     bool
	children   int
	count      int
	slots      [MaxWorkspaceSlots]workspaceSlot
	done       chan struct{}
	once       sync.Once
	closeErr   error
	reusable   bool // completion credits remain owned between terminal facts
	inUse      bool
	ordinary   bool // healthy loan preserves original terminal reservations
	excess     Permit
	generation uint64
}

type workspaceSlot struct {
	workspace *ramWorkspace
	required  WorkspaceRequirement
	permit    Permit
	busy      bool
}

// WorkspaceRetirement retains the actual pre-admitted owners when a callback
// has uncertain physical retirement. The last physical child automatically
// retires the unused slots; no cleanup goroutine or historical registry exists.
type WorkspaceRetirement struct {
	Cause                          error
	owner                          *ramWorkspace
	retainedIO, retainedBackground int
	retainedRAM                    int64
}

func (e *WorkspaceRetirement) Error() string {
	return fmt.Sprintf("%v: %d retained workspace slots (%d IO, %d background, %d bytes RAM)", e.Cause, e.retainedIO+e.retainedBackground, e.retainedIO, e.retainedBackground, e.retainedRAM)
}
func (e *WorkspaceRetirement) Unwrap() error { return e.Cause }

// Called under the original pool mutex. The diagnostic records only finite
// resource classes/sizes at withdrawal, never native identities or payloads.
func newWorkspaceRetirementLocked(cause error, w *ramWorkspace) *WorkspaceRetirement {
	e := &WorkspaceRetirement{Cause: cause, owner: w}
	for i := 0; i < w.count; i++ {
		if !w.slots[i].busy {
			continue
		}
		e.retainedRAM += w.slots[i].required.RAM
		if w.slots[i].required.Class == IO {
			e.retainedIO++
		} else {
			e.retainedBackground++
		}
	}
	return e
}

// WithRAMWorkspace pre-admits a fixed profile and lends only those exact
// same-class slots through TryAcquireContext. Subsequent pressure/CloseAdmission
// cannot revoke owned RAM. This establishes resource ownership, never durable
// acceptance, disk headroom, confirmation or role permission.
func (m *Manager) WithRAMWorkspace(ctx context.Context, requirements []WorkspaceRequirement, visit func(context.Context) error) (resultErr error) {
	if m == nil || visit == nil || len(requirements) == 0 || len(requirements) > MaxWorkspaceSlots {
		return ErrInvalid
	}
	if cap, ok := ctx.Value(workspaceKey{}).(workspaceCapability); ok && cap.loan != nil {
		s := cap.loan.owner
		s.mu.Lock()
		valid := s.validLoanLocked(cap.loan)
		s.mu.Unlock()
		if !valid {
			return ErrClosed
		}
	}
	if w, generation, nested := workspaceFromContext(ctx); nested {
		if w == nil {
			return ErrInvalid
		}
		w.mu.Lock()
		allowed := w.manager == m && w.active && w.reusable && w.generation == generation
		var selected uint32
		for _, requirement := range requirements {
			found := false
			for i := 0; i < w.count; i++ {
				if w.slots[i].required == requirement && !w.slots[i].busy && selected&(1<<i) == 0 {
					found = true
					selected |= 1 << i
					break
				}
			}
			allowed = allowed && found
		}
		w.mu.Unlock()
		if !allowed {
			return ErrInvalid
		}
		return visit(ctx)
	}
	w := &ramWorkspace{manager: m, active: true, done: make(chan struct{})}
	defer func() {
		w.mu.Lock()
		w.active = false
		children := w.children
		var debt *WorkspaceRetirement
		if children > 0 && resultErr != nil {
			debt = newWorkspaceRetirementLocked(errors.Join(resultErr, ownedfile.ErrRetirement), w)
		}
		w.mu.Unlock()
		if children == 0 {
			w.finish()
		}
		if children > 0 && resultErr != nil {
			resultErr = debt
			return
		}
		<-w.done // joins final retained/transferred physical children, also on panic
		resultErr = errors.Join(resultErr, w.closeErr)
	}()
	for _, requirement := range requirements {
		if (requirement.Class != IO && requirement.Class != Background) || requirement.RAM <= 0 {
			return ErrInvalid
		}
		p, err := m.TryAcquireContext(ctx, requirement.Class, Cost{RAM: requirement.RAM})
		if err != nil {
			return err
		}
		m.mu.Lock()
		if m.limits.MaxOwners < 2 {
			m.mu.Unlock()
			return errors.Join(ErrOwners, p.Release())
		}
		slot := &w.slots[w.count]
		*slot = workspaceSlot{workspace: w, required: requirement, permit: p}
		p.claim.workspace = slot
		w.count++
		m.mu.Unlock()
	}
	return visit(context.WithValue(ctx, workspaceKey{}, workspaceCapability{owner: w}))
}

func (w *ramWorkspace) acquire(m *Manager, class Class, cost Cost, generation uint64) (Permit, error) {
	if m == nil || m != w.manager || cost.RAM <= 0 || cost.Disk != 0 || cost.Unconfirmed != 0 || (class != IO && class != Background) {
		return Permit{}, ErrInvalid
	}
	w.mu.Lock()
	if !w.active || generation != w.generation {
		w.mu.Unlock()
		return Permit{}, ErrClosed
	}
	var selected *workspaceSlot
	for i := 0; i < w.count; i++ {
		slot := &w.slots[i]
		if !slot.busy && slot.required.Class == class && slot.required.RAM >= cost.RAM && (selected == nil || slot.required.RAM < selected.required.RAM) {
			selected = slot
		}
	}
	if selected == nil {
		w.mu.Unlock()
		return Permit{}, ErrExhausted
	}
	selected.busy = true
	w.children++
	ordinary := w.ordinary
	w.mu.Unlock()
	var p Permit
	var err error
	if ordinary {
		// Only the actual private workspace may lend a reserved slot. Its
		// original owner remains reserved; the bounded child can report cost
		// and retain physical readers without activating terminal authority.
		m.mu.Lock()
		if !selected.permit.validLocked() {
			err = ErrInvalid
		} else {
			p, err = selected.permit.retainLocked()
			if err == nil {
				p.claim.owners[p.slot].ordinaryWorkspace = true
			}
		}
		m.mu.Unlock()
	} else {
		p, err = selected.permit.Retain()
	}
	if err != nil {
		selected.retireChild()
	}
	return p, err
}

// Called after the Manager mutex has been released, when only the private
// slot owner remains. A retained child therefore keeps the slot occupied.
func (s *workspaceSlot) retireChild() {
	w := s.workspace
	w.mu.Lock()
	if s.busy {
		s.busy = false
		w.children--
	}
	finish := !w.active && !w.reusable && w.children == 0
	w.mu.Unlock()
	if finish {
		w.finish()
	}
}

func (w *ramWorkspace) finish() {
	w.once.Do(func() {
		for i := 0; i < w.count; i++ {
			w.closeErr = errors.Join(w.closeErr, w.slots[i].permit.Release())
		}
		if w.excess.claim != nil {
			w.closeErr = errors.Join(w.closeErr, w.excess.Release())
		}
		close(w.done)
	})
}

// RetireRAMWorkspace ends the protected phase after its physical children
// have retired. Later observers may acquire their own ordinary resources;
// an escaped protected context cannot borrow a completed phase's credits.
// A context without this resource capability is unchanged.
func RetireRAMWorkspace(ctx context.Context) error {
	if cap, ok := ctx.Value(workspaceKey{}).(workspaceCapability); ok && cap.loan != nil {
		s := cap.loan.owner
		s.mu.Lock()
		valid := s.validLoanLocked(cap.loan)
		s.mu.Unlock()
		if !valid {
			return ErrClosed
		}
	}
	w, generation, scoped := workspaceFromContext(ctx)
	if !scoped {
		return nil
	}
	if w == nil {
		return ErrInvalid
	}
	w.mu.Lock()
	if generation != w.generation {
		w.mu.Unlock()
		return ErrClosed
	}
	if w.reusable && w.active {
		w.mu.Unlock()
		return nil // the actual terminal continuation owns the whole pool
	}
	if w.children != 0 {
		w.mu.Unlock()
		return ErrExhausted
	}
	w.active = false
	w.mu.Unlock()
	w.finish()
	<-w.done
	return w.closeErr
}
