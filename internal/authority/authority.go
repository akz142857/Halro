// Package authority defines Standalone-compatible mutation and fencing
// primitives on which a future replicated authority can build.
package authority

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

const CurrentSchemaVersion uint16 = 1
const StandaloneEpoch uint64 = 1

type Scope string

const (
	ScopeGlobal  Scope = "global"
	ScopeProject Scope = "project"
	ScopeShard   Scope = "shard"
)

type Token struct {
	Epoch uint64 `json:"epoch"`
}

func (t Token) Validate() error {
	if t.Epoch == 0 {
		return errors.New("ownership epoch is required")
	}
	return nil
}

type Mutation struct {
	ID            string          `json:"id"`
	SchemaVersion uint16          `json:"schema_version"`
	Scope         Scope           `json:"scope"`
	ProjectID     string          `json:"project_id,omitempty"`
	Epoch         uint64          `json:"epoch"`
	Kind          string          `json:"kind"`
	Payload       json.RawMessage `json:"payload"`
}

func New(id string, scope Scope, projectID, kind string, payload any) (Mutation, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Mutation{}, fmt.Errorf("encode mutation payload: %w", err)
	}
	m := Mutation{ID: id, SchemaVersion: CurrentSchemaVersion, Scope: scope, ProjectID: projectID, Epoch: StandaloneEpoch, Kind: kind, Payload: raw}
	return m, m.Validate()
}

func (m Mutation) Validate() error {
	if m.ID == "" {
		return errors.New("mutation id is required")
	}
	if m.SchemaVersion != CurrentSchemaVersion {
		return fmt.Errorf("unsupported mutation schema version %d", m.SchemaVersion)
	}
	if m.Epoch == 0 {
		return errors.New("mutation epoch is required")
	}
	if m.Kind == "" {
		return errors.New("mutation kind is required")
	}
	switch m.Scope {
	case ScopeGlobal, ScopeShard:
		if m.ProjectID != "" {
			return errors.New("project id is only valid for project scope")
		}
	case ScopeProject:
		if m.ProjectID == "" {
			return errors.New("project id is required for project scope")
		}
	default:
		return errors.New("mutation scope is invalid")
	}
	if len(m.Payload) == 0 || !json.Valid(m.Payload) {
		return errors.New("mutation payload must be valid JSON")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, m.Payload); err != nil {
		return errors.New("mutation payload is invalid")
	}
	if !bytes.Equal(compact.Bytes(), m.Payload) {
		return errors.New("mutation payload is not canonical compact JSON")
	}
	return nil
}

func (m Mutation) Canonical() ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(m)
}

var ErrStaleOwnership = errors.New("stale ownership epoch")
var ErrMutationConflict = errors.New("mutation id was reused with different content")

type Commit struct {
	Index    uint64
	Mutation Mutation
}
type Authority interface {
	Token(context.Context, Scope, string) (Token, error)
	Commit(context.Context, Token, Mutation) (Commit, error)
}

type Standalone struct {
	mu      sync.Mutex
	index   uint64
	commits map[string]storedCommit
}

type storedCommit struct {
	digest [32]byte
	commit Commit
}

func NewStandalone() *Standalone { return &Standalone{commits: make(map[string]storedCommit)} }
func (*Standalone) Token(context.Context, Scope, string) (Token, error) {
	return Token{Epoch: StandaloneEpoch}, nil
}
func (s *Standalone) Commit(_ context.Context, token Token, mutation Mutation) (Commit, error) {
	if err := token.Validate(); err != nil {
		return Commit{}, err
	}
	if token.Epoch != StandaloneEpoch || mutation.Epoch != token.Epoch {
		return Commit{}, ErrStaleOwnership
	}
	if err := mutation.Validate(); err != nil {
		return Commit{}, err
	}
	canonical, err := mutation.Canonical()
	if err != nil {
		return Commit{}, err
	}
	digest := sha256.Sum256(canonical)
	s.mu.Lock()
	defer s.mu.Unlock()
	if prior, ok := s.commits[mutation.ID]; ok {
		if prior.digest != digest {
			return Commit{}, ErrMutationConflict
		}
		return prior.commit, nil
	}
	s.index++
	commit := Commit{Index: s.index, Mutation: mutation}
	s.commits[mutation.ID] = storedCommit{digest: digest, commit: commit}
	return commit, nil
}
