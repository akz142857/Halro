package replication

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/akz142857/Halro/internal/resourcebudget"
)

type receivedBeforeNativeSink struct{ visit func(Frame) error }

func (s receivedBeforeNativeSink) Persist(frame Frame) error { return s.visit(frame) }

// This component test proves the original Receiver's first-write ordering
// against its actual prepaid Member/Ordering descriptors. The complete App
// native/source/root permission is covered separately by the actual TLS child.
func TestReplicaPhysicalAcceptedReceiptPrecedesNativeAndMember(t *testing.T) {
	p, j, m, fee := physicalPublicationFixture(t)
	p.state.Role = RoleReplica
	if err := WriteState(p.path, p.state, p.key[:]); err != nil {
		t.Fatal(err)
	}
	o := requireActualPhysicalPublication(t, p, j, m, fee, 4)
	old := p.Snapshot()
	writes := 0
	sink := receivedBeforeNativeSink{visit: func(frame Frame) error {
		raw, err := readPhysicalReceipt(p.path + ".physical.json")
		if err != nil {
			return err
		}
		intent, err := decodeReceivedIntent(raw, frame.Index, o.receipt, p.key[:])
		if err != nil {
			return err
		}
		actual := p.Snapshot()
		digest, err := memberReceiptDigest(actual, p.key[:])
		if err != nil {
			return err
		}
		index, _, mac := j.Head()
		record, err := UnmarshalOrderingRecord(intent.Record, j.key[:], old.ClusterID, old.Incarnation, mac)
		if err != nil || index+1 != frame.Index || record.Index != frame.Index || intent.Before != digest || intent.Primary != "halro-1" {
			return errors.Join(ErrMemberAuthority, err)
		}
		writes++
		return nil
	}}
	receiver, err := NewReplicaReceiver(old.ClusterID, old.Incarnation, old.NodeID, "halro-1", old.Term, old.PromisedTerm, old.ConfirmedIndex, old.AppliedIndex, j, sink, ReplicaReceiverOptions{PersistState: p.PublishReplica, Projection: old.Projection})
	if err != nil {
		t.Fatal(err)
	}
	frame := Frame{Kind: KindLeadershipEstablished, ClusterID: old.ClusterID, Incarnation: old.Incarnation, Term: old.Term, Index: 1}
	encoded, err := frame.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = receiver.Receive(encoded); err != nil {
		t.Fatal(err)
	}
	if writes != 1 || p.Snapshot().DurableIndex != 1 || p.Snapshot().ConfirmedIndex != 1 || p.Snapshot().AppliedIndex != 0 {
		t.Fatal("original persistence order changed", writes, p.Snapshot())
	}
	before, err := os.ReadFile(p.path + ".physical.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = receiver.Receive(encoded); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(p.path + ".physical.json")
	if err != nil || !bytes.Equal(before, after) || writes != 1 {
		t.Fatal("duplicate consumed/replaced accepted proof", writes, err)
	}
	if err = o.ValidateParents(context.Background(), p, j, m); err != nil {
		t.Fatal(err)
	}
}

func TestReplicaReceivedReceiptBoundedSlotsAndIndependentIdentity(t *testing.T) {
	key := bytes.Repeat([]byte{91}, 32)
	r := physicalPublicationReceipt{Version: 2, ClusterID: "cluster", Incarnation: "inc", NodeID: "replica", OrderingEnd: 4096, Ordering: resourcebudget.PhysicalIdentity{Inode: 1}, Receipt: resourcebudget.PhysicalIdentity{Inode: 2}}
	var err error
	r.MAC, err = physicalReceiptMAC(r, key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := encodePhysicalPublicationReceipt(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 8192 {
		t.Fatal("receipt grew beyond original fixed fee", len(raw))
	}
	var decoded physicalPublicationReceipt
	if err = decodePhysicalPublicationReceipt(raw, &decoded); err != nil || !hmac.Equal(decoded.MAC, r.MAC) {
		t.Fatal(decoded, err)
	}
	put := func(index uint64, record []byte) {
		intent := receivedOrderingIntent{Before: [32]byte{9}, Primary: "primary", Record: record}
		intent.MAC, err = receivedIntentMAC(intent, r, key)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(intent)
		if err != nil {
			t.Fatal(err)
		}
		offset := receiveReceiptHeaderBytes + int(index%2)*receiveReceiptSlotBytes
		slot := raw[offset : offset+receiveReceiptSlotBytes]
		binary.LittleEndian.PutUint32(slot, uint32(len(encoded)))
		copy(slot[4:], encoded)
	}
	put(1, []byte("original accepted exact record"))
	first, err := decodeReceivedIntent(raw, 1, r, key)
	if err != nil {
		t.Fatal(err)
	}
	put(2, []byte("next accepted exact record"))
	offset := receiveReceiptHeaderBytes
	raw[offset+8] ^= 0x10 // a torn second slot must not destroy the first proof/header
	if _, err = decodeReceivedIntent(raw, 2, r, key); err == nil {
		t.Fatal("torn candidate slot accepted")
	}
	retained, err := decodeReceivedIntent(raw, 1, r, key)
	if err != nil || !bytes.Equal(retained.Record, first.Record) {
		t.Fatal(retained, err)
	}
	if err = decodePhysicalPublicationReceipt(raw, &decoded); err != nil || decoded.Receipt != r.Receipt || !hmac.Equal(decoded.MAC, r.MAC) {
		t.Fatal("slot write changed static IO identity", decoded, err)
	}
	foreign := r
	foreign.NodeID = "foreign"
	foreign.MAC, err = physicalReceiptMAC(foreign, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = decodeReceivedIntent(raw, 1, foreign, key); err == nil {
		t.Fatal("foreign MAC-valid node receipt borrowed accepted frame")
	}
	for _, size := range []int{0, 2048, 8191, 8193} {
		if _, err = decodeReceivedIntent(make([]byte, size), 1, r, key); err == nil {
			t.Fatal("nonfixed receipt accepted", size)
		}
	}
	binary.LittleEndian.PutUint32(raw[receiveReceiptHeaderBytes+receiveReceiptSlotBytes:], ^uint32(0))
	if _, err = decodeReceivedIntent(raw, 1, r, key); err == nil {
		t.Fatal("unbounded slot allocation accepted")
	}
}

func TestPhysicalPublicationLegacyReceiptMACRemainsExact(t *testing.T) {
	type legacy struct {
		Version                        int
		ClusterID, Incarnation, NodeID string
		State                          [2]resourcebudget.PhysicalIdentity
		Ordering                       resourcebudget.PhysicalIdentity
		OrderingEnd                    int64
		MAC                            []byte
	}
	old := legacy{Version: 1, ClusterID: "cluster", Incarnation: "inc", NodeID: "replica", OrderingEnd: 4096}
	raw, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{13}, 32)
	h := hmac.New(sha256.New, key)
	h.Write([]byte("halro:ha:physical-publication:v1\x00"))
	h.Write(raw)
	r := physicalPublicationReceipt{Version: old.Version, ClusterID: old.ClusterID, Incarnation: old.Incarnation, NodeID: old.NodeID, State: old.State, Ordering: old.Ordering, OrderingEnd: old.OrderingEnd}
	actual, err := physicalReceiptMAC(r, key)
	if err != nil || !hmac.Equal(actual, h.Sum(nil)) {
		t.Fatal("v2 field changed legacy immutable receipt MAC", err)
	}
}
