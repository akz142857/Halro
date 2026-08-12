package bolt

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	bbolt "go.etcd.io/bbolt"
)

func (s *Store) ListAllAdminMFAAuthenticators(ctx context.Context) ([]domain.AdminMFAAuthenticator, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var values []domain.AdminMFAAuthenticator
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketAdminMFAAuthenticators).ForEach(func(_, raw []byte) error {
			var value domain.AdminMFAAuthenticator
			if err := json.Unmarshal(raw, &value); err != nil {
				return err
			}
			values = append(values, value)
			return nil
		})
	})
	return values, err
}

func (s *Store) PutAdminUser(
	ctx context.Context,
	user domain.AdminUser,
	expectedRevision uint64,
) (domain.AdminUser, error) {
	return s.PutAdminUserWithAuditIntent(ctx, user, expectedRevision, nil)
}

// PutAdminUserWithAuditIntent commits an Admin API identity mutation and its
// audit intent in one bbolt transaction. Offline/session-maintenance callers
// use PutAdminUser when there is no Admin HTTP mutation to account for.
func (s *Store) PutAdminUserWithAuditIntent(
	ctx context.Context,
	user domain.AdminUser,
	expectedRevision uint64,
	intent *domain.AdminAuditIntent,
) (domain.AdminUser, error) {
	if err := user.Validate(); err != nil {
		return domain.AdminUser{}, err
	}
	if err := ctx.Err(); err != nil {
		return domain.AdminUser{}, err
	}
	err := s.db.Update(func(tx *bbolt.Tx) error {
		if err := putVersioned(tx.Bucket(bucketAdminUsers), user.Username, expectedRevision, &user); err != nil {
			return err
		}
		return putAdminAuditIntentTx(tx, intent)
	})
	return user, err
}

// CreateFirstAdmin atomically proves that the admin bucket is empty and
// creates its first user. This prevents concurrent setup requests from both
// succeeding.
func (s *Store) CreateFirstAdmin(
	ctx context.Context,
	user domain.AdminUser,
) (domain.AdminUser, error) {
	if err := user.Validate(); err != nil {
		return domain.AdminUser{}, err
	}
	if err := ctx.Err(); err != nil {
		return domain.AdminUser{}, err
	}
	err := s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketAdminUsers)
		if bucket.Stats().KeyN != 0 {
			return ErrAdminInitialized
		}
		return putVersioned(bucket, user.Username, 0, &user)
	})
	return user, err
}

func (s *Store) GetAdminUser(ctx context.Context, username string) (domain.AdminUser, error) {
	var user domain.AdminUser
	err := s.getJSON(ctx, bucketAdminUsers, username, &user)
	return user, err
}

// ListAdminUsers returns every administrator, ordered by username. Small,
// operator-scale data (there is no reason to ever expect more than a
// handful of admin accounts), so an unpaginated full scan is the right
// shape rather than premature cursor-based pagination.
func (s *Store) ListAdminUsers(ctx context.Context) ([]domain.AdminUser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var users []domain.AdminUser
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketAdminUsers).ForEach(func(_, raw []byte) error {
			var user domain.AdminUser
			if err := json.Unmarshal(raw, &user); err != nil {
				return err
			}
			users = append(users, user)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(users, func(i, j int) bool { return users[i].Username < users[j].Username })
	return users, nil
}

// DeleteAdminUser removes an administrator outright — unlike most records in
// this store, an admin account has no soft-delete/DeletedAt lifecycle to
// preserve, and its sessions and MFA state are deleted alongside it in the
// same transaction so nothing outlives the identity it belonged to.
func (s *Store) DeleteAdminUser(ctx context.Context, username string, expectedRevision uint64) error {
	return s.DeleteAdminUserWithAuditIntent(ctx, username, expectedRevision, nil)
}

// DeleteAdminUserWithAuditIntent removes the identity and all of its session
// and MFA state in the same transaction that makes the Admin audit record
// durable.
func (s *Store) DeleteAdminUserWithAuditIntent(
	ctx context.Context,
	username string,
	expectedRevision uint64,
	intent *domain.AdminAuditIntent,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		users := tx.Bucket(bucketAdminUsers)
		raw := users.Get([]byte(username))
		if raw == nil {
			return ErrNotFound
		}
		var current domain.AdminUser
		if err := json.Unmarshal(raw, &current); err != nil {
			return err
		}
		if current.Revision != expectedRevision {
			return ErrRevisionConflict
		}
		if err := users.Delete([]byte(username)); err != nil {
			return err
		}
		// Sessions, MFA challenges, authenticators and recovery codes: the
		// same cleanup ResetAdminMFAIdentity already does for a rotated
		// identity, reused here rather than re-implementing key schemes
		// (session records matched by field, MFA records by key prefix) a
		// second time.
		if err := deleteAdminIdentityRecords(tx, username, true); err != nil {
			return err
		}
		return putAdminAuditIntentTx(tx, intent)
	})
}

func (s *Store) AdminUserCount(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	var count int
	err := s.db.View(func(tx *bbolt.Tx) error {
		count = tx.Bucket(bucketAdminUsers).Stats().KeyN
		return nil
	})
	return count, err
}

func (s *Store) PutAdminSession(ctx context.Context, session domain.AdminSession) error {
	if err := session.Validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	encoded, err := json.Marshal(session)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketAdminSessions).Put(session.IDHash[:], encoded)
	})
}

func (s *Store) GetAdminSession(
	ctx context.Context,
	hash [32]byte,
) (domain.AdminSession, error) {
	if err := ctx.Err(); err != nil {
		return domain.AdminSession{}, err
	}
	var session domain.AdminSession
	err := s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketAdminSessions).Get(hash[:])
		if raw == nil {
			return ErrNotFound
		}
		return json.Unmarshal(raw, &session)
	})
	return session, err
}

func (s *Store) DeleteAdminSession(ctx context.Context, hash [32]byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketAdminSessions).Delete(hash[:])
	})
}

func (s *Store) DeleteAdminSessionsForUser(ctx context.Context, username string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketAdminSessions)
		cursor := bucket.Cursor()
		for key, raw := cursor.First(); key != nil; key, raw = cursor.Next() {
			var session domain.AdminSession
			if err := json.Unmarshal(raw, &session); err != nil {
				return err
			}
			if session.Username == username {
				if err := cursor.Delete(); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// InvalidateAdminAuthenticationForRestore makes credentials captured in a
// backup unusable before that backup is published as the live data set. The
// user generation changes and both transient authentication buckets are
// cleared in one transaction so a restored database can never expose a mixed
// state.
func (s *Store) InvalidateAdminAuthenticationForRestore(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		users := tx.Bucket(bucketAdminUsers)
		cursor := users.Cursor()
		for key, raw := cursor.First(); key != nil; key, raw = cursor.Next() {
			var user domain.AdminUser
			if err := json.Unmarshal(raw, &user); err != nil {
				return fmt.Errorf("decode admin user %q during restore: %w", key, err)
			}
			if user.SessionGeneration == ^uint64(0) || user.Revision == ^uint64(0) {
				return errors.New("admin authentication version is exhausted")
			}
			user.SessionGeneration++
			user.Revision++
			encoded, err := json.Marshal(user)
			if err != nil {
				return err
			}
			if err := users.Put(key, encoded); err != nil {
				return err
			}
		}
		for _, bucketName := range [][]byte{bucketAdminSessions, bucketAdminMFAChallenges} {
			bucket := tx.Bucket(bucketName)
			bucketCursor := bucket.Cursor()
			for key, _ := bucketCursor.First(); key != nil; key, _ = bucketCursor.Next() {
				if err := bucketCursor.Delete(); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func adminMFAKey(username, id string) string { return username + "\x00" + id }

func (s *Store) PutAdminMFAAuthenticator(ctx context.Context, value domain.AdminMFAAuthenticator, expected uint64) (domain.AdminMFAAuthenticator, error) {
	if err := value.Validate(); err != nil {
		return value, err
	}
	if err := ctx.Err(); err != nil {
		return value, err
	}
	err := s.db.Update(func(tx *bbolt.Tx) error {
		return putVersioned(tx.Bucket(bucketAdminMFAAuthenticators), adminMFAKey(value.Username, value.ID), expected, &value)
	})
	return value, err
}

func (s *Store) GetAdminMFAAuthenticator(ctx context.Context, username, id string) (domain.AdminMFAAuthenticator, error) {
	var value domain.AdminMFAAuthenticator
	err := s.getJSON(ctx, bucketAdminMFAAuthenticators, adminMFAKey(username, id), &value)
	return value, err
}

func (s *Store) ListAdminMFAAuthenticators(ctx context.Context, username string) ([]domain.AdminMFAAuthenticator, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	values := []domain.AdminMFAAuthenticator{}
	err := s.db.View(func(tx *bbolt.Tx) error {
		prefix := []byte(username + "\x00")
		cursor := tx.Bucket(bucketAdminMFAAuthenticators).Cursor()
		for key, raw := cursor.Seek(prefix); key != nil && bytes.HasPrefix(key, prefix); key, raw = cursor.Next() {
			var value domain.AdminMFAAuthenticator
			if err := json.Unmarshal(raw, &value); err != nil {
				return err
			}
			values = append(values, value)
		}
		return nil
	})
	return values, err
}

// AcceptAdminMFATimeStep atomically prevents concurrent or repeated use of a TOTP time step.
func (s *Store) AcceptAdminMFATimeStep(ctx context.Context, username, id string, step int64, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketAdminMFAAuthenticators)
		key := []byte(adminMFAKey(username, id))
		raw := bucket.Get(key)
		if raw == nil {
			return ErrNotFound
		}
		var value domain.AdminMFAAuthenticator
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		if value.Status != domain.AdminMFAStatusActive || step <= value.LastAcceptedTimeStep {
			return ErrRevisionConflict
		}
		value.LastAcceptedTimeStep = step
		used := now.UTC()
		value.LastUsedAt = &used
		value.Revision++
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		return bucket.Put(key, encoded)
	})
}

func (s *Store) ReplaceAdminMFARecoveryCodes(ctx context.Context, username string, codes []domain.AdminMFARecoveryCode) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, code := range codes {
		if code.Username != username {
			return errors.New("MFA recovery code user mismatch")
		}
		if err := code.Validate(); err != nil {
			return err
		}
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketAdminMFARecoveryCodes)
		prefix := []byte(username + "\x00")
		cursor := bucket.Cursor()
		for key, _ := cursor.Seek(prefix); key != nil && bytes.HasPrefix(key, prefix); key, _ = cursor.Next() {
			if err := cursor.Delete(); err != nil {
				return err
			}
		}
		for _, code := range codes {
			raw, err := json.Marshal(code)
			if err != nil {
				return err
			}
			if err := bucket.Put([]byte(adminMFAKey(username, code.ID)), raw); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) ConsumeAdminMFARecoveryCode(ctx context.Context, username string, hash [32]byte, now time.Time) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	remaining := 0
	found := false
	err := s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketAdminMFARecoveryCodes)
		prefix := []byte(username + "\x00")
		cursor := bucket.Cursor()
		for key, raw := cursor.Seek(prefix); key != nil && bytes.HasPrefix(key, prefix); key, raw = cursor.Next() {
			var code domain.AdminMFARecoveryCode
			if err := json.Unmarshal(raw, &code); err != nil {
				return err
			}
			if code.UsedAt == nil && subtle.ConstantTimeCompare(code.CodeHash[:], hash[:]) == 1 {
				used := now.UTC()
				code.UsedAt = &used
				encoded, _ := json.Marshal(code)
				if err := bucket.Put(key, encoded); err != nil {
					return err
				}
				found = true
			} else if code.UsedAt == nil {
				remaining++
			}
		}
		if !found {
			return ErrNotFound
		}
		return nil
	})
	return remaining, err
}

func (s *Store) CountUnusedAdminMFARecoveryCodes(ctx context.Context, username string) (int, error) {
	count := 0
	err := s.db.View(func(tx *bbolt.Tx) error {
		prefix := []byte(username + "\x00")
		cursor := tx.Bucket(bucketAdminMFARecoveryCodes).Cursor()
		for key, raw := cursor.Seek(prefix); key != nil && bytes.HasPrefix(key, prefix); key, raw = cursor.Next() {
			var code domain.AdminMFARecoveryCode
			if err := json.Unmarshal(raw, &code); err != nil {
				return err
			}
			if code.UsedAt == nil {
				count++
			}
		}
		return nil
	})
	return count, err
}

func (s *Store) PutAdminMFAChallenge(ctx context.Context, value domain.AdminMFAChallenge) error {
	if err := value.Validate(); err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketAdminMFAChallenges)
		cursor := bucket.Cursor()
		for key, existingRaw := cursor.First(); key != nil; key, existingRaw = cursor.Next() {
			var existing domain.AdminMFAChallenge
			if err := json.Unmarshal(existingRaw, &existing); err != nil {
				return err
			}
			if existing.Username == value.Username && existing.SessionGeneration == value.SessionGeneration {
				if value.CreatedAt.Before(existing.ExpiresAt) && existing.AttemptsRemaining < value.AttemptsRemaining {
					value.AttemptsRemaining = existing.AttemptsRemaining
				}
				if err := cursor.Delete(); err != nil {
					return err
				}
			}
		}
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		return bucket.Put(value.IDHash[:], raw)
	})
}

func (s *Store) ClaimAdminMFAChallenge(ctx context.Context, hash [32]byte, now time.Time) (domain.AdminMFAChallenge, error) {
	var value domain.AdminMFAChallenge
	err := s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketAdminMFAChallenges)
		raw := bucket.Get(hash[:])
		if raw == nil {
			return ErrNotFound
		}
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		if !now.Before(value.ExpiresAt) {
			_ = bucket.Delete(hash[:])
			return ErrNotFound
		}
		if value.AttemptsRemaining == 0 {
			return ErrNotFound
		}
		if value.Claimed {
			return ErrMFAClaimed
		}
		value.Claimed = true
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		return bucket.Put(hash[:], encoded)
	})
	return value, err
}

func (s *Store) CompleteAdminMFAChallenge(ctx context.Context, hash [32]byte) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketAdminMFAChallenges)
		raw := bucket.Get(hash[:])
		if raw == nil {
			return ErrNotFound
		}
		var value domain.AdminMFAChallenge
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		if !value.Claimed {
			return ErrRevisionConflict
		}
		return bucket.Delete(hash[:])
	})
}

func (s *Store) GetAdminMFAChallenge(ctx context.Context, hash [32]byte) (domain.AdminMFAChallenge, error) {
	var value domain.AdminMFAChallenge
	if err := ctx.Err(); err != nil {
		return value, err
	}
	err := s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketAdminMFAChallenges).Get(hash[:])
		if raw == nil {
			return ErrNotFound
		}
		return json.Unmarshal(raw, &value)
	})
	return value, err
}

func (s *Store) DeleteAdminMFAChallenge(ctx context.Context, hash [32]byte) error {
	return s.db.Update(func(tx *bbolt.Tx) error { return tx.Bucket(bucketAdminMFAChallenges).Delete(hash[:]) })
}

func (s *Store) FailAdminMFAChallenge(ctx context.Context, hash [32]byte) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketAdminMFAChallenges)
		raw := bucket.Get(hash[:])
		if raw == nil {
			return ErrNotFound
		}
		var value domain.AdminMFAChallenge
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		if value.AttemptsRemaining <= 1 {
			value.AttemptsRemaining = 0
			value.Claimed = false
			encoded, err := json.Marshal(value)
			if err != nil {
				return err
			}
			return bucket.Put(hash[:], encoded)
		}
		value.AttemptsRemaining--
		value.Claimed = false
		encoded, _ := json.Marshal(value)
		return bucket.Put(hash[:], encoded)
	})
}

func (s *Store) ActivateAdminMFAAuthenticator(ctx context.Context, username, id string, step int64, now time.Time, limit int) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketAdminMFAAuthenticators)
		prefix := []byte(username + "\x00")
		active := 0
		cursor := bucket.Cursor()
		for key, raw := cursor.Seek(prefix); key != nil && bytes.HasPrefix(key, prefix); key, raw = cursor.Next() {
			var value domain.AdminMFAAuthenticator
			if err := json.Unmarshal(raw, &value); err != nil {
				return err
			}
			if value.Status == domain.AdminMFAStatusActive {
				active++
			}
		}
		if active >= limit {
			return ErrMFALimit
		}
		key := []byte(adminMFAKey(username, id))
		raw := bucket.Get(key)
		if raw == nil {
			return ErrNotFound
		}
		var value domain.AdminMFAAuthenticator
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		if value.Status != domain.AdminMFAStatusPending || value.ExpiresAt == nil || !now.Before(*value.ExpiresAt) {
			return ErrRevisionConflict
		}
		value.Status, value.ConfirmedAt, value.ExpiresAt = domain.AdminMFAStatusActive, &now, nil
		value.LastAcceptedTimeStep = step
		value.Revision++
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		return bucket.Put(key, encoded)
	})
}

func (s *Store) RevokeAdminMFAAuthenticator(ctx context.Context, username, id string, required bool) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketAdminMFAAuthenticators)
		prefix := []byte(username + "\x00")
		active := 0
		cursor := bucket.Cursor()
		for key, raw := cursor.Seek(prefix); key != nil && bytes.HasPrefix(key, prefix); key, raw = cursor.Next() {
			var value domain.AdminMFAAuthenticator
			if err := json.Unmarshal(raw, &value); err != nil {
				return err
			}
			if value.Status == domain.AdminMFAStatusActive {
				active++
			}
		}
		key := []byte(adminMFAKey(username, id))
		raw := bucket.Get(key)
		if raw == nil {
			return ErrNotFound
		}
		var value domain.AdminMFAAuthenticator
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		if value.Status != domain.AdminMFAStatusActive {
			return ErrRevisionConflict
		}
		if required && active <= 1 {
			return ErrMFARequired
		}
		value.Status, value.SecretCiphertext = domain.AdminMFAStatusRevoked, nil
		value.Revision++
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		return bucket.Put(key, encoded)
	})
}

func (s *Store) DeleteAdminMFAForUser(ctx context.Context, username string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		for _, bucketName := range [][]byte{bucketAdminMFAAuthenticators, bucketAdminMFARecoveryCodes} {
			bucket := tx.Bucket(bucketName)
			prefix := []byte(username + "\x00")
			cursor := bucket.Cursor()
			for key, _ := cursor.Seek(prefix); key != nil && bytes.HasPrefix(key, prefix); key, _ = cursor.Next() {
				if err := cursor.Delete(); err != nil {
					return err
				}
			}
		}
		bucket := tx.Bucket(bucketAdminMFAChallenges)
		cursor := bucket.Cursor()
		for key, raw := cursor.First(); key != nil; key, raw = cursor.Next() {
			var value domain.AdminMFAChallenge
			if err := json.Unmarshal(raw, &value); err != nil {
				return err
			}
			if value.Username == username {
				if err := cursor.Delete(); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// RotateAdminIdentity atomically advances the security generation and removes
// every session and pre-auth challenge for one administrator.
func (s *Store) RotateAdminIdentity(ctx context.Context, username string) (domain.AdminUser, error) {
	var user domain.AdminUser
	err := s.db.Update(func(tx *bbolt.Tx) error {
		var err error
		user, err = rotateAdminIdentityTx(tx, username)
		return err
	})
	return user, err
}

func rotateAdminIdentityTx(tx *bbolt.Tx, username string) (domain.AdminUser, error) {
	users := tx.Bucket(bucketAdminUsers)
	raw := users.Get([]byte(username))
	if raw == nil {
		return domain.AdminUser{}, ErrNotFound
	}
	var user domain.AdminUser
	if err := json.Unmarshal(raw, &user); err != nil {
		return user, err
	}
	if user.SessionGeneration == ^uint64(0) || user.Revision == ^uint64(0) {
		return user, errors.New("admin identity version exhausted")
	}
	user.SessionGeneration++
	user.Revision++
	user.UpdatedAt = time.Now().UTC()
	encoded, err := json.Marshal(user)
	if err != nil {
		return user, err
	}
	if err = users.Put([]byte(username), encoded); err != nil {
		return user, err
	}
	if err = deleteAdminIdentityRecords(tx, username, false); err != nil {
		return user, err
	}
	return user, nil
}

func setPendingMFAAuditTx(tx *bbolt.Tx, user *domain.AdminUser, intent domain.AdminMFAAuditIntent) error {
	if err := intent.Validate(); err != nil {
		return err
	}
	if user.PendingMFAAudit != nil && user.PendingMFAAudit.EventID != intent.EventID {
		return errors.New("a previous MFA audit event is still pending delivery")
	}
	user.PendingMFAAudit = &intent
	encoded, err := json.Marshal(*user)
	if err != nil {
		return err
	}
	return tx.Bucket(bucketAdminUsers).Put([]byte(user.Username), encoded)
}

func (s *Store) ClearPendingAdminMFAAudit(ctx context.Context, username, eventID string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketAdminUsers)
		raw := bucket.Get([]byte(username))
		if raw == nil {
			return ErrNotFound
		}
		var user domain.AdminUser
		if err := json.Unmarshal(raw, &user); err != nil {
			return err
		}
		if user.PendingMFAAudit == nil {
			return nil
		}
		if user.PendingMFAAudit.EventID != eventID {
			return ErrRevisionConflict
		}
		user.PendingMFAAudit = nil
		encoded, err := json.Marshal(user)
		if err != nil {
			return err
		}
		return bucket.Put([]byte(username), encoded)
	})
}

func (s *Store) ListPendingAdminMFAAudits(ctx context.Context) ([]domain.AdminMFAAuditIntent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var values []domain.AdminMFAAuditIntent
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketAdminUsers).ForEach(func(_, raw []byte) error {
			var user domain.AdminUser
			if err := json.Unmarshal(raw, &user); err != nil {
				return err
			}
			if user.PendingMFAAudit != nil {
				values = append(values, *user.PendingMFAAudit)
			}
			return nil
		})
	})
	return values, err
}

func replaceAdminMFARecoveryCodesTx(tx *bbolt.Tx, username string, codes []domain.AdminMFARecoveryCode) error {
	for _, code := range codes {
		if err := code.Validate(); err != nil {
			return err
		}
		if code.Username != username {
			return errors.New("MFA recovery code owner does not match")
		}
	}
	bucket := tx.Bucket(bucketAdminMFARecoveryCodes)
	prefix := []byte(username + "\x00")
	cursor := bucket.Cursor()
	for key, _ := cursor.Seek(prefix); key != nil && bytes.HasPrefix(key, prefix); key, _ = cursor.Next() {
		if err := cursor.Delete(); err != nil {
			return err
		}
	}
	for _, code := range codes {
		raw, err := json.Marshal(code)
		if err != nil {
			return err
		}
		if err = bucket.Put([]byte(adminMFAKey(username, code.ID)), raw); err != nil {
			return err
		}
	}
	return nil
}

// ConfirmAdminMFAEnrollment atomically activates a pending factor, creates the
// first recovery-code set when supplied, and rotates the administrator identity.
func (s *Store) ConfirmAdminMFAEnrollment(ctx context.Context, username, id string, step int64, now time.Time, limit int, codes []domain.AdminMFARecoveryCode, intent domain.AdminMFAAuditIntent) (domain.AdminUser, bool, error) {
	var user domain.AdminUser
	first := false
	err := s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketAdminMFAAuthenticators)
		prefix := []byte(username + "\x00")
		active := 0
		cursor := bucket.Cursor()
		for key, raw := cursor.Seek(prefix); key != nil && bytes.HasPrefix(key, prefix); key, raw = cursor.Next() {
			var value domain.AdminMFAAuthenticator
			if err := json.Unmarshal(raw, &value); err != nil {
				return err
			}
			if value.Status == domain.AdminMFAStatusActive {
				active++
			}
		}
		if active >= limit {
			return ErrMFALimit
		}
		key := []byte(adminMFAKey(username, id))
		raw := bucket.Get(key)
		if raw == nil {
			return ErrNotFound
		}
		var value domain.AdminMFAAuthenticator
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		if value.Status != domain.AdminMFAStatusPending || value.ExpiresAt == nil || !now.Before(*value.ExpiresAt) {
			return ErrRevisionConflict
		}
		value.Status = domain.AdminMFAStatusActive
		value.ConfirmedAt = &now
		value.ExpiresAt = nil
		value.LastAcceptedTimeStep = step
		value.Revision++
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if err = bucket.Put(key, encoded); err != nil {
			return err
		}
		if active == 0 {
			if len(codes) == 0 {
				return errors.New("first MFA authenticator requires recovery codes")
			}
			if err = replaceAdminMFARecoveryCodesTx(tx, username, codes); err != nil {
				return err
			}
			first = true
		}
		user, err = rotateAdminIdentityTx(tx, username)
		if err != nil {
			return err
		}
		return setPendingMFAAuditTx(tx, &user, intent)
	})
	return user, first, err
}

func (s *Store) ReplaceAdminMFARecoveryCodesAndRotate(ctx context.Context, username string, codes []domain.AdminMFARecoveryCode, intent domain.AdminMFAAuditIntent) (domain.AdminUser, error) {
	var user domain.AdminUser
	err := s.db.Update(func(tx *bbolt.Tx) error {
		if err := replaceAdminMFARecoveryCodesTx(tx, username, codes); err != nil {
			return err
		}
		var err error
		user, err = rotateAdminIdentityTx(tx, username)
		if err != nil {
			return err
		}
		return setPendingMFAAuditTx(tx, &user, intent)
	})
	return user, err
}

func (s *Store) RevokeAdminMFAAuthenticatorAndRotate(ctx context.Context, username, id string, required bool, clearRecovery bool, intent domain.AdminMFAAuditIntent) (domain.AdminUser, error) {
	var user domain.AdminUser
	err := s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketAdminMFAAuthenticators)
		prefix := []byte(username + "\x00")
		active := 0
		cursor := bucket.Cursor()
		for key, raw := cursor.Seek(prefix); key != nil && bytes.HasPrefix(key, prefix); key, raw = cursor.Next() {
			var value domain.AdminMFAAuthenticator
			if err := json.Unmarshal(raw, &value); err != nil {
				return err
			}
			if value.Status == domain.AdminMFAStatusActive {
				active++
			}
		}
		key := []byte(adminMFAKey(username, id))
		raw := bucket.Get(key)
		if raw == nil {
			return ErrNotFound
		}
		var value domain.AdminMFAAuthenticator
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		if value.Status != domain.AdminMFAStatusActive {
			return ErrRevisionConflict
		}
		if required && active <= 1 {
			return ErrMFARequired
		}
		value.Status = domain.AdminMFAStatusRevoked
		value.SecretCiphertext = nil
		value.Revision++
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if err = bucket.Put(key, encoded); err != nil {
			return err
		}
		if clearRecovery || active <= 1 {
			if err = replaceAdminMFARecoveryCodesTx(tx, username, nil); err != nil {
				return err
			}
		}
		user, err = rotateAdminIdentityTx(tx, username)
		if err != nil {
			return err
		}
		return setPendingMFAAuditTx(tx, &user, intent)
	})
	return user, err
}

func (s *Store) DisableAdminMFAAndRotate(ctx context.Context, username string, recoveryHash *[32]byte, intent domain.AdminMFAAuditIntent) (domain.AdminUser, error) {
	var user domain.AdminUser
	err := s.db.Update(func(tx *bbolt.Tx) error {
		if recoveryHash != nil {
			matched := false
			bucket := tx.Bucket(bucketAdminMFARecoveryCodes)
			prefix := []byte(username + "\x00")
			cursor := bucket.Cursor()
			for key, raw := cursor.Seek(prefix); key != nil && bytes.HasPrefix(key, prefix); key, raw = cursor.Next() {
				var code domain.AdminMFARecoveryCode
				if err := json.Unmarshal(raw, &code); err != nil {
					return err
				}
				if code.UsedAt == nil && subtle.ConstantTimeCompare(code.CodeHash[:], recoveryHash[:]) == 1 {
					matched = true
					break
				}
			}
			if !matched {
				return ErrNotFound
			}
		}
		for _, bucketName := range [][]byte{bucketAdminMFAAuthenticators, bucketAdminMFARecoveryCodes} {
			bucket := tx.Bucket(bucketName)
			prefix := []byte(username + "\x00")
			cursor := bucket.Cursor()
			for key, _ := cursor.Seek(prefix); key != nil && bytes.HasPrefix(key, prefix); key, _ = cursor.Next() {
				if err := cursor.Delete(); err != nil {
					return err
				}
			}
		}
		var err error
		user, err = rotateAdminIdentityTx(tx, username)
		if err != nil {
			return err
		}
		return setPendingMFAAuditTx(tx, &user, intent)
	})
	return user, err
}

// ResetAdminMFAIdentity additionally removes authenticators and recovery codes
// in the same transaction as identity invalidation.
func (s *Store) ResetAdminMFAIdentity(ctx context.Context, username string) (domain.AdminUser, error) {
	var user domain.AdminUser
	err := s.db.Update(func(tx *bbolt.Tx) error {
		users := tx.Bucket(bucketAdminUsers)
		raw := users.Get([]byte(username))
		if raw == nil {
			return ErrNotFound
		}
		if err := json.Unmarshal(raw, &user); err != nil {
			return err
		}
		user.SessionGeneration++
		user.Revision++
		user.UpdatedAt = time.Now().UTC()
		encoded, err := json.Marshal(user)
		if err != nil {
			return err
		}
		if err := users.Put([]byte(username), encoded); err != nil {
			return err
		}
		return deleteAdminIdentityRecords(tx, username, true)
	})
	return user, err
}

func deleteAdminIdentityRecords(tx *bbolt.Tx, username string, includeMFA bool) error {
	for _, bucketName := range [][]byte{bucketAdminSessions, bucketAdminMFAChallenges} {
		bucket := tx.Bucket(bucketName)
		cursor := bucket.Cursor()
		for key, raw := cursor.First(); key != nil; key, raw = cursor.Next() {
			matches := false
			if bytes.Equal(bucketName, bucketAdminSessions) {
				var value domain.AdminSession
				if err := json.Unmarshal(raw, &value); err != nil {
					return err
				}
				matches = value.Username == username
			} else {
				var value domain.AdminMFAChallenge
				if err := json.Unmarshal(raw, &value); err != nil {
					return err
				}
				matches = value.Username == username
			}
			if matches {
				if err := cursor.Delete(); err != nil {
					return err
				}
			}
		}
	}
	if includeMFA {
		prefix := []byte(username + "\x00")
		for _, bucketName := range [][]byte{bucketAdminMFAAuthenticators, bucketAdminMFARecoveryCodes} {
			cursor := tx.Bucket(bucketName).Cursor()
			for key, _ := cursor.Seek(prefix); key != nil && bytes.HasPrefix(key, prefix); key, _ = cursor.Next() {
				if err := cursor.Delete(); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
