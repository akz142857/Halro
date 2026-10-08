package authroot

// This codec is a D-03 protocol experiment. It binds all component commitments
// to explicit native cuts and bootstrap identity; it is not a production trust
// publisher and cannot construct any log's private resume token. In particular,
// a MAC is not evidence that a live metadata DB is a valid authenticated baseline.
import (
	"bytes"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"math"
	"unicode/utf8"
)

const (
	sealFormat        = 1
	sealComponents    = 5 // Ledger, Metadata, Usage, Governance, segment directory.
	sealStores        = 4 // Ledger, Metadata, Governance, Audit, in this exact order.
	sealStandalone    = 1
	sealPrimary       = 2
	sealReplica       = 3
	sealAuthenticated = 1
	sealMixed         = 2
	sealLegacy        = 3
	sealLimit         = 16 << 10
	sealIdentityBytes = 64
	sealNativeBytes   = 8*4 + 1 + 32*2
	sealOrderingBytes = 8*3 + 32*4
	// The format has no caller-controlled lists, lengths or allocation sizes.
	sealBytes = 8 + 2*5 + 2 + 4*(2+sealIdentityBytes) + 8*4 + 32*2 + sealStores*sealNativeBytes + sealOrderingBytes + sealComponents*rootBytes + 32
)

type sealIdentity struct{ Instance, Cluster, Member, Incarnation string }

type sealReaderContext struct {
	Identity       sealIdentity
	MetadataSchema uint64
}

type sealNativeCut struct {
	// Generation means Ledger generation or Metadata epoch; Gov/Audit require 0.
	Generation, FileRevision, Sequence, Offset uint64
	Epoch                                      uint8
	Hash, HeaderDigest                         [32]byte
}

type sealOrderingCut struct {
	Index, Offset, Term                                 uint64
	HeaderDigest, BaselineDigest, MAC, LedgerRollDigest [32]byte
}

type stateSeal struct {
	Identity                                 sealIdentity
	Role, Coverage                           uint8
	Generation, Revision, MetadataSchema     uint64
	PublisherTerm                            uint64
	MetadataBaselineID, MetadataBaselineRoot [32]byte
	Native                                   [sealStores]sealNativeCut
	Ordering                                 sealOrderingCut
	Roots                                    [sealComponents]root
}

// Derive from the saved Ledger envelope key, never from the current master key.
// This deliberately uses a separate experimental domain from native log keys.
func deriveSealKey(ledgerKey []byte) ([]byte, error) {
	if len(ledgerKey) != sha256.Size {
		return nil, errCorrupt
	}
	return hkdf.Key(sha256.New, ledgerKey, []byte("halro:state-seal-experiment:v1"), "halro:state-seal-experiment-hmac-key:v1", sha256.Size)
}

func (s stateSeal) valid() bool {
	validID := func(id string) bool {
		return len(id) > 0 && len(id) <= sealIdentityBytes && utf8.ValidString(id) && !bytes.ContainsRune([]byte(id), 0)
	}
	if !validID(s.Identity.Instance) || s.Generation == 0 || s.Revision == 0 || s.MetadataSchema == 0 || s.MetadataBaselineID == ([32]byte{}) || s.MetadataBaselineRoot == ([32]byte{}) || s.Coverage < sealAuthenticated || s.Coverage > sealLegacy {
		return false
	}
	switch s.Role {
	case sealStandalone:
		if s.PublisherTerm != 0 || s.Identity.Cluster != "" || s.Identity.Member != "" || s.Identity.Incarnation != "" || s.Ordering != (sealOrderingCut{}) {
			return false
		}
	case sealPrimary, sealReplica:
		if s.PublisherTerm == 0 || s.Ordering.Term > s.PublisherTerm || !validID(s.Identity.Cluster) || !validID(s.Identity.Member) || !validID(s.Identity.Incarnation) || s.Ordering.HeaderDigest == ([32]byte{}) || s.Ordering.BaselineDigest == ([32]byte{}) || s.Ordering.LedgerRollDigest == ([32]byte{}) || s.Ordering.Offset > math.MaxInt64 {
			return false
		}
		if s.Ordering.Index == 0 {
			if s.Ordering.MAC != ([32]byte{}) || s.Ordering.Term != 0 {
				return false
			}
		} else if s.Ordering.MAC == ([32]byte{}) || s.Ordering.Term == 0 || s.Ordering.Offset == 0 {
			return false
		}
	default:
		return false
	}
	for i, c := range s.Native {
		if c.HeaderDigest == ([32]byte{}) || c.Offset > math.MaxInt64 || c.Sequence > 0 && c.Offset == 0 {
			return false
		}
		if i == 0 {
			if c.Epoch > 5 || c.Epoch == 0 && (c.Sequence != 0 || c.Hash != ([32]byte{})) || c.Epoch > 0 && c.Epoch < 4 && (c.Hash != ([32]byte{}) || s.Coverage == sealAuthenticated) || c.Epoch >= 4 && c.Hash == ([32]byte{}) {
				return false
			}
		} else if (c.Sequence == 0) != (c.Hash == ([32]byte{})) || c.Epoch != 0 {
			return false
		}
		if i < 2 && (c.Generation == 0 || c.FileRevision == 0) || i >= 2 && (c.Generation != 0 || c.FileRevision != 0 || c.Epoch != 0) {
			return false
		}
	}
	for i, r := range s.Roots {
		// Scope order is the complete component set; duplicate/unknown/omitted
		// components cannot silently acquire a skip-history claim.
		if r.scope != uint16(i+1) || !r.valid() || r.generation != s.Generation || r.revision != s.Revision || r.sequence != 0 || r.factHash != ([32]byte{}) {
			return false
		}
	}
	return true
}

func signSeal(s stateSeal, key []byte) ([]byte, error) {
	if len(key) != sha256.Size || !s.valid() {
		return nil, errCorrupt
	}
	b := make([]byte, 0, sealBytes)
	b = append(b, []byte("HMPSEAL1")...)
	// Envelope, apply algorithm, index codec, key derivation, page format.
	for _, v := range []uint16{sealFormat, 1, 1, 1, pageFormat} {
		b = binary.BigEndian.AppendUint16(b, v)
	}
	b = append(b, s.Role, s.Coverage)
	for _, id := range []string{s.Identity.Instance, s.Identity.Cluster, s.Identity.Member, s.Identity.Incarnation} {
		b = binary.BigEndian.AppendUint16(b, uint16(len(id)))
		var field [sealIdentityBytes]byte
		copy(field[:], id)
		b = append(b, field[:]...)
	}
	for _, v := range []uint64{s.Generation, s.Revision, s.MetadataSchema, s.PublisherTerm} {
		b = binary.BigEndian.AppendUint64(b, v)
	}
	b = append(b, s.MetadataBaselineID[:]...)
	b = append(b, s.MetadataBaselineRoot[:]...)
	for _, c := range s.Native {
		for _, v := range []uint64{c.Generation, c.FileRevision, c.Sequence, c.Offset} {
			b = binary.BigEndian.AppendUint64(b, v)
		}
		b = append(b, c.Epoch)
		b = append(b, c.Hash[:]...)
		b = append(b, c.HeaderDigest[:]...)
	}
	o := s.Ordering
	for _, v := range []uint64{o.Index, o.Offset, o.Term} {
		b = binary.BigEndian.AppendUint64(b, v)
	}
	for _, v := range [][32]byte{o.HeaderDigest, o.BaselineDigest, o.MAC, o.LedgerRollDigest} {
		b = append(b, v[:]...)
	}
	for _, r := range s.Roots {
		encoded, err := signRoot(r, key)
		if err != nil {
			return nil, err
		}
		b = append(b, encoded...)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(b)
	return mac.Sum(b), nil
}

// Verification authenticates a claim, not its production eligibility. Expected
// identity must come from independent bootstrap/member evidence. Legacy/mixed
// claims remain labelled and are not converted into authenticated native facts.
func verifySeal(b, key []byte, expected sealReaderContext) (stateSeal, error) {
	var s stateSeal
	if len(key) != sha256.Size || len(b) != sealBytes || len(b) > sealLimit || !bytes.Equal(b[:8], []byte("HMPSEAL1")) {
		return s, errCorrupt
	}
	for i, v := range []uint16{sealFormat, 1, 1, 1, pageFormat} {
		if binary.BigEndian.Uint16(b[8+2*i:10+2*i]) != v {
			return s, errCorrupt
		}
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(b[:len(b)-32])
	if !hmac.Equal(mac.Sum(nil), b[len(b)-32:]) {
		return s, errCorrupt
	}
	s.Role, s.Coverage = b[18], b[19]
	at := 20
	ids := [4]string{}
	for i := range ids {
		n := int(binary.BigEndian.Uint16(b[at : at+2]))
		at += 2
		if n > sealIdentityBytes {
			return stateSeal{}, errCorrupt
		}
		field := b[at : at+sealIdentityBytes]
		at += sealIdentityBytes
		for _, v := range field[n:] {
			if v != 0 {
				return stateSeal{}, errCorrupt
			}
		}
		ids[i] = string(field[:n])
	}
	s.Identity = sealIdentity{ids[0], ids[1], ids[2], ids[3]}
	u64 := func() uint64 { v := binary.BigEndian.Uint64(b[at : at+8]); at += 8; return v }
	hash := func() (v [32]byte) { copy(v[:], b[at:at+32]); at += 32; return }
	s.Generation, s.Revision, s.MetadataSchema = u64(), u64(), u64()
	s.PublisherTerm = u64()
	s.MetadataBaselineID, s.MetadataBaselineRoot = hash(), hash()
	for i := range s.Native {
		c := &s.Native[i]
		c.Generation, c.FileRevision, c.Sequence, c.Offset = u64(), u64(), u64(), u64()
		c.Epoch = b[at]
		at++
		c.Hash, c.HeaderDigest = hash(), hash()
	}
	o := &s.Ordering
	o.Index, o.Offset, o.Term = u64(), u64(), u64()
	o.HeaderDigest, o.BaselineDigest, o.MAC, o.LedgerRollDigest = hash(), hash(), hash(), hash()
	for i := range s.Roots {
		r, err := verifyRoot(b[at:at+rootBytes], key)
		if err != nil {
			return stateSeal{}, err
		}
		s.Roots[i] = r
		at += rootBytes
	}
	if at != len(b)-32 || !s.valid() || s.Identity != expected.Identity || s.MetadataSchema != expected.MetadataSchema {
		return stateSeal{}, errCorrupt
	}
	return s, nil
}
