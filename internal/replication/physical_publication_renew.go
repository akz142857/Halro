package replication

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

const physicalRenewStaticBytes = 1024
const physicalRenewSlotBytes = 512

type physicalRenewalWindow struct {
	Generation  uint64
	Term, Index uint64
	Head        [32]byte
	End         int64
	MAC         []byte
	Peer        *physicalPeerFutureState `json:",omitempty"`
}

func physicalRenewalMAC(w physicalRenewalWindow, r physicalPublicationReceipt, key []byte) ([]byte, error) {
	w.MAC = nil
	raw, err := json.Marshal(w)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("halro:physical:ordering-window:v1\x00"))
	mac.Write(r.MAC)
	mac.Write(raw)
	return mac.Sum(nil), nil
}
func encodePhysicalRenewal(w physicalRenewalWindow) ([]byte, error) {
	if w.Peer != nil {
		return encodePhysicalPeerControl(w)
	}
	raw, err := json.Marshal(w)
	if err != nil || len(raw) > physicalRenewSlotBytes-4 {
		return nil, errors.Join(ErrMemberAuthority, err)
	}
	slot := make([]byte, physicalRenewSlotBytes)
	binary.LittleEndian.PutUint32(slot, uint32(len(raw)))
	copy(slot[4:], raw)
	return slot, nil
}
func decodePhysicalRenewal(slot []byte, r physicalPublicationReceipt, key []byte) (physicalRenewalWindow, error) {
	var w physicalRenewalWindow
	if len(slot) != physicalRenewSlotBytes {
		return w, ErrMemberAuthority
	}
	if bytes.Equal(slot[:8], physicalPeerControlMagic[:]) {
		return decodePhysicalPeerControl(slot, r, key)
	}
	if bytes.Equal(slot, make([]byte, physicalRenewSlotBytes)) {
		return w, nil
	}
	size := binary.LittleEndian.Uint32(slot)
	if size == 0 || size > physicalRenewSlotBytes-4 {
		return w, ErrMemberAuthority
	}
	d := json.NewDecoder(bytes.NewReader(slot[4 : 4+size]))
	d.DisallowUnknownFields()
	if err := d.Decode(&w); err != nil {
		return physicalRenewalWindow{}, err
	}
	canonical, err := json.Marshal(w)
	if err != nil || !bytes.Equal(canonical, slot[4:4+size]) || w.Generation == 0 || w.Index == 0 || w.Term == 0 {
		return physicalRenewalWindow{}, errors.Join(ErrMemberAuthority, err)
	}
	mac, err := physicalRenewalMAC(w, r, key)
	if err != nil || !hmac.Equal(mac, w.MAC) {
		return physicalRenewalWindow{}, errors.Join(ErrMemberAuthority, err)
	}
	return w, nil
}
func (o *PhysicalPublication) validateRenewalLocked(w physicalRenewalWindow) error {
	p, j := o.publisher, o.journal
	if w.End < o.receipt.OrderingEnd || w.End > j.maxDiskBytes || w.Index > p.state.DurableIndex || w.Term > p.state.Term {
		return ErrMemberAuthority
	}
	record, err := j.recordLocked(w.Index)
	if err != nil || record.Term != w.Term || record.MAC != w.Head {
		return errors.Join(ErrMemberAuthority, err)
	}
	return nil
}
func (o *PhysicalPublication) restoreRenewalWindowLocked(ctx context.Context) error {
	if o.receipt.Version != 3 {
		return nil
	}
	if o.receiptFile == nil || o.receiptOwner == nil {
		return ErrMemberAuthority
	}
	raw := make([]byte, 2*physicalRenewSlotBytes)
	if _, err := o.receiptFile.ReadAt(raw, physicalRenewStaticBytes); err != nil {
		return err
	}
	var selected physicalRenewalWindow
	valid, nonzero, invalid := 0, 0, 0
	peerControl := false
	for slot := 0; slot < 2; slot++ {
		part := raw[slot*physicalRenewSlotBytes : (slot+1)*physicalRenewSlotBytes]
		if !bytes.Equal(part, make([]byte, physicalRenewSlotBytes)) {
			nonzero++
		}
		w, err := decodePhysicalRenewal(part, o.receipt, o.publisher.key[:])
		if err != nil {
			invalid++
			if bytes.Equal(part[:8], physicalPeerControlMagic[:]) {
				peerControl = true
			}
			continue
		}
		if w.Generation == 0 {
			continue
		}
		if err = o.validateRenewalLocked(w); err != nil {
			return err
		}
		valid++
		if w.Peer != nil {
			peerControl = true
		}
		if selected.Generation == w.Generation && !bytes.Equal(selected.MAC, w.MAC) {
			return ErrMemberAuthority
		}
		if w.Generation > selected.Generation {
			selected = w
		}
	}
	// A torn future-promise slot cannot be treated as the absence of the
	// promise. Ordinary allocation-only renewal may use its previous bound,
	// but future IO obligations require both nonempty controls to authenticate.
	if invalid > 0 && peerControl {
		return ErrPhysicalCredit
	}
	if nonzero > 0 && valid == 0 {
		return ErrMemberAuthority
	}
	if selected.Generation != 0 {
		o.end = selected.End
		o.renewalGeneration = selected.Generation
		o.currentControl = selected
	}
	return errors.Join(o.receiptOwner.VerifyFile(o.receiptFile), ctx.Err())
}

// ReplenishFrozen consumes the originating publisher's actual whole-read
// authorize barrier. That private scope owns publisher.mu until all borrowers
// join; callers must not take the Member mutex again. A live/ledger read, saved
// literal cut, escaped context, foreign owner or pressure admits no new range.
// This IO permission supplies no root, role, terminal grant or readiness.
func (o *PhysicalPublication) ReplenishFrozen(ctx context.Context, records int) error {
	if o == nil || o.manager == nil || records < 1 || records > maxUnconfirmedEntries {
		return resourcebudget.ErrInvalid
	}
	return borrowMemberWholeReadScope(ctx, o.publisher, func(member MemberState) (resultErr error) {
		// New allocation deliberately uses normal original Manager admission,
		// ignoring a caller's prepaid completion context. Only Healthy may renew.
		work, err := o.manager.TryAcquire(resourcebudget.IO, resourcebudget.Cost{RAM: OrderingFrozenReadRAMBytes + 8192})
		if err != nil {
			return err
		}
		defer func() {
			if errors.Is(resultErr, ownedfile.ErrRetirement) {
				o.renewalDebt = work
				return
			}
			if err := work.Release(); err != nil {
				o.renewalDebt = work
				resultErr = o.fail(errors.Join(resultErr, ownedfile.ErrRetirement, err))
			}
		}()
		p, j := o.publisher, o.journal
		j.mu.Lock()
		defer j.mu.Unlock()
		if p.closed || p.authorityFault != nil || p.physical != o || j.physical != o || j.closed || j.closing || j.poisoned != nil || j.freezeCancel == nil || o.withdrawn.Load() || o.banksClosed || o.failure() != nil || o.reserved != 0 || physicalPeerFutureActive(o.currentControl.Peer) || o.receipt.Version != 3 || o.receiptFile == nil || o.receiptOwner == nil || member.Term != member.PromisedTerm || member.DurableIndex != member.ConfirmedIndex || member.AppliedIndex != member.DurableIndex || !sameCompleteMemberState(member, p.state) || j.lastIndex != member.DurableIndex || j.lastTerm != member.Term || j.head != member.OrderingHeadMAC {
			return ErrMemberAuthority
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = p.readAuthoritySource(member); err != nil {
			if errors.Is(err, ownedfile.ErrRetirement) {
				return o.fail(err)
			}
			return err
		}
		frozen := FrozenOrderingHead{Header: j.header, Index: j.lastIndex, Term: j.lastTerm, MAC: j.head, Offset: j.nextOffset, StoreCursors: j.tailCursors}
		if err = j.verifyFrozenOrderingLocked(ctx, frozen); err != nil {
			return err
		}
		if err = o.validatePhysicalParentsLocked(ctx); err != nil {
			if errors.Is(err, ownedfile.ErrRetirement) {
				return o.fail(err)
			}
			return err
		}
		end := j.nextOffset + int64(records)*orderingRecordBytes
		if end < j.nextOffset || end > j.maxDiskBytes {
			return ErrOrderingJournalDiskLimit
		}
		if end <= o.end {
			return nil
		}
		if o.renewalGeneration == math.MaxUint64 {
			return ErrMemberAuthority
		}
		next := physicalRenewalWindow{Generation: o.renewalGeneration + 1, Term: member.Term, Index: member.DurableIndex, Head: member.OrderingHeadMAC, End: end, Peer: o.currentControl.Peer}
		next.MAC, err = physicalRenewalMAC(next, o.receipt, p.key[:])
		if err != nil {
			return err
		}
		slot, err := encodePhysicalRenewal(next)
		if err != nil {
			return err
		}
		offset := int64(physicalRenewStaticBytes) + int64(next.Generation%2)*physicalRenewSlotBytes
		if n, e := o.receiptFile.WriteAt(slot, offset); e != nil || n != len(slot) {
			return o.fail(errors.Join(ownedfile.ErrRetirement, e, io.ErrShortWrite))
		}
		if err = o.receiptFile.Sync(); err != nil {
			return o.fail(errors.Join(ownedfile.ErrRetirement, err))
		}
		if o.phase != nil {
			if err = o.phase("renewal-receipt-synced"); err != nil {
				return o.fail(errors.Join(ownedfile.ErrRetirement, err))
			}
		}
		if err = ctx.Err(); err != nil {
			return o.fail(errors.Join(ownedfile.ErrRetirement, err))
		}
		if err = resourcebudget.AllocatePhysicalRange(j.file, j.nextOffset, end-j.nextOffset); err != nil {
			return o.fail(errors.Join(ownedfile.ErrRetirement, err))
		}
		if err = j.file.Sync(); err != nil {
			return o.fail(errors.Join(ownedfile.ErrRetirement, err))
		}
		if o.phase != nil {
			if err = o.phase("renewal-ordering-allocated"); err != nil {
				return o.fail(errors.Join(ownedfile.ErrRetirement, err))
			}
		}
		if err = resourcebudget.CheckPhysicalRange(j.file, j.nextOffset, end); err != nil {
			return o.fail(errors.Join(ownedfile.ErrRetirement, err))
		}
		if err = resourcebudget.CheckPhysicalTail(j.file, j.nextOffset, end); err != nil {
			return o.fail(errors.Join(ownedfile.ErrRetirement, err))
		}
		if err = errors.Join(o.receiptOwner.VerifyFile(o.receiptFile), ctx.Err()); err != nil {
			return o.fail(errors.Join(ownedfile.ErrRetirement, err))
		}
		o.end = end
		o.renewalGeneration = next.Generation
		o.currentControl = next
		return nil
	})
}

// V3 uses a fixed typed static header so every legal 64-byte identity fits
// even when its JSON MAC representation needs escaping. V1/V2 retain their
// byte-for-byte canonical JSON MAC contract; accepted intent MACs also keep
// using the same immutable receipt MAC.
func encodePhysicalReceiptV3(r physicalPublicationReceipt) ([]byte, error) {
	if r.Version != 3 || len(r.MAC) != sha256.Size {
		return nil, ErrMemberAuthority
	}
	raw := append([]byte("HLRPUB03"), byte(3))
	for _, name := range []string{r.ClusterID, r.Incarnation, r.NodeID} {
		if len(name) < 1 || len(name) > MaxIdentityBytes {
			return nil, ErrMemberAuthority
		}
		raw = append(raw, byte(len(name)))
		raw = append(raw, name...)
	}
	for _, id := range []resourcebudget.PhysicalIdentity{r.State[0], r.State[1], r.Ordering, r.Receipt} {
		raw = binary.BigEndian.AppendUint32(raw, id.DeviceMajor)
		raw = binary.BigEndian.AppendUint32(raw, id.DeviceMinor)
		raw = binary.BigEndian.AppendUint64(raw, id.Inode)
		raw = binary.BigEndian.AppendUint64(raw, uint64(id.BornSeconds))
		raw = binary.BigEndian.AppendUint32(raw, id.BornNanos)
	}
	raw = binary.BigEndian.AppendUint64(raw, uint64(r.OrderingEnd))
	raw = append(raw, r.MAC...)
	if len(raw) > physicalRenewStaticBytes {
		return nil, ErrMemberAuthority
	}
	out := make([]byte, physicalRenewStaticBytes)
	copy(out, raw)
	return out, nil
}
func decodePhysicalReceiptV3(raw []byte, r *physicalPublicationReceipt) error {
	if r == nil || len(raw) != physicalRenewStaticBytes || !bytes.Equal(raw[:8], []byte("HLRPUB03")) || raw[8] != 3 {
		return ErrMemberAuthority
	}
	var decoded physicalPublicationReceipt
	decoded.Version = 3
	offset := 9
	names := []*string{&decoded.ClusterID, &decoded.Incarnation, &decoded.NodeID}
	for _, name := range names {
		size := int(raw[offset])
		offset++
		if size < 1 || size > MaxIdentityBytes || offset+size > len(raw) {
			return ErrMemberAuthority
		}
		*name = string(raw[offset : offset+size])
		offset += size
	}
	ids := []*resourcebudget.PhysicalIdentity{&decoded.State[0], &decoded.State[1], &decoded.Ordering, &decoded.Receipt}
	for _, id := range ids {
		if offset+28 > len(raw) {
			return ErrMemberAuthority
		}
		id.DeviceMajor = binary.BigEndian.Uint32(raw[offset:])
		offset += 4
		id.DeviceMinor = binary.BigEndian.Uint32(raw[offset:])
		offset += 4
		id.Inode = binary.BigEndian.Uint64(raw[offset:])
		offset += 8
		id.BornSeconds = int64(binary.BigEndian.Uint64(raw[offset:]))
		offset += 8
		id.BornNanos = binary.BigEndian.Uint32(raw[offset:])
		offset += 4
	}
	if offset+8+sha256.Size > len(raw) {
		return ErrMemberAuthority
	}
	decoded.OrderingEnd = int64(binary.BigEndian.Uint64(raw[offset:]))
	offset += 8
	decoded.MAC = bytes.Clone(raw[offset : offset+sha256.Size])
	canonical, err := encodePhysicalReceiptV3(decoded)
	if err != nil || !bytes.Equal(canonical, raw) {
		return errors.Join(ErrMemberAuthority, err)
	}
	*r = decoded
	return nil
}
