package bolt

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
	bbolt "go.etcd.io/bbolt"
)

// PricingReferenceReader supplies finite owned pages from a trusted frozen
// Ledger view. No metadata transaction may be retained across its calls.
type PricingReferenceReader interface {
	ledger.AccountingLeaseReader
	PendingLeasePage(context.Context, ledger.ProjectionCursor, ledger.ProjectionPageLimits) (ledger.ProjectionPage[ledger.PendingLease], error)
	SettlementPage(context.Context, ledger.ProjectionCursor, ledger.ProjectionPageLimits) (ledger.ProjectionPage[ledger.SettledAttempt], error)
}

// ValidateDeploymentPriceReferences is read-only. Callers must own an immutable
// metadata/Ledger cut; position and transaction checks reject changes during the
// walk, including inserts behind a cursor. They are not a publication protocol.
func (s *Store) ValidateDeploymentPriceReferences(ctx context.Context, reader PricingReferenceReader) error {
	if s == nil || s.db == nil && s.facade == nil {
		return bbolt.ErrDatabaseNotOpen
	}
	if reader == nil {
		return errors.New("Ledger pricing reader is required")
	}
	head, hash, err := reader.ReadPosition(ctx)
	if err != nil {
		return err
	}
	expectedID := 0
	view := func(check func(*Tx) error) error {
		return s.view(func(tx *Tx) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if expectedID == 0 {
				expectedID = tx.ID()
			} else if tx.ID() != expectedID {
				return ErrPricingRecoveryChanged
			}
			return check(tx)
		})
	}
	if err := view(func(*Tx) error { return nil }); err != nil {
		return err
	}
	limits := ledger.ProjectionPageLimits{Rows: pricingRecoveryRows, Bytes: 8 << 20}
	var cursor ledger.ProjectionCursor
	for {
		page, err := reader.PendingLeasePage(ctx, cursor, limits)
		if err != nil {
			return err
		}
		if err := verifyPricingRecoveryPosition(ctx, reader, head, hash); err != nil {
			return err
		}
		if err := view(func(tx *Tx) error {
			for _, pending := range page.Items {
				if err := ctx.Err(); err != nil {
					return err
				}
				snapshot := pending.Reservation.PriceSnapshot
				if snapshot != nil && snapshot.PriceEvidenceStatus == domain.PriceEvidenceVersioned {
					if err := validateSnapshotAgainstPrice(tx.Bucket(bucketDeploymentPriceVersions), *snapshot); err != nil {
						return fmt.Errorf("pending lease %q: %w", pending.Reservation.AttemptID, err)
					}
				}
			}
			return nil
		}); err != nil {
			return err
		}
		if page.Done {
			break
		}
		cursor = page.Next
	}
	cursor = ledger.ProjectionCursor{}
	for {
		page, err := reader.SettlementPage(ctx, cursor, limits)
		if err != nil {
			return err
		}
		if err := verifyPricingRecoveryPosition(ctx, reader, head, hash); err != nil {
			return err
		}
		if err := view(func(tx *Tx) error {
			for _, settled := range page.Items {
				if err := ctx.Err(); err != nil {
					return err
				}
				snapshot := settled.Settlement.PriceSnapshot
				if snapshot != nil && snapshot.PriceEvidenceStatus == domain.PriceEvidenceVersioned {
					if err := validateSnapshotAgainstPrice(tx.Bucket(bucketDeploymentPriceVersions), *snapshot); err != nil {
						return fmt.Errorf("settled attempt %q: %w", settled.Settlement.AttemptID, err)
					}
				}
			}
			return nil
		}); err != nil {
			return err
		}
		if page.Done {
			break
		}
		cursor = page.Next
	}
	for _, name := range [][]byte{bucketDeploymentPricePins, bucketDeploymentPricingHighWater} {
		var after []byte
		for {
			page, err := s.readPricingRecoveryPage(ctx, name, after, expectedID)
			if err != nil {
				return err
			}
			// Validate pins after the finite metadata page has released its reader.
			if bytes.Equal(name, bucketDeploymentPricePins) {
				for i := range page.rows {
					var pin domain.PricePinIntent
					if err := json.Unmarshal(page.rows[i].raw, &pin); err != nil {
						return err
					}
					if pin.State != domain.PricePinCommitted {
						return fmt.Errorf("prepared price pin %q is unresolved", pin.AttemptID)
					}
					if err := prepareRecoveredPricePin(ctx, reader, &page.rows[i]); err != nil {
						return err
					}
				}
			}
			if err := verifyPricingRecoveryPosition(ctx, reader, head, hash); err != nil {
				return err
			}
			if err := view(func(tx *Tx) error {
				for _, row := range page.rows {
					if err := ctx.Err(); err != nil {
						return err
					}
					var priceID string
					if bytes.Equal(name, bucketDeploymentPricePins) {
						var pin domain.PricePinIntent
						if err := json.Unmarshal(row.raw, &pin); err != nil {
							return err
						}
						priceID = pin.PriceVersionID
					} else {
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
						priceID = high.LatestObservedPriceVersionID
					}
					if _, err := boundedBackupPrice(tx.Bucket(bucketDeploymentPriceVersions), priceID); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				return err
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
	return view(func(*Tx) error { return nil })
}

func boundedBackupPrice(prices *Bucket, id string) ([]byte, error) {
	raw := prices.Get([]byte(id))
	if raw == nil {
		return nil, fmt.Errorf("references missing price %q", id)
	}
	if len(raw) > pricingRecoveryWideRowBytes {
		return nil, ErrPricingRecoveryLimit
	}
	return raw, nil
}

func validateSnapshotAgainstPrice(prices *Bucket, snapshot domain.PriceSnapshot) error {
	raw, err := boundedBackupPrice(prices, snapshot.PriceVersionID)
	if err != nil {
		return err
	}
	var price domain.DeploymentPriceVersion
	if err := json.Unmarshal(raw, &price); err != nil {
		return err
	}
	expected, err := domain.NewVersionedPriceSnapshot(price, snapshot.PricingSelectedAt)
	if err != nil {
		return err
	}
	left, err := snapshot.Digest()
	if err != nil {
		return err
	}
	right, err := expected.Digest()
	if err != nil {
		return err
	}
	if left != right {
		return errors.New("price snapshot content conflicts with metadata price version")
	}
	return nil
}

type PricingBackupState struct {
	StateSHA256         string `json:"state_sha256"`
	PendingIntentSHA256 string `json:"pending_intent_sha256"`
	PendingIntents      int    `json:"pending_intents"`
}

func (s *Store) PricingBackupState() (PricingBackupState, error) {
	stateHash, pendingHash := sha256.New(), sha256.New()
	result := PricingBackupState{}
	err := s.view(func(tx *Tx) error {
		for _, name := range [][]byte{
			bucketDeploymentPriceVersions, bucketDeploymentPriceTimeline, bucketDeploymentPricingHighWater,
			bucketDeploymentPricePins, bucketDeploymentPriceNext, bucketPricingIdempotency,
			bucketDeploymentPriceProposals, bucketPricingProposalIdempotency,
		} {
			stateHash.Write(name)
			bucket := tx.Bucket(name)
			if err := hashBucketRecursive(stateHash, bucket); err != nil {
				return err
			}
		}
		if err := tx.Bucket(bucketPricingAuditIntents).ForEach(func(key, value []byte) error {
			if len(key) > pricingRecoveryWideRowBytes || len(value) > pricingRecoveryWideRowBytes-len(key) {
				return ErrPricingRecoveryLimit
			}
			var intent domain.PricingAuditIntent
			if err := json.Unmarshal(value, &intent); err != nil {
				return err
			}
			if !intent.Delivered {
				result.PendingIntents++
				pendingHash.Write(key)
				pendingHash.Write(value)
			}
			return nil
		}); err != nil {
			return err
		}
		return nil
	})
	result.StateSHA256 = "sha256:" + hex.EncodeToString(stateHash.Sum(nil))
	result.PendingIntentSHA256 = "sha256:" + hex.EncodeToString(pendingHash.Sum(nil))
	return result, err
}

// LegacyPricingBackupState computes the schema-13/backup-v2 projection before
// Open performs an in-place migration. It exists only for restore compatibility.
func LegacyPricingBackupState(path string) (PricingBackupState, error) {
	return LegacyPricingBackupStateWithMappingBudget(context.Background(), path, 0, nil)
}

// The legacy hash must precede migration, so this uses a raw read-only handle,
// never an exact-current-schema Store. It still owns the configured file/mapping
// limits, cancellation and actual close before returning the compatibility hash.
func LegacyPricingBackupStateWithMappingBudget(ctx context.Context, path string, maxBytes int64, budget *resourcebudget.Manager) (result PricingBackupState, resultErr error) {
	if ctx == nil {
		return result, resourcebudget.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	var permit resourcebudget.Permit
	var err error
	if maxBytes > 0 {
		if budget == nil {
			return result, resourcebudget.ErrInvalid
		}
		permit, err = acquireMetadataMapping(budget, maxBytes)
		if err != nil {
			return result, err
		}
	}
	db, err := openMetadataFile(path, &bbolt.Options{ReadOnly: true, Timeout: time.Second}, maxBytes)
	if err != nil {
		return PricingBackupState{}, errors.Join(err, releaseMetadataMapping(permit))
	}
	defer func() {
		closeErr := db.Close()
		resultErr = errors.Join(resultErr, closeErr)
		if closeErr == nil {
			resultErr = errors.Join(resultErr, releaseMetadataMapping(permit))
		}
		if resultErr != nil {
			result = PricingBackupState{}
		}
	}()
	stateHash, pendingHash := sha256.New(), sha256.New()
	err = viewRaw(db, func(tx *Tx) error {
		for _, name := range [][]byte{bucketDeploymentPriceVersions, bucketDeploymentPriceTimeline, bucketDeploymentPricingHighWater, bucketDeploymentPricePins} {
			if err := ctx.Err(); err != nil {
				return err
			}
			bucket := tx.Bucket(name)
			if bucket == nil {
				continue
			}
			stateHash.Write(name)
			if err := hashBucketRecursiveContext(ctx, stateHash, bucket); err != nil {
				return err
			}
		}
		bucket := tx.Bucket(bucketPricingAuditIntents)
		if bucket != nil {
			if err := bucket.ForEach(func(key, value []byte) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				if len(key) > pricingRecoveryWideRowBytes || len(value) > pricingRecoveryWideRowBytes-len(key) {
					return ErrPricingRecoveryLimit
				}
				var intent domain.PricingAuditIntent
				if err := json.Unmarshal(value, &intent); err != nil {
					return err
				}
				if !intent.Delivered {
					result.PendingIntents++
					pendingHash.Write(key)
					pendingHash.Write(value)
				}
				return nil
			}); err != nil {
				return err
			}
		}
		return nil
	})
	result.StateSHA256 = "sha256:" + hex.EncodeToString(stateHash.Sum(nil))
	result.PendingIntentSHA256 = "sha256:" + hex.EncodeToString(pendingHash.Sum(nil))
	return result, errors.Join(err, ctx.Err())
}

type hashWriter interface{ Write([]byte) (int, error) }

func hashBucketRecursive(hash hashWriter, bucket *Bucket) error {
	return hashBucketRecursiveContext(context.Background(), hash, bucket)
}

// The native metadata protocol permits at most 32 path components. A fixed
// depth-first cursor stack preserves the historical hash byte order without
// recursive stack growth or retaining all keys/values of a bucket.
func hashBucketRecursiveContext(ctx context.Context, hash hashWriter, bucket *Bucket) error {
	if bucket == nil {
		return nil
	}
	type frame struct {
		bucket  *Bucket
		cursor  *Cursor
		started bool
	}
	var stack [32]frame
	stack[0] = frame{bucket: bucket, cursor: bucket.Cursor()}
	depth := 1
	for depth > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		current := &stack[depth-1]
		var key, value []byte
		if current.started {
			key, value = current.cursor.Next()
		} else {
			key, value = current.cursor.First()
			current.started = true
		}
		if key == nil {
			*current = frame{}
			depth--
			continue
		}
		if _, err := hash.Write(key); err != nil {
			return err
		}
		if value != nil {
			if _, err := hash.Write(value); err != nil {
				return err
			}
			continue
		}
		child := current.bucket.Bucket(key)
		if child != nil {
			if depth == len(stack) {
				return ErrPricingRecoveryLimit
			}
			stack[depth] = frame{bucket: child, cursor: child.Cursor()}
			depth++
		}
	}
	return nil
}
