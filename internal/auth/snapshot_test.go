package auth

import (
	"context"
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
