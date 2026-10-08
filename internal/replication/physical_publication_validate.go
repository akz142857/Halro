package replication

import (
	"context"
	"errors"
	"os"
	"syscall"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// ValidateParents rechecks the actual installed publisher/journal/Manager and
// captured birth pair. It grants neither progress nor source/role permission.
func (o *PhysicalPublication) ValidateParents(ctx context.Context, p *StatePublisher, j *OrderingJournal, m *resourcebudget.Manager) (resultErr error) {
	if o == nil || p == nil || j == nil || m == nil || ctx.Err() != nil {
		return errors.Join(ErrMemberAuthority, ctx.Err())
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	j.mu.Lock()
	defer j.mu.Unlock()
	if o.publisher != p || o.journal != j || o.manager != m || p.physical != o || j.physical != o || p.closed || p.authorityFault != nil || j.closed || j.closing || j.poisoned != nil || o.banksClosed || o.bank == nil || o.withdrawn.Load() || o.failure() != nil {
		return errors.Join(ErrMemberAuthority, o.failure())
	}
	return o.validatePhysicalParentsLocked(ctx)
}

// The caller holds the original publisher and journal barriers.
func (o *PhysicalPublication) validatePhysicalParentsLocked(ctx context.Context) (resultErr error) {
	p, j := o.publisher, o.journal
	if err := p.readAuthoritySource(p.state); err != nil {
		return err
	}
	file, err := os.OpenFile(p.path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer func() {
		if err := ownedfile.CloseFile(file); err != nil {
			resultErr = errors.Join(resultErr, o.fail(ownedfile.CloseError(file, err)))
		}
	}()
	state, err := resourcebudget.CapturePhysicalIdentity(file)
	if err != nil {
		return err
	}
	bank, err := resourcebudget.CapturePhysicalIdentity(o.bank)
	if err != nil {
		return err
	}
	journal, err := resourcebudget.CapturePhysicalIdentity(j.file)
	pair := o.receipt.State
	if err != nil || journal != o.receipt.Ordering || !((state == pair[0] && bank == pair[1]) || (state == pair[1] && bank == pair[0])) {
		return errors.Join(ownedfile.ErrReplaced, err)
	}
	if err := o.bankOwner.VerifyName(); err != nil {
		return err
	}
	if err := o.bankOwner.VerifyFile(o.bank); err != nil {
		return err
	}
	if o.receipt.Version == 2 || o.receipt.Version == 3 {
		if o.receiptFile == nil || o.receiptOwner == nil {
			return ErrMemberAuthority
		}
		if err := errors.Join(o.receiptOwner.VerifyName(), o.receiptOwner.VerifyFile(o.receiptFile)); err != nil {
			return err
		}
		identity, err := resourcebudget.CapturePhysicalIdentity(o.receiptFile)
		if err != nil || identity != o.receipt.Receipt {
			return errors.Join(ownedfile.ErrReplaced, err)
		}
	}
	current, err := file.Stat()
	named, nameErr := os.Lstat(p.path)
	if err != nil || nameErr != nil || !os.SameFile(current, named) || !os.SameFile(current, o.current) {
		return errors.Join(ownedfile.ErrReplaced, err, nameErr)
	}
	if err := resourcebudget.CheckPhysicalRange(o.bank, 0, MaxMemberStateJSON); err != nil {
		return err
	}
	if j.nextOffset > o.end {
		return resourcebudget.ErrPhysicalAllocation
	}
	if j.nextOffset < o.end {
		if err := resourcebudget.CheckPhysicalRange(j.file, j.nextOffset, o.end); err != nil {
			return err
		}
	}
	return ctx.Err()
}
