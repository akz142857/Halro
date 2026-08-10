package modelcatalog

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ReleaseTrustRoots is populated by the release build through -ldflags. Public
// roots are not secrets, but keeping the release value out of ordinary source
// builds ensures a developer binary cannot accidentally become a production
// catalog reader. Format (semicolon separated):
//
//	key-id|base64-public-key|not-before-RFC3339|not-after-RFC3339
//
// The signing private key is never accepted by Halro or this package.
var ReleaseTrustRoots string

func ProductionTrustRoots() []TrustRoot {
	roots, err := ParseTrustRoots(ReleaseTrustRoots)
	if err != nil {
		return nil
	}
	return roots
}

// ParseTrustRoots validates the public-only release representation. It is
// exported for the catalog verification tool; runtime callers use the value
// compiled into the Halro artifact and fail closed if any root is malformed.
func ParseTrustRoots(encodedRoots string) ([]TrustRoot, error) {
	if strings.TrimSpace(encodedRoots) == "" {
		return nil, errors.New("model catalog trust roots are empty")
	}
	var roots []TrustRoot
	seen := make(map[string]struct{})
	for index, encoded := range strings.Split(encodedRoots, ";") {
		fields := strings.Split(strings.TrimSpace(encoded), "|")
		if len(fields) != 4 {
			return nil, fmt.Errorf("model catalog trust root %d must contain four fields", index+1)
		}
		publicKey, keyErr := base64.StdEncoding.DecodeString(fields[1])
		notBefore, beforeErr := time.Parse(time.RFC3339, fields[2])
		notAfter, afterErr := time.Parse(time.RFC3339, fields[3])
		if fields[0] == "" {
			return nil, fmt.Errorf("model catalog trust root %d has an empty key ID", index+1)
		}
		if _, exists := seen[fields[0]]; exists {
			return nil, fmt.Errorf("model catalog trust root key ID %q is duplicated", fields[0])
		}
		if keyErr != nil || len(publicKey) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("model catalog trust root %q has an invalid Ed25519 public key", fields[0])
		}
		if beforeErr != nil || afterErr != nil || !notAfter.After(notBefore) {
			return nil, fmt.Errorf("model catalog trust root %q has an invalid validity interval", fields[0])
		}
		seen[fields[0]] = struct{}{}
		roots = append(roots, TrustRoot{KeyID: fields[0], PublicKey: ed25519.PublicKey(publicKey), NotBefore: notBefore, NotAfter: notAfter})
	}
	return roots, nil
}
