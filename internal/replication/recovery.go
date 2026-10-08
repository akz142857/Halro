package replication

import (
	"errors"
	"fmt"
)

// FrameSource reads exact native bytes for one ordering record. Reconstruction
// does not need authority to repair or truncate a source.
type FrameSource interface {
	Read(store Store, generation, firstSequence, lastSequence uint64) ([]byte, error)
}

// DurableSource exposes native store frame boundaries to startup recovery.
// Cursor describes the physical tail, and Truncate removes a source-fsynced
// suffix for which no
// authenticated ordering record exists. Implementations must fsync both the
// truncated file and any directory entry their native format changes.
type DurableSource interface {
	FrameSource
	Cursor(store Store) (StoreCursor, error)
	HeadAt(store Store, cursor StoreCursor) ([32]byte, error)
	Truncate(store Store, cursor StoreCursor) error
}

type pendingLedgerRollSource interface {
	PendingLedgerRoll(generation uint64) ([]byte, error)
}

type providerObjectSource interface {
	ReadProviderObjectChunk(ProviderObjectMetadata) ([]byte, error)
}

// RecoverReplicaStores reconciles native crash tails to the authenticated
// ordering prefix before any Replica log is opened. Individual log open paths
// intentionally refuse to guess whether a partial frame or interrupted Roll
// belongs to the Primary; this is the one place that supplies that authority.
func RecoverReplicaStores(journal *OrderingJournal, source *NativeSource) error {
	if journal == nil || source == nil {
		return errors.New("replica recovery requires an ordering journal and native source")
	}
	if err := source.RecoverMetadataBaseline(journal); err != nil {
		return err
	}
	if plan := source.options.MetadataBaseline; plan != nil {
		head, _, _ := journal.Head()
		if head > plan.Authority.claims.Index {
			if err := source.ActivateMetadataBaseline(journal, plan.Authority.claims.Index+1); err != nil {
				return err
			}
		}
	}
	expected, err := journal.StoreCursors()
	if err != nil {
		return err
	}
	if err := source.RepairReplicaLedgerRoll(expected[StoreLedger-StoreLedger]); err != nil {
		return fmt.Errorf("repair interrupted Replica Ledger Roll: %w", err)
	}
	for store := StoreLedger; store <= StoreMetadata; store++ {
		cursor := expected[store-StoreLedger]
		actual, cursorErr := source.Cursor(store)
		if cursorErr != nil {
			// A native cursor cannot be read through a torn final frame. Repair is
			// allowed only when the complete authenticated prefix ends exactly at
			// the ordering cursor.
			if err := source.RepairReplicaTail(store, cursor); err != nil {
				return fmt.Errorf("repair Replica store %d tail: %w", store, err)
			}
			actual, cursorErr = source.Cursor(store)
			if cursorErr != nil {
				return fmt.Errorf("read Replica store %d after partial-tail repair: %w", store, cursorErr)
			}
		}
		if sourceCursorBehind(actual, cursor) {
			return fmt.Errorf("Replica store %d cursor %d/%d is behind ordering %d/%d",
				store, actual.Generation, actual.Sequence, cursor.Generation, cursor.Sequence)
		}
		if actual != cursor {
			// A complete native frame is fsynced before its ordering record. A
			// crash in that gap leaves an authenticated but unauthorized suffix;
			// ordering is the authority that permits truncating it here.
			if err := source.Truncate(store, cursor); err != nil {
				return fmt.Errorf("truncate unordered Replica store %d suffix: %w", store, err)
			}
		}
		actual, err := source.Cursor(store)
		if err != nil {
			return fmt.Errorf("verify Replica store %d after repair: %w", store, err)
		}
		if actual != cursor {
			return fmt.Errorf("Replica store %d ended at %d/%d after repair, want %d/%d", store, actual.Generation, actual.Sequence, cursor.Generation, cursor.Sequence)
		}
	}
	return nil
}

// RecoverLocalCommits reconciles the four source stores to the authenticated
// ordering head and reconstructs the exact encoded suffix above confirmed.
// It is intentionally run before a Primary starts network delivery: an
// un-ordered source tail is a failed local commit, while an ordered record with
// missing/different source bytes is corruption and fails closed.
func RecoverLocalCommits(clusterID, incarnation string, confirmed uint64, journal *OrderingJournal, source DurableSource) ([]LocalCommit, error) {
	var commits []LocalCommit
	err := RecoverLocalCommitsEach(clusterID, incarnation, confirmed, journal, source, func(commit LocalCommit) error {
		commits = append(commits, commit)
		return nil
	})
	return commits, err
}

// RecoverLocalCommitsEach reconciles the authoritative stores and yields the
// verified suffix one frame at a time. The consumer must finish or copy that
// frame before returning. No network delivery or member-state publication may
// happen until this entire call succeeds: later source corruption must not
// expose a partially validated suffix. Runtime uses the streaming coordinator
// constructor to enforce that barrier without retaining a second input slice.
func RecoverLocalCommitsEach(clusterID, incarnation string, confirmed uint64, journal *OrderingJournal, source DurableSource, consume func(LocalCommit) error) error {
	if consume == nil {
		return errors.New("replication recovery consumer is required")
	}
	if err := ReconcileLocalStores(clusterID, incarnation, confirmed, journal, source); err != nil {
		return err
	}
	head, _, _ := journal.Head()
	return ReconstructRangeEach(clusterID, incarnation, confirmed+1, head, journal, source, consume)
}

// ReconcileLocalStores authenticates baselines and reconciles crash tails to
// ordering, including completing an authenticated interrupted Ledger Roll.
// It never gathers or delivers a suffix. Run it before the coordinator freezes
// its recovered head; reconciliation may append the missing Roll index.
func ReconcileLocalStores(clusterID, incarnation string, confirmed uint64, journal *OrderingJournal, source DurableSource) error {
	if journal == nil || source == nil {
		return errors.New("replication recovery requires an ordering journal and durable source")
	}
	if owner, ok := source.(interface{ RecoverMetadataBaseline(*OrderingJournal) error }); ok {
		if err := owner.RecoverMetadataBaseline(journal); err != nil {
			return err
		}
	}
	head, term, _ := journal.Head()
	if confirmed > head {
		return fmt.Errorf("confirmed index %d exceeds ordering head %d", confirmed, head)
	}

	expected := make(map[Store]StoreCursor, int(StoreMetadata))
	baseline := journal.BaselineCursors()
	baselineHeads := journal.BaselineHeads()
	for store := StoreLedger; store <= StoreMetadata; store++ {
		expected[store] = baseline[store-StoreLedger]
		if baseline[store-StoreLedger].Sequence > 0 || baselineHeads[store-StoreLedger] != ([32]byte{}) {
			head, err := source.HeadAt(store, baseline[store-StoreLedger])
			if err != nil {
				return fmt.Errorf("authenticate source store %d baseline: %w", store, err)
			}
			if head != baselineHeads[store-StoreLedger] {
				return fmt.Errorf("source store %d does not match its authenticated ordering baseline", store)
			}
		}
	}
	ledgerCursorIndex := uint64(0)
	for index := uint64(1); index <= head; index++ {
		record, err := journal.Record(index)
		if err != nil {
			return fmt.Errorf("read ordering record %d for source reconciliation: %w", index, err)
		}
		switch record.Kind {
		case KindData:
			cursor := expected[record.Store]
			if cursor.Generation == 0 {
				if record.StoreSequenceFirst != 1 {
					return fmt.Errorf("ordering store %d begins at sequence %d", record.Store, record.StoreSequenceFirst)
				}
			} else if cursor.Generation != record.StoreGeneration || cursor.Sequence+1 != record.StoreSequenceFirst {
				return fmt.Errorf("ordering store %d range %d/%d does not continue %d/%d", record.Store, record.StoreGeneration, record.StoreSequenceFirst, cursor.Generation, cursor.Sequence)
			}
			expected[record.Store] = StoreCursor{Generation: record.StoreGeneration, Sequence: record.StoreSequenceLast}
			if record.Store == StoreLedger {
				ledgerCursorIndex = index
			}
		case KindMetadataBaseline:
			owner, ok := source.(metadataBaselineSource)
			if !ok {
				return errors.New("Metadata baseline recovery has no independent source owner")
			}
			boundary, err := DecodeMetadataBaselineMetadata(record.Metadata)
			if err != nil {
				return err
			}
			if expected[StoreMetadata] != (StoreCursor{Generation: boundary.PreviousEpoch, Sequence: boundary.PreviousSequence}) {
				return errors.New("Metadata baseline recovery does not continue old cursor")
			}
			if err := owner.ActivateMetadataBaseline(journal, index); err != nil {
				return err
			}
			expected[StoreMetadata] = StoreCursor{Generation: boundary.Epoch}
		case KindLedgerRoll:
			roll, err := DecodeLedgerRollMetadata(record.Metadata)
			if err != nil {
				return err
			}
			cursor := expected[StoreLedger]
			if cursor.Generation != roll.Generation || cursor.Sequence != roll.LastSequence {
				return errors.New("ordering ledger roll does not match its source cursor")
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
		return fmt.Errorf("read source store %d cursor: %w", StoreLedger, err)
	}
	ledgerExpected := expected[StoreLedger]
	if ledgerExpected.Generation > 0 && ledgerActual.Generation == ledgerExpected.Generation+1 && ledgerActual.Sequence == ledgerExpected.Sequence {
		if ledgerCursorIndex > confirmed {
			return errors.New("cannot recover Ledger Roll whose final data frame is unconfirmed")
		}
		rollSource, ok := source.(pendingLedgerRollSource)
		if !ok {
			return errors.New("source cannot recover a durable Ledger Roll missing from ordering")
		}
		if term == 0 {
			return errors.New("cannot recover Ledger Roll before a leadership term is established")
		}
		metadata, err := rollSource.PendingLedgerRoll(ledgerExpected.Generation)
		if err != nil {
			return fmt.Errorf("authenticate pending Ledger Roll: %w", err)
		}
		roll, err := DecodeLedgerRollMetadata(metadata)
		if err != nil || roll.Generation != ledgerExpected.Generation || roll.LastSequence != ledgerExpected.Sequence {
			return errors.New("pending Ledger Roll does not match the authenticated ordering cursor")
		}
		frame := Frame{
			Kind: KindLedgerRoll, Index: head + 1, Term: term, ConfirmedIndex: confirmed,
			ClusterID: clusterID, Incarnation: incarnation, Metadata: metadata,
		}
		_, digest, err := frame.MarshalBinaryAndDigest()
		if err != nil {
			return err
		}
		if _, err := journal.Append(OrderingRecord{
			Kind: KindLedgerRoll, Index: frame.Index, Term: term, ConfirmedIndex: confirmed,
			Metadata: metadata, FrameDigest: digest,
		}); err != nil {
			return fmt.Errorf("complete pending Ledger Roll ordering record: %w", err)
		}
		head++
		expected[StoreLedger] = ledgerActual
	}

	for store := StoreLedger; store <= StoreMetadata; store++ {
		actual, err := source.Cursor(store)
		if err != nil {
			return fmt.Errorf("read source store %d cursor: %w", store, err)
		}
		want := expected[store]
		// Zero is the canonical empty baseline used by early test fixtures and
		// a newly created store. Native formats still report their generation or
		// epoch even when sequence is zero; there is no suffix to truncate.
		if want == (StoreCursor{}) && actual.Sequence == 0 {
			continue
		}
		if sourceCursorBehind(actual, want) {
			return fmt.Errorf("source store %d cursor %d/%d is behind ordering %d/%d", store, actual.Generation, actual.Sequence, want.Generation, want.Sequence)
		}
		if actual != want {
			if err := source.Truncate(store, want); err != nil {
				return fmt.Errorf("truncate unordered source store %d suffix: %w", store, err)
			}
			verified, err := source.Cursor(store)
			if err != nil {
				return fmt.Errorf("verify source store %d cursor after truncation: %w", store, err)
			}
			if verified != want {
				return fmt.Errorf("source store %d truncation ended at %d/%d, want %d/%d", store, verified.Generation, verified.Sequence, want.Generation, want.Sequence)
			}
		}
	}

	return nil
}

// ReconstructRange rebuilds a finite range into memory. Catch-up paths should
// use ReconstructRangeEach so a lagging Replica cannot make the Primary retain
// its entire history at once.
func ReconstructRange(clusterID, incarnation string, first, last uint64, journal *OrderingJournal, source FrameSource) ([]LocalCommit, error) {
	commits := make([]LocalCommit, 0)
	err := ReconstructRangeEach(clusterID, incarnation, first, last, journal, source, func(commit LocalCommit) error {
		commits = append(commits, commit)
		return nil
	})
	return commits, err
}

// ReconstructRangeEach rebuilds exact wire frames from an authenticated
// ordering range one record at a time. The callback must consume or copy a
// commit before returning. This bounds reconstruction memory by one maximum
// frame instead of by the peer's complete lag.
func ReconstructRangeEach(clusterID, incarnation string, first, last uint64, journal *OrderingJournal, source FrameSource, consume func(LocalCommit) error) error {
	if journal == nil || source == nil {
		return errors.New("replication reconstruction requires an ordering journal and durable source")
	}
	if consume == nil {
		return errors.New("replication reconstruction consumer is required")
	}
	if len(clusterID) == 0 || len(clusterID) > MaxIdentityBytes || len(incarnation) == 0 || len(incarnation) > MaxIdentityBytes {
		return errors.New("replication reconstruction identity is invalid")
	}
	if first > last {
		return nil
	}
	head, _, _ := journal.Head()
	if first == 0 || last > head {
		return fmt.Errorf("replication reconstruction range %d-%d is outside ordering head %d", first, last, head)
	}
	for index := first; index <= last; index++ {
		record, err := journal.Record(index)
		if err != nil {
			return err
		}
		if record.Kind == KindMetadataBaseline {
			owner, ok := source.(interface{ ValidateMetadataBaseline(OrderingRecord) error })
			if !ok {
				return errors.New("Metadata baseline reconstruction has no independently qualified source")
			}
			if err := owner.ValidateMetadataBaseline(record); err != nil {
				return err
			}
		}
		var payload []byte
		if record.Kind == KindData {
			payload, err = source.Read(record.Store, record.StoreGeneration, record.StoreSequenceFirst, record.StoreSequenceLast)
			if err != nil {
				return fmt.Errorf("read source bytes for ordering index %d: %w", index, err)
			}
		} else if record.Kind == KindProviderObject {
			objectSource, ok := source.(providerObjectSource)
			if !ok {
				return fmt.Errorf("source cannot reconstruct provider object at ordering index %d", index)
			}
			metadata, decodeErr := DecodeProviderObjectMetadata(record.Metadata)
			if decodeErr != nil {
				return fmt.Errorf("decode provider object at ordering index %d: %w", index, decodeErr)
			}
			payload, err = objectSource.ReadProviderObjectChunk(metadata)
			if err != nil {
				return fmt.Errorf("read provider object at ordering index %d: %w", index, err)
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
			return fmt.Errorf("reconstruct ordering index %d: %w", index, err)
		}
		if digest != record.FrameDigest {
			return fmt.Errorf("source bytes for ordering index %d do not match its authenticated digest", index)
		}
		if err := consume(LocalCommit{Index: index, Encoded: encoded}); err != nil {
			return fmt.Errorf("consume reconstructed ordering index %d: %w", index, err)
		}
	}
	return nil
}

func sourceCursorBehind(actual, expected StoreCursor) bool {
	if actual.Generation != expected.Generation {
		return actual.Generation < expected.Generation
	}
	return actual.Sequence < expected.Sequence
}
