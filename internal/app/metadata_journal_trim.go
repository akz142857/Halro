package app

import (
	"errors"
	"os"
)

// metadataJournalTrimThresholdBytes is the size the journal may reach before
// its applied prefix is dropped.
//
// A threshold rather than a timer, for the same reason the Ledger seals on one:
// the cost being bounded is file size, and an instance that writes nothing
// should not rewrite its journal on a schedule. The number is small because
// what it retains is small — under Standalone the projection is the only
// reader, so everything at or below the applied sequence is already in bbolt
// and the retained tail is a handful of frames.
//
// It matters more than it looks. The journal is on the request path, not only
// the Admin path: every Attempt with a Deployment prepares and commits a price
// pin, so an untrimmed file grows with traffic rather than with administration.
const metadataJournalTrimThresholdBytes = int64(16) << 20

// trimMetadataJournal drops the frames the projection no longer needs.
//
// Under HA the cut is bounded by the slowest member still eligible for
// incremental catch-up (§11.2 of the HA design). Standalone has no such member:
// bbolt already holds everything the applied frames described, so the applied
// sequence is the cut.
func (r *Runtime) trimMetadataJournal() {
	state, err := r.store.MetadataJournalState()
	if err != nil {
		r.logger.Warn("metadata journal state unreadable", "error", err)
		return
	}
	info, err := os.Stat(state.Path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		r.logger.Warn("metadata journal size unreadable", "error", err)
		return
	}
	if info.Size() < metadataJournalTrimThresholdBytes {
		return
	}
	if state.Applied <= state.TrimmedThrough {
		// Nothing has been applied since the last cut. Rewriting the file to
		// achieve no change would replace a durable artefact for nothing.
		return
	}
	trimmed, err := r.store.TrimMetadataJournal(state.Applied)
	if err != nil {
		r.logger.Error("metadata journal was not trimmed", "error", err)
		return
	}
	r.logger.Info("metadata journal trimmed",
		"epoch", trimmed.Epoch, "trimmed_through", trimmed.TrimmedThrough,
		"sequence", trimmed.Sequence, "bytes_before", info.Size())
}
