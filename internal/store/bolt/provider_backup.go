package bolt

import (
	"context"
	"encoding/json"
	"errors"
)

// ProviderObjectReference contains only the backup paths; unrelated payload
// fields are not decoded. The caller owns an immutable Metadata cut.
type ProviderObjectReference struct {
	ID              string `json:"id"`
	ObjectPath      string `json:"object_path"`
	InputObjectPath string `json:"input_object_path"`
}

var ErrProviderBackupChanged = errors.New("provider backup metadata changed from snapshot")

// WalkProviderObjectReferences uses the same bounded page copier as recovery
// (64 rows/1MiB, one wide legacy row up to the journal payload cap). Callbacks
// run outside the read transaction; every page and the final fence must match
// the supplied snapshot TxID. Effects are tentative until success.
func (s *Store) WalkProviderObjectReferences(ctx context.Context, snapshotTxID uint64, consume func(ProviderObjectReference) error) error {
	if snapshotTxID == 0 || consume == nil {
		return errors.New("provider backup snapshot and consumer are required")
	}
	check := func(tx *Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if uint64(tx.ID()) != snapshotTxID {
			return ErrProviderBackupChanged
		}
		return nil
	}
	var after []byte
	for {
		page, err := s.readPricingRecoveryPageChecked(ctx, bucketProviderResources, after, nil, check)
		if err != nil {
			return err
		}
		for _, row := range page.rows {
			if err := ctx.Err(); err != nil {
				return err
			}
			var reference ProviderObjectReference
			if err := json.Unmarshal(row.raw, &reference); err != nil {
				return err
			}
			if reference.ID == "" || reference.ID != string(row.key) {
				return errors.New("provider backup resource identity mismatch")
			}
			if err := consume(reference); err != nil {
				return err
			}
		}
		if page.done {
			return s.view(check)
		}
		after = page.after
	}
}
