package vault

import (
	"crypto/hkdf"
	"crypto/sha256"
	"errors"
	"fmt"
)

// DeriveGovernanceHMACKey gives the Outcome journal a cryptographic domain
// separate from both Accounting and Audit. The input is the envelope-backed
// Ledger key, so Master Key rotation does not invalidate historical outcomes.
func DeriveGovernanceHMACKey(ledgerKey []byte) ([]byte, error) {
	if len(ledgerKey) != 32 {
		return nil, errors.New("invalid ledger key for governance derivation")
	}
	key, err := hkdf.Key(sha256.New, ledgerKey, []byte("halro:governance:v1"), "halro:governance-hmac-key:v1", 32)
	if err != nil {
		return nil, fmt.Errorf("derive governance HMAC key: %w", err)
	}
	return key, nil
}
