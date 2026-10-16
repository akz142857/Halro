package vault

import (
	"crypto/hkdf"
	"crypto/sha256"
	"errors"
	"fmt"
)

// DeriveArchiveHMACKey derives the key that authenticates each row of the
// governance archive (archive.db). It hangs off the Ledger key like the
// usage history key, so an archive cannot be carried to an instance whose
// Ledger it does not describe.
func DeriveArchiveHMACKey(ledgerKey []byte) ([]byte, error) {
	if len(ledgerKey) != 32 {
		return nil, errors.New("invalid ledger key for archive derivation")
	}
	key, err := hkdf.Key(sha256.New, ledgerKey, []byte("halro:archive:v1"), "halro:archive-row-hmac-key:v1", 32)
	if err != nil {
		return nil, fmt.Errorf("derive archive HMAC key: %w", err)
	}
	return key, nil
}
