package bolt

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"go.etcd.io/bbolt"

	"github.com/akz142857/Heimdall/internal/domain"
)

// legacyAdminUser is what an account created before domain.AdminUser.Role
// existed looks like on disk: every other field present, no role at all.
func writeLegacyAdminUser(t *testing.T, path, username string) {
	t.Helper()
	db, err := bbolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	err = db.Update(func(tx *bbolt.Tx) error {
		for _, name := range requiredBuckets() {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		record := map[string]any{
			"username":           username,
			"password_version":   1,
			"password_salt":      make([]byte, 16),
			"password_hash":      make([]byte, 32),
			"argon_memory_kib":   64 * 1024,
			"argon_iterations":   3,
			"argon_parallelism":  1,
			"session_generation": 1,
			"locale":             "system",
			"created_at":         time.Unix(0, 0).UTC(),
			"updated_at":         time.Unix(0, 0).UTC(),
			"revision":           1,
		}
		encoded, err := json.Marshal(record)
		if err != nil {
			return err
		}
		if err := tx.Bucket(bucketAdminUsers).Put([]byte(username), encoded); err != nil {
			return err
		}
		var version [8]byte
		binary.BigEndian.PutUint64(version[:], 18)
		return tx.Bucket(bucketMeta).Put(keySchemaVersion, version[:])
	})
	if err != nil {
		t.Fatal(err)
	}
}

// An account stored before roles existed has an empty role, which
// AdminUser.Validate rejects and requireAdministratorRole reads as "not an
// administrator". Without this backfill an upgraded instance cannot save its
// own preferences and refuses every administrator-gated write — its only
// operator locked out by a field they were never asked about.
func TestUpgradeBackfillsTheRoleOfAnAccountCreatedBeforeRolesExisted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.db")
	writeLegacyAdminUser(t, path, "admin")

	store, err := Open(path)
	if err != nil {
		t.Fatalf("opening a metadata file with a role-less admin failed: %v", err)
	}
	defer store.Close()

	user, err := store.GetAdminUser(context.Background(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	if user.Role != domain.AdminRoleAdministrator {
		t.Fatalf("role = %q, want administrator — before roles existed this account could do everything", user.Role)
	}
	// The record has to be writable again: saving a preference round-trips the
	// whole user through Validate, which is where the lockout showed up.
	if err := user.Validate(); err != nil {
		t.Fatalf("backfilled record still fails validation: %v", err)
	}
	user.Locale = "zh-CN"
	if _, err := store.PutAdminUser(context.Background(), user, user.Revision); err != nil {
		t.Fatalf("saving a preference on the backfilled account failed: %v", err)
	}
}

// A role that was set deliberately must survive the upgrade untouched —
// a backfill that promotes read_only accounts would hand out capability
// nobody granted.
func TestBackfillLeavesAnExplicitReadOnlyRoleAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.db")
	writeLegacyAdminUser(t, path, "admin")

	db, err := bbolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketAdminUsers).Get([]byte("admin"))
		var record map[string]any
		if err := json.Unmarshal(raw, &record); err != nil {
			return err
		}
		record["role"] = domain.AdminRoleReadOnly
		encoded, err := json.Marshal(record)
		if err != nil {
			return err
		}
		return tx.Bucket(bucketAdminUsers).Put([]byte("admin"), encoded)
	}); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()

	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	user, err := store.GetAdminUser(context.Background(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	if user.Role != domain.AdminRoleReadOnly {
		t.Fatalf("role = %q, want the stored read_only to be left alone", user.Role)
	}
}

// A role that is neither empty nor recognised is not a legacy record — no
// supported write path can produce one, so it means the file is not what this
// binary thinks it is. Promoting it to administrator would answer "something
// is wrong here" with "then have full control", so the value is left alone and
// keeps failing validation where an operator will see it.
func TestBackfillDoesNotPromoteAnUnrecognisedRole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.db")
	writeLegacyAdminUser(t, path, "admin")
	setStoredRole(t, path, "admin", "superuser")

	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	user, err := store.GetAdminUser(context.Background(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	if user.Role != "superuser" {
		t.Fatalf("role = %q, want the unrecognised value left untouched", user.Role)
	}
	if user.Validate() == nil {
		t.Fatal("an unrecognised role validated — it has to keep failing loudly")
	}
	// And it must not be usable as an administrator anywhere.
	if domain.ValidAdminRole(user.Role) {
		t.Fatal("an unrecognised role passed ValidAdminRole")
	}
}

func setStoredRole(t *testing.T, path, username, role string) {
	t.Helper()
	db, err := bbolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Update(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketAdminUsers).Get([]byte(username))
		var record map[string]any
		if err := json.Unmarshal(raw, &record); err != nil {
			return err
		}
		record["role"] = role
		encoded, err := json.Marshal(record)
		if err != nil {
			return err
		}
		return tx.Bucket(bucketAdminUsers).Put([]byte(username), encoded)
	}); err != nil {
		t.Fatal(err)
	}
}
