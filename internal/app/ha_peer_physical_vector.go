package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/metadatastate"
	"github.com/akz142857/Halro/internal/nativephysical"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/statecut"
	"github.com/akz142857/Halro/internal/usage"
)

// The controller must exclude every actual receive/apply producer while this
// capture runs. Original getters join first in native->Member order, followed
// by the genuine complete Member/Ordering/source/common-root qualification.
// Neither a caller-supplied vector nor a configured boolean can create this.
type haPeerPhysicalVector struct {
	Ledger     ledger.HAReplicaPhysicalObservation
	Usage      usage.HAReplicaPhysicalObservation
	Directory  ledger.HAReplicaDirectoryPhysicalObservation
	Metadata   metadatastate.HAReplicaPhysicalObservation
	Audit      nativephysical.PreparedObservation
	Governance governance.HAReplicaPhysicalObservation
	Ordering   replication.PhysicalOrderingWindowObservation
	Member     replication.MemberState
	Seal       authenticatedstate.CandidateSeal
	Window     [32]byte
	Available  replication.PhysicalCreditCost
}
type haPeerPhysicalCOWBank struct {
	Selection, Scope               [32]byte
	Pack, Parent                   resourcebudget.PhysicalIdentity
	Controls                       [2]resourcebudget.PhysicalIdentity
	Native                         authenticatedstate.PhysicalNativeReference
	NativeEnd, COWEnd, AppendBytes int64
}

func haPeerCOWBank(w authenticatedstate.PhysicalTerminalWindowObservation) haPeerPhysicalCOWBank {
	return haPeerPhysicalCOWBank{w.Selection, w.Scope, w.Pack, w.Parent, w.Controls, w.Native, w.NativeEnd, w.COWEnd, w.AppendBytes}
}

type haPeerPhysicalNativeBank struct {
	Identity         [32]byte
	Controls         [2]resourcebudget.PhysicalIdentity
	Native, Parent   resourcebudget.PhysicalIdentity
	Baseline         nativephysical.Position
	End, AppendBytes int64
}

func haPeerNativeBank(w nativephysical.PreparedObservation) haPeerPhysicalNativeBank {
	return haPeerPhysicalNativeBank{w.Identity, w.Controls, w.Native.Native, w.Native.Parent, w.Baseline, w.Native.End, w.AppendBytes}
}
func haPeerAvailableBytes(n int64) (uint64, error) {
	if n < 0 {
		return 0, replication.ErrPhysicalCredit
	}
	return uint64(n), nil
}
func (f *haSelectedRuntimeFoundation) capturePeerPhysicalVector(ctx context.Context) (out haPeerPhysicalVector, resultErr error) {
	if f == nil || f.reopened == nil || f.replication == nil || f.replication.role != replication.RoleReplica || f.physicalPublication == nil || f.peerPromotionRecovery != nil || f.promotionRecovery != nil || f.ctx.Err() != nil {
		return out, replication.ErrPhysicalCredit
	}
	c := f.reopened.Owners.Components
	if c.Budget != f.owners.Components.Budget || c.NativeLedger != f.owners.Components.NativeLedger || c.Publisher != f.owners.Components.Publisher {
		return out, replication.ErrPhysicalCredit
	}
	resultErr = c.Directory.WithPhysicalCatchupWorkspace(ctx, func(ctx context.Context) error {
		var err error
		out.Ledger, err = c.Ledger.ObserveHAReplicaPhysicalCatchup(ctx, c.NativeLedger)
		if err != nil {
			return err
		}
		out.Usage, err = c.Usage.ObserveHAReplicaPhysicalCatchup(ctx, c.NativeLedger, c.Ledger)
		if err != nil {
			return err
		}
		out.Directory, err = c.Directory.ObserveHAReplicaPhysicalCatchup(ctx, c.NativeLedger, c.Ledger)
		if err != nil {
			return err
		}
		out.Metadata, err = c.Metadata.ObserveHAReplicaPhysicalEscrow(ctx)
		if err != nil {
			return err
		}
		out.Audit, err = c.Audit.ObserveHAReplicaPhysicalNative(ctx)
		if err != nil {
			return err
		}
		out.Governance, err = c.Governance.ObserveHAReplicaPhysicalProjection(ctx)
		if err != nil {
			return err
		}
		if out.Usage.WindowCount < 1 || out.Usage.WindowCount > resourcebudget.MaxCompletionBatch || out.Ledger.Native != out.Usage.Native || out.Ledger.Native != out.Directory.Native {
			return replication.ErrPhysicalCredit
		}
		// Read-only observations cannot credit a pending native suffix. Final full
		// scope authenticates all actual source chains and seven selected roots.
		return f.withSelectedCut(ctx, func(ctx context.Context, cut statecut.HASelectedCut) error {
			m := cut.Member
			if m.Role != replication.RoleReplica || m.Term != m.PromisedTerm || m.DurableIndex != m.ConfirmedIndex || m.AppliedIndex != m.DurableIndex || cut.AllowedIndex != m.DurableIndex || cut.Seal.OrderingIndex > m.DurableIndex {
				return replication.ErrPhysicalCredit
			}
			check := func(slot int, generation, sequence uint64, hash [32]byte) bool {
				return cut.Allowed[slot].Generation == generation && cut.Allowed[slot].Sequence == sequence && cut.Heads[slot] == hash
			}
			if !check(0, out.Ledger.Native.Generation, out.Ledger.Native.Sequence, out.Ledger.Native.Hash) || !check(1, 1, out.Audit.Native.Head.Sequence, out.Audit.Native.Head.Hash) || !check(2, 1, out.Governance.Native.Native.Head.Sequence, out.Governance.Native.Native.Head.Hash) || !check(3, out.Metadata.Native.Generation, out.Metadata.Native.Sequence, out.Metadata.Native.Hash) {
				return replication.ErrPhysicalCredit
			}
			raw, err := json.Marshal(cut.Seal)
			if err != nil {
				return err
			}
			selection := sha256.Sum256(raw)
			for _, w := range []authenticatedstate.PhysicalTerminalWindowObservation{out.Ledger.Window, out.Usage.Windows[0], out.Directory.Window, out.Metadata.Window, out.Governance.Window} {
				if w.Selection != selection {
					return replication.ErrPhysicalCredit
				}
			}
			out.Ordering, err = f.physicalPublication.ObservePhysicalOrderingWindowFrozen(ctx)
			if err != nil {
				return err
			}
			if out.Ordering.Index != m.DurableIndex || out.Ordering.Head != m.OrderingHeadMAC {
				return replication.ErrPhysicalCredit
			}
			identity := struct {
				Baseline                                       [32]byte
				Term                                           uint64
				Ledger, Usage, Directory, Metadata, Governance haPeerPhysicalCOWBank
				Audit, GovernanceNative                        haPeerPhysicalNativeBank
				OrderingBanks                                  [2]resourcebudget.PhysicalIdentity
				OrderingFile, OrderingControl                  resourcebudget.PhysicalIdentity
				OrderingEnd                                    int64
			}{f.owners.Baseline.Descriptor().AuthorityDigest, m.Term, haPeerCOWBank(out.Ledger.Window), haPeerCOWBank(out.Usage.Windows[0]), haPeerCOWBank(out.Directory.Window), haPeerCOWBank(out.Metadata.Window), haPeerCOWBank(out.Governance.Window), haPeerNativeBank(out.Audit), haPeerNativeBank(out.Governance.Native), out.Ordering.MemberBanks, out.Ordering.Ordering, out.Ordering.Control, out.Ordering.End}
			raw, err = json.Marshal(identity)
			if err != nil {
				return err
			}
			out.Window = sha256.Sum256(raw)
			out.Member, out.Seal = cut.Member, cut.Seal
			out.Available = replication.PhysicalCreditCost{NativeRecords: [4]uint32{out.Ledger.AvailableRecords, uint32(out.Audit.AvailableBytes/audit.PhysicalBatchBytes) * audit.MaxBatchEvents, out.Governance.AvailableFrames, out.Metadata.AvailableFrames}, COWRecords: [4]uint32{out.Ledger.AvailableRecords, 0, out.Governance.AvailableFrames, out.Metadata.AvailableFrames}, UsageFrames: out.Usage.AvailableFrames, DirectoryRolls: out.Directory.AvailableRolls, OrderingRecords: out.Ordering.RemainingRecords}
			sizes := [4]int64{out.Ledger.AvailableBytes, out.Audit.AvailableBytes, out.Governance.Native.AvailableBytes, out.Metadata.AvailableBytes}
			for i, n := range sizes {
				out.Available.NativeBytes[i], err = haPeerAvailableBytes(n)
				if err != nil {
					return err
				}
			}
			return errors.Join(ctx.Err(), f.ctx.Err())
		})
	})
	if resultErr != nil {
		out = haPeerPhysicalVector{}
	}
	return
}
func haPeerPhysicalTerminalCost(count int) (replication.PhysicalCreditCost, error) {
	if count < 2 || count > resourcebudget.MaxCompletionBatch {
		return replication.PhysicalCreditCost{}, resourcebudget.ErrInvalid
	}
	// Existing HTTP retry accounting accepts one request, each of three bounded
	// attempt starts/finishes, settlement and finalization; price-pin prepare and
	// release use at most twice the same original attempts count.
	ledgerFacts := uint32(3*(count-1) + 2)
	metadataFrames := uint32(2 * (count - 1))
	if ledgerFacts > resourcebudget.MaxCompletionBatch || metadataFrames > resourcebudget.MaxCompletionBatch {
		return replication.PhysicalCreditCost{}, replication.ErrPhysicalCredit
	}
	return replication.PhysicalCreditCost{NativeBytes: [4]uint64{uint64(ledgerFacts) * uint64(ledger.HAPhysicalCreditAppendBytes), uint64(audit.PhysicalBatchBytes), uint64(governance.PhysicalBatchBytes), uint64(metadataFrames) * uint64(metadatajournal.PhysicalAppendBytes)}, NativeRecords: [4]uint32{ledgerFacts, audit.MaxBatchEvents, 1, metadataFrames}, COWRecords: [4]uint32{ledgerFacts, 0, 1, metadataFrames}, UsageFrames: ledgerFacts, OrderingRecords: ledgerFacts + metadataFrames + 2}, nil
}
