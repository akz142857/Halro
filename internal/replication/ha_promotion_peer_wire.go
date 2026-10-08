package replication

import (
	"bytes"
	"encoding/binary"
)

var haPeerRequestMagic = [8]byte{'H', 'A', 'P', 'R', 'E', 'Q', '0', '1'}
var haPeerBoundaryMagic = [8]byte{'H', 'A', 'P', 'B', 'N', 'D', '0', '1'}

// Only a structural Leadership frame may accompany this bounded carrier. It
// supplies no semantic receive permission and never replaces Member/Ordering.
const MaxHAPeerLeadershipFrameBytes = 1024
const maxHAPeerBoundaryBodyBytes = 16 + MaxHAPromotionCarrierBytes + MaxHAPeerLeadershipFrameBytes

func MarshalHAPeerPromotionRequest(carrier []byte) ([]byte, error) {
	if len(carrier) < 33 || len(carrier) > MaxHAPromotionCarrierBytes {
		return nil, ErrMemberAuthority
	}
	out := make([]byte, 12+len(carrier))
	binary.BigEndian.PutUint32(out[:4], uint32(len(out)-4))
	copy(out[4:12], haPeerRequestMagic[:])
	copy(out[12:], carrier)
	return out, nil
}
func UnmarshalHAPeerPromotionRequest(encoded []byte) ([]byte, error) {
	if len(encoded) < 45 || len(encoded) > 12+MaxHAPromotionCarrierBytes || int(binary.BigEndian.Uint32(encoded[:4])) != len(encoded)-4 || !bytes.Equal(encoded[4:12], haPeerRequestMagic[:]) {
		return nil, ErrMemberAuthority
	}
	return encoded[12:], nil
}
func MarshalHAPeerPromotionBoundary(carrier, leadership []byte) ([]byte, error) {
	if len(carrier) < 33 || len(carrier) > MaxHAPromotionCarrierBytes || len(leadership) == 0 || len(leadership) > MaxHAPeerLeadershipFrameBytes {
		return nil, ErrMemberAuthority
	}
	f, err := UnmarshalFrame(leadership)
	if err != nil || f.Kind != KindLeadershipEstablished || f.Store != StoreNone {
		return nil, ErrMemberAuthority
	}
	out := make([]byte, 20+len(carrier)+len(leadership))
	binary.BigEndian.PutUint32(out[:4], uint32(len(out)-4))
	copy(out[4:12], haPeerBoundaryMagic[:])
	binary.BigEndian.PutUint32(out[12:16], uint32(len(carrier)))
	binary.BigEndian.PutUint32(out[16:20], uint32(len(leadership)))
	copy(out[20:], carrier)
	copy(out[20+len(carrier):], leadership)
	return out, nil
}
func UnmarshalHAPeerPromotionBoundary(encoded []byte) (carrier, leadership []byte, err error) {
	if len(encoded) < 54 || len(encoded) > 4+maxHAPeerBoundaryBodyBytes || int(binary.BigEndian.Uint32(encoded[:4])) != len(encoded)-4 || !bytes.Equal(encoded[4:12], haPeerBoundaryMagic[:]) {
		return nil, nil, ErrMemberAuthority
	}
	c, n := int(binary.BigEndian.Uint32(encoded[12:16])), int(binary.BigEndian.Uint32(encoded[16:20]))
	if c < 33 || c > MaxHAPromotionCarrierBytes || n < 1 || n > MaxHAPeerLeadershipFrameBytes || 20+c+n != len(encoded) {
		return nil, nil, ErrMemberAuthority
	}
	carrier, leadership = encoded[20:20+c], encoded[20+c:]
	f, e := UnmarshalFrame(leadership)
	if e != nil || f.Kind != KindLeadershipEstablished || f.Store != StoreNone {
		return nil, nil, ErrMemberAuthority
	}
	return carrier, leadership, nil
}
