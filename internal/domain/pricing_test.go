package domain

import (
	"errors"
	"testing"
	"time"
)

func TestDeploymentPriceVersionValidationSeparatesFreeMeteredAndEvidence(t *testing.T) {
	base := validPriceVersion(t, "price_one", 1, "2026-08-04T00:00:00Z")
	if err := base.Validate(); err != nil {
		t.Fatalf("valid metered price: %v", err)
	}
	free := base
	free.ID, free.Version, free.BillingMode = "price_free", 2, BillingModeFree
	free.InputMicrosPerMillion, free.OutputMicrosPerMillion = 0, 0
	if err := free.Validate(); err != nil {
		t.Fatalf("valid free price: %v", err)
	}
	invalidMetered := free
	invalidMetered.BillingMode = BillingModeMetered
	if err := invalidMetered.Validate(); err == nil {
		t.Fatal("expected all-zero metered price to fail")
	}
	invalidFree := free
	invalidFree.InputMicrosPerMillion = 1
	if err := invalidFree.Validate(); err == nil {
		t.Fatal("expected non-zero free price to fail")
	}
	invalidSource := base
	invalidSource.Source.URI = "https://user:secret@example.test/pricing?token=secret"
	if err := invalidSource.Validate(); err == nil {
		t.Fatal("expected unsafe source URI to fail")
	}
}

func TestSelectDeploymentPriceVersionUsesEffectiveTimeNotCreationOrder(t *testing.T) {
	first := validPriceVersion(t, "price_one", 1, "2026-08-04T00:00:00Z")
	second := validPriceVersion(t, "price_two", 2, "2026-09-01T00:00:00Z")
	selectedAt := mustPriceTime(t, "2026-09-01T00:00:00Z")
	for _, versions := range [][]DeploymentPriceVersion{{first, second}, {second, first}} {
		selected, err := SelectDeploymentPriceVersion(versions, "dep_one", selectedAt)
		if err != nil {
			t.Fatalf("select price: %v", err)
		}
		if selected.ID != second.ID {
			t.Fatalf("selected %q, want %q", selected.ID, second.ID)
		}
	}
}

func TestSelectDeploymentPriceVersionRejectsDuplicateTimelineAndSkipsCancelled(t *testing.T) {
	first := validPriceVersion(t, "price_one", 1, "2026-08-04T00:00:00Z")
	duplicate := validPriceVersion(t, "price_two", 2, "2026-08-04T00:00:00Z")
	if _, err := SelectDeploymentPriceVersion([]DeploymentPriceVersion{first, duplicate}, "dep_one", mustPriceTime(t, "2026-08-05T00:00:00Z")); !errors.Is(err, ErrPriceTimelineConflict) {
		t.Fatalf("duplicate timeline error = %v", err)
	}
	cancelledAt := mustPriceTime(t, "2026-08-03T00:00:00Z")
	first.CancelledAt, first.CancelledBy = &cancelledAt, "admin_one"
	if _, err := SelectDeploymentPriceVersion([]DeploymentPriceVersion{first}, "dep_one", mustPriceTime(t, "2026-08-05T00:00:00Z")); !errors.Is(err, ErrPriceUnavailable) {
		t.Fatalf("cancelled selection error = %v", err)
	}
}

func TestDerivePriceLifecycleDoesNotPersistDerivedStatus(t *testing.T) {
	old := validPriceVersion(t, "price_old", 1, "2026-08-01T00:00:00Z")
	active := validPriceVersion(t, "price_active", 2, "2026-08-03T00:00:00Z")
	future := validPriceVersion(t, "price_future", 3, "2026-09-01T00:00:00Z")
	cancelled := validPriceVersion(t, "price_cancelled", 4, "2026-10-01T00:00:00Z")
	cancelledAt := mustPriceTime(t, "2026-08-04T00:00:00Z")
	cancelled.CancelledAt, cancelled.CancelledBy = &cancelledAt, "admin_one"
	states, err := DerivePriceLifecycle([]DeploymentPriceVersion{future, old, cancelled, active}, "dep_one", mustPriceTime(t, "2026-08-04T12:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if states[old.ID] != PriceLifecycleSuperseded || states[active.ID] != PriceLifecycleActive ||
		states[future.ID] != PriceLifecycleScheduled || states[cancelled.ID] != PriceLifecycleCancelled {
		t.Fatalf("unexpected lifecycle states: %#v", states)
	}
}

func validPriceVersion(t *testing.T, id string, version uint64, effective string) DeploymentPriceVersion {
	t.Helper()
	created := mustPriceTime(t, "2026-08-01T00:00:00Z")
	retrieved := mustPriceTime(t, "2026-07-31T00:00:00Z")
	return DeploymentPriceVersion{
		ID: id, DeploymentID: "dep_one", Version: version, BillingMode: BillingModeMetered,
		Currency: "USD", FormulaVersion: PriceFormulaUSDTokensV1,
		InputMicrosPerMillion: 400_000, OutputMicrosPerMillion: 1_600_000,
		EffectiveFrom: mustPriceTime(t, effective), CreatedBy: "admin_one", CreatedAt: created, Revision: 1,
		Source: PriceSource{Type: PriceSourceOfficialURL, Assurance: PriceAssuranceAsserted,
			URI: "https://example.test/pricing", RetrievedAt: &retrieved, ReceivedAt: created,
			ContentSHA256: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Reference: "standard"},
	}
}

func mustPriceTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func TestPriceSourceRejectsCredentialLikeEvidence(t *testing.T) {
	now := time.Now().UTC()
	retrieved := now
	source := PriceSource{Type: PriceSourceOfficialURL, Assurance: PriceAssuranceAsserted, URI: "https://example.test/pricing", RetrievedAt: &retrieved, ReceivedAt: now,
		ContentSHA256: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Reference: "api_key=super-secret-value-123456"}
	if err := source.Validate(); err == nil {
		t.Fatal("credential-like pricing evidence was accepted")
	}
}
