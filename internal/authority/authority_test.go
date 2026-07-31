package authority

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestMutationCanonicalAndStandaloneCommit(t *testing.T) {
	m, err := New("mut_1", ScopeProject, "prj_1", "project.update", map[string]any{"enabled": true, "revision": 2})
	if err != nil {
		t.Fatal(err)
	}
	a, bErr := m.Canonical()
	if bErr != nil {
		t.Fatal(bErr)
	}
	var decoded Mutation
	if err := json.Unmarshal(a, &decoded); err != nil {
		t.Fatal(err)
	}
	b, _ := decoded.Canonical()
	if string(a) != string(b) {
		t.Fatalf("encoding is not deterministic: %s != %s", a, b)
	}
	auth := NewStandalone()
	token, _ := auth.Token(context.Background(), ScopeProject, "prj_1")
	first, err := auth.Commit(context.Background(), token, m)
	if err != nil {
		t.Fatal(err)
	}
	second, err := auth.Commit(context.Background(), token, m)
	if err != nil {
		t.Fatal(err)
	}
	if first.Index != 1 || second.Index != 1 {
		t.Fatalf("indexes=%d,%d", first.Index, second.Index)
	}
}

func TestStandaloneRejectsMutationIDConflict(t *testing.T) {
	auth := NewStandalone()
	one, _ := New("mut_1", ScopeGlobal, "", "settings.update", map[string]int{"revision": 1})
	two, _ := New("mut_1", ScopeGlobal, "", "settings.update", map[string]int{"revision": 2})
	if _, err := auth.Commit(context.Background(), Token{Epoch: 1}, one); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.Commit(context.Background(), Token{Epoch: 1}, two); !errors.Is(err, ErrMutationConflict) {
		t.Fatalf("err=%v", err)
	}
}

func TestStandaloneRejectsStaleAndInvalidMutation(t *testing.T) {
	m, _ := New("mut_1", ScopeGlobal, "", "settings.update", map[string]bool{"enabled": true})
	auth := NewStandalone()
	if _, err := auth.Commit(context.Background(), Token{Epoch: 2}, m); !errors.Is(err, ErrStaleOwnership) {
		t.Fatalf("err=%v", err)
	}
	m.SchemaVersion++
	if _, err := auth.Commit(context.Background(), Token{Epoch: 1}, m); err == nil {
		t.Fatal("expected schema rejection")
	}
}

func TestMutationRequiresCanonicalPayloadAndScope(t *testing.T) {
	m := Mutation{ID: "mut", SchemaVersion: 1, Scope: ScopeProject, Epoch: 1, Kind: "x", Payload: json.RawMessage(`{ "a": 1 }`)}
	if err := m.Validate(); err == nil {
		t.Fatal("expected non-canonical payload rejection")
	}
	m.Payload = json.RawMessage(`{"a":1}`)
	if err := m.Validate(); err == nil {
		t.Fatal("expected missing project rejection")
	}
}
