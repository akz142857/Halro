package app

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

// Idempotent creates
//
// A create mints a server-side ID, so a retry after a lost response produces a
// second record rather than the same one. The commit protocol makes the
// mutation's outcome durable and recoverable, but it cannot tell a retry from a
// second deliberate create — only the caller knows that, and the way it says so
// is the Idempotency-Key.
//
// Deriving the record ID from that key turns a retry into a storage collision
// instead of a duplicate. This is the shape the Gateway Key and pricing paths
// already use; it is applied here to Provider, Deployment, Route and Project,
// which the API-chain review left as the last creates without it.
//
// The key is required rather than optional. An optional one is only honoured by
// callers that already thought about retries, which is exactly the set of
// callers that did not need it.

// adminCreateIdempotencyKey reads and validates the header, and answers whether
// the request may continue.
func adminCreateIdempotencyKey(writer http.ResponseWriter, request *http.Request) (string, bool) {
	key := strings.TrimSpace(request.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 256 {
		adminBadRequestCode(writer, "idempotency_key_required", "Idempotency-Key is required")
		return "", false
	}
	return key, true
}

// adminCreateID derives a create's record ID from the actor and the idempotency
// key.
//
// The actor is part of the digest so two administrators cannot collide by
// choosing the same key, and the resource is part of it so one key used against
// two endpoints does not produce related IDs.
func adminCreateID(prefix, resource, actor, key string) string {
	digest := sha256.New()
	digest.Write([]byte("halro:admin-create-idempotency:v1\x00"))
	digest.Write([]byte(resource))
	digest.Write([]byte{0})
	digest.Write([]byte(actor))
	digest.Write([]byte{0})
	digest.Write([]byte(key))
	return deterministicMutationID(prefix, "sha256:"+hex.EncodeToString(digest.Sum(nil)))
}

// writeAdminCreateReplay answers a retried create.
//
// It reports the collision rather than returning the existing record, because
// the two are not the same statement: replaying would claim this request
// created what it found, and the stored record may have been built from a
// different body under the same key. The existing ID is named so the caller can
// fetch it and decide.
func writeAdminCreateReplay(writer http.ResponseWriter, err error, resource, existingID string) bool {
	if !errors.Is(err, boltstore.ErrAlreadyExists) {
		return false
	}
	writeJSON(writer, http.StatusConflict, map[string]string{
		"code":  resource + "_idempotency_replay",
		"error": "this request already created " + resource + " " + existingID,
		"id":    existingID,
	})
	return true
}
