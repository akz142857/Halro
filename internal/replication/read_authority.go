package replication

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/replicationlimits"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

var ErrMemberAuthority = errors.New("member publication authority is withdrawn or changed")

const MemberAuthorityRAMBytes int64 = replicationlimits.MemberAuthorityRAMBytes

// WithReadAuthority qualifies the actual authenticated state file, then lends
// one cancelable complete member snapshot and one callback-scoped publication
// barrier. Progress/role transitions remain available and revoke this job.
// authorize serializes only its one bounded publication action with transitions
// and reauthenticates the file first; it does not freeze history scans or permit
// using local durability as confirmation. Runtime must separately prove native
// cuts, data-directory ownership and projection eligibility.
// The callback/action must not Close the publisher; action cannot reenter it.
// The context/snapshot/authorize function cannot be used after the callback.
func (p *StatePublisher) WithReadAuthority(ctx context.Context, budget *resourcebudget.Manager, visit func(context.Context, MemberState, func(func() error) error) error) (resultErr error) {
	return p.withReadAuthority(ctx, budget, visit, false)
}

// WithLedgerReadAuthority grants only the captured Ledger permitted prefix.
// A Primary's unchanged durable cut remains readable while confirmation catches
// up. Every validation authenticates the actual current Member file; no newer
// Applied prefix, role, term or common Seal is granted by this read policy.
// Replica reads retain the exact Applied snapshot and original revocation rule.
func (p *StatePublisher) WithLedgerReadAuthority(ctx context.Context, budget *resourcebudget.Manager, visit func(context.Context, MemberState, func(func() error) error) error) error {
	return p.withReadAuthority(ctx, budget, visit, true)
}

// These typed ordinary readers use the same independently authenticated
// unchanged Primary durable Ordering cut. The caller must prove its own source
// cursor and binding; neither entry point grants a common Seal or confirmation.
// Replica remains constrained by the original exact Applied snapshot.
func (p *StatePublisher) WithMetadataReadAuthority(ctx context.Context, budget *resourcebudget.Manager, visit func(context.Context, MemberState, func(func() error) error) error) error {
	return p.withReadAuthority(ctx, budget, visit, true)
}

func (p *StatePublisher) WithGovernanceReadAuthority(ctx context.Context, budget *resourcebudget.Manager, visit func(context.Context, MemberState, func(func() error) error) error) error {
	return p.withReadAuthority(ctx, budget, visit, true)
}

func (p *StatePublisher) withReadAuthority(ctx context.Context, budget *resourcebudget.Manager, visit func(context.Context, MemberState, func(func() error) error) error, ledgerOnly bool) (resultErr error) {
	if p == nil || budget == nil || visit == nil {
		return ErrMemberAuthority
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	credit, err := budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: MemberAuthorityRAMBytes})
	if err != nil {
		return err
	}
	opCtx, cancel := context.WithCancel(ctx)
	p.mu.Lock()
	if p.closed || p.authorityFault != nil {
		err := errors.Join(ErrMemberAuthority, p.authorityFault)
		p.mu.Unlock()
		cancel()
		return errors.Join(err, credit.Release())
	}
	slot := -1
	for i, job := range p.authorityJobs {
		if job == nil {
			slot = i
			break
		}
	}
	if slot < 0 {
		p.mu.Unlock()
		cancel()
		return errors.Join(ErrMemberAuthority, credit.Release())
	}
	p.authorityJobs[slot] = cancel
	p.authorityWG.Add(1)
	p.mu.Unlock()
	active := false
	var saved MemberState
	defer func() {
		p.mu.Lock()
		if active && opCtx.Err() != nil {
			resultErr = fmt.Errorf("actual Member reader canceled (parent=%v closed=%v saved=%d/%d/%d term=%d/%d current=%d/%d/%d term=%d/%d): %w", ctx.Err(), p.closed, saved.DurableIndex, saved.ConfirmedIndex, saved.AppliedIndex, saved.Term, saved.PromisedTerm, p.state.DurableIndex, p.state.ConfirmedIndex, p.state.AppliedIndex, p.state.Term, p.state.PromisedTerm, errors.Join(resultErr, opCtx.Err()))
		}
		cancel()
		if active {
			p.authorityCancel = nil
		}
		if errors.Is(resultErr, ownedfile.ErrRetirement) {
			p.authorityDebts[slot] = credit
			p.authorityFault = errors.Join(p.authorityFault, resultErr)
			p.mu.Unlock()
		} else {
			p.mu.Unlock()
			if err := credit.Release(); err != nil {
				resultErr = errors.Join(resultErr, ownedfile.ErrRetirement, err)
				p.mu.Lock()
				p.authorityDebts[slot] = credit
				p.authorityFault = errors.Join(p.authorityFault, resultErr)
				p.mu.Unlock()
			} else {
				p.mu.Lock()
				p.authorityJobs[slot] = nil
				p.mu.Unlock()
			}
		}
		if active {
			p.mu.Lock()
			p.authorityLedgerCut = nil
			p.mu.Unlock()
			<-p.authorityLane
		}
		p.authorityWG.Done()
	}()
	// Accepted waiters already own their whole source workspace and are joined
	// by Close. They capture current state only after acquiring the actual lane;
	// a transition can revoke the active scan without waiting for this queue.
	if err := resourcebudget.AcquireWorkLane(opCtx, p.authorityLane); err != nil {
		return err
	}
	active = true
	p.mu.Lock()
	saved = cloneMemberState(p.state)
	p.authorityCancel = cancel
	if ledgerOnly && saved.Role == RolePrimary {
		cut := cloneMemberState(saved)
		p.authorityLedgerCut = &cut
	}
	p.mu.Unlock()
	validate := func() error {
		if err := opCtx.Err(); err != nil {
			return err
		}
		stable := sameCompleteMemberState(saved, p.state) || ledgerOnly && samePrimaryDurableReadCut(saved, p.state)
		if p.closed || p.authorityFault != nil || !stable {
			return errors.Join(ErrMemberAuthority, p.authorityFault)
		}
		if err := p.readAuthoritySource(p.state); err != nil {
			p.authorityFault = errors.Join(ErrMemberAuthority, err)
			cancel()
			return p.authorityFault
		}
		return opCtx.Err()
	}
	p.mu.Lock()
	err = validate()
	p.mu.Unlock()
	if err != nil {
		return err
	}
	// Only the actual whole Member authorize barrier can lend its held mutex.
	// Hot/ledger read policies never obtain physical renewal permission.
	var whole *memberWholeReadScope
	if !ledgerOnly {
		whole = &memberWholeReadScope{owner: p, state: cloneMemberState(saved)}
		opCtx = context.WithValue(opCtx, memberWholeReadScopeKey{}, whole)
	}
	var scope sync.Mutex
	closed, called := false, false
	var barrierErr error
	defer func() { scope.Lock(); closed = true; scope.Unlock() }()
	authorize := func(action func() error) (actionErr error) {
		if !scope.TryLock() {
			return ErrMemberAuthority
		}
		defer scope.Unlock()
		if closed {
			return ErrMemberAuthority
		}
		defer func() { barrierErr = errors.Join(barrierErr, actionErr) }()
		if called || action == nil {
			return ErrMemberAuthority
		}
		called = true
		p.mu.Lock()
		defer p.mu.Unlock()
		if err := validate(); err != nil {
			return err
		}
		returned := false
		defer func() {
			if !returned {
				p.authorityFault = errors.Join(p.authorityFault, ErrMemberAuthority)
			}
		}()
		if whole != nil {
			whole.activate()
		}
		err := func() (actionErr error) {
			if whole != nil {
				defer func() { actionErr = errors.Join(actionErr, whole.revoke()) }()
			}
			return action()
		}()
		returned = true
		if errors.Is(err, ownedfile.ErrRetirement) {
			p.authorityFault = errors.Join(p.authorityFault, err)
		}
		return errors.Join(err, opCtx.Err())
	}
	callbackErr := visit(opCtx, cloneMemberState(saved), authorize)
	scope.Lock()
	closed = true
	authorized := called
	latched := barrierErr
	scope.Unlock()
	if !authorized {
		callbackErr = errors.Join(callbackErr, ErrMemberAuthority)
	}
	return errors.Join(callbackErr, latched, opCtx.Err())
}

func samePrimaryDurableReadCut(a, b MemberState) bool {
	return a.Role == RolePrimary && b.Role == RolePrimary && sameMemberIdentity(a, b) && a.DurableIndex == b.DurableIndex && a.OrderingHeadMAC == b.OrderingHeadMAC && b.ConfirmedIndex >= a.ConfirmedIndex && b.AppliedIndex >= a.AppliedIndex && b.AppliedIndex == b.ConfirmedIndex && b.ConfirmedIndex <= b.DurableIndex && b.Projection.Index == b.AppliedIndex && b.Projection.MetadataEpoch >= a.Projection.MetadataEpoch && (b.Projection.MetadataEpoch > a.Projection.MetadataEpoch || b.Projection.MetadataSequence >= a.Projection.MetadataSequence)
}

func sameCompleteMemberState(a, b MemberState) bool {
	return sameMemberIdentity(a, b) && a.DurableIndex == b.DurableIndex && a.ConfirmedIndex == b.ConfirmedIndex && a.AppliedIndex == b.AppliedIndex && a.OrderingHeadMAC == b.OrderingHeadMAC && a.Projection == b.Projection
}

// No descriptor/key debt is dropped when actual reader Close is uncertain.
// Called only with publisher.mu held and the whole job fee already admitted.
func (p *StatePublisher) readAuthoritySource(expected MemberState) (resultErr error) {
	file, err := os.OpenFile(p.path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return errors.Join(ErrMemberAuthority, err)
	}
	defer func() {
		closeFile := p.closeAuthorityFile
		if closeFile == nil {
			closeFile = ownedfile.CloseFile
		}
		if err := closeFile(file); err != nil {
			resultErr = errors.Join(resultErr, ownedfile.CloseError(file, err))
		}
	}()
	actual, err := file.Stat()
	named, nameErr := os.Lstat(p.path)
	if err != nil || nameErr != nil || !actual.Mode().IsRegular() || !named.Mode().IsRegular() || !os.SameFile(actual, named) || actual.Size() <= 0 || actual.Size() > MaxMemberStateJSON {
		return errors.Join(ErrMemberAuthority, err, nameErr)
	}
	raw, err := io.ReadAll(io.LimitReader(file, MaxMemberStateJSON+1))
	if err != nil {
		return errors.Join(ErrMemberAuthority, err)
	}
	state, err := UnmarshalState(raw, p.key[:])
	if err != nil || !sameCompleteMemberState(state, expected) {
		return errors.Join(ErrMemberAuthority, err)
	}
	final, err := file.Stat()
	finalName, nameErr := os.Lstat(p.path)
	if err != nil || nameErr != nil || !os.SameFile(actual, final) || !os.SameFile(actual, finalName) || final.Size() != actual.Size() || finalName.Size() != actual.Size() {
		return errors.Join(ErrMemberAuthority, err, nameErr)
	}
	return nil
}
