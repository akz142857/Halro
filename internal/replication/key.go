package replication

import (
	"crypto/hkdf"
	"crypto/sha256"
	"errors"
)

const clusterKeyInfo = "halro:cluster:v1"

// DeriveClusterKey separates HA role/order/handshake authentication from every
// other Master-Key domain. The incarnation is salt rather than info so a whole
// cluster restore that changes incarnation cannot authenticate old cluster
// files or handshakes even while it intentionally retains the Master Key.
func DeriveClusterKey(masterKey []byte, incarnation string) ([sha256.Size]byte, error) {
	var key [sha256.Size]byte
	if len(masterKey) != sha256.Size {
		return key, errors.New("Master Key must be 32 bytes")
	}
	if len(incarnation) == 0 || len(incarnation) > MaxIdentityBytes {
		return key, errors.New("cluster incarnation is invalid")
	}
	derived, err := hkdf.Key(sha256.New, masterKey, []byte(incarnation), clusterKeyInfo, sha256.Size)
	if err != nil {
		return key, err
	}
	copy(key[:], derived)
	clear(derived)
	return key, nil
}
