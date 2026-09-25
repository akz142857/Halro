package bolt

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/akz142857/Halro/internal/domain"
)

// maxRouteSuspensions bounds the bucket.
//
// The set is small by construction — one row per credential or credential-model
// pair the upstream is refusing, written only when a scope changes state — but
// it is keyed by identifiers an operator's account list supplies, so it gets a
// ceiling like every other bucket keyed that way. Reaching it means something
// is wrong upstream at a scale where one more remembered suspension is not the
// problem, so the write is dropped rather than failing the request that
// discovered it: the gate holds the suspension in memory either way, and the
// only thing lost is that it will not survive a restart.
const maxRouteSuspensions = 4096

// routeSuspensionKey is the bucket key for one scope. The separator is the one
// domain uses inside a scope handle rather than the NUL that joins a
// credential-and-model pair, so two different pairs cannot collide into one row.
func routeSuspensionKey(kind, key string) []byte {
	return []byte(kind + "\x1f" + key)
}

// PutRouteSuspension stores one suspension, replacing any row for the same
// scope. It is called on state transition only — never on the request path —
// so the cost of a bbolt commit is paid once per suspension rather than once
// per refused attempt.
func (s *Store) PutRouteSuspension(ctx context.Context, suspension domain.RouteSuspension) error {
	if err := suspension.Validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	encoded, err := json.Marshal(suspension)
	if err != nil {
		return err
	}
	return s.update(func(tx *Tx) error {
		bucket := tx.Bucket(bucketRouteSuspensions)
		if bucket == nil {
			return errors.New("route suspensions bucket is missing")
		}
		key := routeSuspensionKey(suspension.ScopeKind, suspension.ScopeKey)
		if bucket.Get(key) == nil && bucket.Stats().KeyN >= maxRouteSuspensions {
			return nil
		}
		return bucket.Put(key, encoded)
	})
}

// DeleteRouteSuspension removes one row. A scope that is not stored is not an
// error: the gate clears suspensions for reasons that never reached the bucket
// — a short availability window, a suspension that came and went between two
// transitions — and every one of those calls through here.
func (s *Store) DeleteRouteSuspension(ctx context.Context, kind, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.update(func(tx *Tx) error {
		bucket := tx.Bucket(bucketRouteSuspensions)
		if bucket == nil {
			return errors.New("route suspensions bucket is missing")
		}
		return bucket.Delete(routeSuspensionKey(kind, key))
	})
}

// DeleteRouteSuspensionWithAuditIntent is the operator's clear.
//
// The removal and the audit record commit in one transaction, the way every
// other administrative mutation here does. That pairing is the reason this
// action waited for persistence at all: a clear that wrote nothing would have
// had nowhere to commit its record, and a standalone intent path would weaken
// exactly the property the pairing exists for.
//
// ErrNotFound distinguishes a scope that was stored from one that never was, so
// the endpoint can answer 404 rather than reporting a clear that cleared
// nothing.
func (s *Store) DeleteRouteSuspensionWithAuditIntent(
	ctx context.Context,
	kind, key string,
	intent *domain.AdminAuditIntent,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.update(func(tx *Tx) error {
		bucket := tx.Bucket(bucketRouteSuspensions)
		if bucket == nil {
			return errors.New("route suspensions bucket is missing")
		}
		bucketKey := routeSuspensionKey(kind, key)
		if bucket.Get(bucketKey) == nil {
			return ErrNotFound
		}
		if err := bucket.Delete(bucketKey); err != nil {
			return err
		}
		return putAdminAuditIntentTx(tx, intent)
	})
}

// ListRouteSuspensions reads every stored suspension, for the gate to restore
// on open and for the offline command to show.
//
// A row that no longer decodes, or that decodes into something Validate
// refuses, is skipped rather than failing the read. This bucket is node-local
// derived state: refusing to start because one remembered refusal is unreadable
// would trade a route that recovers by itself for an instance that does not
// come up at all.
func (s *Store) ListRouteSuspensions(ctx context.Context) ([]domain.RouteSuspension, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var suspensions []domain.RouteSuspension
	err := s.view(func(tx *Tx) error {
		bucket := tx.Bucket(bucketRouteSuspensions)
		if bucket == nil {
			return nil
		}
		return bucket.ForEach(func(_, raw []byte) error {
			if raw == nil {
				return nil
			}
			var suspension domain.RouteSuspension
			if err := json.Unmarshal(raw, &suspension); err != nil {
				return nil
			}
			if err := suspension.Validate(); err != nil {
				return nil
			}
			suspensions = append(suspensions, suspension)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return suspensions, nil
}
