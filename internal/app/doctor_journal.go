package app

import (
	"errors"
	"fmt"
	"os"

	"github.com/akz142857/Halro/internal/metadatajournal"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/vault"
)

// checkDoctorMetadataJournal answers the question the journal made askable:
// does halro.db still describe the same state its write-ahead log does?
//
// It is read-only, like the rest of doctor — it authenticates the chain and
// compares the head against the position stored in bbolt, and never replays.
// A projection behind the journal is the ordinary crash outcome and heals on
// the next start; a projection *ahead* of it cannot be produced by any ordering
// this code performs, so it is reported as a divergence rather than as lag.
func checkDoctorMetadataJournal(
	store *boltstore.Store, secretVault *vault.Vault, masterKey []byte, add func(string, string, string),
) {
	if store == nil {
		add("metadata_journal", "fail", "metadata is unavailable, so the journal has nothing to be compared against")
		return
	}
	state, err := store.MetadataJournalState()
	if err != nil {
		add("metadata_journal", "fail", err.Error())
		return
	}
	if _, statErr := os.Stat(state.Path); errors.Is(statErr, os.ErrNotExist) {
		if state.Epoch == 0 {
			// A data directory written before the journal existed. The next
			// start publishes epoch 1 with this database as its starting
			// projection; nothing is missing.
			add("metadata_journal", "warn",
				"no metadata journal yet; the next start publishes one with this database as its starting state")
			return
		}
		add("metadata_journal", "fail", fmt.Sprintf(
			"this database follows metadata journal epoch %d and the file is gone", state.Epoch))
		return
	} else if statErr != nil {
		add("metadata_journal", "fail", statErr.Error())
		return
	}
	if secretVault == nil || masterKey == nil {
		add("metadata_journal", "unverified", fmt.Sprintf(
			"epoch %d applied through %d; the chain was not authenticated because the Master Key was not unwrapped",
			state.Epoch, state.Applied))
		return
	}
	key, err := loadMetadataJournalHMACKey(store, secretVault, masterKey)
	if err != nil {
		add("metadata_journal", "fail", err.Error())
		return
	}
	defer clear(key)
	head, err := metadatajournal.Verify(state.Path, key)
	if err != nil {
		add("metadata_journal", "fail", err.Error())
		return
	}
	switch {
	case head.Epoch != state.Epoch:
		add("metadata_journal", "fail", fmt.Sprintf(
			"this database follows epoch %d and the journal holds epoch %d", state.Epoch, head.Epoch))
	case state.Applied > head.Sequence:
		add("metadata_journal", "fail", fmt.Sprintf(
			"this database applied sequence %d and the journal ends at %d; the projection is ahead of its log",
			state.Applied, head.Sequence))
	case head.TrimmedThrough > state.Applied:
		add("metadata_journal", "fail", fmt.Sprintf(
			"this database applied sequence %d and the journal was trimmed through %d, so the gap cannot be replayed",
			state.Applied, head.TrimmedThrough))
	case state.Applied < head.Sequence:
		// The window between a frame's fsync and its transaction's commit. The
		// next start replays it; saying so is more useful than calling it a
		// fault, because it is what a clean crash looks like.
		add("metadata_journal", "warn", fmt.Sprintf(
			"epoch %d: %d transaction(s) recorded and not yet applied; the next start replays them",
			head.Epoch, head.Sequence-state.Applied))
	default:
		detail := fmt.Sprintf("epoch %d authenticated through sequence %d", head.Epoch, head.Sequence)
		if head.TrimmedThrough > 0 {
			detail += fmt.Sprintf(", trimmed through %d", head.TrimmedThrough)
		}
		add("metadata_journal", "pass", detail)
	}
}
