package replication

import (
	"errors"
	"fmt"
)

// DurableSource exposes native store frame boundaries to startup recovery.
// Read returns the exact bytes for one ordering record. Cursor describes the
// physical tail, and Truncate removes a source-fsynced suffix for which no
// authenticated ordering record exists. Implementations must fsync both the
// truncated file and any directory entry their native format changes.
type DurableSource interface {
	Cursor(store Store) (StoreCursor, error)
	HeadAt(store Store, cursor StoreCursor) ([32]byte, error)
	Read(store Store, generation, firstSequence, lastSequence uint64) ([]byte, error)
	Truncate(store Store, cursor StoreCursor) error
}

type pendingLedgerRollSource interface {
	PendingLedgerRoll(generation uint64) ([]byte, error)
}

// RecoverLocalCommits reconciles the four source stores to the authenticated
// ordering head and reconstructs the exact encoded suffix above confirmed.
// It is intentionally run before a Primary starts network delivery: an
// un-ordered source tail is a failed local commit, while an ordered record with
// missing/different source bytes is corruption and fails closed.
func RecoverLocalCommits(clusterID, incarnation string, confirmed uint64, journal *OrderingJournal, source DurableSource) ([]LocalCommit, error) {
	if journal == nil || source == nil {
		return nil, errors.New("replication recovery requires an ordering journal and durable source")
	}
	head, term, _ := journal.Head()
	if confirmed > head {
		return nil, fmt.Errorf("confirmed index %d exceeds ordering head %d", confirmed, head)
	}

	expected := make(map[Store]StoreCursor, int(StoreMetadata))
	baseline := journal.BaselineCursors()
	baselineHeads := journal.BaselineHeads()
	for store := StoreLedger; store <= StoreMetadata; store++ {
		expected[store] = baseline[store-StoreLedger]
		if baseline[store-StoreLedger].Sequence > 0 || baselineHeads[store-StoreLedger] != ([32]byte{}) {
			head, err := source.HeadAt(store, baseline[store-StoreLedger])
			if err != nil {
				return nil, fmt.Errorf("authenticate source store %d baseline: %w", store, err)
			}
			if head != baselineHeads[store-StoreLedger] {
				return nil, fmt.Errorf("source store %d does not match its authenticated ordering baseline", store)
			}
		}
	}
	ledgerCursorIndex := uint64(0)
	for index := uint64(1); index <= head; index++ {
		record, err := journal.Record(index)
		if err != nil {
			return nil, fmt.Errorf("read ordering record %d for source reconciliation: %w", index, err)
		}
		switch record.Kind {
		case KindData:
			cursor := expected[record.Store]
			if cursor.Generation == 0 {
				if record.StoreSequenceFirst != 1 {
					return nil, fmt.Errorf("ordering store %d begins at sequence %d", record.Store, record.StoreSequenceFirst)
				}
			} else if cursor.Generation != record.StoreGeneration || cursor.Sequence+1 != record.StoreSequenceFirst {
				return nil, fmt.Errorf("ordering store %d range %d/%d does not continue %d/%d", record.Store, record.StoreGeneration, record.StoreSequenceFirst, cursor.Generation, cursor.Sequence)
			}
			expected[record.Store] = StoreCursor{Generation: record.StoreGeneration, Sequence: record.StoreSequenceLast}
			if record.Store == StoreLedger {
				ledgerCursorIndex = index
			}
		case KindLedgerRoll:
			roll, err := DecodeLedgerRollMetadata(record.Metadata)
			if err != nil {
				return nil, err
			}
			cursor := expected[StoreLedger]
			if cursor.Generation != roll.Generation || cursor.Sequence != roll.LastSequence {
				return nil, errors.New("ordering ledger roll does not match its source cursor")
			}
			expected[StoreLedger] = StoreCursor{Generation: roll.Generation + 1, Sequence: roll.LastSequence}
		}
	}

	// A Roll publishes the native manifest/rename before it records the global
	// structural index. That ordering is required so a Replica never observes a
	// Roll the Primary has not committed locally, but it leaves one deliberate
	// crash state: the Ledger is exactly one empty generation ahead at the same
	// sequence. The sealed manifest is a durable, authenticated intent. Complete
	// that transaction by assigning its missing global index; never roll it back.
	ledgerActual, err := source.Cursor(StoreLedger)
	if err != nil {
		return nil, fmt.Errorf("read source store %d cursor: %w", StoreLedger, err)
	}
	ledgerExpected := expected[StoreLedger]
	if ledgerExpected.Generation > 0 && ledgerActual.Generation == ledgerExpected.Generation+1 && ledgerActual.Sequence == ledgerExpected.Sequence {
		if ledgerCursorIndex > confirmed {
			return nil, errors.New("cannot recover Ledger Roll whose final data frame is unconfirmed")
		}
		rollSource, ok := source.(pendingLedgerRollSource)
		if !ok {
			return nil, errors.New("source cannot recover a durable Ledger Roll missing from ordering")
		}
		if term == 0 {
			return nil, errors.New("cannot recover Ledger Roll before a leadership term is established")
		}
		metadata, err := rollSource.PendingLedgerRoll(ledgerExpected.Generation)
		if err != nil {
			return nil, fmt.Errorf("authenticate pending Ledger Roll: %w", err)
		}
		roll, err := DecodeLedgerRollMetadata(metadata)
		if err != nil || roll.Generation != ledgerExpected.Generation || roll.LastSequence != ledgerExpected.Sequence {
			return nil, errors.New("pending Ledger Roll does not match the authenticated ordering cursor")
		}
		frame := Frame{
			Kind: KindLedgerRoll, Index: head + 1, Term: term, ConfirmedIndex: confirmed,
			ClusterID: clusterID, Incarnation: incarnation, Metadata: metadata,
		}
		_, digest, err := frame.MarshalBinaryAndDigest()
		if err != nil {
			return nil, err
		}
		if _, err := journal.Append(OrderingRecord{
			Kind: KindLedgerRoll, Index: frame.Index, Term: term, ConfirmedIndex: confirmed,
			Metadata: metadata, FrameDigest: digest,
		}); err != nil {
			return nil, fmt.Errorf("complete pending Ledger Roll ordering record: %w", err)
		}
		head++
		expected[StoreLedger] = ledgerActual
	}

	for store := StoreLedger; store <= StoreMetadata; store++ {
		actual, err := source.Cursor(store)
		if err != nil {
			return nil, fmt.Errorf("read source store %d cursor: %w", store, err)
		}
		want := expected[store]
		// Zero is the canonical empty baseline used by early test fixtures and
		// a newly created store. Native formats still report their generation or
		// epoch even when sequence is zero; there is no suffix to truncate.
		if want == (StoreCursor{}) && actual.Sequence == 0 {
			continue
		}
		if sourceCursorBehind(actual, want) {
			return nil, fmt.Errorf("source store %d cursor %d/%d is behind ordering %d/%d", store, actual.Generation, actual.Sequence, want.Generation, want.Sequence)
		}
		if actual != want {
			if err := source.Truncate(store, want); err != nil {
				return nil, fmt.Errorf("truncate unordered source store %d suffix: %w", store, err)
			}
			verified, err := source.Cursor(store)
			if err != nil {
				return nil, fmt.Errorf("verify source store %d cursor after truncation: %w", store, err)
			}
			if verified != want {
				return nil, fmt.Errorf("source store %d truncation ended at %d/%d, want %d/%d", store, verified.Generation, verified.Sequence, want.Generation, want.Sequence)
			}
		}
	}

	commits := make([]LocalCommit, 0, head-confirmed)
	for index := confirmed + 1; index <= head; index++ {
		record, err := journal.Record(index)
		if err != nil {
			return nil, err
		}
		var payload []byte
		if record.Kind == KindData {
			payload, err = source.Read(record.Store, record.StoreGeneration, record.StoreSequenceFirst, record.StoreSequenceLast)
			if err != nil {
				return nil, fmt.Errorf("read source bytes for ordering index %d: %w", index, err)
			}
		}
		frame := Frame{
			Kind: record.Kind, Store: record.Store, Index: record.Index, Term: record.Term,
			ConfirmedIndex: record.ConfirmedIndex, StoreGeneration: record.StoreGeneration,
			StoreSequenceFirst: record.StoreSequenceFirst, StoreSequenceLast: record.StoreSequenceLast,
			ClusterID: clusterID, Incarnation: incarnation,
			Metadata: append([]byte(nil), record.Metadata...), Payload: append([]byte(nil), payload...),
		}
		encoded, digest, err := frame.MarshalBinaryAndDigest()
		if err != nil {
			return nil, fmt.Errorf("reconstruct ordering index %d: %w", index, err)
		}
		if digest != record.FrameDigest {
			return nil, fmt.Errorf("source bytes for ordering index %d do not match its authenticated digest", index)
		}
		commits = append(commits, LocalCommit{Index: index, Encoded: encoded})
	}
	return commits, nil
}

func sourceCursorBehind(actual, expected StoreCursor) bool {
	if actual.Generation != expected.Generation {
		return actual.Generation < expected.Generation
	}
	return actual.Sequence < expected.Sequence
}
