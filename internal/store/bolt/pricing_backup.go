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

func (s *Store) ValidateDeploymentPriceReferences(state *ledger.State) error {
	if state == nil {
		return errors.New("Ledger state is required")
	}
	return s.db.View(func(tx *bbolt.Tx) error {
		prices := tx.Bucket(bucketDeploymentPriceVersions)
		for _, pending := range state.PendingLeases() {
			snapshot := pending.Reservation.PriceSnapshot
			if snapshot != nil && snapshot.PriceEvidenceStatus == domain.PriceEvidenceVersioned {
				if err := validateSnapshotAgainstPrice(prices, *snapshot); err != nil {
					return fmt.Errorf("pending lease %q: %w", pending.Reservation.AttemptID, err)
				}
			}
		}
		for _, settled := range state.SettledAttempts() {
			snapshot := settled.Settlement.PriceSnapshot
			if snapshot != nil && snapshot.PriceEvidenceStatus == domain.PriceEvidenceVersioned {
				if err := validateSnapshotAgainstPrice(prices, *snapshot); err != nil {
					return fmt.Errorf("settled attempt %q: %w", settled.Settlement.AttemptID, err)
				}
			}
		}
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
			lease, ok := state.AccountingLease(pin.AttemptID)
			if !ok || lease.Event.PriceSnapshot == nil {
				return fmt.Errorf("price pin %q has no authoritative Ledger lease", pin.AttemptID)
			}
			digest, err := lease.Event.PriceSnapshot.Digest()
			if err != nil || digest != pin.SnapshotSHA256 || lease.Sequence != pin.LedgerSequence {
				return fmt.Errorf("price pin %q conflicts with Ledger", pin.AttemptID)
			}
			if prices.Get([]byte(pin.PriceVersionID)) == nil {
				return fmt.Errorf("price pin %q references missing price", pin.AttemptID)
			}
			return nil
		}); err != nil {
			return err
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
