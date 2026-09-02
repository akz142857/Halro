package app

import (
	"errors"
	"os"
)

// Sealing on the maintenance tick.
//
// Two steps that deliberately do not happen together. Rolling holds the ledger
// writer for the length of a rename and must stay that cheap; compressing a
// sealed generation reads and rewrites gigabytes and must not hold it at all.
// Splitting them also means a crash between the two costs nothing: the rolled
// generation is already a complete, readable, verified file, and compaction is
// an optimisation that can be repeated or skipped.
//
// Nothing here deletes anything. A sealed generation is the accounting archive
// — the balances replay out of it on every start — so the growth this bounds is
// the growth of the *active* file and the size of what is kept, not the length
// of the history. An operator who needs the disk back moves old generations off
// the box; the manifest tells them exactly which bytes those are.

// sealLedgerGeneration rolls the active WAL once it passes the configured size.
func (r *Runtime) sealLedgerGeneration() {
	seal := r.config.Ledger.Seal
	if !seal.Enabled || seal.MaxActiveBytes <= 0 {
		return
	}
	if r.ledger.ActiveBytes() < seal.MaxActiveBytes {
		return
	}
	result, err := r.ledger.Roll()
	if err != nil {
		r.logger.Error("ledger generation was not sealed", "error", err)
		return
	}
	if !result.Rolled {
		return
	}
	r.logger.Info("ledger generation sealed",
		"generation", result.Sealed.Generation, "active_generation", result.Active,
		"bytes", result.Bytes, "records", result.Records,
		"first_sequence", result.Sealed.FirstSequence, "last_sequence", result.Sealed.LastSequence)
	// Read back what was just sealed rather than trusting the write. A segment
	// that cannot re-authenticate is the one failure this feature could
	// introduce into the accounting authority, and it has to be discovered now
	// — while the operator can still act — rather than at the next start.
	if _, err := r.ledger.VerifySealed(result.Sealed.Generation); err != nil {
		r.logger.Error("a sealed ledger generation does not verify",
			"generation", result.Sealed.Generation, "error", err)
	}
}

// compactLedgerSegments compresses one sealed generation per tick.
//
// One, not all: compaction competes with a process that is also serving
// requests, and a backlog of generations should be worked off over several
// ticks rather than in one long stall.
//
// The bar is that everything in the generation has reached both derivatives —
// the Parquet archive and the durable usage checkpoint. Compaction does not
// need it to be correct (the bytes are proven identical before anything points
// at the compressed copy), but a generation past that bar is one an operator
// can move off the box, and compacting in the same order makes "compressed"
// and "safe to move" mean the same thing.
func (r *Runtime) compactLedgerSegments() {
	seal := r.config.Ledger.Seal
	if !seal.Enabled || !seal.Compress {
		return
	}
	segments := r.ledger.Segments()
	if len(segments) == 0 {
		return
	}
	through, ok := r.ledgerArchivedThrough()
	if !ok {
		return
	}
	for _, segment := range segments {
		if segment.Compressed || segment.LastSequence > through {
			continue
		}
		compacted, err := r.ledger.Compact(segment.Generation)
		if err != nil {
			r.logger.Error("ledger generation was not compacted",
				"generation", segment.Generation, "error", err)
			return
		}
		r.logger.Info("ledger generation compacted",
			"generation", compacted.Generation, "bytes", compacted.Length, "file", compacted.File)
		return
	}
}

// ledgerArchivedThrough is the highest ledger sequence both derivatives have
// caught up to. It is a minimum rather than either one alone: a generation is
// only safely archivable once neither the archive nor the checkpoint still
// needs to read it.
func (r *Runtime) ledgerArchivedThrough() (uint64, bool) {
	manifest, err := r.usageExporter.LoadManifest()
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			r.logger.Warn("ledger segments not compacted: the export manifest could not be read", "error", err)
		}
		return 0, false
	}
	watermark, _, err := r.store.UsageCheckpoint()
	if err != nil {
		return 0, false
	}
	return min(manifest.LastSequence, watermark.Sequence), true
}
