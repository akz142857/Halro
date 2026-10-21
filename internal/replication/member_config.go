package replication

import (
	"errors"
	"sort"
)

// ValidateMemberConfiguration prevents an authenticated membership snapshot
// from being silently rewritten by config. Membership changes are protocol
// operations; editing YAML may update neither identity nor peers.
func ValidateMemberConfiguration(state MemberState, clusterID, nodeID string, peers []StatePeer) error {
	if err := state.Validate(); err != nil {
		return err
	}
	if state.ClusterID != clusterID {
		return errors.New("replication.cluster_id does not match authenticated member state")
	}
	if state.NodeID != nodeID {
		return errors.New("replication.node_id does not match authenticated member state")
	}
	configured := append([]StatePeer(nil), peers...)
	authenticated := append([]StatePeer(nil), state.Peers...)
	sort.Slice(configured, func(i, j int) bool { return configured[i].Name < configured[j].Name })
	sort.Slice(authenticated, func(i, j int) bool { return authenticated[i].Name < authenticated[j].Name })
	if len(configured) != len(authenticated) {
		return errors.New("replication.peers does not match authenticated member state")
	}
	for index := range configured {
		if configured[index] != authenticated[index] {
			return errors.New("replication.peers does not match authenticated member state")
		}
	}
	return nil
}
