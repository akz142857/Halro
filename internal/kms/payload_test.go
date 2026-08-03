package kms

import (
	"bytes"
	"errors"
	"testing"
)

func TestProtectedPayloadRoundTripAndCanonicalLayout(t *testing.T) {
	binding := PayloadBinding{InstanceID: "instance-1", SlotID: "slot_primary"}
	masterKey := bytes.Repeat([]byte{0x42}, MasterKeyBytes)
	payload, err := EncodeProtectedPayload(binding, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) != ProtectedPayloadSize || string(payload[:8]) != "HKMSKEY1" {
		t.Fatalf("unexpected payload header or size: %d %q", len(payload), payload[:8])
	}
	if bytes.Contains(payload[:76], masterKey) || bytes.Contains(payload[108:], masterKey) {
		t.Fatal("Master Key appeared outside its fixed payload field")
	}
	decoded, err := DecodeProtectedPayload(binding, payload)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(decoded)
	if !bytes.Equal(decoded, masterKey) {
		t.Fatal("protected payload changed Master Key")
	}
}

func TestProtectedPayloadRejectsEveryBindingAndFormatMutation(t *testing.T) {
	binding := PayloadBinding{InstanceID: "instance-1", SlotID: "slot_primary"}
	payload, err := EncodeProtectedPayload(binding, bytes.Repeat([]byte{7}, MasterKeyBytes))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		binding PayloadBinding
		mutate  func([]byte) []byte
	}{
		{name: "wrong instance", binding: PayloadBinding{InstanceID: "instance-2", SlotID: binding.SlotID}},
		{name: "wrong slot", binding: PayloadBinding{InstanceID: binding.InstanceID, SlotID: "slot_recovery"}},
		{name: "short", binding: binding, mutate: func(value []byte) []byte { return value[:len(value)-1] }},
		{name: "trailing", binding: binding, mutate: func(value []byte) []byte { return append(value, 0) }},
		{name: "magic", binding: binding, mutate: flipPayloadByte(0)},
		{name: "version", binding: binding, mutate: flipPayloadByte(9)},
		{name: "flags", binding: binding, mutate: flipPayloadByte(11)},
		{name: "instance digest", binding: binding, mutate: flipPayloadByte(12)},
		{name: "slot digest", binding: binding, mutate: flipPayloadByte(44)},
		{name: "reserved", binding: binding, mutate: flipPayloadByte(111)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := bytes.Clone(payload)
			if test.mutate != nil {
				candidate = test.mutate(candidate)
			}
			if _, err := DecodeProtectedPayload(test.binding, candidate); !errors.Is(err, ErrInvalidProtectedPayload) {
				t.Fatalf("mutation error=%v", err)
			}
		})
	}
}

func flipPayloadByte(index int) func([]byte) []byte {
	return func(value []byte) []byte {
		value[index] ^= 1
		return value
	}
}
