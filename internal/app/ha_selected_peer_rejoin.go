package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/statecut"
	"github.com/akz142857/Halro/internal/store/lock"
)

// RejoinHASelectedMember continues this node's independently signed, durable
// promotion Promise while stopped. It cannot choose a candidate, term or root:
// the existing proposal and actual new Primary's fulfilled carrier decide all
// three. Ordinary Runtime opens only after real Leadership apply/publication.
func RejoinHASelectedMember(ctx context.Context, cfg config.Config) (result replication.MemberState, resultErr error) {
	if cfg.Replication == nil || cfg.ResourceBudget == nil || cfg.ResourceBudget.PhysicalTerminalReserve {
		return result, authenticatedstate.ErrSealIneligible
	}
	guard, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		return result, fmt.Errorf("peer rejoin requires stopped Halro: %w", err)
	}
	defer func() {
		if errors.Is(resultErr, ownedfile.ErrRetirement) {
			resultErr = &haSelectedPeerRejoinFailure{Guard: guard, Cause: resultErr}
			return
		}
		if err := guard.Close(); err != nil {
			resultErr = &haSelectedPeerRejoinFailure{Guard: guard, Cause: errors.Join(resultErr, ownedfile.ErrRetirement, err)}
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
		result, err = rejoinHASelectedMemberSampled(ctx, cfg, master, guard, pressure.logicalBudget(), slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
		if payload != nil {
			err = errors.Join(err, payload.Close())
		}
		return err
	})
	return
}

// A source whose Close is unknown keeps both its concrete owner in Cause and
// the stopped-directory lock. Another generation cannot race that descriptor.
type haSelectedPeerRejoinFailure struct {
	Guard *lock.Lock
	Cause error
}

func (e *haSelectedPeerRejoinFailure) Error() string { return e.Cause.Error() }
func (e *haSelectedPeerRejoinFailure) Unwrap() error { return e.Cause }

func rejoinHASelectedMemberSampled(ctx context.Context, cfg config.Config, master []byte, guard *lock.Lock, manager *resourcebudget.Manager, logger *slog.Logger, phase func(string) error) (result replication.MemberState, resultErr error) {
	member, key, err := readMemberState(cfg, master)
	if err != nil {
		return result, err
	}
	defer clear(key[:])
	peer, err := readHASelectedPeerPromotionCarrier(ctx, haSelectedPeerPromotionPath(cfg, true), key[:], manager)
	if err != nil {
		return result, err
	}
	if peer.Claims().Before.NodeID != member.NodeID {
		return result, replication.ErrMemberAuthority
	}
	var completion *replication.HAPromotionPeerCompletion
	if candidate, err := readHASelectedPromotionCarrier(ctx, haSelectedPromotionPath(cfg, true), key[:], manager); err == nil {
		completion, err = replication.QualifyHAPromotionPeerCompletion(peer, candidate)
		if err != nil {
			return result, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return result, err
	}
	f, err := openHASelectedRuntimeFoundationPeerCompletionMode(ctx, cfg, master, guard, manager, logger, nil, peer, completion)
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
	if f.peerPromotionRecovery == nil {
		return f.replication.publisher.Snapshot(), nil
	}
	step := func(name string) error {
		if phase != nil {
			return phase(name)
		}
		return ctx.Err()
	}
	consume := func(ctx context.Context, actual *replication.HAPromotionPeerCompletion, leadership []byte) error {
		if actual == nil || actual.Peer() != peer {
			return replication.ErrMemberAuthority
		}
		candidate, err := persistHASelectedPromotionCarrier(ctx, cfg, actual.Candidate().Claims(), key[:], manager)
		if err != nil {
			return err
		}
		completion, err = replication.QualifyHAPromotionPeerCompletion(peer, candidate)
		if err != nil {
			return err
		}
		f.owners.PeerPromotionCompletion = completion
		f.reopened.Owners.PeerPromotionCompletion = completion
		if err = step("peer-candidate-carrier"); err != nil {
			return err
		}
		return f.applyPeerLeadership(ctx, completion, leadership, step)
	}
	if completion != nil {
		if member.Term == peer.Claims().Proposal.Term && member.DurableIndex == peer.Claims().Before.AppliedIndex+1 {
			returnedErr := consume(ctx, completion, nil)
			return f.replication.publisher.Snapshot(), returnedErr
		}
	}
	p := peer.Claims()
	hello := promotionHello(f.replication.publisher.Snapshot())
	hello.PromisedTerm = p.Proposal.Term
	hello.Protocol = replication.VersionRange{Current: 2, Minimum: 2, Maximum: 2}
	hello.Metadata = hello.Protocol
	hello.MetadataBaselineAuthority = p.BaselineDigest
	var endpoint replication.PeerEndpoint
	for _, m := range member.Peers {
		if m.Name == p.Proposal.CandidateNodeID {
			endpoint = replication.PeerEndpoint{NodeID: m.Name, Address: m.Address, SPKISHA256: m.SPKISHA256}
		}
	}
	if endpoint.NodeID == "" {
		return result, replication.ErrMemberAuthority
	}
	err = replication.WithHAPeerPromotionBoundary(ctx, replication.TLSFiles{CAFile: cfg.Replication.TLS.CAFile, CertFile: cfg.Replication.TLS.CertFile, KeyFile: cfg.Replication.TLS.KeyFile}, key[:], hello, endpoint, peer, manager, consume)
	if err == nil {
		result = f.replication.publisher.Snapshot()
	}
	return result, err
}

// The native sink has exactly one structural slot. Any semantic or second
// record is refused before touching native bytes or Ordering.
type haPeerLeadershipSink struct {
	completion *replication.HAPromotionPeerCompletion
}

func (s *haPeerLeadershipSink) Persist(frame replication.Frame) error {
	if s == nil || s.completion == nil {
		return replication.ErrMemberAuthority
	}
	p := s.completion.Peer().Claims()
	if frame.ClusterID != p.Before.ClusterID || frame.Incarnation != p.Before.Incarnation || frame.Term != p.Proposal.Term || frame.Index != p.Before.AppliedIndex+1 || frame.ConfirmedIndex != p.Before.ConfirmedIndex || frame.Kind != replication.KindLeadershipEstablished || frame.Store != replication.StoreNone {
		return replication.ErrMemberAuthority
	}
	return nil
}

type haPeerLeadershipProjection struct {
	owner    *haSelectedRuntimeFoundation
	receiver *replication.ReplicaReceiver
}

func (*haPeerLeadershipProjection) ApplyLedgerThrough(context.Context, uint64, uint64) error {
	return authenticatedstate.ErrSealIneligible
}
func (*haPeerLeadershipProjection) ApplyMetadataThrough(context.Context, uint64, uint64) error {
	return authenticatedstate.ErrSealIneligible
}
func (p *haPeerLeadershipProjection) ApplyConfirmedPrefix(ctx context.Context, prefix *replication.ReplicaConfirmedPrefix) (replication.ProjectionState, error) {
	return statecut.ApplyHAPeerLeadership(ctx, p.owner.reopened.Owners, p.receiver, prefix)
}

func (f *haSelectedRuntimeFoundation) applyPeerLeadership(ctx context.Context, completion *replication.HAPromotionPeerCompletion, encoded []byte, step func(string) error) error {
	r := f.replication
	m := r.publisher.Snapshot()
	p := completion.Peer().Claims()
	if completion.ValidateRecoveryMember(m, r.journal) != nil {
		return replication.ErrMemberAuthority
	}
	receiver, err := replication.NewReplicaReceiver(m.ClusterID, m.Incarnation, m.NodeID, p.Proposal.CandidateNodeID, m.Term, m.PromisedTerm, m.ConfirmedIndex, m.AppliedIndex, r.journal, &haPeerLeadershipSink{completion: completion}, replication.ReplicaReceiverOptions{PersistState: r.publisher.PublishReplica, Projection: m.Projection})
	if err != nil {
		return err
	}
	if m.Term < p.Proposal.Term {
		if err = receiver.AdoptTerm(p.Proposal.Term, p.Proposal.CandidateNodeID, func() error { _, err := r.publisher.AdoptHigherTerm(p.Proposal.Term, p.Proposal.Term); return err }); err != nil {
			return err
		}
	}
	if err = step("peer-adopted"); err != nil {
		return err
	}
	if m.DurableIndex == p.Before.AppliedIndex {
		if len(encoded) == 0 {
			return replication.ErrMemberAuthority
		}
		if _, err = receiver.Receive(encoded); err != nil {
			return err
		}
	}
	if err = completion.ValidateLeadershipMember(r.publisher.Snapshot(), r.journal); err != nil {
		return err
	}
	if err = step("peer-leadership-received"); err != nil {
		return err
	}
	applier, err := replication.NewReplicaApplier(receiver, r.journal, &haPeerLeadershipProjection{owner: f, receiver: receiver})
	if err != nil {
		return err
	}
	if _, err = applier.ApplyConfirmed(ctx); err != nil {
		return fmt.Errorf("peer original confirmed apply: %w", err)
	}
	if err = step("peer-leadership-applied"); err != nil {
		return err
	}
	publication, err := statecut.PublishHAPeerLeadership(ctx, f.reopened.Owners, func(ctx context.Context) (statecut.StartupCacheCandidates, error) {
		return f.preparePromotionStartupCaches(ctx, step)
	})
	if publication.CleanupError() != nil {
		return &statecut.HAPromotionPeerCaptureFailure{Owner: publication, Cause: err}
	}
	if err != nil {
		return fmt.Errorf("peer genuine Replica common publication: %w", err)
	}
	if err = step("peer-published"); err != nil {
		return err
	}
	return ctx.Err()
}
