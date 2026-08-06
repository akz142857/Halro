package vault

import (
	"crypto/hkdf"
	"crypto/sha256"
	"errors"
	"fmt"
)

// DeriveLedgerHMACKey is the bootstrap-only path for the Ledger frame
// integrity key: deriving it from the Master Key on every open would make
// Master Key rotation silently invalidate every historical frame, since
// rotation changes the derivation input. The key of record lives in an
// encrypted Vault envelope (see loadLedgerHMACKey in internal/app); this
// derivation only seeds that envelope the first time, mirroring
// DeriveAuditHMACKey exactly except for its domain separation strings.
func DeriveLedgerHMACKey(masterKey []byte) ([]byte, error) {
	if len(masterKey) != MasterKeySize {
		return nil, errors.New("invalid master key for ledger derivation")
	}
	key, err := hkdf.Key(
		sha256.New,
		masterKey,
		[]byte("heimdall:ledger:v1"),
		"heimdall:ledger-hmac-key:v1",
		32,
	)
	if err != nil {
		return nil, fmt.Errorf("derive ledger HMAC key: %w", err)
	}
	return key, nil
}
