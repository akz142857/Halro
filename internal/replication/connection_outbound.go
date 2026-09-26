package replication

import (
	"errors"
	"fmt"
	"sort"
)

type PeerRecordSender interface {
	Send(peer string, encoded []byte) error
}

// ConnectionOutbound fans each frame/notice to every configured Replica while
// requiring only one successful enqueue: Primary plus one Replica is quorum in
// both supported topologies. The coordinator retains bytes until ACK/applied
// progress, so a disconnected peer is caught up by RetryPending after its
// session returns rather than turning a healthy three-member quorum off.
type ConnectionOutbound struct {
	peers  []string
	sender PeerRecordSender
}

func NewConnectionOutbound(peers []string, sender PeerRecordSender) (*ConnectionOutbound, error) {
	if len(peers) < 1 || len(peers) > 2 || sender == nil {
		return nil, errors.New("connection outbound requires one or two peers and a sender")
	}
	peers = append([]string(nil), peers...)
	sort.Strings(peers)
	for index, peer := range peers {
		if peer == "" || index > 0 && peer == peers[index-1] {
			return nil, errors.New("connection outbound peer identity is invalid or duplicated")
		}
	}
	return &ConnectionOutbound{peers: peers, sender: sender}, nil
}

func (o *ConnectionOutbound) QueueFrame(_ uint64, encoded []byte) error {
	return o.broadcast(encoded)
}

func (o *ConnectionOutbound) QueueCommitNotice(_ uint64, encoded []byte) error {
	return o.broadcast(encoded)
}

func (o *ConnectionOutbound) Acknowledge(string, uint64, uint64) {}

func (o *ConnectionOutbound) broadcast(encoded []byte) error {
	if len(encoded) == 0 {
		return errors.New("connection outbound record is empty")
	}
	var failures []error
	succeeded := 0
	for _, peer := range o.peers {
		if err := o.sender.Send(peer, append([]byte(nil), encoded...)); err != nil {
			failures = append(failures, fmt.Errorf("queue replication record for %s: %w", peer, err))
			continue
		}
		succeeded++
	}
	if succeeded > 0 {
		return nil
	}
	return errors.Join(failures...)
}
