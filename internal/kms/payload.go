package kms

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"regexp"
	"strings"
)

const (
	ProtectedPayloadVersion = uint16(1)
	MasterKeyBytes          = 32
)

var (
	ErrInvalidProtectedPayload = errors.New("invalid KMS protected payload")
	payloadIdentifier          = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	payloadMagic               = [8]byte{'H', 'K', 'M', 'S', 'K', 'E', 'Y', '1'}
)

type PayloadBinding struct {
	InstanceID string
	SlotID     string
}

func (b PayloadBinding) Validate() error {
	if !payloadIdentifier.MatchString(b.InstanceID) || !payloadIdentifier.MatchString(b.SlotID) {
		return errors.New("KMS payload instance and Slot identifiers are invalid")
	}
	return nil
}

func EncodeProtectedPayload(binding PayloadBinding, masterKey []byte) ([]byte, error) {
	if err := binding.Validate(); err != nil {
		return nil, err
	}
	if len(masterKey) != MasterKeyBytes {
		return nil, errors.New("Master Key must contain exactly 32 bytes")
	}
	payload := make([]byte, ProtectedPayloadSize)
	copy(payload[0:8], payloadMagic[:])
	binary.BigEndian.PutUint16(payload[8:10], ProtectedPayloadVersion)
	// flags [10:12] and reserved [108:112] remain zero in v1.
	instanceDigest := sha256.Sum256([]byte(binding.InstanceID))
	slotDigest := sha256.Sum256([]byte(binding.SlotID))
	copy(payload[12:44], instanceDigest[:])
	copy(payload[44:76], slotDigest[:])
	copy(payload[76:108], masterKey)
	return payload, nil
}

func DecodeProtectedPayload(binding PayloadBinding, payload []byte) ([]byte, error) {
	if err := binding.Validate(); err != nil {
		return nil, err
	}
	if len(payload) != ProtectedPayloadSize || subtle.ConstantTimeCompare(payload[0:8], payloadMagic[:]) != 1 ||
		binary.BigEndian.Uint16(payload[8:10]) != ProtectedPayloadVersion ||
		binary.BigEndian.Uint16(payload[10:12]) != 0 ||
		binary.BigEndian.Uint32(payload[108:112]) != 0 {
		return nil, ErrInvalidProtectedPayload
	}
	instanceDigest := sha256.Sum256([]byte(binding.InstanceID))
	slotDigest := sha256.Sum256([]byte(binding.SlotID))
	if subtle.ConstantTimeCompare(payload[12:44], instanceDigest[:]) != 1 ||
		subtle.ConstantTimeCompare(payload[44:76], slotDigest[:]) != 1 {
		return nil, ErrInvalidProtectedPayload
	}
	return append([]byte(nil), payload[76:108]...), nil
}

func BindingSHA256(value string) (string, error) {
	if strings.TrimSpace(value) == "" || len(value) > 128 {
		return "", errors.New("KMS binding value is invalid")
	}
	digest := sha256.Sum256([]byte(value))
	const hex = "0123456789abcdef"
	encoded := make([]byte, sha256.Size*2)
	for index, value := range digest {
		encoded[index*2] = hex[value>>4]
		encoded[index*2+1] = hex[value&0x0f]
	}
	return string(encoded), nil
}
