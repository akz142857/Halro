package vault

import (
	"crypto/hkdf"
	"crypto/sha256"
	"errors"
	"fmt"
)

// DeriveMetadataJournalHMACKey is the bootstrap-only path for the metadata
// journal's frame integrity key, and it mirrors DeriveLedgerHMACKey exactly
// except for its domain separation strings (halro:metadata:v1, from §6.1.4 of
// the HA design).
//
// The same reasoning applies for the same reason: deriving on every open would
// make a Master Key rotation silently invalidate every historical frame, since
// rotation changes the derivation input. The key of record lives in an
// encrypted Vault envelope in `meta`, class D, so it travels with the metadata
// a Replica needs in order to authenticate the journal at all.
func DeriveMetadataJournalHMACKey(masterKey []byte) ([]byte, error) {
	if len(masterKey) != MasterKeySize {
		return nil, errors.New("invalid master key for metadata journal derivation")
	}
	key, err := hkdf.Key(
		sha256.New,
		masterKey,
		[]byte("halro:metadata:v1"),
		"halro:metadata-journal-hmac-key:v1",
		32,
	)
	if err != nil {
		return nil, fmt.Errorf("derive metadata journal HMAC key: %w", err)
	}
	return key, nil
}
