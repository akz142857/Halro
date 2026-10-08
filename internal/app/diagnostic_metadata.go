package app

import (
	"context"
	"errors"
	"fmt"
	"github.com/akz142857/Halro/internal/config"

	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

func assertConfiguredMetadataSchemaCurrent(ctx context.Context, cfg config.Config) error {
	store, err := openCommandMetadataReadOnly(ctx, cfg, cfg.MetadataPath())
	if err == nil {
		return store.Close()
	}
	return diagnosticSchemaError(err)
}

// assertMetadataSchemaCurrent refuses a read-only diagnostic on a data
// directory this build would have to migrate first.
//
// The reason is the shape of the migration rather than the migration itself.
// `boltstore.Open` runs every pending step, and those steps are one-way: once
// the schema has moved, the previous release refuses the directory and the only
// way back is a backup taken beforehand. That is the right trade for a command
// whose job is to write. It is the wrong trade for `ledger verify`, `audit
// verify` and `usage verify`, which an operator runs precisely because they have
// not decided to upgrade yet — and which, before this check, migrated the
// directory as a side effect and then reported a failure, so the operator read
// "do not upgrade" off a command that had already made upgrading irreversible.
//
// The refusal names both versions and the one command that is allowed to
// migrate, because "schema mismatch" alone reads as corruption to someone who
// has done nothing wrong.
func assertMetadataSchemaCurrent(path string, ceilings ...int64) error {
	if len(ceilings) > 1 {
		return errors.New("Metadata schema guard accepts one file ceiling")
	}
	limit := int64(0)
	if len(ceilings) == 1 {
		limit = ceilings[0]
	}
	store, err := boltstore.OpenReadOnlyWithDiskLimit(path, limit)
	if err == nil {
		return store.Close()
	}
	return diagnosticSchemaError(err)
}

func diagnosticSchemaError(err error) error {
	if errors.Is(err, boltstore.ErrSchemaVersionMismatch) {
		return fmt.Errorf(
			"%w; this command does not migrate. Run `halro start` to upgrade the data directory, "+
				"or run this command with the binary that wrote it. Take a backup first: the upgrade "+
				"is one-way and the previous release will refuse the directory afterwards", err,
		)
	}
	return err
}
