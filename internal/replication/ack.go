package replication

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
)

const ackFixedBytes = 60

var ackMagic = [8]byte{'H', 'L', 'R', 'A', 'C', 'K', '0', '1'}

type Acknowledgement struct {
	ClusterID    string
	Incarnation  string
	NodeID       string
	Index        uint64
	Term         uint64
	DurableIndex uint64
	AppliedIndex uint64
}

func (a Acknowledgement) Validate() error {
	for name, value := range map[string]string{"cluster_id": a.ClusterID, "incarnation": a.Incarnation, "node_id": a.NodeID} {
		if len(value) == 0 || len(value) > MaxIdentityBytes {
			return fmt.Errorf("acknowledgement %s must be 1-%d bytes", name, MaxIdentityBytes)
		}
	}
	if a.Index == 0 || a.Term == 0 || a.DurableIndex != a.Index || a.AppliedIndex > a.DurableIndex {
		return errors.New("acknowledgement indexes or term are invalid")
	}
	return nil
}

func (a Acknowledgement) MarshalBinary() ([]byte, error) {
	if err := a.Validate(); err != nil {
		return nil, err
	}
	bodyLength := ackFixedBytes + len(a.ClusterID) + len(a.Incarnation) + len(a.NodeID)
	encoded := make([]byte, 4+bodyLength)
	binary.BigEndian.PutUint32(encoded[:4], uint32(bodyLength))
	body := encoded[4:]
	copy(body[:8], ackMagic[:])
	binary.BigEndian.PutUint16(body[8:10], ProtocolVersion)
	// body[10:12] is reserved.
	binary.BigEndian.PutUint64(body[12:20], a.Index)
	binary.BigEndian.PutUint64(body[20:28], a.Term)
	binary.BigEndian.PutUint64(body[28:36], a.DurableIndex)
	binary.BigEndian.PutUint64(body[36:44], a.AppliedIndex)
	binary.BigEndian.PutUint16(body[44:46], uint16(len(a.ClusterID)))
	binary.BigEndian.PutUint16(body[46:48], uint16(len(a.Incarnation)))
	binary.BigEndian.PutUint16(body[48:50], uint16(len(a.NodeID)))
	// body[50:60] is reserved for version-1 extension and must remain zero.
	copy(body[ackFixedBytes:], a.ClusterID)
	copy(body[ackFixedBytes+len(a.ClusterID):], a.Incarnation)
	copy(body[ackFixedBytes+len(a.ClusterID)+len(a.Incarnation):], a.NodeID)
	return encoded, nil
}

func UnmarshalAcknowledgement(encoded []byte) (Acknowledgement, error) {
	if len(encoded) < 4+ackFixedBytes {
		return Acknowledgement{}, errors.New("acknowledgement is truncated")
	}
	declared := int(binary.BigEndian.Uint32(encoded[:4]))
	if declared != len(encoded)-4 || declared > ackFixedBytes+3*MaxIdentityBytes {
		return Acknowledgement{}, errors.New("acknowledgement length is not canonical")
	}
	body := encoded[4:]
	if !bytes.Equal(body[:8], ackMagic[:]) {
		return Acknowledgement{}, errors.New("acknowledgement magic is invalid")
	}
	if version := binary.BigEndian.Uint16(body[8:10]); version != ProtocolVersion {
		return Acknowledgement{}, fmt.Errorf("unsupported acknowledgement version %d", version)
	}
	if !allZero(body[10:12]) || !allZero(body[50:60]) {
		return Acknowledgement{}, errors.New("acknowledgement reserved field is non-zero")
	}
	clusterLength := int(binary.BigEndian.Uint16(body[44:46]))
	incarnationLength := int(binary.BigEndian.Uint16(body[46:48]))
	nodeLength := int(binary.BigEndian.Uint16(body[48:50]))
	if clusterLength == 0 || clusterLength > MaxIdentityBytes || incarnationLength == 0 || incarnationLength > MaxIdentityBytes ||
		nodeLength == 0 || nodeLength > MaxIdentityBytes || ackFixedBytes+clusterLength+incarnationLength+nodeLength != len(body) {
		return Acknowledgement{}, errors.New("acknowledgement identity lengths are invalid")
	}
	offset := ackFixedBytes
	take := func(length int) string {
		value := string(body[offset : offset+length])
		offset += length
		return value
	}
	ack := Acknowledgement{
		Index: binary.BigEndian.Uint64(body[12:20]), Term: binary.BigEndian.Uint64(body[20:28]),
		DurableIndex: binary.BigEndian.Uint64(body[28:36]), AppliedIndex: binary.BigEndian.Uint64(body[36:44]),
		ClusterID: take(clusterLength), Incarnation: take(incarnationLength), NodeID: take(nodeLength),
	}
	if err := ack.Validate(); err != nil {
		return Acknowledgement{}, err
	}
	return ack, nil
}

func allZero(value []byte) bool {
	for _, item := range value {
		if item != 0 {
			return false
		}
	}
	return true
}

type pendingConfirmation struct {
	kind Kind
	acks map[string]struct{}
}

// ConfirmationTracker advances only a contiguous prefix acknowledged by at
// least one configured Replica. With two configured peers this is a majority
// of a three-member group; with one peer it is both members.
type ConfirmationTracker struct {
	mu             sync.Mutex
	clusterID      string
	incarnation    string
	term           uint64
	peers          map[string]struct{}
	confirmed      uint64
	lastRegistered uint64
	anchorIndex    uint64
	pending        map[uint64]*pendingConfirmation
}

func NewConfirmationTracker(clusterID, incarnation string, term, confirmed uint64, peers []string, termEstablished bool) (*ConfirmationTracker, error) {
	if len(clusterID) == 0 || len(clusterID) > MaxIdentityBytes || len(incarnation) == 0 || len(incarnation) > MaxIdentityBytes || term == 0 {
		return nil, errors.New("confirmation tracker identity or term is invalid")
	}
	if len(peers) < 1 || len(peers) > 2 {
		return nil, errors.New("confirmation tracker requires one or two peers")
	}
	peerSet := make(map[string]struct{}, len(peers))
	for _, peer := range peers {
		if len(peer) == 0 || len(peer) > MaxIdentityBytes {
			return nil, errors.New("confirmation tracker peer is invalid")
		}
		if _, exists := peerSet[peer]; exists {
			return nil, errors.New("confirmation tracker peer is duplicated")
		}
		peerSet[peer] = struct{}{}
	}
	tracker := &ConfirmationTracker{
		clusterID: clusterID, incarnation: incarnation, term: term, peers: peerSet,
		confirmed: confirmed, lastRegistered: confirmed, pending: make(map[uint64]*pendingConfirmation),
	}
	if termEstablished {
		if confirmed == 0 {
			return nil, errors.New("an established term requires a confirmed leadership anchor")
		}
		tracker.anchorIndex = confirmed
	}
	return tracker, nil
}

func (t *ConfirmationTracker) Register(frame Frame) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := frame.Validate(); err != nil {
		return err
	}
	if frame.ClusterID != t.clusterID || frame.Incarnation != t.incarnation || frame.Term != t.term {
		return errors.New("registered frame identity or term does not match the confirmation tracker")
	}
	if frame.Index != t.lastRegistered+1 {
		return fmt.Errorf("registered frame index %d does not continue %d", frame.Index, t.lastRegistered)
	}
	if t.anchorIndex == 0 {
		if frame.Kind != KindLeadershipEstablished {
			return errors.New("the first frame in a term must establish leadership")
		}
		t.anchorIndex = frame.Index
	} else if frame.Kind == KindLeadershipEstablished {
		return errors.New("a term cannot contain a second leadership anchor")
	}
	t.pending[frame.Index] = &pendingConfirmation{kind: frame.Kind, acks: make(map[string]struct{})}
	t.lastRegistered = frame.Index
	return nil
}

// Acknowledge returns the greatest newly confirmed contiguous index. An ACK
// for an unknown/future record, wrong term, wrong identity, or non-member is a
// protocol error rather than an ignorable hint.
func (t *ConfirmationTracker) Acknowledge(ack Acknowledgement) (uint64, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := ack.Validate(); err != nil {
		return t.confirmed, err
	}
	if ack.ClusterID != t.clusterID || ack.Incarnation != t.incarnation || ack.Term != t.term {
		return t.confirmed, errors.New("acknowledgement identity or term does not match the active tenure")
	}
	if _, ok := t.peers[ack.NodeID]; !ok {
		return t.confirmed, errors.New("acknowledgement came from an unconfigured peer")
	}
	if ack.Index <= t.confirmed {
		return t.confirmed, nil
	}
	if ack.Index > t.lastRegistered {
		return t.confirmed, errors.New("acknowledgement refers to an unknown future index")
	}
	for index := t.confirmed + 1; index <= ack.Index; index++ {
		pending, ok := t.pending[index]
		if !ok {
			return t.confirmed, errors.New("acknowledgement crosses an unregistered index")
		}
		pending.acks[ack.NodeID] = struct{}{}
	}
	for {
		next, ok := t.pending[t.confirmed+1]
		if !ok || len(next.acks) < 1 {
			break
		}
		if t.confirmed < t.anchorIndex && t.confirmed+1 != t.anchorIndex {
			break
		}
		delete(t.pending, t.confirmed+1)
		t.confirmed++
	}
	return t.confirmed, nil
}

func (t *ConfirmationTracker) Confirmed() uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.confirmed
}
