package replication

import (
	"strings"
	"testing"
)

func TestAuthenticatedMemberSnapshotCannotBeChangedByConfiguration(t *testing.T) {
	state := validMemberState()
	configured := []StatePeer{state.Peers[1], state.Peers[0]}
	if err := ValidateMemberConfiguration(state, state.ClusterID, state.NodeID, configured); err != nil {
		t.Fatalf("same unordered peer set rejected: %v", err)
	}
	tests := []struct {
		name      string
		clusterID string
		nodeID    string
		peers     []StatePeer
		want      string
	}{
		{name: "cluster", clusterID: "other", nodeID: state.NodeID, peers: configured, want: "cluster_id"},
		{name: "node", clusterID: state.ClusterID, nodeID: "halro-9", peers: configured, want: "node_id"},
		{name: "missing peer", clusterID: state.ClusterID, nodeID: state.NodeID, peers: configured[:1], want: "peers"},
		{name: "address", clusterID: state.ClusterID, nodeID: state.NodeID, peers: []StatePeer{
			{Name: configured[0].Name, Address: "changed.internal:9910", SPKISHA256: configured[0].SPKISHA256}, configured[1],
		}, want: "peers"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateMemberConfiguration(state, test.clusterID, test.nodeID, test.peers); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("configuration mismatch error=%v", err)
			}
		})
	}
}
