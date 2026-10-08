package replication

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

// PhysicalCreditProtocolVersion requires explicit mutual negotiation. Its
// control records describe original prepaid IO promises, never confirmation,
// role, root selection or a replacement for genuine durable acknowledgements.
const PhysicalCreditProtocolVersion uint16 = 3
const MaxPhysicalCreditRecordBytes = 1024

var physicalCreditMagic = [8]byte{'H', 'L', 'R', 'C', 'R', 'E', '0', '3'}

type PhysicalCreditOperation uint8

const (
	PhysicalCreditWindow PhysicalCreditOperation = iota + 1
	PhysicalCreditReserve
	PhysicalCreditReserved
	PhysicalCreditRelease
	PhysicalCreditReleased
	PhysicalCreditRejected
)

// Four native slots are Ledger, Audit, Governance, Metadata in the same
// ordinal order as authenticated native source cursors. Usage/Directory and
// Ordering are independent IO obligations; a native frame count cannot supply
// their targets by construction.
type PhysicalCreditCost struct {
	NativeBytes     [4]uint64
	NativeRecords   [4]uint32
	COWRecords      [4]uint32
	UsageFrames     uint32
	DirectoryRolls  uint32
	OrderingRecords uint32
}

const physicalCreditCostBytes = 4*(8+4+4) + 3*4

func (c PhysicalCreditCost) IsZero() bool { return c == PhysicalCreditCost{} }
func (c PhysicalCreditCost) Covers(other PhysicalCreditCost) bool {
	for i := 0; i < 4; i++ {
		if c.NativeBytes[i] < other.NativeBytes[i] || c.NativeRecords[i] < other.NativeRecords[i] || c.COWRecords[i] < other.COWRecords[i] {
			return false
		}
	}
	return c.UsageFrames >= other.UsageFrames && c.DirectoryRolls >= other.DirectoryRolls && c.OrderingRecords >= other.OrderingRecords
}
func (c PhysicalCreditCost) Subtract(other PhysicalCreditCost) (PhysicalCreditCost, error) {
	if !c.Covers(other) {
		return c, ErrPhysicalCredit
	}
	for i := 0; i < 4; i++ {
		c.NativeBytes[i] -= other.NativeBytes[i]
		c.NativeRecords[i] -= other.NativeRecords[i]
		c.COWRecords[i] -= other.COWRecords[i]
	}
	c.UsageFrames -= other.UsageFrames
	c.DirectoryRolls -= other.DirectoryRolls
	c.OrderingRecords -= other.OrderingRecords
	return c, nil
}

var ErrPhysicalCredit = errors.New("original Replica physical IO promise unavailable or changed")

type PhysicalCreditRecord struct {
	Operation PhysicalCreditOperation
	// Status is a genuine observation only. The ordinary ACK path must still
	// call the original coordinator before it can advance confirmation.
	Status         Acknowledgement
	BaselineDigest [32]byte
	CommonIndex    uint64
	CommonMAC      [32]byte
	WindowID       [32]byte
	TicketID       [16]byte
	TicketSerial   uint64
	RequestDigest  [32]byte
	// Cost is requested/reserved or actually consumed at known completion.
	// Available is a post-promise observation, never a refundable allowance.
	Cost      PhysicalCreditCost
	Available PhysicalCreditCost
}

const physicalCreditFixedBytes = 8 + 2 + 1 + 1 + 2 + 32 + 8 + 32 + 32 + 16 + 8 + 32 + 2*physicalCreditCostBytes

func (r PhysicalCreditRecord) Validate() error {
	if err := r.Status.Validate(); err != nil {
		return err
	}
	if r.Operation < PhysicalCreditWindow || r.Operation > PhysicalCreditRejected || r.BaselineDigest == ([32]byte{}) || r.WindowID == ([32]byte{}) || r.CommonIndex == 0 || r.CommonIndex > r.Status.AppliedIndex || r.CommonMAC == ([32]byte{}) {
		return ErrPhysicalCredit
	}
	if r.Operation == PhysicalCreditWindow {
		if r.TicketID != ([16]byte{}) || r.RequestDigest != ([32]byte{}) || !r.Cost.IsZero() {
			return ErrPhysicalCredit
		}
	} else if r.TicketID == ([16]byte{}) || r.RequestDigest == ([32]byte{}) || r.TicketSerial == 0 || binary.BigEndian.Uint64(r.TicketID[8:]) != r.TicketSerial || !bytes.Equal(r.TicketID[:8], r.WindowID[:8]) {
		return ErrPhysicalCredit
	}
	if (r.Operation == PhysicalCreditReserve || r.Operation == PhysicalCreditReserved) && r.Cost.OrderingRecords == 0 {
		return ErrPhysicalCredit
	}
	if r.Operation == PhysicalCreditReserve && !r.Available.IsZero() {
		return ErrPhysicalCredit
	}
	return nil
}
func encodePhysicalCreditCost(out []byte, c PhysicalCreditCost) {
	off := 0
	for i := 0; i < 4; i++ {
		binary.BigEndian.PutUint64(out[off:], c.NativeBytes[i])
		off += 8
		binary.BigEndian.PutUint32(out[off:], c.NativeRecords[i])
		off += 4
		binary.BigEndian.PutUint32(out[off:], c.COWRecords[i])
		off += 4
	}
	for _, n := range []uint32{c.UsageFrames, c.DirectoryRolls, c.OrderingRecords} {
		binary.BigEndian.PutUint32(out[off:], n)
		off += 4
	}
}
func decodePhysicalCreditCost(in []byte) (c PhysicalCreditCost) {
	off := 0
	for i := 0; i < 4; i++ {
		c.NativeBytes[i] = binary.BigEndian.Uint64(in[off:])
		off += 8
		c.NativeRecords[i] = binary.BigEndian.Uint32(in[off:])
		off += 4
		c.COWRecords[i] = binary.BigEndian.Uint32(in[off:])
		off += 4
	}
	c.UsageFrames = binary.BigEndian.Uint32(in[off:])
	off += 4
	c.DirectoryRolls = binary.BigEndian.Uint32(in[off:])
	off += 4
	c.OrderingRecords = binary.BigEndian.Uint32(in[off:])
	return
}
func (r PhysicalCreditRecord) MarshalBinary() ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	ack, err := r.Status.MarshalBinary()
	if err != nil {
		return nil, err
	}
	body := physicalCreditFixedBytes + len(ack)
	if body+4 > MaxPhysicalCreditRecordBytes {
		return nil, ErrPhysicalCredit
	}
	out := make([]byte, 4+body)
	binary.BigEndian.PutUint32(out, uint32(body))
	b := out[4:]
	copy(b, physicalCreditMagic[:])
	binary.BigEndian.PutUint16(b[8:], PhysicalCreditProtocolVersion)
	b[10] = byte(r.Operation)
	binary.BigEndian.PutUint16(b[12:], uint16(len(ack)))
	off := 14
	for _, raw := range [][]byte{r.BaselineDigest[:]} {
		copy(b[off:], raw)
		off += len(raw)
	}
	binary.BigEndian.PutUint64(b[off:], r.CommonIndex)
	off += 8
	for _, raw := range [][]byte{r.CommonMAC[:], r.WindowID[:], r.TicketID[:]} {
		copy(b[off:], raw)
		off += len(raw)
	}
	binary.BigEndian.PutUint64(b[off:], r.TicketSerial)
	off += 8
	copy(b[off:], r.RequestDigest[:])
	off += 32
	encodePhysicalCreditCost(b[off:], r.Cost)
	off += physicalCreditCostBytes
	encodePhysicalCreditCost(b[off:], r.Available)
	off += physicalCreditCostBytes
	copy(b[off:], ack)
	return out, nil
}
func UnmarshalPhysicalCreditRecord(raw []byte) (r PhysicalCreditRecord, err error) {
	if len(raw) < 4+physicalCreditFixedBytes+4+ackFixedBytes || len(raw) > MaxPhysicalCreditRecordBytes || int(binary.BigEndian.Uint32(raw)) != len(raw)-4 {
		return r, ErrPhysicalCredit
	}
	b := raw[4:]
	if !bytes.Equal(b[:8], physicalCreditMagic[:]) || binary.BigEndian.Uint16(b[8:]) != PhysicalCreditProtocolVersion || b[11] != 0 {
		return r, ErrPhysicalCredit
	}
	size := int(binary.BigEndian.Uint16(b[12:]))
	if physicalCreditFixedBytes+size != len(b) {
		return r, ErrPhysicalCredit
	}
	r.Operation = PhysicalCreditOperation(b[10])
	off := 14
	copy(r.BaselineDigest[:], b[off:])
	off += 32
	r.CommonIndex = binary.BigEndian.Uint64(b[off:])
	off += 8
	copy(r.CommonMAC[:], b[off:])
	off += 32
	copy(r.WindowID[:], b[off:])
	off += 32
	copy(r.TicketID[:], b[off:])
	off += 16
	r.TicketSerial = binary.BigEndian.Uint64(b[off:])
	off += 8
	copy(r.RequestDigest[:], b[off:])
	off += 32
	r.Cost = decodePhysicalCreditCost(b[off:])
	off += physicalCreditCostBytes
	r.Available = decodePhysicalCreditCost(b[off:])
	off += physicalCreditCostBytes
	r.Status, err = UnmarshalAcknowledgement(b[off:])
	if err != nil {
		return PhysicalCreditRecord{}, err
	}
	if err = r.Validate(); err != nil {
		return PhysicalCreditRecord{}, err
	}
	canonical, err := r.MarshalBinary()
	if err != nil || !bytes.Equal(canonical, raw) {
		return PhysicalCreditRecord{}, fmt.Errorf("noncanonical physical credit: %w", errors.Join(ErrPhysicalCredit, err))
	}
	return r, nil
}

// ValidatePeer binds this control observation to the original authenticated
// data session. Protocol support in Hello is necessary but never supplies IO
// credit; only a qualified source producer and persisted original promise do.
func (r PhysicalCreditRecord) ValidatePeer(peer Hello, localRole Role) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if peer.Protocol != (VersionRange{Current: PhysicalCreditProtocolVersion, Minimum: PhysicalCreditProtocolVersion, Maximum: PhysicalCreditProtocolVersion}) || peer.Metadata != (VersionRange{Current: 2, Minimum: 2, Maximum: 2}) || peer.MetadataBaselineAuthority != r.BaselineDigest || peer.ClusterID != r.Status.ClusterID || peer.Incarnation != r.Status.Incarnation || peer.NodeID != r.Status.NodeID || peer.Term != r.Status.Term || peer.PromisedTerm != peer.Term {
		return ErrPhysicalCredit
	}
	if localRole == RolePrimary {
		if peer.Role != RoleReplica || (r.Operation != PhysicalCreditWindow && r.Operation != PhysicalCreditReserved && r.Operation != PhysicalCreditReleased && r.Operation != PhysicalCreditRejected) {
			return ErrPhysicalCredit
		}
	} else if localRole == RoleReplica {
		if peer.Role != RolePrimary || (r.Operation != PhysicalCreditReserve && r.Operation != PhysicalCreditRelease) {
			return ErrPhysicalCredit
		}
	} else {
		return ErrPhysicalCredit
	}
	return nil
}
