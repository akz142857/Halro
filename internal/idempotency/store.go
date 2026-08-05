// Package idempotency validates the Idempotency-Key header callers send on
// resource-creating requests.
//
// It once also held a durable request-lifecycle store — Open, Reserve,
// Transition, Get — built for a design the gateway settled differently: the
// implementation that ships keys idempotency off ProviderResource records in
// the metadata store, under the same revision checks as the resource itself.
// The unreached store stayed behind with tests attached, which made it read as
// a second live implementation of the same thing and left open which one was
// authoritative. Its history is in git if it is ever wanted back.
package idempotency

import (
	"errors"
	"unicode"
)

// ValidateKey enforces the header's shape before it is used as a lookup key.
func ValidateKey(key string) error {
	if len(key) < 1 || len(key) > 128 {
		return errors.New("idempotency key must contain 1 to 128 characters")
	}
	for _, r := range key {
		if r > unicode.MaxASCII || unicode.IsSpace(r) || unicode.IsControl(r) {
			return errors.New("idempotency key must be visible ASCII without whitespace")
		}
	}
	return nil
}
