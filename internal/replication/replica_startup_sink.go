package replication

import (
	"errors"
	"slices"
)

// This sink has no receive capability while a real ReplicaApplier restores a
// confirmed prefix. Its original receiver is retained through the one-time
// handoff; no copied watermark or second receiver can manufacture that job.
type replicaStartupNativeSink struct{ native *NativeSink }

func (s *replicaStartupNativeSink) Persist(frame Frame) error {
	if s == nil || s.native == nil {
		return ErrMemberAuthority
	}
	return s.native.Persist(frame)
}

func NewReplicaStartupReceiver(clusterID, incarnation, nodeID, primaryNodeID string, term, promisedTerm, confirmedIndex, appliedIndex uint64, journal *OrderingJournal, options ReplicaReceiverOptions) (*ReplicaReceiver, error) {
	sink := &replicaStartupNativeSink{}
	r, err := NewReplicaReceiver(clusterID, incarnation, nodeID, primaryNodeID, term, promisedTerm, confirmedIndex, appliedIndex, journal, sink, options)
	if err != nil {
		return nil, err
	}
	r.startupSink = sink
	return r, nil
}

// InstallStartupNativeSink follows successful whole-prefix Applied publication
// and actual construction of the four native owners. It admits no root/term
// permission; the real NativeSink still authenticates every frame before IO.
func (r *ReplicaReceiver) InstallStartupNativeSink(sink *NativeSink) error {
	if r == nil || sink == nil || sink.ledger == nil || sink.audit == nil || sink.governance == nil || sink.metadata == nil || sink.metadataBaseline == nil {
		return ErrMemberAuthority
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.startupSink == nil || r.sink != r.startupSink || r.startupSink.native != nil || r.poisoned != nil || r.term != r.promisedTerm || r.appliedIndex != r.confirmedIndex || r.projection.Index != r.appliedIndex {
		return ErrMemberAuthority
	}
	baseline := sink.metadataBaseline.authority
	if baseline == nil {
		return ErrMemberAuthority
	}
	claims := baseline.Claims()
	version, err := sink.metadata.ReplicationVersion()
	if err != nil || version != 2 || claims.ClusterID != r.clusterID || claims.Incarnation != r.incarnation || baseline.CurrentPrimary() != r.primaryNodeID || baseline.CurrentTerm() != r.term || !slices.Contains(claims.Members, r.nodeID) {
		return errors.Join(ErrMemberAuthority, err)
	}
	landing := sink.metadataBaseline
	if landing.source == nil || landing.successor != sink.metadata {
		return ErrMemberAuthority
	}
	landing.source.mu.RLock()
	active := !landing.source.closed && landing.source.metadataBaselineActive && landing.source.options.MetadataBaseline != nil && landing.source.options.MetadataBaseline.Authority == baseline
	landing.source.mu.RUnlock()
	if !active {
		return ErrMemberAuthority
	}
	if err := landing.source.ValidateActivatedMetadataBaselineOrdering(r.journal); err != nil {
		return errors.Join(ErrMemberAuthority, err)
	}
	r.startupSink.native = sink
	return nil
}
