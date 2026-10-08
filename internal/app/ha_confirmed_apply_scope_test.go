package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/statecut"
)

type haConfirmedScopeTestProjection struct {
	*haSelectedReplicaProjection
	inspect   func(context.Context, *replication.ReplicaConfirmedPrefix) error
	skipApply bool
}

func (p *haConfirmedScopeTestProjection) ApplyConfirmedPrefix(ctx context.Context, prefix *replication.ReplicaConfirmedPrefix) (replication.ProjectionState, error) {
	if err := p.inspect(ctx, prefix); err != nil {
		return replication.ProjectionState{}, err
	}
	if p.skipApply {
		return replication.ProjectionState{}, nil
	}
	return p.haSelectedReplicaProjection.ApplyConfirmedPrefix(ctx, prefix)
}

func TestHAConfirmedApplyActualTLSLoansRevokeAndJoin(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	fixture, owners, metadata := prepareHASelectedTransportFixture(t, ctx)
	f := owners[1]
	checked := make(chan error, 1)
	var once sync.Once
	p := &haConfirmedScopeTestProjection{haSelectedReplicaProjection: &haSelectedReplicaProjection{owner: f}}
	p.inspect = func(ctx context.Context, prefix *replication.ReplicaConfirmedPrefix) (resultErr error) {
		once.Do(func() {
			defer func() { checked <- resultErr }()
			head, err := f.reopened.Owners.Components.Metadata.EpochHead()
			if err != nil {
				resultErr = err
				return
			}
			// An arbitrary context and a foreign real owner grant no apply loan.
			visit := func(context.Context) error { return nil }
			if err := statecut.WithHAConfirmedMetadataApplyAuthority(ctx, f.owners, f.owners.Components.Metadata, head, head.Sequence, visit); !errors.Is(err, authenticatedstate.ErrSealIneligible) {
				resultErr = errors.New("context without genuine source loan granted Metadata apply")
				return
			}
			var escaped context.Context
			var through uint64
			started, release := make(chan struct{}), make(chan struct{})
			borrowDone, scopeDone := make(chan error, 1), make(chan error, 1)
			go func() {
				scopeDone <- statecut.WithHAConfirmedSourceAuthority(ctx, f.owners, f.replication.receiver, prefix, func(ctx context.Context, cut statecut.HAConfirmedSources) error {
					escaped, through = ctx, cut.Allowed[3].Sequence
					go func() {
						borrowDone <- statecut.WithHAConfirmedMetadataApplyAuthority(ctx, f.owners, f.owners.Components.Metadata, head, through, func(context.Context) error {
							close(started)
							<-release
							return nil
						})
					}()
					select {
					case <-started:
						return nil
					case err := <-borrowDone:
						return errors.Join(errors.New("Metadata borrower refused before entry"), err)
					case <-ctx.Done():
						return ctx.Err()
					}
				})
			}()
			select {
			case <-started:
			case err := <-scopeDone:
				resultErr = errors.Join(errors.New("actual confirmed source loan refused"), err)
				close(release)
				return
			case <-ctx.Done():
				resultErr = ctx.Err()
				close(release)
				return
			}
			select {
			case err := <-scopeDone:
				close(release)
				resultErr = errors.Join(errors.New("source loan exited before actual borrower joined"), err)
				return
			case <-time.After(20 * time.Millisecond):
			}
			close(release)
			if err := <-borrowDone; err != nil {
				resultErr = err
				return
			}
			if err := <-scopeDone; err != nil {
				resultErr = err
				return
			}
			if err := statecut.WithHAConfirmedMetadataApplyAuthority(escaped, f.owners, f.owners.Components.Metadata, head, through, visit); !errors.Is(err, authenticatedstate.ErrSealIneligible) {
				resultErr = errors.New("escaped Metadata source loan remained valid")
				return
			}
			// Genuine receiver + four-source proof grants the confirmed Gov cut.
			applied := f.replication.publisher.Snapshot().AppliedIndex
			if err := statecut.WithHAConfirmedSelectedCut(ctx, f.owners, f.replication.receiver, prefix, func(ctx context.Context, cut statecut.HASelectedCut) error {
				if cut.Member.AppliedIndex != applied || cut.AllowedIndex <= applied || cut.Seal.OrderingIndex > applied || cut.Authority.OrderingIndex != cut.Seal.OrderingIndex {
					return errors.New("startup confirmed loan replaced saved/applied authority")
				}
				return statecut.WithHAConfirmedMetadataApplyAuthority(ctx, f.owners, f.owners.Components.Metadata, head, cut.Allowed[3].Sequence, visit)
			}); err != nil {
				resultErr = err
				return
			}
			resultErr = statecut.WithHAGovernanceConfirmedApplyAuthority(ctx, f.owners, f.replication.receiver, prefix, func(_ context.Context, a authenticatedstate.SealEligibility, p governance.HASelectedNativePosition) error {
				if a.Role != authenticatedstate.SealReplica || a.OrderingIndex <= applied || p.Sequence != 0 {
					return errors.New("Governance apply loan did not name real confirmed prefix")
				}
				return nil
			})
		})
		return resultErr
	}
	var err error
	f.replication.applier, err = replication.NewReplicaApplier(f.replication.receiver, f.replication.journal, p)
	if err != nil {
		t.Fatal(err)
	}
	installHAConfirmedScopeTestHandler(t, f)
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 2)
	for _, owner := range owners {
		go func(f *haSelectedRuntimeFoundation) { done <- f.serveSelectedTransport(runCtx) }(owner)
	}
	defer func() {
		stop()
		for range 2 {
			if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		}
	}()
	for !owners[0].replication.manager.PeerConnections()[fixture.configs[1].Replication.NodeID] {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if _, err := metadata[0].PutProject(ctx, domain.Project{ID: "actual-confirmed-loan", Name: "Confirmed loan", Enabled: true}, 0, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-checked:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func installHAConfirmedScopeTestHandler(t *testing.T, f *haSelectedRuntimeFoundation) {
	t.Helper()
	// The actual record handler owns its applier pointer. Install a fresh real
	// TLS handler around the inspection adapter before either manager runs.
	if err := f.replication.manager.Close(); err != nil {
		t.Fatal(err)
	}
	handler, err := replication.NewReplicaRecordHandler(f.replication.receiver, f.replication.applier)
	if err != nil {
		t.Fatal(err)
	}
	session, err := replicationSessionOptions(f.cfg, f.owners.Components.Budget)
	if err != nil {
		t.Fatal(err)
	}
	state := f.replication.publisher.Snapshot()
	peers := make([]replication.PeerEndpoint, 0, len(state.Peers))
	for _, peer := range state.Peers {
		peers = append(peers, replication.PeerEndpoint{NodeID: peer.Name, Address: peer.Address, SPKISHA256: peer.SPKISHA256})
	}
	f.replication.manager, err = replication.NewConnectionManager(replication.ConnectionManagerOptions{LocalNode: state.NodeID, TLS: replication.TLSFiles{CAFile: f.cfg.Replication.TLS.CAFile, CertFile: f.cfg.Replication.TLS.CertFile, KeyFile: f.cfg.Replication.TLS.KeyFile}, ClusterKey: f.replication.clusterKey[:], Peers: peers, LocalHello: f.selectedHello, OnAuthenticatedHello: f.authorizeSelectedHello, Handle: f.replication.handleRecords(handler.Handle), Session: session})
	if err != nil {
		t.Fatal(err)
	}
	f.sender.Set(f.replication.manager)
}

func TestHAConfirmedApplyActualTLSIgnoredBorrowerFailureRefusesApplied(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	fixture, owners, metadata := prepareHASelectedTransportFixture(t, ctx)
	f := owners[1]
	old := f.replication.publisher.Snapshot().AppliedIndex
	checked := make(chan error, 1)
	var once sync.Once
	p := &haConfirmedScopeTestProjection{haSelectedReplicaProjection: &haSelectedReplicaProjection{owner: f}, skipApply: true}
	p.inspect = func(ctx context.Context, prefix *replication.ReplicaConfirmedPrefix) error {
		once.Do(func() {
			err := statecut.WithHAConfirmedSourceAuthority(ctx, f.owners, f.replication.receiver, prefix, func(ctx context.Context, cut statecut.HAConfirmedSources) error {
				head, err := f.owners.Components.Metadata.EpochHead()
				if err != nil {
					return err
				}
				entered := false
				if err := statecut.WithHAConfirmedMetadataApplyAuthority(ctx, owners[0].owners, owners[0].owners.Components.Metadata, head, cut.Allowed[3].Sequence, func(context.Context) error { entered = true; return nil }); !errors.Is(err, authenticatedstate.ErrSealIneligible) || entered {
					return errors.New("foreign actual Metadata owner entered confirmed job")
				}
				// A buggy consumer ignores its refused target. The owner must latch
				// that error and the real applier must refuse Member Applied anyway.
				_ = statecut.WithHAConfirmedMetadataApplyAuthority(ctx, f.owners, f.owners.Components.Metadata, head, cut.Allowed[3].Sequence+1, func(context.Context) error { entered = true; return nil })
				if entered {
					return errors.New("unconfirmed Metadata target entered reducer")
				}
				return nil
			})
			checked <- err
		})
		return nil // Deliberately ignore the real source owner's failure as well.
	}
	var err error
	f.replication.applier, err = replication.NewReplicaApplier(f.replication.receiver, f.replication.journal, p)
	if err != nil {
		t.Fatal(err)
	}
	installHAConfirmedScopeTestHandler(t, f)
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 2)
	for _, owner := range owners {
		go func(f *haSelectedRuntimeFoundation) { done <- f.serveSelectedTransport(runCtx) }(owner)
	}
	defer func() {
		stop()
		for range 2 {
			if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		}
	}()
	for !owners[0].replication.manager.PeerConnections()[fixture.configs[1].Replication.NodeID] {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if _, err := metadata[0].PutProject(ctx, domain.Project{ID: "refused-confirmed-loan", Name: "Refused loan", Enabled: true}, 0, nil); err != nil {
		t.Log("intentional refused replica reducer withdrew transport", err)
	}
	select {
	case err := <-checked:
		if !errors.Is(err, authenticatedstate.ErrSealIneligible) {
			t.Fatal("ignored borrower failure escaped", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	stop()
	// Closing the actual handler joins the callback before observing progress.
	if err := f.replication.manager.Close(); err != nil {
		t.Fatal(err)
	}
	if actual := f.replication.publisher.Snapshot(); actual.AppliedIndex != old || actual.ConfirmedIndex <= old || actual.Projection.Index != old {
		t.Fatal("failed reducer published Member Applied", actual)
	}
}
