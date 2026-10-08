package metadatastate

import (
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"errors"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// ConfigureHAReplicaJournal is a maintenance-only physical mode handoff. The
// independent caller already selected this content root under a group carrier.
// It returns a borrowed actual sink log; this Baseline remains its close owner.
// It grants neither a business facade nor permission to publish seven HA roots.
func (b *Baseline) ConfigureHAReplicaJournal(ctx context.Context, ledgerKey []byte, a metadatajournal.BoundReplicationAuthority) (*metadatajournal.Log, error) {
	if b == nil || !b.mu.TryLock() {
		return nil, authenticatedstate.ErrBusy
	}
	defer b.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if b.readOnly || b.haPeerTransition != nil || !b.ready.Load() || !b.linked || b.journal == nil || b.selectedStartup || len(ledgerKey) != 32 || sha256.Sum256(ledgerKey) != b.ledgerKeyTag || a.Baseline != b.position.Baseline || a.FileRevision != b.position.FileRevision {
		return nil, ErrSource
	}
	if err := a.Validate(); err != nil {
		return nil, err
	}
	key, err := hkdf.Key(sha256.New, ledgerKey, a.Baseline.Scope[:], "halro:metadata:bound-journal:hmac:v1", 32)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	if b.haPhysicalSource != nil && b.haPhysicalSource.role == authenticatedstate.SealReplica && b.haPhysicalSource.permission == a && b.haPhysicalSource.recoveryPermitted.Offset > 0 {
		if err := b.journal.CompleteBoundPhysicalRecovery(a, true); err != nil {
			return nil, err
		}
		b.haReplicaPrepared = &a
		if w := b.haPhysicalSource.receiveWindow; w != nil {
			if err := b.installHAPhysicalReceiveLocked(); err != nil {
				b.ready.Store(false)
				return nil, err
			}
		}
		return b.journal, nil
	}
	if err := b.journal.Close(); err != nil {
		b.ready.Store(false)
		b.retirementErr = errors.Join(b.retirementErr, err)
		return nil, err
	}
	b.journal = nil
	journal, err := metadatajournal.OpenBoundReplicaContext(ctx, b.journalPath, key, a)
	if err != nil {
		b.ready.Store(false)
		return nil, err
	}
	b.journal = journal
	b.haReplicaPrepared = &a
	return journal, nil
}

// ConfigureHAPrimaryJournal installs the actual native admission -> fsync ->
// global ordering -> confirmed wait hooks. Standalone prepared COW transactions
// remain excluded by the native log's existing mode guard.
func (b *Baseline) ConfigureHAPrimaryJournal(ctx context.Context, a metadatajournal.BoundReplicationAuthority, before func(metadatajournal.DurableBatch) (metadatajournal.AppendAdmission, error), after func(metadatajournal.DurableBatch) (uint64, error), wait func(context.Context, uint64) error) error {
	if b == nil || !b.mu.TryLock() {
		return authenticatedstate.ErrBusy
	}
	defer b.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if b.readOnly || b.haPeerTransition != nil || !b.ready.Load() || !b.linked || b.journal == nil || b.selectedStartup || b.position.Baseline != a.Baseline || b.position.FileRevision != a.FileRevision {
		return ErrSource
	}
	if err := b.journal.SetBoundReplicationAdmissionAndAfterDurable(a, before, after, wait); err != nil {
		return err
	}
	if b.haPhysicalSource != nil && b.haPhysicalSource.recoveryPermitted.Offset > 0 {
		if err := b.journal.CompleteBoundPhysicalRecovery(a, false); err != nil {
			return err
		}
	}
	b.haPrimaryPrepared = &a
	return nil
}

// VerifyHAMaintenanceRoot checks every selected row with bounded pages before
// maintenance may acknowledge its baseline. The selected root already came
// from an independent signed preparation; this audit grants no role authority.
func (b *Baseline) VerifyHAMaintenanceRoot(ctx context.Context) (resultErr error) {
	if b == nil || !b.mu.TryLock() {
		return authenticatedstate.ErrBusy
	}
	defer b.mu.Unlock()
	if !b.ready.Load() || !b.linked || b.store == nil || b.selectedStartup {
		return ErrSource
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(b.ctx, cancel)
	defer func() { stop(); cancel() }()
	credit, err := b.budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: 32 << 20})
	if err != nil {
		return err
	}
	defer func() {
		if errors.Is(resultErr, ownedfile.ErrRetirement) {
			b.sealDebt = credit
			b.retirementErr = errors.Join(b.retirementErr, resultErr)
			b.ready.Store(false)
		} else if err := credit.Release(); err != nil {
			resultErr = errors.Join(resultErr, err)
			b.sealDebt = credit
			b.retirementErr = errors.Join(b.retirementErr, resultErr)
			b.ready.Store(false)
		}
	}()
	identity, err := b.store.RootIdentity(ctx)
	if err != nil {
		return err
	}
	pin, err := b.store.Snapshot(ctx)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, pin.Close()) }()
	var rows uint64
	cursor := ""
	for {
		page, err := pin.Range(ctx, authenticatedstate.RangeOptions{Cursor: cursor, MaxRows: 64, MaxBytes: 8 << 20, MaxPages: 4096})
		if err != nil {
			return err
		}
		if uint64(len(page.Rows)) > identity.Rows-rows {
			return ErrSource
		}
		rows += uint64(len(page.Rows))
		if page.Done {
			if rows != identity.Rows {
				return ErrSource
			}
			break
		}
		if len(page.Rows) == 0 || page.Cursor == cursor {
			return ErrSource
		}
		cursor = page.Cursor
	}
	return ctx.Err()
}
