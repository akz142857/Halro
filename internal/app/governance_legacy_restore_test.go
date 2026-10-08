package app

// Explicit legacy-checkpoint adapter coverage only. Production Runtime and
// backup validation use authenticated fresh disk projections.
import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/akz142857/Halro/internal/governance"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

var errGovernanceCheckpointMismatch = errors.New("governance checkpoint does not match its journal frame")

func restoreGovernanceState(metadata *boltstore.Store, governanceLog *governance.Log, governanceKey []byte) (*governance.State, error) {
	journalSummary := governanceLog.Summary()
	anchor, anchorErr := metadata.GovernanceJournalAnchor()
	hasAnchor := anchorErr == nil
	if anchorErr != nil && !errors.Is(anchorErr, boltstore.ErrNotFound) {
		return nil, fmt.Errorf("load governance journal anchor: %w", anchorErr)
	}
	if !hasAnchor && journalSummary.Records > 0 {
		return nil, errGovernanceAnchorMismatch
	}
	if hasAnchor {
		if !governance.VerifyJournalAnchorAuth(governanceKey, anchor.Sequence, anchor.Offset, anchor.Hash, anchor.Authentication) ||
			anchor.Sequence > journalSummary.Records || anchor.Offset > journalSummary.Bytes {
			return nil, errGovernanceAnchorMismatch
		}
	}

	checkpoint, checkpointErr := metadata.LoadGovernanceCheckpoint()
	checkpointSequence := uint64(0)
	state := governance.NewState()
	if checkpointErr == nil {
		var snapshot governance.Snapshot
		valid := checkpoint.Version == governance.SnapshotVersion && checkpoint.Sequence <= journalSummary.Records && checkpoint.Offset <= journalSummary.Bytes &&
			governance.VerifyCheckpointAuth(governanceKey, checkpoint.Sequence, checkpoint.Offset, checkpoint.JournalHash,
				checkpoint.PayloadSHA256, checkpoint.Authentication) &&
			json.Unmarshal(checkpoint.Payload, &snapshot) == nil && snapshot.Sequence == checkpoint.Sequence && state.Restore(snapshot) == nil
		if valid {
			checkpointSequence = checkpoint.Sequence
		} else {
			_ = metadata.ResetGovernanceCheckpoint()
			state = governance.NewState()
		}
	} else if !errors.Is(checkpointErr, boltstore.ErrNotFound) {
		_ = metadata.ResetGovernanceCheckpoint()
	}

	replay := func(target *governance.State, skip uint64) error {
		anchorMatched := !hasAnchor
		checkpointMatched := skip == 0
		_, err := governanceLog.Replay(func(record governance.Record) error {
			if hasAnchor && record.Sequence == anchor.Sequence {
				if record.Offset != anchor.Offset || record.Hash != anchor.Hash {
					return errGovernanceAnchorMismatch
				}
				anchorMatched = true
			}
			if skip > 0 && record.Sequence == skip {
				if record.Offset != checkpoint.Offset || record.Hash != checkpoint.JournalHash {
					return errGovernanceCheckpointMismatch
				}
				checkpointMatched = true
			}
			if record.Sequence <= skip {
				return nil
			}
			return target.Apply(record)
		})
		if err != nil {
			return err
		}
		if !anchorMatched {
			return errGovernanceAnchorMismatch
		}
		if !checkpointMatched {
			return errGovernanceCheckpointMismatch
		}
		return nil
	}
	if err := replay(state, checkpointSequence); errors.Is(err, errGovernanceCheckpointMismatch) {
		_ = metadata.ResetGovernanceCheckpoint()
		state = governance.NewState()
		if err := replay(state, 0); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}

	if journalSummary.Records > 0 {
		head, authentication := governanceLog.AuthenticatedHead()
		if err := metadata.PutGovernanceJournalAnchor(head.Records, head.Bytes, head.LastHash, authentication); err != nil {
			return nil, fmt.Errorf("advance governance journal anchor: %w", err)
		}
	}
	return state, nil
}
