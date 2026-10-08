package bolt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

// Restoration owns an offline unpublished candidate. This job authenticates
// native index relationships at a fixed TxID before any high-water mutation.
// Later source changes revoke it; only its own completed writes advance the ID.
// A partial job is retryable, never a completed restore or a served candidate.
func (s *Store) quarantineRestoredScheduledPricesBounded(ctx context.Context, backupAt, restoredAt time.Time) (int, error) {
	return s.quarantineRestoredScheduledPricesWithPhase(ctx, backupAt, restoredAt, nil)
}

// The operation-local phase callback is private and runs outside actual DB
// transactions, so fault tests exercise completed native writes and retries.
func (s *Store) quarantineRestoredScheduledPricesWithPhase(ctx context.Context, backupAt, restoredAt time.Time, phase func(string) error) (count int, resultErr error) {
	if ctx == nil {
		return 0, ErrPricingRecoveryLimit
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	backupAt, restoredAt = backupAt.UTC(), restoredAt.UTC()
	if backupAt.IsZero() || !restoredAt.After(backupAt) {
		return 0, nil
	}
	expectedID, pricesCount := 0, uint64(0)
	var upper []byte
	if err := s.view(func(tx *Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		expectedID = tx.ID()
		root := tx.Bucket(bucketDeploymentPriceTimeline)
		if root == nil {
			return ErrPricingRecoveryChanged
		}
		key, _ := root.Cursor().Last()
		if len(key) > 4096 {
			return ErrPricingRecoveryLimit
		}
		upper = bytes.Clone(key)
		return nil
	}); err != nil {
		return 0, err
	}
	check := func(tx *Tx) error {
		if tx.ID() != expectedID {
			return ErrPricingRecoveryChanged
		}
		return ctx.Err()
	}
	// This also rejects unindexed price facts; walking an index alone would hide
	// a malformed legacy row or an omitted scheduled price.
	var after []byte
	for {
		page, err := s.readPricingRecoveryPageChecked(ctx, bucketDeploymentPriceVersions, after, nil, check)
		if err != nil {
			return count, err
		}
		for _, row := range page.rows {
			var price domain.DeploymentPriceVersion
			if len(row.key) > 4096 {
				return count, ErrPricingRecoveryLimit
			}
			if err := decodeMetadataMaintenanceJSON(ctx, row.raw, &price); err != nil {
				return count, err
			}
			if price.ID != string(row.key) || len(price.DeploymentID) == 0 || len(price.DeploymentID) > 4096 {
				return count, domain.ErrPriceTimelineConflict
			}
			if err := price.Validate(); err != nil {
				return count, err
			}
			if err := s.view(func(tx *Tx) error {
				if err := check(tx); err != nil {
					return err
				}
				timeline := tx.Bucket(bucketDeploymentPriceTimeline).Bucket([]byte(price.DeploymentID))
				if timeline == nil || !bytes.Equal(timeline.Get(deploymentPriceTimelineKey(price.EffectiveFrom)), row.key) {
					return domain.ErrPriceTimelineConflict
				}
				return nil
			}); err != nil {
				return count, err
			}
			if pricesCount == math.MaxUint64 {
				return count, ErrPricingRecoveryLimit
			}
			pricesCount++
		}
		if page.done {
			break
		}
		after = page.after
	}
	// Validate the complete inverse index before the first mutation. Otherwise
	// an extra timeline reference discovered at the end would mutate a failed
	// candidate before its native index had ever qualified as complete.
	var qualifiedCount uint64
	after = nil
	if upper != nil {
		for {
			ids, next, done, err := s.readRestorePriceDeploymentPage(ctx, after, upper, expectedID)
			if err != nil {
				return 0, err
			}
			for _, id := range ids {
				_, rows, err := s.readRestorePriceDue(ctx, id, expectedID, backupAt, restoredAt)
				if err != nil {
					return 0, err
				}
				if rows > math.MaxUint64-qualifiedCount {
					return 0, ErrPricingRecoveryLimit
				}
				qualifiedCount += rows
			}
			if done {
				break
			}
			after = next
		}
	}
	if qualifiedCount != pricesCount {
		return 0, domain.ErrPriceTimelineConflict
	}
	if phase != nil {
		if err := phase("qualified"); err != nil {
			return 0, err
		}
	}
	var timelineCount uint64
	after = nil
	if upper != nil {
		for {
			ids, next, done, err := s.readRestorePriceDeploymentPage(ctx, after, upper, expectedID)
			if err != nil {
				return count, err
			}
			for _, id := range ids {
				price, rows, err := s.readRestorePriceDue(ctx, id, expectedID, backupAt, restoredAt)
				if err != nil {
					return count, err
				}
				if rows > math.MaxUint64-timelineCount {
					return count, ErrPricingRecoveryLimit
				}
				timelineCount += rows
				if price.ID == "" {
					continue
				}
				if phase != nil {
					if err := phase("before_high_water"); err != nil {
						return count, err
					}
				}
				changed := false
				err = s.update(func(tx *Tx) error {
					if err := ctx.Err(); err != nil {
						return err
					}
					if tx.ID() != expectedID+1 {
						return ErrPricingRecoveryChanged
					}
					highs := tx.Bucket(bucketDeploymentPricingHighWater)
					if highs == nil {
						return ErrPricingRecoveryChanged
					}
					raw := highs.Get(id)
					var high domain.DeploymentPricingHighWater
					if raw != nil {
						if len(raw) > pricingRecoveryWideRowBytes-len(id) {
							return ErrPricingRecoveryLimit
						}
						if err := decodeMetadataMaintenanceJSON(ctx, raw, &high); err != nil {
							return err
						}
						if high.DeploymentID != string(id) {
							return domain.ErrPriceTimelineConflict
						}
						if high.Quarantined {
							return nil
						}
						if high.Revision == math.MaxUint64 {
							return ErrPricingRecoveryLimit
						}
						high.Quarantined = true
						high.QuarantineReason = "restored_scheduled_price_requires_confirmation"
						high.Revision++
					} else {
						high = domain.DeploymentPricingHighWater{DeploymentID: string(id), LatestObservedPriceVersionID: price.ID, LatestSelectedAt: restoredAt, LatestObservedEffectiveFrom: price.EffectiveFrom, Quarantined: true, QuarantineReason: "restored_scheduled_price_requires_confirmation", Revision: 1}
					}
					if err := high.Validate(); err != nil {
						return err
					}
					encoded, err := json.Marshal(high)
					if err != nil {
						return err
					}
					if len(encoded) > pricingRecoveryWideRowBytes-len(id) {
						return ErrPricingRecoveryLimit
					}
					if err := highs.Put(id, encoded); err != nil {
						return err
					}
					changed = true
					return ctx.Err()
				})
				if err != nil {
					return count, err
				}
				expectedID++
				if changed {
					if count == math.MaxInt {
						return count, ErrPricingRecoveryLimit
					}
					count++
				}
				if phase != nil {
					if err := phase("high_water_committed"); err != nil {
						return count, err
					}
				}
			}
			if done {
				break
			}
			after = next
		}
	}
	if timelineCount != pricesCount {
		return count, domain.ErrPriceTimelineConflict
	}
	return count, s.view(check)
}

func (s *Store) readRestorePriceDeploymentPage(ctx context.Context, after, upper []byte, id int) (ids [][]byte, next []byte, done bool, resultErr error) {
	resultErr = s.view(func(tx *Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if tx.ID() != id {
			return ErrPricingRecoveryChanged
		}
		root := tx.Bucket(bucketDeploymentPriceTimeline)
		if root == nil {
			return ErrPricingRecoveryChanged
		}
		cursor := root.Cursor()
		key, value := cursor.First()
		if after != nil {
			key, value = cursor.Seek(after)
			if bytes.Equal(key, after) {
				key, value = cursor.Next()
			}
		}
		total := 0
		for key != nil && bytes.Compare(key, upper) <= 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
			if len(key) == 0 || len(key) > 4096 {
				return ErrPricingRecoveryLimit
			}
			if value != nil || root.Bucket(key) == nil {
				return domain.ErrPriceTimelineConflict
			}
			if len(ids) == pricingRecoveryRows || total+len(key) > pricingRecoveryBytes {
				break
			}
			ids = append(ids, bytes.Clone(key))
			total += len(key)
			key, value = cursor.Next()
		}
		done = key == nil || bytes.Compare(key, upper) > 0
		if len(ids) > 0 {
			next = bytes.Clone(ids[len(ids)-1])
		}
		return nil
	})
	return
}

func (s *Store) readRestorePriceDue(ctx context.Context, id []byte, expectedID int, backupAt, restoredAt time.Time) (due domain.DeploymentPriceVersion, count uint64, resultErr error) {
	var after []byte
	for {
		var rows []pricingRecoveryRow
		done := false
		resultErr = s.view(func(tx *Tx) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if tx.ID() != expectedID {
				return ErrPricingRecoveryChanged
			}
			root := tx.Bucket(bucketDeploymentPriceTimeline)
			prices := tx.Bucket(bucketDeploymentPriceVersions)
			if root == nil || prices == nil {
				return ErrPricingRecoveryChanged
			}
			timeline := root.Bucket(id)
			if timeline == nil {
				return domain.ErrPriceTimelineConflict
			}
			cursor := timeline.Cursor()
			key, priceID := cursor.First()
			if after != nil {
				key, priceID = cursor.Seek(after)
				if bytes.Equal(key, after) {
					key, priceID = cursor.Next()
				}
			}
			total := 0
			for key != nil {
				if err := ctx.Err(); err != nil {
					return err
				}
				if len(key) != 12 || len(priceID) == 0 || len(priceID) > 4096 {
					return domain.ErrPriceTimelineConflict
				}
				raw := prices.Get(priceID)
				if raw == nil {
					return errors.New("restored price timeline references a missing record")
				}
				size := len(key) + len(priceID) + len(raw)
				if size > pricingRecoveryWideRowBytes {
					return ErrPricingRecoveryLimit
				}
				if len(rows) == pricingRecoveryRows || len(rows) > 0 && total+size > pricingRecoveryBytes {
					break
				}
				combined := make([]byte, 12+len(priceID))
				copy(combined, key)
				copy(combined[12:], priceID)
				rows = append(rows, pricingRecoveryRow{key: combined, raw: bytes.Clone(raw)})
				total += size
				key, priceID = cursor.Next()
				if total >= pricingRecoveryBytes {
					break
				}
			}
			done = key == nil
			return nil
		})
		if resultErr != nil {
			return domain.DeploymentPriceVersion{}, 0, resultErr
		}
		for _, row := range rows {
			var price domain.DeploymentPriceVersion
			if err := decodeMetadataMaintenanceJSON(ctx, row.raw, &price); err != nil {
				return domain.DeploymentPriceVersion{}, 0, err
			}
			if price.ID != string(row.key[12:]) || price.DeploymentID != string(id) || !bytes.Equal(row.key[:12], deploymentPriceTimelineKey(price.EffectiveFrom)) {
				return domain.DeploymentPriceVersion{}, 0, domain.ErrPriceTimelineConflict
			}
			if err := price.Validate(); err != nil {
				return domain.DeploymentPriceVersion{}, 0, err
			}
			if count == math.MaxUint64 {
				return domain.DeploymentPriceVersion{}, 0, ErrPricingRecoveryLimit
			}
			count++
			if price.EffectiveFrom.After(backupAt) && !price.EffectiveFrom.After(restoredAt) && (price.CancelledAt == nil || price.CancelledAt.After(backupAt)) {
				due = price
			}
		}
		if done {
			break
		}
		if len(rows) == 0 {
			return domain.DeploymentPriceVersion{}, 0, ErrPricingRecoveryChanged
		}
		after = bytes.Clone(rows[len(rows)-1].key[:12])
	}
	return due, count, nil
}
