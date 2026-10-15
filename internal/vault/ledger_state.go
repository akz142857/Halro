package vault

import (
	"crypto/hkdf"
	"crypto/sha256"
	"errors"
	"fmt"
)

// DeriveLedgerStateMACKey is the key that authenticates the accounting state
// snapshot. It hangs off the Ledger key, so a snapshot cannot be carried to an
// instance whose Ledger it does not describe.
func DeriveLedgerStateMACKey(ledgerKey []byte) ([]byte, error) {
	if len(ledgerKey) != 32 {
		return nil, errors.New("invalid ledger key for state snapshot derivation")
	}
	key, err := hkdf.Key(sha256.New, ledgerKey, []byte("halro:ledger-state:v1"), "halro:ledger-hot-state-mac-key:v1", 32)
	if err != nil {
		return nil, fmt.Errorf("derive ledger state snapshot key: %w", err)
	}
	return key, nil
}
