package vault

import (
	"crypto/hkdf"
	"crypto/sha256"
	"errors"
	"fmt"
)

func DeriveAdminSessionKey(masterKey []byte) ([]byte, error) {
	if len(masterKey) != MasterKeySize {
		return nil, errors.New("invalid master key for admin session derivation")
	}
	key, err := hkdf.Key(
		sha256.New,
		masterKey,
		[]byte("halro:admin-session:v1"),
		"halro:admin-csrf-key:v1",
		32,
	)
	if err != nil {
		return nil, fmt.Errorf("derive admin session key: %w", err)
	}
	return key, nil
}
