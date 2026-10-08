package replication

import (
	"bytes"
	"context"
	"crypto/hmac"
	"encoding/binary"
	"errors"
	"math"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

const physicalPeerFutureSlots = 2

// These are persistent IO bookkeeping observations. A snapshot does not grant
// a native role, select a root, credit an ACK or construct a caller's token.
type physicalPeerFutureTicket struct {
	ID      [16]byte
	Window  [32]byte
	Request [32]byte
	Cost    PhysicalCreditCost
	Peer    uint8
	Active  bool
}
type physicalPeerFutureState struct {
	WindowID        [32]byte
	Serial          uint64
	Granted         PhysicalCreditCost
	Tickets         [physicalPeerFutureSlots]physicalPeerFutureTicket
	PeerSerial      [2]uint64
	PreviousTickets [physicalPeerFutureSlots][16]byte
}

func physicalPeerFutureActive(s *physicalPeerFutureState) bool {
	if s != nil {
		for _, t := range s.Tickets {
			if t.Active {
				return true
			}
		}
	}
	return false
}
func peerFutureReserved(s *physicalPeerFutureState) (out PhysicalCreditCost, err error) {
	if s != nil {
		for _, t := range s.Tickets {
			if t.Active {
				out, err = out.Add(t.Cost)
				if err != nil {
					return out, err
				}
			}
		}
	}
	return
}
func (c PhysicalCreditCost) Add(other PhysicalCreditCost) (PhysicalCreditCost, error) {
	for i := 0; i < 4; i++ {
		if math.MaxUint64-c.NativeBytes[i] < other.NativeBytes[i] || math.MaxUint32-c.NativeRecords[i] < other.NativeRecords[i] || math.MaxUint32-c.COWRecords[i] < other.COWRecords[i] {
			return c, ErrPhysicalCredit
		}
		c.NativeBytes[i] += other.NativeBytes[i]
		c.NativeRecords[i] += other.NativeRecords[i]
		c.COWRecords[i] += other.COWRecords[i]
	}
	if math.MaxUint32-c.UsageFrames < other.UsageFrames || math.MaxUint32-c.DirectoryRolls < other.DirectoryRolls || math.MaxUint32-c.OrderingRecords < other.OrderingRecords {
		return c, ErrPhysicalCredit
	}
	c.UsageFrames += other.UsageFrames
	c.DirectoryRolls += other.DirectoryRolls
	c.OrderingRecords += other.OrderingRecords
	return c, nil
}

// RequireNoPeerPhysicalPromises consumes the actual originating whole Member
// barrier. Even D=C=A is insufficient while prepaid future work is promised.
func (o *PhysicalPublication) RequireNoPeerPhysicalPromises(ctx context.Context) error {
	if o == nil {
		return ErrMemberAuthority
	}
	return borrowMemberWholeReadScope(ctx, o.publisher, func(member MemberState) error {
		j := o.journal
		j.mu.Lock()
		defer j.mu.Unlock()
		if err := o.validatePeerControlSourceLocked(ctx, member, false); err != nil {
			return err
		}
		if physicalPeerFutureActive(o.currentControl.Peer) {
			return ErrPhysicalCredit
		}
		return nil
	})
}
func (o *PhysicalPublication) validatePeerControlLocked(ctx context.Context, m MemberState) error {
	return o.validatePeerControlSourceLocked(ctx, m, true)
}
func (o *PhysicalPublication) validatePeerControlSourceLocked(ctx context.Context, m MemberState, requireFull bool) error {
	p, j := o.publisher, o.journal
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if p.closed || p.authorityFault != nil || p.physical != o || j.physical != o || j.closed || j.closing || j.poisoned != nil || j.freezeCancel == nil || o.withdrawn.Load() || o.banksClosed || o.failure() != nil || o.receipt.Version != 3 || o.receiptFile == nil || o.receiptOwner == nil || m.Term != m.PromisedTerm || requireFull && (m.DurableIndex != m.ConfirmedIndex || m.AppliedIndex != m.DurableIndex) || !sameCompleteMemberState(m, p.state) || j.lastIndex != m.DurableIndex || j.lastTerm != m.Term || j.head != m.OrderingHeadMAC {
		return ErrMemberAuthority
	}
	if err := p.readAuthoritySource(m); err != nil {
		return err
	}
	return errors.Join(o.validatePhysicalParentsLocked(ctx), ctx.Err())
}

// withPeerControlFrozen records promises only; actual Runtime producers must
// qualify all native/COW banks and original semantic completion owners before
// they may accept/send this bookkeeping outcome as a usable remote promise.
// No exported snapshot or caller-supplied capacity receives IO write authority.
func (o *PhysicalPublication) withPeerControlFrozen(ctx context.Context, visit func(MemberState, *physicalPeerFutureState) error) error {
	if o == nil || visit == nil {
		return ErrMemberAuthority
	}
	return borrowMemberWholeReadScope(ctx, o.publisher, func(m MemberState) (resultErr error) {
		work, err := o.manager.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: 4096})
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
		j := o.journal
		j.mu.Lock()
		defer j.mu.Unlock()
		if err = o.validatePeerControlLocked(ctx, m); err != nil {
			return err
		}
		next := o.currentControl
		next.Generation = o.renewalGeneration + 1
		if next.Generation == 0 {
			return ErrMemberAuthority
		}
		next.Term, next.Index, next.Head, next.End = m.Term, m.DurableIndex, m.OrderingHeadMAC, o.end
		var peer physicalPeerFutureState
		if next.Peer != nil {
			peer = *next.Peer
		}
		next.Peer = &peer
		if err = visit(m, &peer); err != nil {
			return err
		}
		next.MAC, err = physicalRenewalMAC(next, o.receipt, o.publisher.key[:])
		if err != nil {
			return err
		}
		slot, err := encodePhysicalRenewal(next)
		if err != nil {
			return err
		}
		offset := int64(physicalRenewStaticBytes) + int64(next.Generation%2)*physicalRenewSlotBytes
		if n, e := o.receiptFile.WriteAt(slot, offset); e != nil || n != len(slot) {
			return o.fail(errors.Join(ownedfile.ErrRetirement, e, ErrPhysicalCredit))
		}
		if err = o.receiptFile.Sync(); err != nil {
			return o.fail(errors.Join(ownedfile.ErrRetirement, err))
		}
		if o.phase != nil {
			if err = o.phase("peer-future-promise-synced"); err != nil {
				return o.fail(errors.Join(ownedfile.ErrRetirement, err))
			}
		}
		if err = errors.Join(o.receiptOwner.VerifyFile(o.receiptFile), ctx.Err()); err != nil {
			return o.fail(errors.Join(ownedfile.ErrRetirement, err))
		}
		o.currentControl = next
		o.renewalGeneration = next.Generation
		return nil
	})
}

var physicalPeerControlMagic = [8]byte{'H', 'L', 'R', 'P', 'F', 'U', 'T', '2'}

const physicalPeerCostCompactBytes = 4*(4+2+2) + 3*2

func appendPhysicalPeerCost(out []byte, c PhysicalCreditCost) ([]byte, error) {
	for i := 0; i < 4; i++ {
		if c.NativeBytes[i] > math.MaxUint32 || c.NativeRecords[i] > math.MaxUint16 || c.COWRecords[i] > math.MaxUint16 {
			return nil, ErrPhysicalCredit
		}
		out = binary.BigEndian.AppendUint32(out, uint32(c.NativeBytes[i]))
		out = binary.BigEndian.AppendUint16(out, uint16(c.NativeRecords[i]))
		out = binary.BigEndian.AppendUint16(out, uint16(c.COWRecords[i]))
	}
	for _, n := range []uint32{c.UsageFrames, c.DirectoryRolls, c.OrderingRecords} {
		if n > math.MaxUint16 {
			return nil, ErrPhysicalCredit
		}
		out = binary.BigEndian.AppendUint16(out, uint16(n))
	}
	return out, nil
}
func takePhysicalPeerCost(in []byte) (c PhysicalCreditCost) {
	off := 0
	for i := 0; i < 4; i++ {
		c.NativeBytes[i] = uint64(binary.BigEndian.Uint32(in[off:]))
		off += 4
		c.NativeRecords[i] = uint32(binary.BigEndian.Uint16(in[off:]))
		off += 2
		c.COWRecords[i] = uint32(binary.BigEndian.Uint16(in[off:]))
		off += 2
	}
	c.UsageFrames = uint32(binary.BigEndian.Uint16(in[off:]))
	off += 2
	c.DirectoryRolls = uint32(binary.BigEndian.Uint16(in[off:]))
	off += 2
	c.OrderingRecords = uint32(binary.BigEndian.Uint16(in[off:]))
	return
}
func encodePhysicalPeerControl(w physicalRenewalWindow) ([]byte, error) {
	if w.Peer == nil || w.Generation == 0 || w.Term == 0 || w.Index == 0 || len(w.MAC) != 32 {
		return nil, ErrPhysicalCredit
	}
	s := w.Peer
	raw := append([]byte{}, physicalPeerControlMagic[:]...)
	raw = binary.BigEndian.AppendUint64(raw, w.Generation)
	raw = binary.BigEndian.AppendUint64(raw, w.Term)
	raw = binary.BigEndian.AppendUint64(raw, w.Index)
	raw = append(raw, w.Head[:]...)
	raw = binary.BigEndian.AppendUint64(raw, uint64(w.End))
	raw = append(raw, s.WindowID[:]...)
	raw = binary.BigEndian.AppendUint64(raw, s.Serial)
	var err error
	raw, err = appendPhysicalPeerCost(raw, s.Granted)
	if err != nil {
		return nil, err
	}
	for _, t := range s.Tickets {
		raw = append(raw, t.ID[:]...)
		raw = append(raw, t.Window[:]...)
		raw = append(raw, t.Request[:]...)
		raw, err = appendPhysicalPeerCost(raw, t.Cost)
		if err != nil {
			return nil, err
		}
		if t.Peer > 1 {
			return nil, ErrPhysicalCredit
		}
		raw = append(raw, t.Peer)
		active := byte(0)
		if t.Active {
			active = 1
		}
		raw = append(raw, active)
	}
	for _, serial := range s.PeerSerial {
		raw = binary.BigEndian.AppendUint64(raw, serial)
	}
	for _, id := range s.PreviousTickets {
		raw = append(raw, id[:]...)
	}
	raw = append(raw, w.MAC...)
	if len(raw) > physicalRenewSlotBytes {
		return nil, ErrPhysicalCredit
	}
	out := make([]byte, physicalRenewSlotBytes)
	copy(out, raw)
	return out, nil
}
func decodePhysicalPeerControl(raw []byte, r physicalPublicationReceipt, key []byte) (w physicalRenewalWindow, resultErr error) {
	if len(raw) != physicalRenewSlotBytes || !bytes.Equal(raw[:8], physicalPeerControlMagic[:]) {
		return w, ErrPhysicalCredit
	}
	off := 8
	w.Generation = binary.BigEndian.Uint64(raw[off:])
	off += 8
	w.Term = binary.BigEndian.Uint64(raw[off:])
	off += 8
	w.Index = binary.BigEndian.Uint64(raw[off:])
	off += 8
	copy(w.Head[:], raw[off:])
	off += 32
	w.End = int64(binary.BigEndian.Uint64(raw[off:]))
	off += 8
	s := &physicalPeerFutureState{}
	w.Peer = s
	copy(s.WindowID[:], raw[off:])
	off += 32
	s.Serial = binary.BigEndian.Uint64(raw[off:])
	off += 8
	s.Granted = takePhysicalPeerCost(raw[off:])
	off += physicalPeerCostCompactBytes
	for i := range s.Tickets {
		t := &s.Tickets[i]
		copy(t.ID[:], raw[off:])
		off += 16
		copy(t.Window[:], raw[off:])
		off += 32
		copy(t.Request[:], raw[off:])
		off += 32
		t.Cost = takePhysicalPeerCost(raw[off:])
		off += physicalPeerCostCompactBytes
		t.Peer = raw[off]
		off++
		if t.Peer > 1 {
			return physicalRenewalWindow{}, ErrPhysicalCredit
		}
		if raw[off] > 1 {
			return physicalRenewalWindow{}, ErrPhysicalCredit
		}
		t.Active = raw[off] == 1
		off++
		if t.Active && (t.ID == ([16]byte{}) || t.Request == ([32]byte{}) || t.Cost.OrderingRecords == 0 || binary.BigEndian.Uint64(t.ID[8:]) > s.Serial && binary.BigEndian.Uint64(t.ID[8:]) > s.PeerSerial[t.Peer] || !bytes.Equal(t.ID[:8], t.Window[:8])) {
			return physicalRenewalWindow{}, ErrPhysicalCredit
		}
	}
	for i := range s.PeerSerial {
		s.PeerSerial[i] = binary.BigEndian.Uint64(raw[off:])
		off += 8
	}
	for i := range s.PreviousTickets {
		copy(s.PreviousTickets[i][:], raw[off:])
		off += 16
	}
	w.MAC = bytes.Clone(raw[off : off+32])
	canonical, err := encodePhysicalPeerControl(w)
	if err != nil || !bytes.Equal(canonical, raw) {
		return physicalRenewalWindow{}, errors.Join(ErrPhysicalCredit, err)
	}
	mac, err := physicalRenewalMAC(w, r, key)
	if err != nil || !hmac.Equal(mac, w.MAC) {
		return physicalRenewalWindow{}, errors.Join(ErrPhysicalCredit, err)
	}
	return w, nil
}

// PeerPhysicalFutureObservation is persistent bookkeeping, not an IO grant.
// Actual Runtime consumers must requalify the originating bank/source owners.
type PeerPhysicalFutureTicketObservation struct {
	ID      [16]byte
	Window  [32]byte
	Request [32]byte
	Cost    PhysicalCreditCost
	Peer    uint8
	Active  bool
}
type PeerPhysicalFutureObservation struct {
	WindowID [32]byte
	Serial   uint64
	Granted  PhysicalCreditCost
	Tickets  [physicalPeerFutureSlots]PeerPhysicalFutureTicketObservation
}

func (o *PhysicalPublication) ObservePeerPhysicalPromisesFrozen(ctx context.Context) (out PeerPhysicalFutureObservation, resultErr error) {
	if o == nil {
		return out, ErrMemberAuthority
	}
	resultErr = borrowMemberWholeReadScope(ctx, o.publisher, func(m MemberState) error {
		j := o.journal
		j.mu.Lock()
		defer j.mu.Unlock()
		if err := o.validatePeerControlLocked(ctx, m); err != nil {
			return err
		}
		s := o.currentControl.Peer
		if s == nil {
			return nil
		}
		out.WindowID, out.Serial, out.Granted = s.WindowID, s.Serial, s.Granted
		for i, t := range s.Tickets {
			out.Tickets[i] = PeerPhysicalFutureTicketObservation(t)
		}
		return nil
	})
	if resultErr != nil {
		out = PeerPhysicalFutureObservation{}
	}
	return
}

// RecordPeerFuturePromiseFrozen writes only an IO obligation. Raw available
// values have no authorization semantics; the original Runtime producer must
// obtain them from genuine native/COW getters before calling this method.
// No caller can use the observation/receipt to acquire role or root authority.
func (o *PhysicalPublication) RecordPeerFuturePromiseFrozen(ctx context.Context, window [32]byte, ticket [16]byte, request [32]byte, cost, available PhysicalCreditCost, peer uint8) error {
	return o.recordPeerFuturePromiseFrozen(ctx, window, window, ticket, request, cost, available, peer, false)
}

// RecordPeerFuturePromiseForPeerFrozen mirrors one original authenticated peer
// promise before Primary acceptance. Distinct Replica banks remain distinct;
// it records no local native/COW write or confirmation permission.
func (o *PhysicalPublication) RecordPeerFuturePromiseForPeerFrozen(ctx context.Context, bank, remote [32]byte, ticket [16]byte, request [32]byte, cost, available PhysicalCreditCost, peer uint8) error {
	return o.recordPeerFuturePromiseFrozen(ctx, bank, remote, ticket, request, cost, available, peer, true)
}
func (o *PhysicalPublication) recordPeerFuturePromiseFrozen(ctx context.Context, window, remote [32]byte, ticket [16]byte, request [32]byte, cost, available PhysicalCreditCost, peer uint8, mirror bool) error {
	if window == ([32]byte{}) || ticket == ([16]byte{}) || request == ([32]byte{}) || cost.OrderingRecords == 0 || peer > 1 || remote == ([32]byte{}) || !bytes.Equal(ticket[:8], remote[:8]) {
		return ErrPhysicalCredit
	}
	return o.withPeerControlFrozen(ctx, func(m MemberState, s *physicalPeerFutureState) error {
		if peer >= uint8(len(m.Peers)) || mirror && m.Role != RolePrimary {
			return ErrPhysicalCredit
		}
		if s.WindowID != window {
			if physicalPeerFutureActive(s) {
				return ErrPhysicalCredit
			}
			// Preserve one immediately previous pair for idempotent lost-release ACKs.
			var old [physicalPeerFutureSlots][16]byte
			for i, t := range s.Tickets {
				old[i] = t.ID
			}
			*s = physicalPeerFutureState{WindowID: window, PreviousTickets: old}
		}
		serial := binary.BigEndian.Uint64(ticket[8:])
		for _, old := range s.Tickets {
			if old.ID == ticket {
				if old.Active && old.Request == request && old.Cost == cost && old.Peer == peer && old.Window == remote {
					return nil
				}
				return ErrPhysicalCredit
			}
		}
		expected := s.Serial + 1
		if mirror {
			expected = s.PeerSerial[peer] + 1
			prior := s.Tickets[peer]
			if !prior.Active && prior.Window != remote {
				expected = 1
			}
		}
		if serial == 0 || s.Serial == math.MaxUint64 || serial != expected {
			return ErrPhysicalCredit
		}
		held, err := peerFutureReserved(s)
		if mirror {
			held = PhysicalCreditCost{}
			for _, old := range s.Tickets {
				if old.Active && old.Peer == peer {
					held, err = held.Add(old.Cost)
					if err != nil {
						break
					}
				}
			}
		}
		if err != nil {
			return err
		}
		spare, err := available.Subtract(held)
		if err != nil || !spare.Covers(cost) {
			return ErrPhysicalCredit
		}
		index := -1
		if mirror {
			if !s.Tickets[peer].Active {
				index = int(peer)
			}
		} else {
			for i, t := range s.Tickets {
				if !t.Active {
					index = i
					break
				}
			}
		}
		if index < 0 {
			return ErrPhysicalCredit
		}
		granted, err := s.Granted.Add(cost)
		if err != nil {
			return err
		}
		if _, err = appendPhysicalPeerCost(nil, granted); err != nil {
			return err
		}
		s.Granted = granted
		if mirror {
			s.PeerSerial[peer] = serial
			s.Serial++
		} else {
			s.Serial = serial
		}
		s.Tickets[index] = physicalPeerFutureTicket{ID: ticket, Window: remote, Request: request, Cost: cost, Peer: peer, Active: true}
		return nil
	})
}

// RecordPeerFutureReleaseFrozen never refunds the credit advertised in an ACK.
// It removes the one original persistent obligation only after the actual
// Runtime's semantic completion owner has known completion and genuine native
// source counters account for all consumed IO. Duplicate releases do not debit
// or add any allowance a second time, including one prior published bank.
func (o *PhysicalPublication) RecordPeerFutureReleaseFrozen(ctx context.Context, window [32]byte, ticket [16]byte, used, consumed PhysicalCreditCost) error {
	if window == ([32]byte{}) || ticket == ([16]byte{}) {
		return ErrPhysicalCredit
	}
	return o.withPeerControlFrozen(ctx, func(_ MemberState, s *physicalPeerFutureState) error {
		matching := false
		for _, t := range s.Tickets {
			matching = matching || t.ID == ticket && t.Window == window
		}
		if !matching {
			if bytes.Equal(ticket[:8], window[:8]) {
				for _, old := range s.PreviousTickets {
					if old == ticket {
						return nil
					}
				}
			}
			return ErrPhysicalCredit
		}
		for i, t := range s.Tickets {
			if t.ID != ticket || t.Window != window {
				continue
			}
			if !t.Active {
				return nil
			}
			unused, err := t.Cost.Subtract(used)
			if err != nil {
				return err
			}
			next, err := s.Granted.Subtract(unused)
			if err != nil || !next.Covers(consumed) {
				return ErrPhysicalCredit
			}
			s.Granted = next
			s.Tickets[i].Active = false
			return nil
		}
		return ErrPhysicalCredit
	})
}

// RecordPeerFutureWindowFrozen records the new original bank identity before
// its first Window ACK. It can replace an epoch only after every old promise
// has known retirement; a reconnect of the same bank preserves serial/tickets.
func (o *PhysicalPublication) RecordPeerFutureWindowFrozen(ctx context.Context, window [32]byte) error {
	if window == ([32]byte{}) {
		return ErrPhysicalCredit
	}
	return o.withPeerControlFrozen(ctx, func(_ MemberState, s *physicalPeerFutureState) error {
		if s.WindowID == window {
			return nil
		}
		if physicalPeerFutureActive(s) {
			return ErrPhysicalCredit
		}
		var old [physicalPeerFutureSlots][16]byte
		for i, t := range s.Tickets {
			old[i] = t.ID
		}
		*s = physicalPeerFutureState{WindowID: window, PreviousTickets: old}
		return nil
	})
}

// RecordPeerFutureRejectionFrozen records only a known negative decision for
// the next serial. It reserves no byte, root, confirmation or native authority.
// Runtime must independently prove the requested promise absent under the
// actual original receive/apply barrier before replying with this tombstone.
func (o *PhysicalPublication) RecordPeerFutureRejectionFrozen(ctx context.Context, window [32]byte, ticket [16]byte, request [32]byte, peer uint8) error {
	if window == ([32]byte{}) || ticket == ([16]byte{}) || request == ([32]byte{}) || peer > 1 || !bytes.Equal(ticket[:8], window[:8]) {
		return ErrPhysicalCredit
	}
	return o.withPeerControlFrozen(ctx, func(m MemberState, s *physicalPeerFutureState) error {
		if m.Role != RoleReplica || peer >= uint8(len(m.Peers)) || s.WindowID != window {
			return ErrPhysicalCredit
		}
		for _, old := range s.Tickets {
			if old.ID == ticket {
				if !old.Active && old.Window == window && old.Request == request && old.Cost.IsZero() {
					return nil
				}
				return ErrPhysicalCredit
			}
		}
		serial := binary.BigEndian.Uint64(ticket[8:])
		if serial == 0 || s.Serial == math.MaxUint64 || serial != s.Serial+1 {
			return ErrPhysicalCredit
		}
		index := -1
		for i, old := range s.Tickets {
			if !old.Active {
				index = i
				break
			}
		}
		if index < 0 {
			return ErrPhysicalCredit
		}
		s.Serial = serial
		s.Tickets[index] = physicalPeerFutureTicket{ID: ticket, Window: window, Request: request, Peer: peer}
		return nil
	})
}
