package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"reflect"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/metadatastate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/vault"
)

type haGroupResumeKey struct{}

// Minted only after every configured directory is stopped and its identical
// intent was read through the bounded real-FD reader and decrypted by an actual
// File DEK. These keys do not carry Member/Ordering/root permission.
type haGroupResumeKeys struct {
	intent  *haGroupRotationIntent
	digest  [32]byte
	slot    int
	guards  [3]*lock.Lock
	names   [3]os.FileInfo
	missing [3]bool
}

func (p *haGroupResumeKeys) member(cfg config.Config) (int, error) {
	if p == nil || p.intent == nil || cfg.Replication == nil {
		return -1, authenticatedstate.ErrSealIneligible
	}
	for n := 0; n < int(p.intent.Count); n++ {
		e := p.intent.Members[n]
		if e.DataDir == cfg.Storage.DataDir && e.KeyFile == haGroupKeyDestination(cfg) && e.Source.Member.NodeID == cfg.Replication.NodeID && e.Source.Member.ClusterID == cfg.Replication.ClusterID && p.guards[n] != nil && p.guards[n].CoversDataDirectory(e.DataDir) && p.names[n] != nil {
			return n, nil
		}
	}
	return -1, authenticatedstate.ErrSealIneligible
}
func groupResumeProof(ctx context.Context, cfg config.Config) (*haGroupResumeKeys, int) {
	p, _ := ctx.Value(haGroupResumeKey{}).(*haGroupResumeKeys)
	n, err := p.member(cfg)
	if err != nil {
		return nil, -1
	}
	return p, n
}
func readHAGroupMemberForOpen(ctx context.Context, cfg config.Config, master []byte) (replication.MemberState, [32]byte, error) {
	p, n := groupResumeProof(ctx, cfg)
	if p == nil {
		return readMemberState(cfg, master)
	}
	if !bytes.Equal(master, p.intent.OldKey) && !bytes.Equal(master, p.intent.NewKey) {
		return replication.MemberState{}, [32]byte{}, authenticatedstate.ErrSealIneligible
	}
	k := p.intent.ClusterKey
	m, err := replication.ReadState(cfg.ReplicationStatePath(), k[:])
	if err != nil {
		clear(k[:])
		return m, k, err
	}
	before := p.intent.Members[n].Source.Member
	if m.ClusterID != before.ClusterID || m.NodeID != before.NodeID || m.Incarnation != before.Incarnation || m.Role != before.Role || m.Term != before.Term || m.PromisedTerm != before.PromisedTerm || m.AppliedIndex < before.AppliedIndex || m.ConfirmedIndex < before.ConfirmedIndex || m.DurableIndex < before.DurableIndex {
		clear(k[:])
		return replication.MemberState{}, k, replication.ErrMemberAuthority
	}
	peers := make([]replication.StatePeer, 0, len(cfg.Replication.Peers))
	for _, peer := range cfg.Replication.Peers {
		peers = append(peers, replication.StatePeer{Name: peer.Name, Address: peer.Address, SPKISHA256: peer.SPKISHA256})
	}
	if err := replication.ValidateMemberConfiguration(m, cfg.Replication.ClusterID, cfg.Replication.NodeID, peers); err != nil {
		clear(k[:])
		return replication.MemberState{}, k, err
	}
	return m, k, nil
}
func readHAGroupBootstrapForOpen(ctx context.Context, cfg config.Config, master []byte, manager *resourcebudget.Manager, member replication.MemberState, authority *replication.MetadataBaselineAuthority, evidence metadatastate.BaselineEvidence) (*haMemoryStateBootstrap, error) {
	p, n := groupResumeProof(ctx, cfg)
	keys := [][]byte{master}
	if p != nil {
		keys = [][]byte{p.intent.OldKey, p.intent.NewKey}
	}
	var resultErr error
	for _, key := range keys {
		v, err := vault.New(key)
		if err != nil {
			return nil, err
		}
		b, err := readHAMemoryStateBootstrap(ctx, cfg, v, manager, member, authority, evidence)
		v.Close()
		if err != nil {
			if errors.Is(err, ownedfile.ErrRetirement) {
				return nil, err
			}
			resultErr = errors.Join(resultErr, err)
			continue
		}
		if b == nil {
			return nil, authenticatedstate.ErrSealIneligible
		}
		if p != nil && !reflect.DeepEqual(*b, p.intent.Members[n].Bootstrap) {
			b.clearKeys()
			return nil, authenticatedstate.ErrSealIneligible
		}
		return b, nil
	}
	return nil, resultErr
}

func (p *haGroupResumeKeys) checkRaw(cfg config.Config, slot int, key, raw []byte, info os.FileInfo) bool {
	n, err := p.member(cfg)
	return err == nil && slot == p.slot && (bytes.Equal(key, p.intent.OldKey) || bytes.Equal(key, p.intent.NewKey)) && sha256.Sum256(raw) == p.digest && sameHAGroupReadFile(p.names[n], info)
}
