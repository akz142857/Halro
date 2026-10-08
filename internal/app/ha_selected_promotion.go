package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/statecut"
	"github.com/akz142857/Halro/internal/store/lock"
)

// The public command retains the real stopped-directory lock. No source owner
// from this recovery path is exposed to a transport or ordinary business DAO.
func promoteHASelectedMember(ctx context.Context, cfg config.Config, guard *lock.Lock, options PromoteMemberOptions, phase func(string) error) (result PromoteMemberResult, resultErr error) {
	ctx, pressure, err := commandBudgetContext(ctx, cfg)
	if err != nil || pressure == nil || pressure.logicalBudget() == nil {
		return result, errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	if cfg.ResourceBudget.PhysicalTerminalReserve {
		return result, errors.New("HA physical promotion vector is not prepared")
	}
	resultErr = pressure.monitor.WithSampling(ctx, func(ctx context.Context) error {
		var err error
		result, err = promoteHASelectedMemberSampled(ctx, cfg, guard, options, phase, pressure)
		return err
	})
	return
}
func promoteHASelectedMemberSampled(ctx context.Context, cfg config.Config, guard *lock.Lock, options PromoteMemberOptions, phase func(string) error, pressure *resourcePressureRuntime) (result PromoteMemberResult, resultErr error) {
	manager := pressure.logicalBudget()
	var err error
	var master []byte
	var unlock *haMemoryUnlockPayload
	if cfg.Storage.MasterKey.Mode == config.MasterKeyModeKeySlots {
		master, unlock, err = unlockHAMemoryCandidate(ctx, cfg, manager, defaultKMSWrapperFactory)
	} else {
		master, err = unlockMemberMasterKey(ctx, cfg)
	}
	if err != nil {
		return result, err
	}
	defer clear(master)
	if unlock != nil {
		defer func() { resultErr = errors.Join(resultErr, unlock.Close()) }()
	}
	member, key, err := readMemberState(cfg, master)
	if err != nil {
		return result, err
	}
	defer clear(key[:])
	prepared, err := readHASelectedPromotionCarrier(ctx, haSelectedPromotionPath(cfg, false), key[:], manager)
	if errors.Is(err, os.ErrNotExist) {
		prepared = nil
		err = nil
	}
	if err != nil {
		return result, err
	}
	var recovery *replication.HAPromotionAuthority
	if prepared != nil {
		if !sameHAPromotionRequest(prepared.Claims(), options) {
			return result, replication.ErrMemberAuthority
		}
		// A promise is already irreversible. Read old roots only through this
		// explicitly stopped recovery mode, never through ordinary term authority.
		if member.PromisedTerm != member.Term || member.Term != prepared.Claims().Before.Term {
			recovery = prepared
		}
		fulfilled, e := readHASelectedPromotionCarrier(ctx, haSelectedPromotionPath(cfg, true), key[:], manager)
		if e == nil {
			if !sameHAPromotionRequest(fulfilled.Claims(), options) || !fulfilled.MatchesPreparation(prepared) || fulfilled.ValidateRecoveryMember(member) != nil {
				return result, replication.ErrMemberAuthority
			}
			recovery = fulfilled
		} else if !errors.Is(e, os.ErrNotExist) {
			return result, e
		} else if prepared.ValidateRecoveryMember(member) != nil {
			return result, replication.ErrMemberAuthority
		}
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	f, err := openHASelectedRuntimeFoundationMode(ctx, cfg, master, guard, manager, logger, recovery)
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
	if f.promotionRecovery != nil {
		f.metadataStore, err = f.reopened.OpenHAPromotionOperatorStore(ctx)
	} else {
		f.metadataStore, err = f.reopened.OpenHAMetadataStore(ctx)
	}
	if err != nil {
		return result, err
	}
	if unlock != nil {
		if err = unlock.validateSelected(ctx, f); err != nil {
			return result, err
		}
	}
	if err = authenticateClusterOperatorMetadata(ctx, cfg, master, f.metadataStore, options.Username, options.Password, options.TOTPCode); err != nil {
		return result, err
	}
	if prepared != nil && f.promotionRecovery == nil && member.Term == prepared.Claims().Proposal.Term && f.reopened.Seal.Term == member.Term {
		committed, e := readHASelectedPromotionCarrier(ctx, haSelectedPromotionPath(cfg, true), key[:], manager)
		if e != nil || !sameHAPromotionRequest(committed.Claims(), options) || committed.ValidateOrdering(f.replication.journal) != nil {
			return result, errors.Join(replication.ErrMemberAuthority, e)
		}
		return PromoteMemberResult{State: f.replication.publisher.Snapshot(), Promises: committed.Claims().Promises}, nil
	}
	step := func(name string) error {
		if phase != nil {
			return phase(name)
		}
		return ctx.Err()
	}
	var claims replication.HAPromotionClaims
	if prepared == nil {
		if member.Term != options.ExpectedTerm || member.AppliedIndex != options.ExpectedAppliedIndex || member.PromisedTerm == ^uint64(0) {
			return result, replication.ErrMemberAuthority
		}
		p := replication.PromotionProposal{Version: replication.PromotionProtocolVersion, ClusterID: member.ClusterID, Incarnation: member.Incarnation, CandidateNodeID: member.NodeID, Term: member.PromisedTerm + 1, ExpectedTerm: options.ExpectedTerm, ExpectedAppliedIndex: options.ExpectedAppliedIndex, OldPrimaryNodeID: options.OldPrimaryNodeID, FencedBy: options.FencedBy, Self: options.Self, PlannedStepdown: options.PlannedStepdown, ActorID: options.Username}
		claims, err = statecut.CaptureHAPromotion(ctx, f.reopened.Owners, p, options.NoPeerPromise)
		if err != nil {
			return result, err
		}
		prepared, err = persistHASelectedPromotionCarrier(ctx, cfg, claims, key[:], manager)
		if err != nil {
			return result, err
		}
		if err = step("prepared"); err != nil {
			return result, err
		}
	} else {
		claims = prepared.Claims()
	}
	publisher := f.replication.publisher
	if member.Term == claims.Before.Term && member.PromisedTerm == claims.Before.Term {
		member, err = publisher.Promise(claims.Proposal.Term)
		if err != nil {
			return result, err
		}
		if err = step("promised"); err != nil {
			return result, err
		}
	}
	// Withdraw old ordinary authority immediately. The same private owner now
	// borrows only the carrier's old roots for the stopped transition publication.
	f.promotionRecovery = prepared
	f.owners.PromotionRecovery = prepared
	f.reopened.Owners.PromotionRecovery = prepared
	fulfilled, err := readHASelectedPromotionCarrier(ctx, haSelectedPromotionPath(cfg, true), key[:], manager)
	if errors.Is(err, os.ErrNotExist) {
		hello := promotionHello(member)
		hello.Protocol = replication.VersionRange{Current: 2, Minimum: 2, Maximum: 2}
		hello.Metadata = hello.Protocol
		hello.MetadataBaselineAuthority = f.owners.Baseline.Descriptor().AuthorityDigest
		promises := make([]replication.PromotionPromise, 0, len(member.Peers))
		var peerErrors []error
		if !options.NoPeerPromise {
			for _, p := range member.Peers {
				promise, e := replication.RequestPromotionPromise(ctx, replication.TLSFiles{CAFile: cfg.Replication.TLS.CAFile, CertFile: cfg.Replication.TLS.CertFile, KeyFile: cfg.Replication.TLS.KeyFile}, key[:], hello, replication.PeerEndpoint{NodeID: p.Name, Address: p.Address, SPKISHA256: p.SPKISHA256}, claims.Proposal)
				if e != nil {
					peerErrors = append(peerErrors, fmt.Errorf("peer %s: %w", p.Name, e))
				} else {
					promises = append(promises, promise)
				}
			}
		}
		if err = replication.ValidatePromotionPromises(member, claims.Proposal, promises, options.NoPeerPromise); err != nil {
			return result, errors.Join(err, errors.Join(peerErrors...))
		}
		sort.Slice(promises, func(i, j int) bool { return promises[i].NodeID < promises[j].NodeID })
		claims.Promises, claims.Fulfilled = promises, true
		fulfilled, err = persistHASelectedPromotionCarrier(ctx, cfg, claims, key[:], manager)
	}
	if err != nil {
		return result, err
	}
	if err = step("fulfilled"); err != nil {
		return result, err
	}
	f.promotionRecovery = fulfilled
	f.owners.PromotionRecovery = fulfilled
	f.reopened.Owners.PromotionRecovery = fulfilled
	if member.Term == claims.Before.Term {
		member, err = publisher.Promote(claims.Before.Term, claims.Before.AppliedIndex, claims.Proposal.Term)
		if err != nil {
			return result, err
		}
	}
	if err = step("promoted"); err != nil {
		return result, err
	}
	if member.DurableIndex == claims.Before.AppliedIndex {
		peers := make([]string, 0, len(member.Peers))
		for _, p := range member.Peers {
			peers = append(peers, p.Name)
		}
		outbound, e := replication.NewConnectionOutbound(peers, offlinePeerSender{})
		if e != nil {
			return result, e
		}
		coordinator, e := replication.NewPrimaryCoordinator(member.ClusterID, member.Incarnation, member.NodeID, member.Term, member.ConfirmedIndex, peers, f.replication.journal, outbound, publisher.PublishPrimary)
		if e != nil {
			return result, e
		}
		defer coordinator.Stop()
		if _, err = coordinator.RecordDurable(replication.Frame{Kind: replication.KindLeadershipEstablished, Store: replication.StoreNone}); err != nil {
			return result, err
		}
		member = publisher.Snapshot()
	}
	if err = fulfilled.ValidateOrdering(f.replication.journal); err != nil {
		return result, err
	}
	if err = step("leadership"); err != nil {
		return result, err
	}
	if err = f.appendSelectedPromotionAudit(ctx, fulfilled, step); err != nil {
		return result, fmt.Errorf("promotion Audit must finish before common publication: %w", err)
	}
	publication, err := statecut.PublishHAPromotionWithStartupCaches(ctx, f.reopened.Owners, func(ctx context.Context) (statecut.StartupCacheCandidates, error) {
		return f.preparePromotionStartupCaches(ctx, step)
	})
	if err != nil {
		return result, fmt.Errorf("promotion is durable; keep stopped until authenticated common publication resumes: %w", err)
	}
	if publication.Seal.Term != claims.Proposal.Term || publication.Seal.Role != authenticatedstate.SealPrimary {
		return result, authenticatedstate.ErrSealIneligible
	}
	if err = step("published"); err != nil {
		return result, err
	}
	return PromoteMemberResult{State: publisher.Snapshot(), Promises: fulfilled.Claims().Promises}, nil
}
