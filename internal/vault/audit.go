package vault

import (
	"crypto/hkdf"
	"crypto/sha256"
	"errors"
	"fmt"
)

func DeriveAuditHMACKey(masterKey []byte) ([]byte, error) {
	if len(masterKey) != MasterKeySize {
		return nil, errors.New("invalid master key for audit derivation")
	}
	key, err := hkdf.Key(
		sha256.New,
		masterKey,
		[]byte("halro:audit:v1"),
		"halro:audit-hmac-key:v1",
		32,
	)
	if err != nil {
		return nil, fmt.Errorf("derive audit HMAC key: %w", err)
	}
	return key, nil
}
