package vault

import (
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
)

func DeriveMetricsBearerToken(masterKey []byte) ([]byte, error) {
	if len(masterKey) != MasterKeySize {
		return nil, errors.New("invalid master key for metrics token derivation")
	}
	key, err := hkdf.Key(
		sha256.New,
		masterKey,
		[]byte("halro:metrics:v1"),
		"halro:metrics-bearer-token:v1",
		32,
	)
	if err != nil {
		return nil, fmt.Errorf("derive metrics bearer token: %w", err)
	}
	defer clear(key)
	encoded := make([]byte, base64.RawURLEncoding.EncodedLen(len(key)))
	base64.RawURLEncoding.Encode(encoded, key)
	return append([]byte("hmt_"), encoded...), nil
}
