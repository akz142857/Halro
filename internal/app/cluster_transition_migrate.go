package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/store/lock"
)

// TransitionMigrationOptions binds an explicit offline migration to the
// authenticated state the operator inspected before stopping the member.
type TransitionMigrationOptions struct {
	ExpectedIncarnation string
	ExpectedRole        replication.Role
	ExpectedTerm        uint64
	ExpectedPromised    uint64
	ExpectedDurable     uint64
	ExpectedConfirmed   uint64
	ExpectedApplied     uint64
	Username            string
	Password            []byte
	TOTPCode            string
}

// MigrateMemberTransitionJournal is the supported operator entrypoint for
// moving one stopped member from state v2 to an explicit legacy baseline in
// state v3. It makes no claim about events preceding that baseline.
func MigrateMemberTransitionJournal(ctx context.Context, cfg config.Config, options TransitionMigrationOptions) (replication.MemberState, error) {
	if cfg.Replication == nil {
		return replication.MemberState{}, errors.New("transition migration requires replication configuration")
	}
	if options.ExpectedIncarnation == "" || options.ExpectedTerm == 0 ||
		(options.ExpectedRole != replication.RolePrimary && options.ExpectedRole != replication.RoleReplica) ||
		options.ExpectedPromised < options.ExpectedTerm || options.Username == "" || len(options.Password) == 0 {
		return replication.MemberState{}, errors.New("transition migration requires inspected incarnation, role, term, promised term, applied index and administrator reauthentication")
	}
	if err := replication.RequireMemberRuntime(cfg.Storage.DataDir, true, "cluster migrate-transitions"); err != nil {
		return replication.MemberState{}, err
	}
	dataLock, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		return replication.MemberState{}, fmt.Errorf("transition migration requires the local Halro process to be stopped: %w", err)
	}
	defer dataLock.Close()
	masterKey, err := unlockMemberMasterKey(ctx, cfg)
	if err != nil {
		return replication.MemberState{}, err
	}
	defer clear(masterKey)
	state, clusterKey, err := readMemberState(cfg, masterKey)
	if err != nil {
		return replication.MemberState{}, err
	}
	defer clear(clusterKey[:])
	if state.Incarnation != options.ExpectedIncarnation || state.Role != options.ExpectedRole ||
		state.Term != options.ExpectedTerm || state.PromisedTerm != options.ExpectedPromised ||
		state.DurableIndex != options.ExpectedDurable || state.ConfirmedIndex != options.ExpectedConfirmed || state.AppliedIndex != options.ExpectedApplied {
		return replication.MemberState{}, errors.New("inspected incarnation, role, term, promise or replication index changed before transition migration")
	}
	if err := authenticateClusterOperator(ctx, cfg, masterKey, options.Username, options.Password, options.TOTPCode); err != nil {
		return replication.MemberState{}, err
	}
	migrated, err := replication.MigrateStateTransitionJournal(cfg.ReplicationStatePath(), clusterKey[:])
	if err != nil {
		return replication.MemberState{}, err
	}
	// A successful low-level write is not the command's success criterion:
	// authenticate the published state and full journal again before returning.
	verified, err := replication.ReadState(cfg.ReplicationStatePath(), clusterKey[:])
	if err != nil {
		return replication.MemberState{}, err
	}
	if verified.Version != replication.TransitionStateVersion || verified.Transition != migrated.Transition ||
		verified.ClusterID != state.ClusterID || verified.Incarnation != state.Incarnation || verified.NodeID != state.NodeID ||
		verified.Role != state.Role || verified.Term != state.Term || verified.PromisedTerm != state.PromisedTerm ||
		verified.DurableIndex != state.DurableIndex || verified.ConfirmedIndex != state.ConfirmedIndex || verified.AppliedIndex != state.AppliedIndex {
		return replication.MemberState{}, errors.New("transition migration publication changed or cannot be verified")
	}
	journal, err := replication.OpenTransitionJournal(replication.TransitionJournalPath(cfg.ReplicationStatePath()), clusterKey[:], verified)
	if err != nil {
		return replication.MemberState{}, err
	}
	if err := journal.Close(); err != nil {
		return replication.MemberState{}, err
	}
	return verified, nil
}
