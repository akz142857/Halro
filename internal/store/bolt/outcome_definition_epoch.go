package bolt

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"

	"github.com/akz142857/Halro/internal/metadatajournal"
)

// Pending survives migration commit, old-tail recovery, baseline commit and
// epoch-file publication. Phase 2 also binds the exact prior native head and
// prepared prefix, allowing the file-before-DB publication window to finish.
const definitionExportPreparedBytes = 89

func (s *Store) definitionExportEpochPending() ([]byte, error) {
	var pending []byte
	err := s.view(func(tx *Tx) error {
		if bucket := tx.Bucket(bucketOutcomeDefinitionExportMeta); bucket != nil {
			raw := bucket.Get(definitionExportPendingKey)
			if raw != nil {
				if len(raw) != 1 && len(raw) != definitionExportPreparedBytes {
					return ErrDefinitionExportCorrupt
				}
				if len(raw) == 1 && raw[0] != 1 || len(raw) == definitionExportPreparedBytes && raw[0] != 2 && raw[0] != 3 {
					return ErrDefinitionExportCorrupt
				}
				pending = bytes.Clone(raw)
			}
		}
		return nil
	})
	return pending, err
}

func (s *Store) attachDefinitionExportEpoch(key []byte, path string, stored journalPosition, state JournalState, reason string, pending []byte, maxBytes int64) (JournalState, error) {
	var oldHead metadatajournal.Head
	_, statErr := os.Stat(path)
	if statErr == nil {
		log, err := metadatajournal.OpenContextWithLimit(s.closingContext(), path, key, maxBytes)
		if err != nil {
			return state, fmt.Errorf("authenticate pre-migration metadata journal: %w", err)
		}
		head := log.Head()
		state.Epoch, state.Sequence, state.Applied, state.TrimmedThrough = head.Epoch, head.Sequence, stored.sequence, head.TrimmedThrough
		if len(pending) == definitionExportPreparedBytes && stored.epoch != ^uint64(0) && head.Epoch == stored.epoch+1 {
			if err := s.finishDefinitionExportEpoch(key, path, stored, head, pending); err != nil {
				log.Close()
				return state, err
			}
			s.journal, s.journalKey, s.journalMaxBytes = log, bytes.Clone(key), maxBytes
			state.Applied, state.StartedEpoch = 0, true
			return state, nil
		}
		// Migration 41 only reserves its buckets at Open. Reconcile while the
		// old authenticated journal still exists, before building the prefix.
		if err := s.reconcileJournal(log, stored, &state, key, path); err != nil {
			log.Close()
			return state, err
		}
		oldHead = head
		if err := log.Close(); err != nil {
			return state, err
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return state, statErr
	} else if len(pending) == definitionExportPreparedBytes && pending[0] == 2 {
		return state, fmt.Errorf("%w: prepared metadata epoch lost its native journal", ErrJournalDiverged)
	} else {
		// A first publication or an existing whole-file restore without a
		// carried journal. Preserve its prior position, and distinguish this
		// explicitly from losing a journal that was authenticated at prepare.
		oldHead.Epoch, oldHead.Sequence = stored.epoch, stored.sequence
	}
	if err := s.prepareDefinitionExportEpoch(oldHead); err != nil {
		return state, err
	}
	return s.startEpoch(key, path, stored, state, reason, maxBytes)
}

func (s *Store) prepareDefinitionExportEpoch(oldHead metadatajournal.Head) error {
	if err := s.rebuildPricingMigrationAfterJournal(nil); err != nil {
		return err
	}
	prefix, err := s.rebuildOutcomeDefinitionExportBounded(nil)
	if err != nil {
		return err
	}
	return s.migrate(func(tx *Tx) error {
		current, err := definitionExportHead(tx)
		if err != nil {
			return err
		}
		if current.Count != prefix.Count || current.Hash != prefix.Hash {
			return ErrDefinitionExportCorrupt
		}
		phase := byte(2)
		if oldHead.Hash == ([32]byte{}) {
			phase = 3
		}
		pending := []byte{phase}
		pending = append(pending, encodeUint64(oldHead.Epoch)...)
		pending = append(pending, encodeUint64(oldHead.Sequence)...)
		pending = append(pending, oldHead.Hash[:]...)
		pending = append(pending, encodeUint64(prefix.Count)...)
		pending = append(pending, prefix.Hash[:]...)
		return tx.Bucket(bucketOutcomeDefinitionExportMeta).Put(definitionExportPendingKey, pending)
	})
}

func (s *Store) finishDefinitionExportEpoch(key []byte, path string, stored journalPosition, head metadatajournal.Head, pending []byte) error {
	if head.Sequence != 0 || binary.BigEndian.Uint64(pending[1:9]) != stored.epoch || binary.BigEndian.Uint64(pending[9:17]) != stored.sequence {
		return ErrJournalDiverged
	}
	var header metadatajournal.EpochHeader
	if _, err := metadatajournal.ReplayContext(s.closingContext(), path, key, func(record metadatajournal.Record) error {
		if record.Kind == metadatajournal.KindEpoch {
			header = record.Header
		}
		return nil
	}); err != nil {
		return err
	}
	priorHashMatches := bytes.Equal(header.PreviousChainHead, pending[17:49])
	if pending[0] == 3 && len(header.PreviousChainHead) == 0 && bytes.Equal(pending[17:49], make([]byte, 32)) {
		priorHashMatches = true
	}
	if header.Epoch != head.Epoch || header.PreviousEpoch != stored.epoch || !priorHashMatches {
		return ErrJournalDiverged
	}
	// A cross-process prepared index is not trusted by its last entry alone.
	// Rebuild from the complete source and compare its entire count/hash binding.
	if _, err := s.rebuildOutcomeDefinitionExportBounded(nil); err != nil {
		return err
	}
	if err := s.view(func(tx *Tx) error {
		prefix, err := definitionExportHead(tx)
		if err != nil {
			return err
		}
		if prefix.Count != binary.BigEndian.Uint64(pending[49:57]) || !bytes.Equal(prefix.Hash[:], pending[57:89]) {
			return ErrDefinitionExportCorrupt
		}
		return nil
	}); err != nil {
		return err
	}
	// The epoch file may have reached disk before the position/ready commit.
	// Rebuild finite blocks from the prepared facts instead of trusting a bare
	// intermediate index/progress marker left by the previous process.
	if err := s.rebuildPricingMigrationAfterJournal(nil); err != nil {
		return err
	}
	return s.writeJournalPosition(head.Epoch, 0)
}
