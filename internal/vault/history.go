package vault

import (
	"crypto/hkdf"
	"crypto/sha256"
	"errors"
	"fmt"
)

// DeriveHistoryHMACKey derives the key that authenticates each row of the
// usage history index. It hangs off the Ledger key, as the Governance key
// does, so an index cannot be carried to an instance whose Ledger it does not
// describe: a foreign history.db fails its key check and is rebuilt.
func DeriveHistoryHMACKey(ledgerKey []byte) ([]byte, error) {
	if len(ledgerKey) != 32 {
		return nil, errors.New("invalid ledger key for history derivation")
	}
	key, err := hkdf.Key(sha256.New, ledgerKey, []byte("halro:history:v1"), "halro:history-row-hmac-key:v1", 32)
	if err != nil {
		return nil, fmt.Errorf("derive history HMAC key: %w", err)
	}
	return key, nil
}
