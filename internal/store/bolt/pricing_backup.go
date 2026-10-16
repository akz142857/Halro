package bolt

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	bbolt "go.etcd.io/bbolt"
)

// PriceReferenceCheck validates the pricing references against the Ledger as
// the Ledger is replayed, one record at a time, so the check needs neither
// every settled attempt nor every lease in memory. Observe every replayed
// record, then Finish.
//
// Each observation reads in its own short transaction: a backup may run beside
// a live instance, and one read transaction held across a whole replay would
// stop the metadata file from growing under it.
type PriceReferenceCheck struct {
	store   *Store
	matched int
}

func (s *Store) BeginPriceReferenceCheck() *PriceReferenceCheck {
	return &PriceReferenceCheck{store: s}
}

// Observe checks one replayed record: a lease or settlement priced from a
// versioned price must match that price, and a lease that a price pin names
// must be the one the pin recorded.
func (c *PriceReferenceCheck) Observe(record ledger.Record) error {
	event := record.Event
	if event.Kind != ledger.EventReservationCreated && event.Kind != ledger.EventAttemptSettled {
		return nil
	}
	return c.store.db.View(func(tx *bbolt.Tx) error {
		prices := tx.Bucket(bucketDeploymentPriceVersions)
		snapshot := event.PriceSnapshot
		if snapshot != nil && snapshot.PriceEvidenceStatus == domain.PriceEvidenceVersioned {
			if err := validateSnapshotAgainstPrice(prices, *snapshot); err != nil {
				return fmt.Errorf("attempt %q: %w", event.AttemptID, err)
			}
		}
		if event.Kind != ledger.EventReservationCreated {
			return nil
		}
		raw := tx.Bucket(bucketDeploymentPricePins).Get([]byte(event.AttemptID))
		if raw == nil {
			return nil
		}
		var pin domain.PricePinIntent
		if err := json.Unmarshal(raw, &pin); err != nil {
			return err
		}
		if err := pin.Validate(); err != nil {
			return err
		}
		if pin.State != domain.PricePinCommitted {
			return fmt.Errorf("prepared price pin %q is unresolved", pin.AttemptID)
		}
		if snapshot == nil {
			return fmt.Errorf("price pin %q has no authoritative Ledger lease", pin.AttemptID)
		}
		digest, err := snapshot.Digest()
		if err != nil || digest != pin.SnapshotSHA256 || record.Sequence != pin.LedgerSequence {
			return fmt.Errorf("price pin %q conflicts with Ledger", pin.AttemptID)
		}
		if prices.Get([]byte(pin.PriceVersionID)) == nil {
			return fmt.Errorf("price pin %q references missing price", pin.AttemptID)
		}
		c.matched++
		return nil
	})
}

// Finish checks what only the whole replay can show: every committed pin met
// its lease, none is left prepared, and the pricing high-water marks name
// prices that exist.
func (c *PriceReferenceCheck) Finish() error {
	return c.store.db.View(func(tx *bbolt.Tx) error {
		prices := tx.Bucket(bucketDeploymentPriceVersions)
		committed := 0
		if err := tx.Bucket(bucketDeploymentPricePins).ForEach(func(_, raw []byte) error {
			var pin domain.PricePinIntent
			if err := json.Unmarshal(raw, &pin); err != nil {
				return err
			}
			if err := pin.Validate(); err != nil {
				return err
			}
			if pin.State != domain.PricePinCommitted {
				return fmt.Errorf("prepared price pin %q is unresolved", pin.AttemptID)
			}
			committed++
			return nil
		}); err != nil {
			return err
		}
		if committed != c.matched {
			return fmt.Errorf("%d committed price pins have no authoritative Ledger lease", committed-c.matched)
		}
		return tx.Bucket(bucketDeploymentPricingHighWater).ForEach(func(_, raw []byte) error {
			var high domain.DeploymentPricingHighWater
			if err := json.Unmarshal(raw, &high); err != nil {
				return err
			}
			if err := high.Validate(); err != nil {
				return err
			}
			if prices.Get([]byte(high.LatestObservedPriceVersionID)) == nil {
				return fmt.Errorf("pricing high-water for %q references missing price", high.DeploymentID)
			}
			return nil
		})
	})
}

func validateSnapshotAgainstPrice(prices *bbolt.Bucket, snapshot domain.PriceSnapshot) error {
	raw := prices.Get([]byte(snapshot.PriceVersionID))
	if raw == nil {
		return fmt.Errorf("references missing price %q", snapshot.PriceVersionID)
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
	err := s.db.View(func(tx *bbolt.Tx) error {
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
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{ReadOnly: true, Timeout: time.Second})
	if err != nil {
		return PricingBackupState{}, err
	}
	defer db.Close()
	stateHash, pendingHash := sha256.New(), sha256.New()
	result := PricingBackupState{}
	err = db.View(func(tx *bbolt.Tx) error {
		for _, name := range [][]byte{bucketDeploymentPriceVersions, bucketDeploymentPriceTimeline, bucketDeploymentPricingHighWater, bucketDeploymentPricePins} {
			bucket := tx.Bucket(name)
			if bucket == nil {
				continue
			}
			stateHash.Write(name)
			if err := hashBucketRecursive(stateHash, bucket); err != nil {
				return err
			}
		}
		bucket := tx.Bucket(bucketPricingAuditIntents)
		if bucket != nil {
			if err := bucket.ForEach(func(key, value []byte) error {
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
	return result, err
}

type hashWriter interface{ Write([]byte) (int, error) }

func hashBucketRecursive(hash hashWriter, bucket *bbolt.Bucket) error {
	return bucket.ForEach(func(key, value []byte) error {
		hash.Write(key)
		if value != nil {
			hash.Write(value)
			return nil
		}
		child := bucket.Bucket(key)
		if child != nil {
			return hashBucketRecursive(hash, child)
		}
		return nil
	})
}
