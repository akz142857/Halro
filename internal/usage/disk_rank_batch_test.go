package usage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"path/filepath"
	"testing"
	"unsafe"

	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/scratchstate"
)

func TestPrivateScratchRankBatchMatchesOrdinaryAcrossFlushes(t *testing.T) {
	f := newTypedUsageFixtureAt(t, false, -1)
	ctx := context.Background()
	o := f.options
	o.ScratchPath = filepath.Join(t.TempDir(), "private-rank.reducer")
	o.ScratchMaxDiskBytes = 32 << 20
	fee, err := o.Budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: rankScratchBatchRAMBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer fee.Release()
	if unsafe.Sizeof(rankScratchBatch{}) > rankScratchBatchRAMBytes {
		t.Fatal("buffer exceeds charged ceiling")
	}
	scratch, err := createFixedUsageScratch(ctx, o, f.owner)
	if err != nil {
		t.Fatal(err)
	}
	defer scratch.Discard()
	ordinary, err := CreateDiskProjection(filepath.Join(t.TempDir(), "ordinary.db"), f.owner)
	if err != nil {
		t.Fatal(err)
	}
	defer ordinary.Discard()
	// Distinct scopes fill the worst-case buffer; repeated scopes exercise
	// virtual parent/child counts across several flushed batches.
	for _, distinct := range []bool{true, false} {
		records := diskReplayBatchRows
		if distinct {
			records = rankScratchBatchRecords + 1
		}
		for batch := 0; batch < 3; batch++ {
			apply := func(tx *usageTx, buffered bool) error {
				if buffered {
					tx.rankScratch = new(rankScratchBatch)
				}
				for row := 0; row < records; row++ {
					if buffered {
						previous := tx.rankScratch.count
						if distinct && row == rankScratchBatchRecords && previous != rankScratchBatchMaxKeys {
							return ErrDiskUsageCorrupt
						}
						if err := tx.rankScratch.beforeRecord(tx); err != nil {
							return err
						}
						if !distinct && previous != tx.rankScratch.count {
							return ErrDiskUsageCorrupt
						}
					}
					for scope := 0; scope < 2; scope++ {
						var input [16]byte
						var expected int64
						if distinct {
							binary.BigEndian.PutUint64(input[:8], uint64(1+batch*records*2+row*2+scope))
						} else {
							binary.BigEndian.PutUint64(input[:8], uint64(1000+scope))
							expected = int64(batch*records + row)
						}
						key := sha256.Sum256(input[:])
						latency := (uint64(batch*records+row+1) * 0x9e3779b97f4a7c15) % diskMaxRequestLatencyMillis
						if !distinct {
							latency = uint64(row % 17)
						}
						if err := diskRankIncrement(tx, key[:], latency, expected); err != nil {
							return err
						}
					}
				}
				if buffered {
					if distinct && tx.rankScratch.count != rankScratchMaxRecordKeys {
						return ErrDiskUsageCorrupt
					}
					return tx.rankScratch.flush(tx)
				}
				return nil
			}
			if err := scratch.engine.Update(func(tx *usageTx) error { return apply(tx, true) }); err != nil {
				t.Fatal(distinct, batch, err)
			}
			if err := ordinary.engine.Update(func(tx *usageTx) error { return apply(tx, false) }); err != nil {
				t.Fatal(err)
			}
			comparePrivateScratchRankRows(t, scratch, ordinary)
		}
	}
}

func TestPrivateScratchRankBatchRejectsCorruptionAndPoisonsCandidate(t *testing.T) {
	f := newTypedUsageFixtureAt(t, false, -1)
	ctx := context.Background()
	o := f.options
	o.ScratchPath = filepath.Join(t.TempDir(), "private-rank.reducer")
	o.ScratchMaxDiskBytes = 32 << 20
	fee, err := o.Budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: rankScratchBatchRAMBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer fee.Release()
	scratch, err := createFixedUsageScratch(ctx, o, f.owner)
	if err != nil {
		t.Fatal(err)
	}
	defer scratch.Discard()
	scope := sha256.Sum256([]byte("corrupt-rank"))
	if err := scratch.engine.Update(func(tx *usageTx) error {
		return tx.Bucket([]byte("request_latency_rank")).Put(diskRankKey(scope[:], 0, 0), diskUsageSequence(1))
	}); err != nil {
		t.Fatal(err)
	}
	err = scratch.engine.Update(func(tx *usageTx) error {
		tx.rankScratch = new(rankScratchBatch)
		return diskRankIncrement(tx, scope[:], 42, 1)
	})
	if !errors.Is(err, ErrDiskUsageCorrupt) {
		t.Fatal("inconsistent children accepted", err)
	}
	if err := scratch.engine.View(func(tx *usageTx) error { return nil }); !errors.Is(err, scratchstate.ErrPoisoned) {
		t.Fatal("failed scratch candidate remained readable", err)
	}
}

func comparePrivateScratchRankRows(t *testing.T, scratch, ordinary *DiskProjection) {
	t.Helper()
	if err := scratch.engine.View(func(tx *usageTx) error {
		return ordinary.engine.View(func(want *usageTx) error {
			a := tx.Bucket([]byte("request_latency_rank")).Cursor()
			b := want.Bucket([]byte("request_latency_rank")).Cursor()
			ak, av := a.First()
			bk, bv := b.First()
			for ak != nil || bk != nil {
				if !bytes.Equal(ak, bk) || !bytes.Equal(av, bv) {
					return ErrDiskUsageCorrupt
				}
				ak, av = a.Next()
				bk, bv = b.Next()
			}
			return nil
		})
	}); err != nil {
		t.Fatal("scratch/ordinary rank namespace mismatch", err)
	}
}
