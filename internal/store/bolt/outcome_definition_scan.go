package bolt

import (
	"context"

	"github.com/akz142857/Halro/internal/domain"
)

// The source is ordered by ID/uint64 version. One current group and one target
// replace the history-sized latestByID map. A captured TxID fences every page;
// head also fences earlier definition callbacks merged into the same write Tx.
type outcomeDefinitionScan struct {
	txID   int
	head   OutcomeDefinitionExportHead
	active uint64
	latest domain.OutcomeDefinition
}

func (s outcomeDefinitionScan) checkTx(tx *Tx) error {
	if tx.ID() != s.txID+1 {
		return ErrRevisionConflict
	}
	head, err := definitionExportHead(tx)
	if err != nil {
		return err
	}
	if head.Count != s.head.Count || head.Hash != s.head.Hash {
		return ErrRevisionConflict
	}
	return nil
}

func (s *Store) scanOutcomeDefinitions(ctx context.Context, projectID, targetID string, step func(string) error) (result outcomeDefinitionScan, resultErr error) {
	leave, err := s.enter()
	if err != nil {
		return result, err
	}
	defer leave()
	s.lifetime.mu.Lock()
	closing := s.lifetime.ctx
	s.lifetime.mu.Unlock()
	check := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return closing.Err()
	}
	if err := check(); err != nil {
		return result, err
	}
	if err := s.view(func(tx *Tx) error {
		result.txID = tx.ID()
		var err error
		result.head, err = definitionExportHead(tx)
		return err
	}); err != nil {
		return result, err
	}
	var after []byte
	var groupID string
	var current domain.OutcomeDefinition
	flush := func() {
		if current.ID != "" && current.Enabled {
			result.active++
		}
		current = domain.OutcomeDefinition{}
	}
	for {
		if err := check(); err != nil {
			return outcomeDefinitionScan{}, err
		}
		// Copy a finite page, release its reader, then validate and reduce. Use the
		// definition-specific wide allowance (raw8MiB+key128B), not pricing's ceiling.
		page, err := s.readDefinitionMigrationPage(after, 0)
		if err != nil {
			return outcomeDefinitionScan{}, err
		}
		if page.txID != result.txID {
			return outcomeDefinitionScan{}, ErrRevisionConflict
		}
		for _, row := range page.rows {
			if err := check(); err != nil {
				return outcomeDefinitionScan{}, err
			}
			item, err := validateDefinitionExportRow(row.key, row.raw)
			if err != nil {
				return outcomeDefinitionScan{}, err
			}
			if item.ID != groupID {
				flush()
				groupID = item.ID
			}
			if projectID == "" || item.ProjectID == projectID {
				current = item
				if item.ID == targetID {
					result.latest = item
				}
			}
			if step != nil {
				if err := step("definition_source_row"); err != nil {
					return outcomeDefinitionScan{}, err
				}
			}
		}
		if page.done {
			break
		}
		after = page.after
	}
	flush()
	if err := check(); err != nil {
		return outcomeDefinitionScan{}, err
	}
	err = s.view(func(tx *Tx) error {
		if tx.ID() != result.txID {
			return ErrRevisionConflict
		}
		head, err := definitionExportHead(tx)
		if err != nil {
			return err
		}
		if head.Count != result.head.Count || head.Hash != result.head.Hash {
			return ErrRevisionConflict
		}
		return nil
	})
	if err != nil {
		return outcomeDefinitionScan{}, err
	}
	return result, nil
}
