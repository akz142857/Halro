package bolt

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

func adminIntent(eventID, action, targetType, targetID string) *domain.AdminAuditIntent {
	return &domain.AdminAuditIntent{
		EventID: eventID, OccurredAt: time.Date(2026, 8, 11, 9, 0, 0, 0, time.UTC),
		ActorType: "admin_user", ActorID: "admin_one",
		Action: action, TargetType: targetType, TargetID: targetID,
	}
}

// The point of the intent is that it cannot disagree with the mutation. A
// mutation that commits without its record, or a record that survives a
// mutation that did not commit, are the two states this protocol exists to
// make unreachable — one leaves a change nothing accounted for, the other
// puts a change in the audit log that never happened.
func TestAdminMutationAndAuditIntentCommitTogether(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	seedPricingDeployment(t, store, "dep_intent", 0, 0, 0)

	deployment, err := store.GetDeployment(ctx, "dep_intent")
	if err != nil {
		t.Fatal(err)
	}
	deployment.Name = "renamed"
	if _, err := store.PutDeployment(ctx, deployment, deployment.Revision, adminIntent("aud_ok", "deployment.update", "deployment", deployment.ID)); err != nil {
		t.Fatal(err)
	}
	pending, err := store.ListPendingAdminAuditIntents(ctx)
	if err != nil || len(pending) != 1 || pending[0].EventID != "aud_ok" {
		t.Fatalf("intent did not commit with the mutation: %#v err=%v", pending, err)
	}

	// A refused mutation must take its record down with it. The revision is
	// deliberately stale, which is the ordinary way an admin write loses a race.
	stale := deployment
	stale.Name = "not applied"
	if _, err := store.PutDeployment(ctx, stale, 0, adminIntent("aud_rolled_back", "deployment.update", "deployment", stale.ID)); err == nil {
		t.Fatal("stale revision was accepted")
	}
	pending, err = store.ListPendingAdminAuditIntents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, intent := range pending {
		if intent.EventID == "aud_rolled_back" {
			t.Fatal("a refused mutation left an audit record behind")
		}
	}
	current, err := store.GetDeployment(ctx, "dep_intent")
	if err != nil || current.Name != "renamed" {
		t.Fatalf("refused mutation changed the record: %q err=%v", current.Name, err)
	}
}

func TestDeliveredAdminAuditIntentIsRetired(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	seedPricingDeployment(t, store, "dep_retire", 0, 0, 0)
	deployment, err := store.GetDeployment(ctx, "dep_retire")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutDeployment(ctx, deployment, deployment.Revision, adminIntent("aud_retire", "deployment.update", "deployment", deployment.ID)); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteAdminAuditIntent(ctx, "aud_retire"); err != nil {
		t.Fatal(err)
	}
	pending, err := store.ListPendingAdminAuditIntents(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatalf("delivered intent still pending: %#v err=%v", pending, err)
	}
	count, err := store.PendingAdminAuditIntentCount(ctx)
	if err != nil || count != 0 {
		t.Fatalf("backlog count=%d err=%v", count, err)
	}
	// Retiring twice is what a drain that raced another drain does. It must be
	// the same answer, not an error that stops the rest of the backlog.
	if err := store.DeleteAdminAuditIntent(ctx, "aud_retire"); err != nil {
		t.Fatalf("retiring an already delivered intent failed: %v", err)
	}
}

// Two mutations must never share an event ID: the audit log would then carry
// one record for two changes, and the drain could not tell which it delivered.
func TestAdminAuditIntentRejectsDuplicateEventID(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	seedPricingDeployment(t, store, "dep_dup", 0, 0, 0)
	deployment, err := store.GetDeployment(ctx, "dep_dup")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutDeployment(ctx, deployment, deployment.Revision, adminIntent("aud_dup", "deployment.update", "deployment", deployment.ID)); err != nil {
		t.Fatal(err)
	}
	current, err := store.GetDeployment(ctx, "dep_dup")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutDeployment(ctx, current, current.Revision, adminIntent("aud_dup", "deployment.update", "deployment", current.ID)); err == nil {
		t.Fatal("duplicate audit event id was accepted")
	}
	after, err := store.GetDeployment(ctx, "dep_dup")
	if err != nil || after.Revision != current.Revision {
		t.Fatalf("mutation survived its rejected audit record: revision=%d err=%v", after.Revision, err)
	}
}
