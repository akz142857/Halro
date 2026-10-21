package app

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/usage"
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
// of the history.
//
// Nor can an operator take a generation away yet, whatever the size of the
// archive. The sealed history has to read as an unbroken run from generation
// one, because every check that says the archive is whole is defined against
// what the manifest lists: a manifest edited to forget its oldest generations
// is still ordered, still present, still verifies — and a full replay would
// rebuild balances from a suffix of the history and report nothing wrong.
// Removing one safely needs a durable record of what was removed and of the
// balances it carried, and there is not one. Compression is the lever that
// exists today; it takes the archive to roughly a fifth.

// warnAboutUnsealedLedger tells the operator of an upgraded install that the
// sealing default never reached them. Migration keeps values an operator wrote
// down, and the v0.8.5 example wrote `enabled: false` explicitly, so a config
// copied from it still has sealing off — and every start still reads the whole
// active WAL, which is the cost sealing exists to bound. Past the default
// threshold that cost is already being paid; below it, nothing is said.
func (r *Runtime) warnAboutUnsealedLedger(activeBytes int64) {
	if r.config.Ledger.Seal.Enabled || activeBytes <= config.DefaultLedgerSealMaxActiveBytes {
		return
	}
	r.logger.Warn(
		"ledger sealing is off and the active WAL is past the default seal threshold; "+
			"every start reads the whole active file, so start time keeps growing with history",
		"active_bytes", activeBytes, "default_threshold_bytes", config.DefaultLedgerSealMaxActiveBytes,
		"remedy", "set ledger.seal.enabled: true (and ledger.seal.max_active_bytes: 268435456)",
	)
}

// sealLedgerGeneration rolls the active WAL once it passes the configured size.
//
// Never on a Replica. The Primary decides where a generation ends and the
// roll reaches a Replica as an ordered record (HA design §6.2.3); one that
// rolled on its own size check would end a generation where the Primary's
// does not, and every frame after it would conflict. Its Ledger refuses a
// local roll as well, but asking it every few seconds would only log that.
func (r *Runtime) sealLedgerGeneration() {
	seal := r.config.Ledger.Seal
	if !seal.Enabled || seal.MaxActiveBytes <= 0 || r.isReplica() {
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
	//
	// Logging it was not enough. A detected failure in the accounting authority
	// that leaves the process serving is a fail-open: the run would go on
	// settling requests, and would later compress and back up the generation it
	// had already found unreadable. The status latches instead, which stops
	// traffic and says so, and the operator's next step is
	// `halro ledger verify` on a stopped instance.
	if _, err := r.ledger.VerifySealed(result.Sealed.Generation); err != nil {
		r.logger.Error("a sealed ledger generation does not verify; accounting requires recovery",
			"generation", result.Sealed.Generation, "error", err,
			"remedy", "stop the instance and run `halro ledger verify`")
		r.status.RequireRecovery()
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
// only safely archivable once neither the archive nor the history index still
// needs to read it.
//
// A Replica exports no Parquet (HA design §6.2), so the manifest is not its
// bar: there it is the history index and the applied prefix. The index only
// ever holds applied records, so the second is the first restated, kept so the
// bar does not rest on how the index is fed. Compression is node-local and the
// two nodes need not agree on it (§6.2.3).
func (r *Runtime) ledgerArchivedThrough() (uint64, bool) {
	replica := r.isReplica()
	var exported uint64
	if replica {
		exported = r.replication.ledgerApplied.sequence()
	} else {
		manifest, err := r.usageExporter.LoadManifest()
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				r.logger.Warn("ledger segments not compacted: the export manifest could not be read", "error", err)
			}
			return 0, false
		}
		exported = manifest.LastSequence
	}
	state, err := r.usage.History().State()
	if err != nil {
		// An index with nothing written yet holds no position and says
		// nothing. Anything else is the index failing, which silently stops
		// compaction from ever running — the same treatment as the manifest.
		if !usage.IsHistoryEmpty(err) {
			r.logger.Warn("ledger segments not compacted: the usage history could not be read", "error", err)
		}
		return 0, false
	}
	return min(exported, state.Watermark.Sequence), true
}

// runReplicaMaintenance is intentionally smaller than runUsageMaintenance.
// A Replica keeps its usage history caught up with what it applied and checks
// it against its state — the history is what the console reads once it is
// promoted — and may compact generations the Primary has already sealed. It
// never exports Parquet, rolls the Ledger, delivers alerts, anchors Audit, or
// writes the Token Guard checkpoint. Its state snapshots run on their own loop.
func (r *Runtime) runReplicaMaintenance(ctx context.Context) {
	checkpointTicker := time.NewTicker(r.config.Usage.CheckpointInterval.Value())
	compactionTicker := time.NewTicker(r.config.Usage.ParquetInterval.Value())
	defer checkpointTicker.Stop()
	defer compactionTicker.Stop()
	for {
		select {
		case <-checkpointTicker.C:
			r.reopenHistoryFiles(true)
			r.catchUpUsageHistory()
		case <-compactionTicker.C:
			r.reconcileUsageHistory(time.Now())
			r.compactLedgerSegments()
			r.reopenHistoryFiles(false)
		case <-ctx.Done():
			return
		}
	}
}
