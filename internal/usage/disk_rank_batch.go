package usage

import (
	"encoding/binary"
	"math"
)

// Only one fresh, unpublished scratch transaction may use this buffer. It
// coalesces repeated writes without changing rank validation or stored rows.
// Reserve room for the worst possible next record before applying it. Distinct
// scopes flush within 64 records; repeated scopes can share the same fixed
// buffer through the complete 512-record unpublished transaction.
const rankScratchBatchRecords = 64
const rankScratchMaxRecordKeys = 2 * (1 + 2*diskLatencyBits)
const rankScratchBatchMaxKeys = rankScratchBatchRecords * rankScratchMaxRecordKeys
const rankScratchBatchSlots = 32768
const rankScratchBatchRAMBytes = 4 << 20

type rankScratchEntry struct {
	key   [41]byte
	count uint64
	used  bool
	dirty bool
}

type rankScratchBatch struct {
	entries [rankScratchBatchSlots]rankScratchEntry
	indices [rankScratchBatchMaxKeys]uint16
	count   int
}

func (r *rankScratchBatch) beforeRecord(tx *usageTx) error {
	if r.count > rankScratchBatchMaxKeys-rankScratchMaxRecordKeys {
		return r.flush(tx)
	}
	return nil
}

func (r *rankScratchBatch) entry(b *usageBucket, key []byte) (*rankScratchEntry, error) {
	if b == nil || b.tx == nil || b.tx.scratch == nil || b.name != "request_latency_rank" || len(key) != 41 || b.tx.failure != nil {
		return nil, ErrDiskUsageCorrupt
	}
	var exact [41]byte
	copy(exact[:], key)
	hash := uint64(14695981039346656037)
	for _, v := range exact {
		hash = (hash ^ uint64(v)) * 1099511628211
	}
	for slot := int(hash & (rankScratchBatchSlots - 1)); ; slot = (slot + 1) & (rankScratchBatchSlots - 1) {
		e := &r.entries[slot]
		if e.used {
			if e.key == exact {
				return e, nil
			}
			continue
		}
		if r.count == len(r.indices) {
			return nil, ErrDiskUsageLimit
		}
		raw := b.Get(key)
		if b.tx.failure != nil {
			return nil, b.tx.failure
		}
		if raw != nil && len(raw) != 8 {
			return nil, ErrDiskUsageCorrupt
		}
		var count uint64
		if raw != nil {
			count = binary.BigEndian.Uint64(raw)
			if count == 0 || count > math.MaxInt64 {
				return nil, ErrDiskUsageCorrupt
			}
		}
		*e = rankScratchEntry{key: exact, count: count, used: true}
		r.indices[r.count] = uint16(slot)
		r.count++
		return e, nil
	}
}

func (r *rankScratchBatch) get(b *usageBucket, key []byte) (uint64, error) {
	e, err := r.entry(b, key)
	if err != nil {
		return 0, err
	}
	return e.count, nil
}

func diskRankPut(b *usageBucket, key []byte, count uint64) error {
	if b.tx.rankScratch == nil {
		return b.Put(key, diskUsageSequence(count))
	}
	if count == 0 || count > math.MaxInt64 {
		return ErrDiskUsageCorrupt
	}
	e, err := b.tx.rankScratch.entry(b, key)
	if err != nil {
		return err
	}
	e.count, e.dirty = count, true
	return nil
}

func (r *rankScratchBatch) flush(tx *usageTx) error {
	b := tx.Bucket([]byte("request_latency_rank"))
	for _, slot := range r.indices[:r.count] {
		e := &r.entries[slot]
		if e.dirty {
			if err := b.Put(e.key[:], diskUsageSequence(e.count)); err != nil {
				return err
			}
		}
	}
	for _, slot := range r.indices[:r.count] {
		r.entries[slot] = rankScratchEntry{}
	}
	r.count = 0
	return nil
}
