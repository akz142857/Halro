package auth

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

type snapshotSource struct {
	keys     []domain.GatewayKey
	projects []domain.Project
}

func (s snapshotSource) ListGatewayKeys(context.Context) ([]domain.GatewayKey, error) {
	return s.keys, nil
}

func (s snapshotSource) ListProjects(context.Context) ([]domain.Project, error) {
	return s.projects, nil
}

// gatedSource serves whatever state currently sits in the pointer, and lets
// the test hold one refresh open in the middle of its read while others run.
type gatedSource struct {
	state       atomic.Pointer[snapshotSource]
	readStarted chan struct{} // closed when the gated read has begun
	readGate    chan struct{} // the gated read returns only once this closes
	// calls gates the first reader only. Deliberately not a sync.Once: a
	// blocked Once makes every later Do wait for it, which would serialize
	// the two refreshes inside the stub and hide the very race being tested.
	calls atomic.Int32
}

func (g *gatedSource) ListProjects(context.Context) ([]domain.Project, error) {
	return g.state.Load().projects, nil
}

func (g *gatedSource) ListGatewayKeys(context.Context) ([]domain.GatewayKey, error) {
	keys := g.state.Load().keys
	if g.calls.Add(1) == 1 {
		close(g.readStarted)
		<-g.readGate
	}
	return keys, nil
}

// TestStaleRefreshCannotResurrectARevokedKey pins the ordering guarantee
// Refresh must give: the snapshot installed last is the snapshot read last.
// The interleaving it drives is the production race between the background
// activation-recovery refresh and an admin mutation's own refresh — the
// background read starts first (seeing the key still enabled), the revocation
// commits and refreshes, and only then does the older read try to finish.
// Without serialization the older read installs last and the revoked key
// authenticates again; with it, the second refresh cannot even read until the
// first has installed, so the newest state always lands last.
func TestStaleRefreshCannotResurrectARevokedKey(t *testing.T) {
	plaintext, key, err := GenerateGatewayKey("prj_1", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	project := domain.Project{ID: "prj_1", Name: "project", Enabled: true}
	enabled := snapshotSource{keys: []domain.GatewayKey{key}, projects: []domain.Project{project}}
	revokedKey := key
	revokedKey.Enabled = false
	revoked := snapshotSource{keys: []domain.GatewayKey{revokedKey}, projects: []domain.Project{project}}

	source := &gatedSource{readStarted: make(chan struct{}), readGate: make(chan struct{})}
	source.state.Store(&enabled)
	snapshot := NewSnapshot()

	stale := make(chan error, 1)
	go func() { stale <- snapshot.Refresh(context.Background(), source) }()
	<-source.readStarted // the stale refresh has read the enabled key and is held open

	// The revocation lands and refreshes. Serialized Refresh parks this call
	// until the stale one finishes; the broken code let it complete first.
	source.state.Store(&revoked)
	fresh := make(chan error, 1)
	go func() { fresh <- snapshot.Refresh(context.Background(), source) }()

	// Give the fresh refresh time to run to completion if nothing is holding
	// it back — which is exactly the defect state this test exists to catch.
	time.Sleep(20 * time.Millisecond)
	close(source.readGate)
	if err := <-stale; err != nil {
		t.Fatal(err)
	}
	if err := <-fresh; err != nil {
		t.Fatal(err)
	}

	if _, err := snapshot.Authenticate(plaintext, time.Now()); err != ErrKeyDisabled {
		t.Fatalf("revoked key authenticated after a stale refresh finished last: err=%v, want ErrKeyDisabled", err)
	}
}

func TestSnapshotAuthentication(t *testing.T) {
	plaintext, key, err := GenerateGatewayKey("prj_1", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := NewSnapshot()
	if err := snapshot.Refresh(context.Background(), snapshotSource{
		keys:     []domain.GatewayKey{key},
		projects: []domain.Project{{ID: "prj_1", Name: "project", Enabled: true}},
	}); err != nil {
		t.Fatal(err)
	}
	result, err := snapshot.Authenticate(plaintext, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if result.Key.ID != key.ID || result.Project.ID != "prj_1" {
		t.Fatalf("unexpected auth result: %#v", result)
	}
	if _, err := snapshot.Authenticate("gw_invalid", time.Now()); err != ErrInvalidKey {
		t.Fatalf("unexpected invalid key error: %v", err)
	}
}
