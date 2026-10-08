package bolt

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"sync"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/metadatajournal"
)

const (
	OutcomeDefinitionExportMaxRows  = 200
	OutcomeDefinitionExportMaxBytes = 16 << 20
	// A metadata transaction cannot contain a definition larger than this.
	outcomeDefinitionExportRowBytes = metadatajournal.MaxPayloadSize
	outcomeDefinitionExportKeyBytes = 128
)

var (
	ErrDefinitionExportCorrupt    = errors.New("outcome definition export prefix is inconsistent")
	ErrDefinitionExportLimit      = errors.New("outcome definition export page limit exceeded")
	ErrDefinitionExportIncomplete = errors.New("outcome definition export is incomplete")
	ErrDefinitionExportClosed     = errors.New("outcome definition export view is closed")
	definitionExportHeadKey       = []byte("head")
	definitionExportPendingKey    = []byte("pending_epoch")
	definitionExportSeed          = sha256.Sum256([]byte("halro/outcome-definition-export/v1"))
)

// The ordinal is a metadata-projection prefix, not a Ledger/Governance cut.
// Hash is an in-process consistency witness. Persisted indexes are not trusted
// startup authentication and do not implement the D-03 snapshot protocol.
type OutcomeDefinitionExportHead struct {
	Epoch    uint64
	Sequence uint64
	Count    uint64
	Hash     [32]byte
}

type OutcomeDefinitionExportPage struct {
	Items []domain.OutcomeDefinition
	Bytes int
	Done  bool
}

type definitionExportEntry struct {
	key     []byte
	rowHash [32]byte
	chain   [32]byte
}

func definitionExportChain(previous [32]byte, ordinal uint64, key []byte, rowHash [32]byte) [32]byte {
	h := sha256.New()
	h.Write(previous[:])
	h.Write(encodeUint64(ordinal))
	h.Write(encodeUint64(uint64(len(key))))
	h.Write(key)
	h.Write(rowHash[:])
	var result [32]byte
	copy(result[:], h.Sum(nil))
	return result
}

func decodeDefinitionExportEntry(raw []byte) (definitionExportEntry, error) {
	if len(raw) < 65 || len(raw) > outcomeDefinitionExportKeyBytes+64 {
		return definitionExportEntry{}, ErrDefinitionExportCorrupt
	}
	entry := definitionExportEntry{key: raw[:len(raw)-64]}
	copy(entry.rowHash[:], raw[len(raw)-64:len(raw)-32])
	copy(entry.chain[:], raw[len(raw)-32:])
	return entry, nil
}

func definitionExportHead(tx *Tx) (OutcomeDefinitionExportHead, error) {
	meta, index := tx.Bucket(bucketOutcomeDefinitionExportMeta), tx.Bucket(bucketOutcomeDefinitionExport)
	if meta == nil || index == nil || tx.Bucket(bucketOutcomeDefinitions) == nil {
		return OutcomeDefinitionExportHead{}, ErrDefinitionExportCorrupt
	}
	raw := meta.Get(definitionExportHeadKey)
	if len(raw) != 40 {
		return OutcomeDefinitionExportHead{}, ErrDefinitionExportCorrupt
	}
	head := OutcomeDefinitionExportHead{Count: binary.BigEndian.Uint64(raw[:8])}
	copy(head.Hash[:], raw[8:])
	if head.Count == 0 {
		if head.Hash != definitionExportSeed {
			return OutcomeDefinitionExportHead{}, ErrDefinitionExportCorrupt
		}
	} else {
		entry, err := decodeDefinitionExportEntry(index.Get(encodeUint64(head.Count)))
		if err != nil || entry.chain != head.Hash {
			return OutcomeDefinitionExportHead{}, ErrDefinitionExportCorrupt
		}
	}
	return head, nil
}

func appendOutcomeDefinitionExport(tx *Tx, key, raw []byte) error {
	if len(key) == 0 || len(key) > outcomeDefinitionExportKeyBytes || len(raw) == 0 || len(raw) > outcomeDefinitionExportRowBytes {
		return ErrDefinitionExportLimit
	}
	head, err := definitionExportHead(tx)
	if err != nil {
		return err
	}
	if head.Count == math.MaxUint64 {
		return ErrDefinitionExportLimit
	}
	ordinal := head.Count + 1
	index := tx.Bucket(bucketOutcomeDefinitionExport)
	if index.Get(encodeUint64(ordinal)) != nil {
		return ErrDefinitionExportCorrupt
	}
	rowHash := sha256.Sum256(raw)
	chain := definitionExportChain(head.Hash, ordinal, key, rowHash)
	encoded := make([]byte, 0, len(key)+64)
	encoded = append(encoded, key...)
	encoded = append(encoded, rowHash[:]...)
	encoded = append(encoded, chain[:]...)
	if err := index.Put(encodeUint64(ordinal), encoded); err != nil {
		return err
	}
	return tx.Bucket(bucketOutcomeDefinitionExportMeta).Put(definitionExportHeadKey, append(encodeUint64(ordinal), chain[:]...))
}

// Open-time schema migration is exclusive. It streams legacy rows in ID/version
// order without a history-sized Go map/slice. bbolt dirty pages, migration IO,
// and the maintenance window still require O-01/Linux capacity acceptance.
func migrateOutcomeDefinitionExport(tx *Tx, step func(string) error) error {
	if err := migrationStep(step, "before_outcome_definition_export_prefix"); err != nil {
		return err
	}
	for _, name := range [][]byte{bucketOutcomeDefinitionExport, bucketOutcomeDefinitionExportMeta} {
		// Initial schema creation already creates required buckets. Rebuild
		// these indexes exclusively from definition rows, never inherit an
		// index from an older projection layout or a staged schema fixture.
		if tx.Bucket(name) != nil {
			if err := tx.DeleteBucket(name); err != nil {
				return err
			}
		}
		if _, err := tx.CreateBucket(name); err != nil {
			return err
		}
	}
	if err := tx.Bucket(bucketOutcomeDefinitionExportMeta).Put(definitionExportHeadKey, append(encodeUint64(0), definitionExportSeed[:]...)); err != nil {
		return err
	}
	// The HMAC key is only available at AttachMetadataJournal. Recover its
	// durable tail before constructing the baseline or replacing its epoch.
	if err := tx.Bucket(bucketOutcomeDefinitionExportMeta).Put(definitionExportPendingKey, []byte{1}); err != nil {
		return err
	}
	return migrationStep(step, "after_outcome_definition_export_prefix")
}

func validateDefinitionExportRow(key, raw []byte) (domain.OutcomeDefinition, error) {
	if len(key) == 0 || len(key) > outcomeDefinitionExportKeyBytes || len(raw) == 0 || len(raw) > outcomeDefinitionExportRowBytes {
		return domain.OutcomeDefinition{}, ErrDefinitionExportCorrupt
	}
	var row domain.OutcomeDefinition
	if err := json.Unmarshal(raw, &row); err != nil {
		return domain.OutcomeDefinition{}, ErrDefinitionExportCorrupt
	}
	if row.Validate() != nil || row.Revision != row.Version || !bytes.Equal(key, outcomeDefinitionKey(row.ID, row.Version)) {
		return domain.OutcomeDefinition{}, ErrDefinitionExportCorrupt
	}
	return row, nil
}

// No transaction survives a read. The revocable entry retains its Store owner
// until seal clears it; each actual read joins Close. The callback must enumerate
// the whole prefix and Complete before external publication. It owns/join its
// resources through callback exit; seal waits for entered ReadPage/Complete.
type OutcomeDefinitionExportView struct {
	mu        sync.Mutex
	read      func(func(*Tx) error) error
	ctx       context.Context
	head      OutcomeDefinitionExportHead
	seen      uint64
	chain     [32]byte
	active    bool
	completed bool
	failure   error
}

func (s *Store) WithOutcomeDefinitionExport(ctx context.Context, visit func(*OutcomeDefinitionExportView) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if visit == nil {
		return ErrDefinitionExportIncomplete
	}
	read, err := s.captureView()
	if err != nil {
		return err
	}
	v := &OutcomeDefinitionExportView{read: read, ctx: ctx, active: true, chain: definitionExportSeed}
	err = read(func(tx *Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if b := tx.Bucket(bucketOutcomeDefinitionExportMeta); b == nil || b.Get(definitionExportPendingKey) != nil {
			return ErrDefinitionExportIncomplete
		}
		var err error
		v.head, err = definitionExportHead(tx)
		if err != nil {
			return err
		}
		meta := tx.Bucket(bucketMeta)
		if meta == nil {
			return ErrDefinitionExportCorrupt
		}
		var ok bool
		v.head.Epoch, ok = decodeUint64(meta.Get(keyMetadataJournalEpoch))
		if !ok || v.head.Epoch == 0 {
			return ErrJournalUnavailable
		}
		v.head.Sequence, ok = decodeUint64(meta.Get(keyAppliedJournalSequence))
		if !ok {
			return ErrJournalUnavailable
		}
		return ctx.Err()
	})
	if err != nil {
		v.seal()
		return err
	}
	defer v.seal()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := visit(v); err != nil {
		return err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.failure != nil {
		return v.failure
	}
	if !v.completed {
		return ErrDefinitionExportIncomplete
	}
	return nil
}

func (v *OutcomeDefinitionExportView) Head() OutcomeDefinitionExportHead { return v.head }

func (v *OutcomeDefinitionExportView) seal() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.clear()
}

func (v *OutcomeDefinitionExportView) clear() {
	v.active = false
	v.read = nil
	v.ctx = nil
}

func (v *OutcomeDefinitionExportView) ownerRead(ctx context.Context, fn func(*Tx) error) error {
	if !v.active {
		return ErrDefinitionExportClosed
	}
	if v.failure != nil {
		return v.failure
	}
	err := v.ctx.Err()
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		err = v.read(func(tx *Tx) error {
			if err := v.ctx.Err(); err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if b := tx.Bucket(bucketOutcomeDefinitionExportMeta); b == nil || b.Get(definitionExportPendingKey) != nil {
				return ErrDefinitionExportIncomplete
			}
			meta := tx.Bucket(bucketMeta)
			if meta == nil {
				return ErrDefinitionExportCorrupt
			}
			epoch, ok := decodeUint64(meta.Get(keyMetadataJournalEpoch))
			seq, hasSeq := decodeUint64(meta.Get(keyAppliedJournalSequence))
			if !ok || !hasSeq || epoch != v.head.Epoch || seq < v.head.Sequence {
				return ErrDefinitionExportCorrupt
			}
			head, err := definitionExportHead(tx)
			if err != nil {
				return err
			}
			if head.Count < v.head.Count || head.Count == v.head.Count && head.Hash != v.head.Hash {
				return ErrDefinitionExportCorrupt
			}
			if v.head.Count > 0 {
				entry, err := decodeDefinitionExportEntry(tx.Bucket(bucketOutcomeDefinitionExport).Get(encodeUint64(v.head.Count)))
				if err != nil || entry.chain != v.head.Hash {
					return ErrDefinitionExportCorrupt
				}
			}
			if err := fn(tx); err != nil {
				return err
			}
			if err := v.ctx.Err(); err != nil {
				return err
			}
			return ctx.Err()
		})
	}
	if err == nil {
		err = v.ctx.Err()
	}
	if err == nil {
		err = ctx.Err()
	}
	if err != nil && !errors.Is(err, ErrDefinitionExportLimit) {
		v.failure = err
	}
	return err
}

func (v *OutcomeDefinitionExportView) ReadPage(ctx context.Context, rows, sourceBytes int) (OutcomeDefinitionExportPage, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.active {
		return OutcomeDefinitionExportPage{}, ErrDefinitionExportClosed
	}
	if rows < 1 || rows > OutcomeDefinitionExportMaxRows || sourceBytes < 1 || sourceBytes > OutcomeDefinitionExportMaxBytes {
		return OutcomeDefinitionExportPage{}, ErrDefinitionExportLimit
	}
	page := OutcomeDefinitionExportPage{Items: make([]domain.OutcomeDefinition, 0, rows)}
	seen, chain := v.seen, v.chain
	err := v.ownerRead(ctx, func(tx *Tx) error {
		index, definitions := tx.Bucket(bucketOutcomeDefinitionExport), tx.Bucket(bucketOutcomeDefinitions)
		for seen < v.head.Count && len(page.Items) < rows {
			if err := v.ctx.Err(); err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			ordinal := seen + 1
			entryRaw := index.Get(encodeUint64(ordinal))
			entry, err := decodeDefinitionExportEntry(entryRaw)
			if err != nil {
				return err
			}
			raw := definitions.Get(entry.key)
			if len(raw) == 0 || len(raw) > outcomeDefinitionExportRowBytes {
				return ErrDefinitionExportCorrupt
			}
			size := 8 + len(entryRaw) + len(raw)
			if size > sourceBytes-page.Bytes {
				if len(page.Items) == 0 {
					return ErrDefinitionExportLimit
				}
				break
			}
			rowHash := sha256.Sum256(raw)
			next := definitionExportChain(chain, ordinal, entry.key, rowHash)
			if rowHash != entry.rowHash || next != entry.chain {
				return ErrDefinitionExportCorrupt
			}
			row, err := validateDefinitionExportRow(entry.key, raw)
			if err != nil {
				return err
			}
			page.Items = append(page.Items, row)
			page.Bytes += size
			seen, chain = ordinal, next
		}
		if seen == v.head.Count && chain != v.head.Hash {
			return ErrDefinitionExportCorrupt
		}
		return nil
	})
	if err != nil {
		return OutcomeDefinitionExportPage{}, err
	}
	v.seen, v.chain = seen, chain
	page.Done = seen == v.head.Count
	return page, nil
}

func (v *OutcomeDefinitionExportView) Complete(ctx context.Context) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	err := v.ownerRead(ctx, func(*Tx) error {
		if v.seen != v.head.Count || v.chain != v.head.Hash {
			return ErrDefinitionExportIncomplete
		}
		return nil
	})
	if err != nil {
		return err
	}
	v.completed = true
	v.clear()
	return nil
}
