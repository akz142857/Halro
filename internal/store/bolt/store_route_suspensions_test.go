package bolt

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	bbolt "go.etcd.io/bbolt"
)

func suspensionFixture() domain.RouteSuspension {
	return domain.RouteSuspension{
		ScopeKind: "credential", ScopeKey: "cred_1",
		Reason: "invalid_credential", ProviderStatus: 401,
		ObservedAt: time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC),
		Indefinite: true, CredentialRevision: 4,
	}
}

func openSuspensionStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestRouteSuspensionsRoundTrip(t *testing.T) {
	store := openSuspensionStore(t)
	ctx := context.Background()
	stored := suspensionFixture()
	if err := store.PutRouteSuspension(ctx, stored); err != nil {
		t.Fatal(err)
	}
	rows, err := store.ListRouteSuspensions(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	if rows[0] != stored {
		t.Fatalf("read back %+v, want %+v", rows[0], stored)
	}
	// Writing the same scope replaces rather than accumulating: a doubled
	// window is the same suspension, not a second one.
	stored.CredentialRevision = 5
	if err := store.PutRouteSuspension(ctx, stored); err != nil {
		t.Fatal(err)
	}
	rows, _ = store.ListRouteSuspensions(ctx)
	if len(rows) != 1 || rows[0].CredentialRevision != 5 {
		t.Fatalf("rows after replace = %+v", rows)
	}
	if err := store.DeleteRouteSuspension(ctx, "credential", "cred_1"); err != nil {
		t.Fatal(err)
	}
	if rows, _ := store.ListRouteSuspensions(ctx); len(rows) != 0 {
		t.Fatalf("rows after delete = %+v", rows)
	}
	// Deleting a scope that was never stored is how every probe-driven clear
	// arrives here, so it is not an error.
	if err := store.DeleteRouteSuspension(ctx, "deployment", "dep_never_stored"); err != nil {
		t.Fatalf("deleting an absent scope failed: %v", err)
	}
}

// The credential-and-model scope joins its halves with a NUL, and the bucket
// key joins kind and scope key with a different byte. Two pairs that differ
// only in where the split falls must not land on one row.
func TestCredentialModelScopesDoNotCollide(t *testing.T) {
	store := openSuspensionStore(t)
	ctx := context.Background()
	for _, key := range []string{"cred_1\x00model-a", "cred_1\x00model-b", "cred_2\x00model-a"} {
		row := suspensionFixture()
		row.ScopeKind = "credential_model"
		row.ScopeKey = key
		if err := store.PutRouteSuspension(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	rows, _ := store.ListRouteSuspensions(ctx)
	if len(rows) != 3 {
		t.Fatalf("stored %d rows, want 3 distinct scopes", len(rows))
	}
}

// The pairing this whole action waited for: the removal and the audit record
// are one transaction, so an intent that cannot become a well-formed audit
// event takes the removal down with it.
func TestClearingCommitsTheRemovalAndTheAuditRecordTogether(t *testing.T) {
	store := openSuspensionStore(t)
	ctx := context.Background()
	if err := store.PutRouteSuspension(ctx, suspensionFixture()); err != nil {
		t.Fatal(err)
	}
	// Missing an actor and an action: Validate refuses it.
	broken := &domain.AdminAuditIntent{EventID: "aud_1", OccurredAt: time.Now().UTC()}
	if err := store.DeleteRouteSuspensionWithAuditIntent(ctx, "credential", "cred_1", broken); err == nil {
		t.Fatal("a clear with an unusable audit record was accepted")
	}
	rows, _ := store.ListRouteSuspensions(ctx)
	if len(rows) != 1 {
		t.Fatalf("the row was removed without its audit record: %+v", rows)
	}
	intents, err := store.ListPendingAdminAuditIntents(ctx)
	if err != nil || len(intents) != 0 {
		t.Fatalf("intents=%+v err=%v, want none", intents, err)
	}

	good := &domain.AdminAuditIntent{
		EventID: "aud_2", OccurredAt: time.Now().UTC(),
		ActorType: "admin", ActorID: "admin", Action: "route_suspension.clear",
		TargetType: "route_suspension", TargetID: "handle",
	}
	if err := store.DeleteRouteSuspensionWithAuditIntent(ctx, "credential", "cred_1", good); err != nil {
		t.Fatal(err)
	}
	if rows, _ := store.ListRouteSuspensions(ctx); len(rows) != 0 {
		t.Fatalf("rows after the clear = %+v", rows)
	}
	intents, _ = store.ListPendingAdminAuditIntents(ctx)
	if len(intents) != 1 || intents[0].Action != "route_suspension.clear" {
		t.Fatalf("intents after the clear = %+v", intents)
	}
}

// Clearing a scope that is not stored is a 404 rather than a silent success:
// the endpoint must not report clearing something it did not clear.
func TestClearingAnAbsentScopeIsNotFound(t *testing.T) {
	store := openSuspensionStore(t)
	err := store.DeleteRouteSuspensionWithAuditIntent(context.Background(), "credential", "cred_absent", nil)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// Node-local derived state: one unreadable row must not be the reason an
// instance refuses to start.
func TestAnUnreadableRowIsSkippedRatherThanFailingTheRead(t *testing.T) {
	store := openSuspensionStore(t)
	ctx := context.Background()
	if err := store.PutRouteSuspension(ctx, suspensionFixture()); err != nil {
		t.Fatal(err)
	}
	if err := store.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketRouteSuspensions).Put([]byte("credential\x1fcorrupt"), []byte("{not json"))
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := store.ListRouteSuspensions(ctx)
	if err != nil {
		t.Fatalf("one corrupt row failed the whole read: %v", err)
	}
	if len(rows) != 1 || rows[0].ScopeKey != "cred_1" {
		t.Fatalf("rows = %+v, want the one readable suspension", rows)
	}
}
