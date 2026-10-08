package replication

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
)

// MetadataBaselineProtocolVersion is negotiated by the explicit maintenance
// upgrade path. Legacy traffic retains its byte-exact version-one encoding.
const MetadataBaselineProtocolVersion uint16 = 2
const MetadataBaselineMetadataBytes = 128

// MetadataBaselineMetadata crosses one independently prepared Metadata epoch.
// AuthorityDigest names the qualified group carrier, not the candidate header.
// The ordering record at N+1 binds the old native cursor at N to the new header.
// It never resets cluster/incarnation, global index, term, or other store cuts.
type MetadataBaselineMetadata struct {
	PreviousEpoch, PreviousSequence uint64
	PreviousHead                    [sha256.Size]byte
	Epoch                           uint64
	HeaderHash, AuthorityDigest     [sha256.Size]byte
}

func (m MetadataBaselineMetadata) Validate() error {
	if m.PreviousEpoch == 0 || m.PreviousEpoch == ^uint64(0) || m.Epoch != m.PreviousEpoch+1 || m.PreviousHead == ([32]byte{}) || m.HeaderHash == ([32]byte{}) || m.AuthorityDigest == ([32]byte{}) {
		return errors.New("metadata baseline boundary has an invalid independent epoch binding")
	}
	return nil
}

func (m MetadataBaselineMetadata) MarshalBinary() ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	b := make([]byte, MetadataBaselineMetadataBytes)
	b[0] = 1
	binary.BigEndian.PutUint64(b[8:16], m.PreviousEpoch)
	binary.BigEndian.PutUint64(b[16:24], m.PreviousSequence)
	copy(b[24:56], m.PreviousHead[:])
	binary.BigEndian.PutUint64(b[56:64], m.Epoch)
	copy(b[64:96], m.HeaderHash[:])
	copy(b[96:128], m.AuthorityDigest[:])
	return b, nil
}

func DecodeMetadataBaselineMetadata(b []byte) (MetadataBaselineMetadata, error) {
	if len(b) != MetadataBaselineMetadataBytes || b[0] != 1 {
		return MetadataBaselineMetadata{}, errors.New("metadata baseline descriptor length or version is invalid")
	}
	for _, value := range b[1:8] {
		if value != 0 {
			return MetadataBaselineMetadata{}, errors.New("metadata baseline reserved bytes are nonzero")
		}
	}
	m := MetadataBaselineMetadata{PreviousEpoch: binary.BigEndian.Uint64(b[8:16]), PreviousSequence: binary.BigEndian.Uint64(b[16:24]), Epoch: binary.BigEndian.Uint64(b[56:64])}
	copy(m.PreviousHead[:], b[24:56])
	copy(m.HeaderHash[:], b[64:96])
	copy(m.AuthorityDigest[:], b[96:128])
	return m, m.Validate()
}

func frameProtocolVersion(kind Kind) uint16 {
	if kind == KindMetadataBaseline {
		return MetadataBaselineProtocolVersion
	}
	return ProtocolVersion
}
