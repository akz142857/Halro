package bolt

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
)

var usageRollupSlots = [2][]byte{[]byte("usage_daily_rollup_a"), []byte("usage_daily_rollup_b")}

func selectedUsageRollupBucket(tx *Tx) (*Bucket, int, error) {
	meta := tx.Bucket(bucketMeta)
	if meta == nil {
		return nil, 0, errors.New("Usage Metadata bucket is missing")
	}
	var state UsageRollupState
	if raw := meta.Get(keyUsageRollupState); raw != nil {
		if len(raw) > 64<<10 || json.Unmarshal(raw, &state) != nil || state.Slot < 0 || state.Slot > len(usageRollupSlots) {
			return nil, 0, errors.New("Usage rollup root is invalid")
		}
	}
	name := bucketUsageDailyRollup
	if state.Slot != 0 {
		name = usageRollupSlots[state.Slot-1]
	}
	bucket := tx.Bucket(name)
	if bucket == nil {
		return nil, 0, errors.New("selected Usage rollup bucket is missing")
	}
	return bucket, state.Slot, nil
}

type UsageRollupReplacementRow struct {
	Key domain.RollupKey
	Row domain.DailyRollup
}

type UsageRollupReplacement struct {
	Rows, AccountingDays int
}

// ReplaceUsageRollups requires an exclusive, offline Standalone owner and an
// independently authenticated, immutable source. It does not authenticate that
// source or authorize a role. Each source/clear/write batch is finite; a final
// atomic root publishes the complete generation and a deliberately unrestorable
// checkpoint marker. A subsequent Aggregate startup MUST replay native facts.
// Failures keep the previous selected root and leave only one invisible fixed
// staging bucket, which the next invocation clears in finite transactions.
// bbolt mmap/freelist and physical disk fees remain deployment acceptance work.
func (s *Store) ReplaceUsageRollups(ctx context.Context, head ledger.Watermark, next func(context.Context) ([]UsageRollupReplacementRow, bool, error)) (report UsageRollupReplacement, err error) {
	if err := ctx.Err(); err != nil {
		return report, err
	}
	if next == nil || head != (ledger.Watermark{}) && (head.Generation == 0 || head.Sequence == 0 || head.Offset <= 0) {
		return report, errors.New("Usage rebuild source is invalid")
	}
	leave, enterErr := s.enter()
	if enterErr != nil {
		return report, enterErr
	}
	defer leave()
	s.journalMu.RLock()
	offline := !s.replica && s.journal == nil
	s.journalMu.RUnlock()
	if !offline {
		return report, errors.New("Usage rebuild requires offline Standalone Metadata")
	}
	if !s.usageRollupRebuild.CompareAndSwap(false, true) {
		return report, errors.New("Usage rebuild is already active")
	}
	defer s.usageRollupRebuild.Store(false)
	// Hash old envelopes in their mapped read transaction, without cloning a
	// historical checkpoint. Root changes invalidate every staging transaction.
	var witness [2][32]byte
	slot := 1
	err = s.view(func(tx *Tx) error {
		meta := tx.Bucket(bucketMeta)
		if meta == nil {
			return errors.New("Usage Metadata bucket is missing")
		}
		// A corrupt/missing old derivative is precisely why an operator may
		// rebuild. Only use a syntactically recognizable slot to avoid writing
		// over an existing visible slot; never import its contents or totals.
		var saved UsageRollupState
		raw := meta.Get(keyUsageRollupState)
		if len(raw) <= 64<<10 && json.Unmarshal(raw, &saved) == nil && saved.Slot == 1 {
			slot = 2
		}
		witness[0] = sha256.Sum256(meta.Get(keyUsageCheckpoint))
		witness[1] = sha256.Sum256(meta.Get(keyUsageRollupState))
		return nil
	})
	if err != nil {
		return report, err
	}
	check := func(tx *Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		meta := tx.Bucket(bucketMeta)
		if meta == nil || sha256.Sum256(meta.Get(keyUsageCheckpoint)) != witness[0] || sha256.Sum256(meta.Get(keyUsageRollupState)) != witness[1] {
			return errors.New("Usage rebuild selected root changed")
		}
		return nil
	}
	name := usageRollupSlots[slot-1]
	// Never DeleteBucket a historical generation in a single transaction.
	// Clear no more than 100 keys/1 MiB of key bytes per physical commit.
	cleared := false
	for step := 0; step < 65536; step++ {
		err = s.updateContext(ctx, func(tx *Tx) error {
			if err := check(tx); err != nil {
				return err
			}
			bucket, err := tx.CreateBucketIfNotExists(name)
			if err != nil {
				return err
			}
			c := bucket.Cursor()
			keys := make([][]byte, 0, 100)
			keyBytes := 0
			for k, v := c.First(); k != nil && len(keys) < 100; k, v = c.Next() {
				if v == nil {
					return errors.New("Usage staging bucket contains a nested bucket")
				}
				if len(k) > (1<<20)-keyBytes {
					break
				}
				keys = append(keys, append([]byte(nil), k...))
				keyBytes += len(k)
			}
			for _, k := range keys {
				if err := bucket.Delete(k); err != nil {
					return err
				}
			}
			k, _ := bucket.Cursor().First()
			cleared = k == nil
			return ctx.Err()
		})
		if err != nil {
			return UsageRollupReplacement{}, err
		}
		if cleared {
			break
		}
	}
	if !cleared {
		return UsageRollupReplacement{}, errors.New("Usage staging cleanup limit reached; retry")
	}
	done, writtenBytes := false, 0
	for page := 0; page < 65536; page++ {
		if err := ctx.Err(); err != nil {
			return UsageRollupReplacement{}, err
		}
		rows, end, err := next(ctx)
		if err != nil {
			return UsageRollupReplacement{}, err
		}
		if len(rows) > 100 || len(rows) == 0 && !end || len(rows) > 1000000-report.Rows {
			return UsageRollupReplacement{}, errors.New("Usage rebuild row/page limit reached")
		}
		if head == (ledger.Watermark{}) && len(rows) != 0 {
			return UsageRollupReplacement{}, errors.New("empty Usage source contains rollups")
		}
		payloads := make([][]byte, len(rows))
		keys := make([][]byte, len(rows))
		pageBytes, days := 0, 0
		for i, row := range rows {
			// Admit raw string width before key/JSON allocation. Production rows
			// already come from the bounded native disk snapshot reader.
			if len(row.Key.PeriodID)+len(row.Key.Dimension)+len(row.Key.DimensionKey)+len(row.Row.PeriodID)+len(row.Row.Timezone) > 1<<20 {
				return UsageRollupReplacement{}, errors.New("Usage rebuild row width exceeded")
			}
			if row.Key.Validate() != nil || row.Row.Version != domain.RollupVersion || row.Key.PeriodID != row.Row.PeriodID || row.Key.TimezoneVersion != row.Row.TimezoneVersion || row.Row.FirstSequence == 0 || row.Row.FirstSequence > head.Sequence {
				return UsageRollupReplacement{}, errors.New("Usage rebuild rollup identity is invalid")
			}
			keys[i] = []byte(row.Key.Encode())
			payloads[i], err = json.Marshal(row.Row)
			if err != nil {
				return UsageRollupReplacement{}, err
			}
			if len(keys[i])+len(payloads[i]) > (8<<20)-pageBytes {
				return UsageRollupReplacement{}, errors.New("Usage rebuild page bytes exceeded")
			}
			pageBytes += len(keys[i]) + len(payloads[i])
			if row.Key.Dimension == domain.RollupDimensionTotal {
				days++
			}
		}
		if pageBytes > (256<<20)-writtenBytes {
			return UsageRollupReplacement{}, errors.New("Usage rebuild write bytes exceeded")
		}
		err = s.updateContext(ctx, func(tx *Tx) error {
			if err := check(tx); err != nil {
				return err
			}
			bucket := tx.Bucket(name)
			if bucket == nil {
				return errors.New("Usage staging bucket is missing")
			}
			for i, k := range keys {
				if err := ctx.Err(); err != nil {
					return err
				}
				if bucket.Get(k) != nil {
					return errors.New("Usage rebuild contains duplicate rollup keys")
				}
				if err := bucket.Put(k, payloads[i]); err != nil {
					return err
				}
			}
			return ctx.Err()
		})
		if err != nil {
			return UsageRollupReplacement{}, err
		}
		report.Rows += len(rows)
		report.AccountingDays += days
		writtenBytes += pageBytes
		if end {
			done = true
			break
		}
	}
	if !done {
		return UsageRollupReplacement{}, errors.New("Usage rebuild complete-source page limit reached")
	}
	state, err := json.Marshal(UsageRollupState{Version: domain.RollupVersion, Watermark: head, Slot: slot})
	if err != nil {
		return UsageRollupReplacement{}, err
	}
	marker, err := json.Marshal(struct {
		Version   int              `json:"version"`
		Watermark ledger.Watermark `json:"watermark"`
		Reducer   string           `json:"reducer_semantics"`
	}{0, head, "disk-rollup-rebuild-v1"})
	if err != nil {
		return UsageRollupReplacement{}, err
	}
	envelope, err := json.Marshal(usageCheckpoint{Watermark: head, Payload: marker})
	if err != nil {
		return UsageRollupReplacement{}, err
	}
	err = s.updateContext(ctx, func(tx *Tx) error {
		if err := check(tx); err != nil {
			return err
		}
		meta := tx.Bucket(bucketMeta)
		if err := meta.Put(keyUsageRollupState, state); err != nil {
			return err
		}
		if head == (ledger.Watermark{}) {
			return meta.Delete(keyUsageCheckpoint)
		}
		return meta.Put(keyUsageCheckpoint, envelope)
	})
	if err != nil {
		return UsageRollupReplacement{}, fmt.Errorf("publish Usage rebuild: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return UsageRollupReplacement{}, err
	}
	return report, nil
}
