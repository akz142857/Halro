package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/statecut"
	"github.com/akz142857/Halro/internal/store/lock"
)

// RestartHASelectedReceiveReceipt is an explicit offline retry for an incomplete
// first receipt. It cannot recover an unexplained durable ordering suffix or
// alter progress. Such a suffix still requires the original pre-native proof.
func RestartHASelectedReceiveReceipt(ctx context.Context, cfg config.Config) (result replication.MemberState, resultErr error) {
	if ctx == nil || cfg.Replication == nil || cfg.ResourceBudget == nil || cfg.ResourceBudget.PhysicalTerminalReserve {
		return result, authenticatedstate.ErrSealIneligible
	}
	guard, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		return result, fmt.Errorf("received receipt restart requires stopped Halro: %w", err)
	}
	defer func() {
		if errors.Is(resultErr, ownedfile.ErrRetirement) {
			resultErr = &haSelectedReceiveRestartFailure{Guard: guard, Cause: resultErr}
			return
		}
		if err := guard.Close(); err != nil {
			resultErr = &haSelectedReceiveRestartFailure{Guard: guard, Cause: errors.Join(resultErr, ownedfile.ErrRetirement, err)}
		}
	}()
	ctx, pressure, err := commandBudgetContext(ctx, cfg)
	if err != nil || pressure == nil {
		return result, errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	resultErr = pressure.monitor.WithSampling(ctx, func(ctx context.Context) error {
		var master []byte
		var payload *haMemoryUnlockPayload
		var err error
		if cfg.Storage.MasterKey.Mode == config.MasterKeyModeKeySlots {
			master, payload, err = unlockHAMemoryCandidate(ctx, cfg, pressure.logicalBudget(), defaultKMSWrapperFactory)
		} else {
			master, err = unlockMemberMasterKey(ctx, cfg)
		}
		if err != nil {
			return err
		}
		defer clear(master)
		result, err = restartHASelectedReceiveReceiptSampled(ctx, cfg, master, guard, pressure.logicalBudget(), slog.New(slog.NewTextHandler(io.Discard, nil)))
		if payload != nil {
			err = errors.Join(err, payload.Close())
		}
		return err
	})
	return
}

type haSelectedReceiveRestartFailure struct {
	Guard *lock.Lock
	Cause error
}

func (e *haSelectedReceiveRestartFailure) Error() string { return e.Cause.Error() }
func (e *haSelectedReceiveRestartFailure) Unwrap() error { return e.Cause }

func restartHASelectedReceiveReceiptSampled(ctx context.Context, cfg config.Config, master []byte, guard *lock.Lock, manager *resourcebudget.Manager, logger *slog.Logger) (result replication.MemberState, resultErr error) {
	before, key, err := readMemberState(cfg, master)
	if err != nil {
		return result, err
	}
	defer clear(key[:])
	if before.Role != replication.RoleReplica || before.Term != before.PromisedTerm || before.DurableIndex != before.ConfirmedIndex || before.AppliedIndex != before.DurableIndex {
		return result, replication.ErrMemberAuthority
	}
	inspectionFee, err := manager.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: 128 << 10})
	if err != nil {
		return result, err
	}
	report, err := replication.InspectOrderingJournalWithLimit(ctx, cfg.OrderingJournalPath(), key[:], before.ClusterID, before.Incarnation, before.DurableIndex, before.OrderingHeadMAC, before.DurableIndex, cfg.ResourceBudget.OrderingJournalDiskBytes)
	if errors.Is(err, replication.ErrOrderingInspectionRetirement) || errors.Is(err, ownedfile.ErrRetirement) {
		return result, &haSelectedReceiveInspectionFailure{Fee: inspectionFee, Cause: errors.Join(err, ownedfile.ErrRetirement)}
	}
	if err = errors.Join(err, inspectionFee.Release()); err != nil {
		return result, err
	}
	if report.Head.Index != before.DurableIndex || report.Head.MAC != before.OrderingHeadMAC || report.Head.Term > before.Term {
		return result, replication.ErrMemberAuthority
	}
	f, err := openHASelectedRuntimeFoundation(ctx, cfg, master, guard, manager, logger)
	if f != nil {
		defer func() {
			if cleanup := f.Close(); cleanup != nil {
				resultErr = &haSelectedRuntimeFoundationFailure{Owner: f, Cause: errors.Join(resultErr, ownedfile.ErrRetirement, cleanup)}
			}
		}()
	}
	if err != nil {
		return result, err
	}
	if err = f.prepareSelectedRoots(ctx); err != nil {
		return result, err
	}
	if f.replication.role != replication.RoleReplica || f.peerPromotionRecovery != nil || f.promotionRecovery != nil || f.reopened == nil {
		return result, authenticatedstate.ErrSealIneligible
	}
	// Reauthenticate all four independent source permissions and the selected
	// seven-root cut before preserving any candidate name. No candidate is read
	// as authority and no transport, DAO or semantic writer is exposed.
	if err = f.withSelectedCut(ctx, func(ctx context.Context, _ statecut.HASelectedCut) error { return ctx.Err() }); err != nil {
		return result, err
	}
	c, r := f.owners.Components, f.replication
	if err = r.publisher.ValidatePortableReceivedPublication(ctx, r.journal, c.Budget); err == nil {
		return r.publisher.Snapshot(), nil
	} else if err != replication.ErrNoReceivedPublication {
		return result, err
	}
	fee, err := c.Budget.TryAcquireContext(ctx, resourcebudget.Background, resourcebudget.Cost{Disk: replication.PortableReceivedBankDiskBytes})
	if err != nil {
		return result, err
	}
	f.fees = append(f.fees, fee)
	if _, err = r.publisher.PreparePortableReceivedPublicationWithOptions(ctx, r.journal, r.source, f.owners.Baseline, c.Budget, fee, replication.PortableReceivedPublicationOptions{Restart: true}); err != nil {
		return result, err
	}
	return r.publisher.Snapshot(), nil
}

// An unknown read Close retains the original concrete FD in Cause and its
// paid workspace; the offline lock remains retained by the public parent.
type haSelectedReceiveInspectionFailure struct {
	Fee   resourcebudget.Permit
	Cause error
}

func (e *haSelectedReceiveInspectionFailure) Error() string { return e.Cause.Error() }
func (e *haSelectedReceiveInspectionFailure) Unwrap() error { return e.Cause }
