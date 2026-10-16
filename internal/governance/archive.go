package governance

import "github.com/akz142857/Halro/internal/domain"

// OutcomeQuery selects archived Outcomes; empty fields match anything, and
// After is exclusive and orders by ID.
type OutcomeQuery struct {
	ProjectID    string
	WorkUnitID   string
	DefinitionID string
	After        string
}

// Archive is where Outcomes go once reported. Reads that fail return an error
// and are never taken for "absent": an idempotent retry read as absent would
// be reported a second time.
type Archive interface {
	// WriteOutcomes stores items durably and records that every Outcome at or
	// before the journal record through, whose hash is hash, is stored.
	WriteOutcomes(items []SnapshotRecord, through uint64, hash [32]byte) error
	// OutcomesThrough is the position and journal hash WriteOutcomes last
	// recorded.
	OutcomesThrough() (uint64, [32]byte, error)
	// ResetOutcomes discards every archived Outcome and the recorded
	// position, before a replay writes them again.
	ResetOutcomes() error
	ArchivedOutcome(id string) (domain.Outcome, bool, error)
	ArchivedHead(projectID, workUnitID, definitionID string, version uint64) (domain.Outcome, bool, error)
	ArchivedOutcomeIdempotency(projectID, keyHash string) (fingerprint, outcomeID string, found bool, err error)
	VisitArchivedOutcomes(query OutcomeQuery, visit func(domain.Outcome) (bool, error)) error
}
