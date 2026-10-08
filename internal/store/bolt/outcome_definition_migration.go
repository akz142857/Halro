package bolt

import (
	"bytes"
	"errors"
)

// The caller owns unpublished exclusive metadata, after old-tail recovery.
// A durable epoch-pending gate hides every intermediate block. On retry all
// intermediate rows are retired in finite commits and rebuilt from facts.
func (s *Store) rebuildOutcomeDefinitionExportBounded(step func(string) error) (result OutcomeDefinitionExportHead, resultErr error) {
	leave, err := s.enter()
	if err != nil {
		return result, err
	}
	defer leave()
	if err := s.migrate(func(tx *Tx) error {
		control := tx.Bucket(bucketOutcomeDefinitionExportMeta)
		if control == nil || tx.Bucket(bucketOutcomeDefinitionExport) == nil {
			return ErrDefinitionExportCorrupt
		}
		if control.Get(definitionExportPendingKey) == nil {
			return control.Put(definitionExportPendingKey, []byte{1})
		}
		return nil
	}); err != nil {
		return result, err
	}
	for {
		done, err := s.clearPricingMigrationBlock(bucketOutcomeDefinitionExport)
		if err != nil {
			return result, err
		}
		if err := migrationStep(step, "after_definition_clear_batch"); err != nil {
			return result, err
		}
		if done {
			break
		}
	}
	if err := s.migrate(func(tx *Tx) error {
		return tx.Bucket(bucketOutcomeDefinitionExportMeta).Put(definitionExportHeadKey, append(encodeUint64(0), definitionExportSeed[:]...))
	}); err != nil {
		return result, err
	}
	var after []byte
	expectedID := 0
	for {
		page, err := s.readDefinitionMigrationPage(after, expectedID)
		if err != nil {
			return result, err
		}
		for _, row := range page.rows {
			if _, err := validateDefinitionExportRow(row.key, row.raw); err != nil {
				return result, err
			}
			if err := migrationStep(step, "definition_export_row"); err != nil {
				return result, err
			}
		}
		expectedID = page.txID
		if len(page.rows) > 0 {
			if err := s.migrate(func(tx *Tx) error {
				if tx.ID() != page.txID+1 {
					return ErrDefinitionExportCorrupt
				}
				source := tx.Bucket(bucketOutcomeDefinitions)
				if source == nil {
					return ErrDefinitionExportCorrupt
				}
				for _, row := range page.rows {
					if !bytes.Equal(source.Get(row.key), row.raw) {
						return ErrDefinitionExportCorrupt
					}
					if err := appendOutcomeDefinitionExport(tx, row.key, row.raw); err != nil {
						return err
					}
				}
				return migrationStep(step, "before_definition_index_commit")
			}); err != nil {
				return result, err
			}
			expectedID++
			if err := migrationStep(step, "after_definition_index_batch"); err != nil {
				return result, err
			}
		}
		if page.done {
			break
		}
		after = page.after
	}
	if err := s.view(func(tx *Tx) error {
		if tx.ID() != expectedID {
			return ErrDefinitionExportCorrupt
		}
		var err error
		result, err = definitionExportHead(tx)
		return err
	}); err != nil {
		return OutcomeDefinitionExportHead{}, err
	}
	return result, nil
}

// Definition rows retain their own existing native allowance: raw<=8MiB and
// key<=128B. The aggregate wide-row ceiling is therefore8MiB+128B; borrowing
// the pricing aggregate ceiling would reject a valid legacy definition.
func (s *Store) readDefinitionMigrationPage(after []byte, expectedID int) (page pricingRecoveryPage, resultErr error) {
	err := s.view(func(tx *Tx) error {
		if expectedID != 0 && expectedID != tx.ID() {
			return ErrDefinitionExportCorrupt
		}
		page.txID = tx.ID()
		source := tx.Bucket(bucketOutcomeDefinitions)
		if source == nil {
			return ErrDefinitionExportCorrupt
		}
		cursor := source.Cursor()
		key, raw := cursor.First()
		if after != nil {
			key, raw = cursor.Seek(after)
			if bytes.Equal(key, after) {
				key, raw = cursor.Next()
			}
		}
		used := 0
		for key != nil {
			if len(key) == 0 || len(key) > outcomeDefinitionExportKeyBytes || raw == nil || len(raw) > outcomeDefinitionExportRowBytes {
				return ErrDefinitionExportCorrupt
			}
			size := len(key) + len(raw)
			if len(page.rows) == pricingRecoveryRows || len(page.rows) > 0 && size > pricingRecoveryBytes-used {
				break
			}
			page.rows = append(page.rows, pricingRecoveryRow{key: bytes.Clone(key), raw: bytes.Clone(raw)})
			used += size
			key, raw = cursor.Next()
		}
		page.done = key == nil
		if len(page.rows) > 0 {
			page.after = bytes.Clone(page.rows[len(page.rows)-1].key)
		}
		return nil
	})
	if err != nil {
		return pricingRecoveryPage{}, err
	}
	if !page.done && len(page.rows) == 0 {
		return pricingRecoveryPage{}, errors.New("definition migration made no progress")
	}
	return page, nil
}
