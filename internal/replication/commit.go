package replication

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

const commitNoticeFixedBytes = 44

var commitNoticeMagic = [8]byte{'H', 'L', 'R', 'C', 'F', 'M', '0', '1'}

type CommitNotice struct {
	ClusterID      string
	Incarnation    string
	NodeID         string
	Term           uint64
	ConfirmedIndex uint64
}

func (n CommitNotice) Validate() error {
	for name, value := range map[string]string{"cluster_id": n.ClusterID, "incarnation": n.Incarnation, "node_id": n.NodeID} {
		if len(value) == 0 || len(value) > MaxIdentityBytes {
			return fmt.Errorf("commit notice %s must be 1-%d bytes", name, MaxIdentityBytes)
		}
	}
	if n.Term == 0 || n.ConfirmedIndex == 0 {
		return errors.New("commit notice term and confirmed index must be positive")
	}
	return nil
}

func (n CommitNotice) MarshalBinary() ([]byte, error) {
	if err := n.Validate(); err != nil {
		return nil, err
	}
	bodyLength := commitNoticeFixedBytes + len(n.ClusterID) + len(n.Incarnation) + len(n.NodeID)
	encoded := make([]byte, 4+bodyLength)
	binary.BigEndian.PutUint32(encoded[:4], uint32(bodyLength))
	body := encoded[4:]
	copy(body[:8], commitNoticeMagic[:])
	binary.BigEndian.PutUint16(body[8:10], ProtocolVersion)
	// body[10:12] is reserved.
	binary.BigEndian.PutUint64(body[12:20], n.Term)
	binary.BigEndian.PutUint64(body[20:28], n.ConfirmedIndex)
	binary.BigEndian.PutUint16(body[28:30], uint16(len(n.ClusterID)))
	binary.BigEndian.PutUint16(body[30:32], uint16(len(n.Incarnation)))
	binary.BigEndian.PutUint16(body[32:34], uint16(len(n.NodeID)))
	// body[34:44] is reserved.
	copy(body[commitNoticeFixedBytes:], n.ClusterID)
	copy(body[commitNoticeFixedBytes+len(n.ClusterID):], n.Incarnation)
	copy(body[commitNoticeFixedBytes+len(n.ClusterID)+len(n.Incarnation):], n.NodeID)
	return encoded, nil
}

func UnmarshalCommitNotice(encoded []byte) (CommitNotice, error) {
	if len(encoded) < 4+commitNoticeFixedBytes {
		return CommitNotice{}, errors.New("commit notice is truncated")
	}
	declared := int(binary.BigEndian.Uint32(encoded[:4]))
	if declared != len(encoded)-4 || declared > commitNoticeFixedBytes+3*MaxIdentityBytes {
		return CommitNotice{}, errors.New("commit notice length is not canonical")
	}
	body := encoded[4:]
	if !bytes.Equal(body[:8], commitNoticeMagic[:]) {
		return CommitNotice{}, errors.New("commit notice magic is invalid")
	}
	if version := binary.BigEndian.Uint16(body[8:10]); version != ProtocolVersion {
		return CommitNotice{}, fmt.Errorf("unsupported commit notice version %d", version)
	}
	if !allZero(body[10:12]) || !allZero(body[34:44]) {
		return CommitNotice{}, errors.New("commit notice reserved field is non-zero")
	}
	clusterLength := int(binary.BigEndian.Uint16(body[28:30]))
	incarnationLength := int(binary.BigEndian.Uint16(body[30:32]))
	nodeLength := int(binary.BigEndian.Uint16(body[32:34]))
	if clusterLength == 0 || clusterLength > MaxIdentityBytes || incarnationLength == 0 || incarnationLength > MaxIdentityBytes ||
		nodeLength == 0 || nodeLength > MaxIdentityBytes || commitNoticeFixedBytes+clusterLength+incarnationLength+nodeLength != len(body) {
		return CommitNotice{}, errors.New("commit notice identity lengths are invalid")
	}
	offset := commitNoticeFixedBytes
	take := func(length int) string {
		value := string(body[offset : offset+length])
		offset += length
		return value
	}
	notice := CommitNotice{
		Term: binary.BigEndian.Uint64(body[12:20]), ConfirmedIndex: binary.BigEndian.Uint64(body[20:28]),
		ClusterID: take(clusterLength), Incarnation: take(incarnationLength), NodeID: take(nodeLength),
	}
	if err := notice.Validate(); err != nil {
		return CommitNotice{}, err
	}
	return notice, nil
}
