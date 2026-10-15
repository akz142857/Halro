package bolt

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

func testAdminUser(username, role string, now time.Time) domain.AdminUser {
	return domain.AdminUser{
		Username: username, Role: role, PasswordVersion: 1,
		PasswordSalt: []byte("0123456789abcdef"), PasswordHash: []byte("01234567890123456789012345678901"),
		ArgonMemoryKiB: 64 * 1024, ArgonIterations: 3, ArgonParallelism: 1,
		SessionGeneration: 1, CreatedAt: now, UpdatedAt: now,
	}
}

func TestListAdminUsersReturnsEveryAdministratorOrderedByUsername(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := store.CreateFirstAdmin(ctx, testAdminUser("zed", domain.AdminRoleAdministrator, now)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutAdminUser(ctx, testAdminUser("alice", domain.AdminRoleReadOnly, now), 0); err != nil {
		t.Fatal(err)
	}
	users, err := store.ListAdminUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 || users[0].Username != "alice" || users[1].Username != "zed" {
		t.Fatalf("users=%#v", users)
	}
	if users[0].Role != domain.AdminRoleReadOnly || users[1].Role != domain.AdminRoleAdministrator {
		t.Fatalf("roles not preserved: %#v", users)
	}
}

func TestDeleteAdminUserRemovesSessionsAndMFAState(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	user, err := store.CreateFirstAdmin(ctx, testAdminUser("admin", domain.AdminRoleAdministrator, now))
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.PutAdminUser(ctx, testAdminUser("viewer", domain.AdminRoleReadOnly, now), 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutAdminSession(ctx, domain.AdminSession{
		IDHash: [32]byte{1}, Username: second.Username, Generation: second.SessionGeneration,
		CreatedAt: now, LastSeenAt: now, AbsoluteExpiresAt: now.Add(time.Hour), IdleExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutAdminMFAAuthenticator(ctx, domain.AdminMFAAuthenticator{
		ID: "mfa_1", Username: second.Username, Name: "phone", Type: domain.AdminMFATypeTOTP,
		SecretCiphertext: []byte("ciphertext"), Status: domain.AdminMFAStatusActive, CreatedAt: now, ConfirmedAt: &now,
	}, 0); err != nil {
		t.Fatal(err)
	}

	if err := store.DeleteAdminUser(ctx, second.Username, second.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetAdminUser(ctx, second.Username); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted user still readable: err=%v", err)
	}
	if _, err := store.GetAdminSession(ctx, [32]byte{1}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted user's session survived: err=%v", err)
	}
	authenticators, err := store.ListAdminMFAAuthenticators(ctx, second.Username)
	if err != nil || len(authenticators) != 0 {
		t.Fatalf("deleted user's MFA authenticators survived: %#v err=%v", authenticators, err)
	}
	// The other administrator is untouched.
	if _, err := store.GetAdminUser(ctx, user.Username); err != nil {
		t.Fatalf("unrelated admin was affected: %v", err)
	}
}

func TestDeleteAdminUserRejectsRevisionConflictAndMissingUser(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	user, err := store.CreateFirstAdmin(ctx, testAdminUser("admin", domain.AdminRoleAdministrator, now))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteAdminUser(ctx, user.Username, user.Revision+1); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("err=%v, want ErrRevisionConflict", err)
	}
	if err := store.DeleteAdminUser(ctx, "does-not-exist", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err=%v, want ErrNotFound", err)
	}
}

func TestAdminUserMutationsCommitTheirAuditIntentAtomically(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := store.CreateFirstAdmin(ctx, testAdminUser("admin", domain.AdminRoleAdministrator, now)); err != nil {
		t.Fatal(err)
	}

	viewer := testAdminUser("viewer", domain.AdminRoleReadOnly, now)
	created, err := store.PutAdminUserWithAuditIntent(ctx, viewer, 0, adminIntent("aud_user_create", "admin_user.create", "admin_user", viewer.Username))
	if err != nil {
		t.Fatal(err)
	}
	pending, err := store.ListPendingAdminAuditIntents(ctx)
	if err != nil || len(pending) != 1 || pending[0].EventID != "aud_user_create" {
		t.Fatalf("create intent=%#v err=%v", pending, err)
	}

	if err := store.DeleteAdminUserWithAuditIntent(ctx, created.Username, created.Revision+1, adminIntent("aud_refused", "admin_user.delete", "admin_user", created.Username)); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale delete error=%v", err)
	}
	pending, err = store.ListPendingAdminAuditIntents(ctx)
	if err != nil || len(pending) != 1 {
		t.Fatalf("refused delete changed intents=%#v err=%v", pending, err)
	}
	if _, err := store.GetAdminUser(ctx, created.Username); err != nil {
		t.Fatalf("refused delete removed user: %v", err)
	}

	if err := store.DeleteAdminUserWithAuditIntent(ctx, created.Username, created.Revision, adminIntent("aud_user_delete", "admin_user.delete", "admin_user", created.Username)); err != nil {
		t.Fatal(err)
	}
	pending, err = store.ListPendingAdminAuditIntents(ctx)
	if err != nil || len(pending) != 2 || pending[1].EventID != "aud_user_delete" {
		t.Fatalf("delete intent=%#v err=%v", pending, err)
	}
}

func TestFirstAdminBootstrapCommitsCompletionAndAuditIntentAtomically(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Date(2026, 9, 17, 8, 0, 0, 0, time.UTC)
	user := testAdminUser("admin", domain.AdminRoleAdministrator, now)
	intent := *adminIntent("aud_bootstrap", "admin.bootstrap", "admin_user", user.Username)
	completion := AdminBootstrapCompletion{
		OperationID: "install-production-20260917", Username: user.Username,
		AuditEventID: intent.EventID, CommittedAt: now,
	}

	storedUser, storedCompletion, created, err := store.CreateFirstAdminWithAuditIntent(ctx, user, completion, intent)
	if err != nil || !created || storedUser.Username != user.Username || storedCompletion != completion {
		t.Fatalf("user=%#v completion=%#v created=%v err=%v", storedUser, storedCompletion, created, err)
	}
	pending, err := store.ListPendingAdminAuditIntents(ctx)
	if err != nil || len(pending) != 1 || pending[0].EventID != intent.EventID {
		t.Fatalf("pending=%#v err=%v", pending, err)
	}
	persisted, err := store.AdminBootstrapCompletion(ctx)
	if err != nil || persisted != completion {
		t.Fatalf("completion=%#v err=%v", persisted, err)
	}

	// A controller retry may carry a freshly hashed password and a fresh
	// in-memory intent, but the operation id makes it a read-only replay.
	retryUser := testAdminUser("admin", domain.AdminRoleAdministrator, now.Add(time.Minute))
	retryIntent := *adminIntent("aud_retry_must_not_commit", "admin.bootstrap", "admin_user", retryUser.Username)
	retryCompletion := completion
	retryCompletion.AuditEventID = retryIntent.EventID
	retryCompletion.CommittedAt = now.Add(time.Minute)
	replayedUser, replayedCompletion, created, err := store.CreateFirstAdminWithAuditIntent(ctx, retryUser, retryCompletion, retryIntent)
	if err != nil || created || replayedCompletion != completion {
		t.Fatalf("replay completion=%#v created=%v err=%v", replayedCompletion, created, err)
	}
	if string(replayedUser.PasswordHash) != string(user.PasswordHash) {
		t.Fatal("idempotent replay replaced the administrator password")
	}
	pending, err = store.ListPendingAdminAuditIntents(ctx)
	if err != nil || len(pending) != 1 || pending[0].EventID != intent.EventID {
		t.Fatalf("replay changed pending intent: %#v err=%v", pending, err)
	}

	conflict := retryCompletion
	conflict.OperationID = "another-install"
	if _, _, _, err := store.CreateFirstAdminWithAuditIntent(ctx, retryUser, conflict, retryIntent); !errors.Is(err, ErrAdminBootstrapConflict) {
		t.Fatalf("different operation error=%v", err)
	}
}

func TestFirstAdminBootstrapRefusesAnUntrackedExistingAdministrator(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	user := testAdminUser("admin", domain.AdminRoleAdministrator, now)
	if _, err := store.CreateFirstAdmin(ctx, user); err != nil {
		t.Fatal(err)
	}
	intent := *adminIntent("aud_bootstrap", "admin.bootstrap", "admin_user", user.Username)
	completion := AdminBootstrapCompletion{
		OperationID: "install-production", Username: user.Username,
		AuditEventID: intent.EventID, CommittedAt: now,
	}
	if _, _, _, err := store.CreateFirstAdminWithAuditIntent(ctx, user, completion, intent); !errors.Is(err, ErrAdminInitialized) {
		t.Fatalf("error=%v, want ErrAdminInitialized", err)
	}
	if _, err := store.AdminBootstrapCompletion(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ambiguous existing admin acquired a completion: %v", err)
	}
}

func TestFirstAdminBootstrapRejectsUnsafeOperationID(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	user := testAdminUser("admin", domain.AdminRoleAdministrator, now)
	intent := *adminIntent("aud_bootstrap", "admin.bootstrap", "admin_user", user.Username)
	completion := AdminBootstrapCompletion{
		OperationID: "install\nforged-log-line", Username: user.Username,
		AuditEventID: intent.EventID, CommittedAt: now,
	}
	if _, _, _, err := store.CreateFirstAdminWithAuditIntent(context.Background(), user, completion, intent); err == nil {
		t.Fatal("operation id containing a control character was accepted")
	}
	count, err := store.AdminUserCount(context.Background())
	if err != nil || count != 0 {
		t.Fatalf("invalid operation changed admin count=%d err=%v", count, err)
	}
}
