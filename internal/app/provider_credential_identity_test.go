package app

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

// A target has to know which secret it authenticates with, and which version of
// it was in force.
//
// The scope of an upstream refusal is not always the target. "Out of quota",
// "subscription lapsed" and "this key is no longer valid" are properties of the
// credential, and one credential commonly backs several deployments — so a
// refusal learned from one is knowledge about all of them. Without the identity
// on the target there is nothing to attribute that knowledge to, and it has to
// be rediscovered once per deployment.
//
// The revision is the other half: it is what lets such knowledge expire without
// anyone having to clear it. An operator who replaces a dead key advances the
// revision, and anything remembered against the old one is stale by
// construction rather than by a second step somebody has to remember.
func TestTargetsCarryTheCredentialTheyAuthenticateWith(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	seedProvider(t, cfg, false)
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	target, ok := runtime.providers.Resolve("chat")
	if !ok {
		t.Fatal("the seeded route resolved to no target")
	}
	if target.CredentialID != "cred_1" {
		t.Fatalf("credential_id = %q, want cred_1", target.CredentialID)
	}
	before := target.CredentialRevision
	if before == 0 {
		t.Fatal("credential revision is zero; a stored record always carries one")
	}

	// Replacing the secret is what an operator does to a credential that an
	// upstream has stopped honouring, and it is the signal anything remembering
	// that refusal has to key on.
	// Through the runtime's own handle: it holds the data directory lock, so a
	// second store cannot be opened while it is running.
	credential, err := runtime.store.GetCredential(context.Background(), "cred_1")
	if err != nil {
		t.Fatal(err)
	}
	credential.Name = "OpenAI (rotated)"
	if _, err := runtime.store.PutCredential(context.Background(), credential, credential.Revision, nil); err != nil {
		t.Fatal(err)
	}

	if err := runtime.reloadProviderRegistry(context.Background()); err != nil {
		t.Fatal(err)
	}
	reloaded, ok := runtime.providers.Resolve("chat")
	if !ok {
		t.Fatal("the route resolved to no target after reload")
	}
	if reloaded.CredentialRevision <= before {
		t.Fatalf("credential revision did not advance across an update: before=%d after=%d",
			before, reloaded.CredentialRevision)
	}
}
