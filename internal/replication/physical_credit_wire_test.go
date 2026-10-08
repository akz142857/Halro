package replication

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func testPhysicalCreditRecord() PhysicalCreditRecord {
	return PhysicalCreditRecord{Operation: PhysicalCreditReserved, Status: Acknowledgement{ClusterID: "group", Incarnation: "incarnation", NodeID: "replica", Index: 9, Term: 7, DurableIndex: 9, AppliedIndex: 9}, BaselineDigest: [32]byte{1}, CommonIndex: 8, CommonMAC: [32]byte{2}, WindowID: [32]byte{3}, TicketID: [16]byte{3, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}, TicketSerial: 1, RequestDigest: [32]byte{9}, Cost: PhysicalCreditCost{NativeBytes: [4]uint64{1024, 512, 512, 2048}, NativeRecords: [4]uint32{11, 1, 1, 6}, COWRecords: [4]uint32{11, 0, 1, 6}, UsageFrames: 11, OrderingRecords: 19}, Available: PhysicalCreditCost{NativeRecords: [4]uint32{21, 31, 31, 26}, OrderingRecords: 13}}
}
func TestPhysicalCreditWireExactBoundedAndCannotSupplyACK(t *testing.T) {
	r := testPhysicalCreditRecord()
	raw, err := r.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := UnmarshalPhysicalCreditRecord(raw)
	if err != nil || decoded != r {
		t.Fatal(decoded, err)
	}
	if len(raw) > MaxPhysicalCreditRecordBytes {
		t.Fatal(len(raw))
	}
	if _, err = UnmarshalAcknowledgement(raw); err == nil {
		t.Fatal("credit wrapper advanced ordinary confirmation")
	}
	candidates := [][]byte{append(bytes.Clone(raw), 0), raw[:len(raw)-1], bytes.Clone(raw), bytes.Clone(raw), bytes.Clone(raw)}
	candidates[2][4+11] = 1
	binary.BigEndian.PutUint16(candidates[3][4+8:], 2)
	candidates[4][4+10] = 99
	for _, candidate := range candidates {
		if _, err = UnmarshalPhysicalCreditRecord(candidate); err == nil {
			t.Fatal("malformed/old-protocol record accepted")
		}
	}
	r.Operation = PhysicalCreditWindow
	r.TicketID = [16]byte{}
	r.TicketSerial = 0
	r.RequestDigest = [32]byte{}
	r.Cost = PhysicalCreditCost{}
	raw, err = r.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = UnmarshalPhysicalCreditRecord(raw); err != nil {
		t.Fatal(err)
	}
	r.CommonIndex = r.Status.AppliedIndex + 1
	if _, err = r.MarshalBinary(); err == nil {
		t.Fatal("unapplied common supplied IO promise")
	}
}
func TestPhysicalCreditSubtractionDoesNotWrapOrRefundOriginalPromise(t *testing.T) {
	c := PhysicalCreditCost{NativeBytes: [4]uint64{100}, NativeRecords: [4]uint32{32}, COWRecords: [4]uint32{32}, UsageFrames: 32, OrderingRecords: 32}
	future := PhysicalCreditCost{NativeBytes: [4]uint64{50}, NativeRecords: [4]uint32{11}, COWRecords: [4]uint32{11}, UsageFrames: 11, OrderingRecords: 11}
	left, err := c.Subtract(future)
	if err != nil {
		t.Fatal(err)
	}
	left, err = left.Subtract(future)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = left.Subtract(future); err == nil {
		t.Fatal("third terminal promise overdrew original finite bank")
	}
	if left.NativeRecords[0] != 10 || left.UsageFrames != 10 || left.OrderingRecords != 10 {
		t.Fatal(left)
	}
	tooLarge := PhysicalCreditCost{NativeBytes: [4]uint64{101}}
	if _, err = c.Subtract(tooLarge); err == nil {
		t.Fatal("unsigned byte capacity wrapped")
	}
}
func TestPhysicalCreditOnlyExplicitWire3AuthenticatedDirection(t *testing.T) {
	r := testPhysicalCreditRecord()
	peer := Hello{ClusterID: r.Status.ClusterID, Incarnation: r.Status.Incarnation, NodeID: r.Status.NodeID, Role: RoleReplica, Term: r.Status.Term, PromisedTerm: r.Status.Term, Protocol: VersionRange{Current: 3, Minimum: 3, Maximum: 3}, Metadata: VersionRange{Current: 2, Minimum: 2, Maximum: 2}, MetadataBaselineAuthority: r.BaselineDigest}
	if err := r.ValidatePeer(peer, RolePrimary); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Hello){func(h *Hello) { h.Protocol = VersionRange{Current: 2, Minimum: 2, Maximum: 2} }, func(h *Hello) { h.NodeID = "foreign" }, func(h *Hello) { h.MetadataBaselineAuthority[0] ^= 1 }, func(h *Hello) { h.PromisedTerm++ }, func(h *Hello) { h.Role = RolePrimary }} {
		foreign := peer
		mutate(&foreign)
		if err := r.ValidatePeer(foreign, RolePrimary); err == nil {
			t.Fatal("foreign or legacy peer supplied original promise", foreign)
		}
	}
	if err := r.ValidatePeer(peer, RoleReplica); err == nil {
		t.Fatal("reverse control direction accepted")
	}
}
