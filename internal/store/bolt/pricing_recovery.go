package bolt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/metadatajournal"
)

const pricingRecoveryRows = 64
const pricingRecoveryBytes = 1 << 20

// Ordinary pages stay small. A legacy row wider than that page is processed
// alone, up to the authoritative journal's existing transaction payload cap.
// The journal still validates the actual encoded mutation size before fsync.
const pricingRecoveryWideRowBytes = metadatajournal.MaxPayloadSize

var ErrPricingRecoveryChanged = errors.New("pricing recovery input changed; retry under exclusive maintenance")
var ErrPricingRecoveryLimit = errors.New("pricing recovery row exceeds bounded page")

type pricingRecoveryRow struct {
	key, raw, replacement []byte
	remove                bool
}
type pricingRecoveryPage struct {
	rows  []pricingRecoveryRow
	after []byte
	done  bool
	txID  int
}

// Pages copy a finite key/value set and release the metadata reader before any
// Ledger call. Tx IDs detect other metadata transactions, including insertion
// behind the cursor. Owners still must withdraw business admission; this is
// not a cross-database locking/trust/publication protocol.
func (s *Store) readPricingRecoveryPage(ctx context.Context, name, after []byte, expectedID int) (pricingRecoveryPage, error) {
	return s.readPricingRecoveryPageChecked(ctx, name, after, nil, func(tx *Tx) error {
		if expectedID != 0 && expectedID != tx.ID() {
			return ErrPricingRecoveryChanged
		}
		return nil
	})
}

func (s *Store) readPricingRecoveryPageChecked(ctx context.Context, name, after, upper []byte, check func(*Tx) error) (pricingRecoveryPage, error) {
	var page pricingRecoveryPage
	err := s.viewPricePinRecovery(ctx, func(tx *Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		page.txID = tx.ID()
		if check != nil {
			if err := check(tx); err != nil {
				return err
			}
		}
		bucket := tx.Bucket(name)
		if bucket == nil {
			return errors.New("pricing recovery bucket is absent")
		}
		cursor := bucket.Cursor()
		key, raw := cursor.First()
		if len(after) > 0 {
			key, raw = cursor.Seek(after)
			if bytes.Equal(key, after) {
				key, raw = cursor.Next()
			}
		}
		total := 0
		for key != nil && (upper == nil || bytes.Compare(key, upper) <= 0) {
			if err := ctx.Err(); err != nil {
				return err
			}
			if raw == nil {
				return errors.New("pricing recovery encountered nested bucket")
			}
			if len(key) > pricingRecoveryWideRowBytes || len(raw) > pricingRecoveryWideRowBytes-len(key) {
				return ErrPricingRecoveryLimit
			}
			size := len(key) + len(raw)
			if len(page.rows) == pricingRecoveryRows || (len(page.rows) > 0 && total+size > pricingRecoveryBytes) {
				break
			}
			page.rows = append(page.rows, pricingRecoveryRow{key: bytes.Clone(key), raw: bytes.Clone(raw)})
			total += size
			key, raw = cursor.Next()
			if total >= pricingRecoveryBytes {
				break
			}
		}
		page.done = key == nil || upper != nil && bytes.Compare(key, upper) > 0
		if len(page.rows) > 0 {
			page.after = bytes.Clone(page.rows[len(page.rows)-1].key)
		}
		return nil
	})
	return page, err
}

func verifyPricingRecoveryPosition(ctx context.Context, reader ledger.AccountingLeaseReader, head ledger.Watermark, hash [32]byte) error {
	actual, actualHash, err := reader.ReadPosition(ctx)
	if err != nil {
		return fmt.Errorf("read accounting recovery position: %w", err)
	}
	if actual != head || actualHash != hash {
		return ErrPricingRecoveryChanged
	}
	return nil
}

// RecoverDeploymentPricePins commits only complete validated pages. A later
// error leaves earlier pages durable and retryable; it never rolls back facts.
// The caller must use a trusted frozen Ledger view with business admission
// withdrawn. Both legacy State and a fresh authenticated projection can serve
// reads, without retaining all pin/high-water updates in memory.
func (s *Store) RecoverDeploymentPricePins(ctx context.Context, reader ledger.AccountingLeaseReader) error {
	if reader == nil {
		return errors.New("accounting reader is required for price pin recovery")
	}
	var borrowErr error
	ctx, borrowErr = s.pricePinRecoveryContext(ctx, 0)
	if borrowErr != nil {
		return borrowErr
	}
	head, hash, err := reader.ReadPosition(ctx)
	if err != nil {
		return err
	}
	expectedID := 0
	for _, name := range [][]byte{bucketDeploymentPricingHighWater, bucketDeploymentPricePins} {
		var after []byte
		for {
			page, err := s.readPricingRecoveryPage(ctx, name, after, expectedID)
			if err != nil {
				return err
			}
			expectedID = page.txID
			if bytes.Equal(name, bucketDeploymentPricingHighWater) && len(page.rows) > 0 {
				// Determine mutations under the same read revision. Authenticated
				// Metadata intentionally does not publish a successor for a no-op;
				// do not infer a committed revision from an empty write callback.
				err = s.viewPricePinRecovery(ctx, func(tx *Tx) error {
					if tx.ID() != page.txID {
						return ErrPricingRecoveryChanged
					}
					for i := range page.rows {
						if err := ctx.Err(); err != nil {
							return err
						}
						if err := prepareQuarantinedHighWater(tx, &page.rows[i]); err != nil {
							return err
						}
					}
					return nil
				})
				if err != nil {
					return err
				}
			} else if bytes.Equal(name, bucketDeploymentPricePins) {
				for i := range page.rows {
					if err := prepareRecoveredPricePin(ctx, reader, &page.rows[i]); err != nil {
						return err
					}
				}
			}
			if err := verifyPricingRecoveryPosition(ctx, reader, head, hash); err != nil {
				return err
			}
			changed := false
			for _, row := range page.rows {
				changed = changed || row.remove || row.replacement != nil
			}
			if changed {
				updateCtx := ctx
				if bytes.Equal(name, bucketDeploymentPricePins) {
					count := 0
					for _, row := range page.rows {
						if row.remove || row.replacement != nil {
							count++
						}
					}
					updateCtx, err = s.pricePinRecoveryContext(ctx, count)
					if err != nil {
						return err
					}
				}
				err = s.updateContext(updateCtx, func(tx *Tx) error {
					if err := ctx.Err(); err != nil {
						return err
					}
					if tx.ID() != page.txID+1 {
						return ErrPricingRecoveryChanged
					}
					bucket := tx.Bucket(name)
					for i := range page.rows {
						if err := ctx.Err(); err != nil {
							return err
						}
						row := &page.rows[i]
						if !bytes.Equal(bucket.Get(row.key), row.raw) {
							return ErrPricingRecoveryChanged
						}
						if row.remove {
							if err := bucket.Delete(row.key); err != nil {
								return err
							}
						} else if row.replacement != nil {
							if err := bucket.Put(row.key, row.replacement); err != nil {
								return err
							}
						}
					}
					return ctx.Err()
				})
				if err != nil {
					return err
				}
				expectedID = page.txID + 1
			}
			if page.done {
				break
			}
			after = page.after
		}
	}
	if err := verifyPricingRecoveryPosition(ctx, reader, head, hash); err != nil {
		return err
	}
	err = s.viewPricePinRecovery(ctx, func(tx *Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if tx.ID() != expectedID {
			return ErrPricingRecoveryChanged
		}
		return nil
	})
	if err != nil {
		return err
	}
	return s.finishPricePinPhysicalRecovery(ctx)
}

func prepareRecoveredPricePin(ctx context.Context, reader ledger.AccountingLeaseReader, row *pricingRecoveryRow) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var intent domain.PricePinIntent
	if err := json.Unmarshal(row.raw, &intent); err != nil {
		return err
	}
	if err := intent.Validate(); err != nil {
		return err
	}
	if !bytes.Equal(row.key, []byte(intent.AttemptID)) {
		return errors.New("price pin key conflicts with attempt identity")
	}
	lease, exists, err := reader.ReadLease(ctx, intent.AttemptID)
	if err != nil {
		return fmt.Errorf("price pin %q accounting read: %w", intent.AttemptID, err)
	}
	if !exists {
		if intent.State == domain.PricePinPrepared {
			row.remove = true
			return nil
		}
		return fmt.Errorf("committed price pin %q has no Ledger lease", intent.AttemptID)
	}
	if lease.Event.Kind != ledger.EventReservationCreated || lease.Event.AttemptID != intent.AttemptID || lease.Sequence == 0 || lease.Event.PriceSnapshot == nil {
		return fmt.Errorf("price pin %q has invalid authoritative lease", intent.AttemptID)
	}
	snapshot := lease.Event.PriceSnapshot
	digest, err := snapshot.Digest()
	if err != nil {
		return fmt.Errorf("price pin %q snapshot digest: %w", intent.AttemptID, err)
	}
	if digest != intent.SnapshotSHA256 || snapshot.PriceVersionID != intent.PriceVersionID || (snapshot.PriceVersion == nil || *snapshot.PriceVersion != intent.PriceVersion) || !snapshot.PricingSelectedAt.Equal(intent.PricingSelectedAt) {
		return fmt.Errorf("price pin %q snapshot conflicts with Ledger", intent.AttemptID)
	}
	if intent.State == domain.PricePinCommitted {
		if intent.LedgerSequence != lease.Sequence {
			return fmt.Errorf("price pin %q Ledger sequence mismatch", intent.AttemptID)
		}
		return nil
	}
	now := time.Now().UTC()
	intent.State, intent.LedgerSequence, intent.CommittedAt = domain.PricePinCommitted, lease.Sequence, &now
	if err := intent.Validate(); err != nil {
		return err
	}
	row.replacement, err = json.Marshal(intent)
	if len(row.replacement) > pricingRecoveryWideRowBytes {
		return ErrPricingRecoveryLimit
	}
	return err
}

func prepareQuarantinedHighWater(tx *Tx, row *pricingRecoveryRow) error {
	var high domain.DeploymentPricingHighWater
	if err := json.Unmarshal(row.raw, &high); err != nil {
		return err
	}
	if err := high.Validate(); err != nil {
		return err
	}
	if !bytes.Equal(row.key, []byte(high.DeploymentID)) {
		return errors.New("pricing high-water key conflicts with deployment identity")
	}
	if high.Quarantined {
		return nil
	}
	raw := tx.Bucket(bucketDeploymentPriceVersions).Get([]byte(high.LatestObservedPriceVersionID))
	if len(raw) > metadatajournal.MaxPayloadSize {
		return ErrPricingRecoveryLimit
	}
	incoherent := raw == nil
	if !incoherent {
		var price domain.DeploymentPriceVersion
		if err := json.Unmarshal(raw, &price); err != nil {
			return err
		}
		incoherent = price.DeploymentID != high.DeploymentID || !price.EffectiveFrom.Equal(high.LatestObservedEffectiveFrom)
	}
	if !incoherent {
		return nil
	}
	if high.Revision == math.MaxUint64 {
		return errors.New("pricing high-water revision overflow")
	}
	high.Quarantined, high.QuarantineReason = true, "restored_high_water_incoherent"
	high.Revision++
	var err error
	row.replacement, err = json.Marshal(high)
	if len(row.replacement) > pricingRecoveryWideRowBytes {
		return ErrPricingRecoveryLimit
	}
	return err
}
