package bolt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"

	"github.com/akz142857/Halro/internal/domain"
)

// The caller holds unpublished exclusive metadata ownership (Attach holds
// journalMu), after authenticating/reconciling the old native journal. The
// persistent epoch-pending gate stays set through every block until the actual
// epoch-file + position commit. Cross-process intermediate content is rebuilt,
// never accepted from an unsigned cursor. Physical/shared migration fees and
// older migrations still require the complete O-01 protocol.
func (s *Store) rebuildPricingMigrationAfterJournal(step func(string) error) error {
	leave, err := s.enter()
	if err != nil {
		return err
	}
	defer leave()
	var needed bool
	if err := s.view(func(tx *Tx) error {
		// All supported older fixtures may pass this epoch preparation function;
		// proposal buckets are reserved only after migration45 has run.
		needed = tx.Bucket(bucketPricingProposalOrder) != nil && tx.Bucket(bucketPricingProposalCount) != nil
		return nil
	}); err != nil || !needed {
		return err
	}
	for _, name := range [][]byte{bucketPricingProposalOrder, bucketPricingProposalCount} {
		for {
			done, err := s.clearPricingMigrationBlock(name)
			if err != nil {
				return err
			}
			if err := migrationStep(step, "after_proposal_clear_batch"); err != nil {
				return err
			}
			if done {
				break
			}
		}
	}
	var after []byte
	expectedID := 0
	for {
		page, err := s.readPricingRecoveryPage(context.Background(), bucketDeploymentPriceProposals, after, expectedID)
		if err != nil {
			return err
		}
		for _, row := range page.rows {
			var p domain.DeploymentPriceProposal
			if err := json.Unmarshal(row.raw, &p); err != nil {
				return err
			}
			if p.ID != string(row.key) {
				return ErrProposalIndexConflict
			}
			if err := p.Validate(); err != nil {
				return err
			}
			if err := migrationStep(step, "proposal_order_row"); err != nil {
				return err
			}
		}
		expectedID = page.txID
		if len(page.rows) > 0 {
			if err := s.migrate(func(tx *Tx) error {
				if tx.ID() != page.txID+1 {
					return ErrPricingRecoveryChanged
				}
				for _, row := range page.rows {
					if !bytes.Equal(tx.Bucket(bucketDeploymentPriceProposals).Get(row.key), row.raw) {
						return ErrPricingRecoveryChanged
					}
					if err := tx.updateProposalOrder(row.key, nil, row.raw); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				return err
			}
			expectedID++
			if err := migrationStep(step, "after_proposal_index_batch"); err != nil {
				return err
			}
		}
		if page.done {
			break
		}
		after = page.after
	}
	// Older native frames do not contain the schema43 counter. Recount from the
	// post-reconciliation facts in finite readers, with one final scalar write.
	var pending uint64
	after = nil
	for {
		page, err := s.readPricingRecoveryPage(context.Background(), bucketPricingAuditIntents, after, expectedID)
		if err != nil {
			return err
		}
		expectedID = page.txID
		for _, row := range page.rows {
			n, err := pricingPendingRaw(row.raw)
			if err != nil {
				return err
			}
			if n != 0 {
				if pending == math.MaxUint64 {
					return ErrProposalIndexConflict
				}
				pending++
			}
		}
		if page.done {
			break
		}
		after = page.after
	}
	if err := s.migrate(func(tx *Tx) error {
		if tx.ID() != expectedID+1 {
			return ErrPricingRecoveryChanged
		}
		control := tx.Bucket(bucketPricingAuditControl)
		if control == nil {
			return ErrProposalIndexConflict
		}
		return control.Put(pricingPendingCountKey, encodeUint64(pending))
	}); err != nil {
		return err
	}
	return migrationStep(step, "after_proposal_index_complete")
}

// Retire at most64 actual keys/buckets or1MiB of key/value bytes per commit.
// Dropping a large nested bucket wholesale would itself free history-sized
// dirty pages. Only already-empty child buckets are removed here.
func (s *Store) clearPricingMigrationBlock(name []byte) (done bool, resultErr error) {
	err := s.migrate(func(tx *Tx) error {
		root := tx.Bucket(name)
		if root == nil {
			return ErrProposalIndexConflict
		}
		rows, used := 0, 0
		for rows < pricingRecoveryRows {
			key, raw := root.Cursor().First()
			if key == nil {
				done = true
				return nil
			}
			parent := root
			child := root.Bucket(key)
			if child != nil {
				if !bytes.Equal(name, bucketPricingProposalOrder) {
					return errors.New("pricing count contains a child bucket")
				}
				childKey, childRaw := child.Cursor().First()
				if childKey == nil {
					// The empty child's root name is also a bounded native identity.
					if len(key) > pricingRecoveryWideRowBytes {
						return ErrPricingRecoveryLimit
					}
					if rows > 0 && len(key) > pricingRecoveryBytes-used {
						return nil
					}
					used += len(key)
					if err := root.DeleteBucket(key); err != nil {
						return err
					}
					rows++
					continue
				}
				parent, key, raw = child, childKey, childRaw
			}
			if raw == nil {
				return errors.New("pricing migration encountered a nested index bucket")
			}
			if len(key) > pricingRecoveryWideRowBytes || len(raw) > pricingRecoveryWideRowBytes-len(key) {
				return ErrPricingRecoveryLimit
			}
			size := len(key) + len(raw)
			if rows > 0 && size > pricingRecoveryBytes-used {
				return nil
			}
			if err := parent.Delete(key); err != nil {
				return err
			}
			rows++
			used += size
		}
		return nil
	})
	return done, err
}
