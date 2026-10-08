package bolt

import (
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strings"

	"github.com/akz142857/Halro/internal/metadatajournal"
)

// An absent deployment key is the immutable schema-42 migration baseline.
// This fence detects concurrent price changes; native authentication still owns
// the integrity of the price records and this bucket together.
func deploymentPriceRevisionTx(tx *Tx, deployment string) (uint64, error) {
	bucket := tx.Bucket(bucketDeploymentPriceRevision)
	if bucket == nil {
		return 0, errors.New("deployment price revision bucket is missing")
	}
	raw := bucket.Get([]byte(deployment))
	if raw == nil {
		return 0, nil
	}
	value, ok := decodeUint64(raw)
	if !ok {
		return 0, errors.New("invalid deployment price revision")
	}
	return value, nil
}

func migrateDeploymentPriceRevision(tx *Tx, step func(string) error) error {
	if err := migrationStep(step, "before_deployment_price_revision"); err != nil {
		return err
	}
	if _, err := tx.CreateBucketIfNotExists(bucketDeploymentPriceRevision); err != nil {
		return err
	}
	return migrationStep(step, "after_deployment_price_revision")
}

func (r *recorder) trackPriceMutation(kind metadatajournal.OpKind, path []string, previous, value []byte) error {
	if len(path) == 0 {
		return nil
	}
	add := func(id string) error {
		if strings.TrimSpace(id) == "" {
			return errors.New("price mutation has no deployment identity")
		}
		if r.priceDeployments == nil {
			r.priceDeployments = make(map[string]struct{})
		}
		r.priceDeployments[id] = struct{}{}
		return nil
	}
	switch path[0] {
	case string(bucketDeploymentPriceRevision):
		if !r.updatingPriceRevisions {
			return errors.New("price revision is owned by the transaction entry")
		}
	case string(bucketDeploymentPriceTimeline):
		if len(path) >= 2 {
			return add(path[1])
		}
		if kind == metadatajournal.OpDeleteBucket || kind == metadatajournal.OpPut || kind == metadatajournal.OpDelete {
			return errors.New("price timeline root mutation requires whole-file publication")
		}
	case string(bucketDeploymentPriceVersions):
		if len(path) != 1 || kind == metadatajournal.OpDeleteBucket {
			return errors.New("price record bucket mutation requires whole-file publication")
		}
		for _, raw := range [][]byte{previous, value} {
			if len(raw) == 0 {
				continue
			}
			var record struct {
				DeploymentID string `json:"deployment_id"`
			}
			if err := json.Unmarshal(raw, &record); err != nil {
				return err
			}
			if err := add(record.DeploymentID); err != nil {
				return err
			}
		}
	}
	return nil
}

// Run exactly once after all merged callbacks succeed. The fence operation is
// in the same native frame and database commit as every affected price record.
func (tx *Tx) bumpPriceRevisions() error {
	tx.recorder.updatingPriceRevisions = true
	defer func() { tx.recorder.updatingPriceRevisions = false }()
	ids := make([]string, 0, len(tx.recorder.priceDeployments))
	for id := range tx.recorder.priceDeployments {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		current, err := deploymentPriceRevisionTx(tx, id)
		if err != nil {
			return err
		}
		if current == math.MaxUint64 {
			return errors.New("deployment price revision exhausted")
		}
		if err := tx.Bucket(bucketDeploymentPriceRevision).Put([]byte(id), encodeUint64(current+1)); err != nil {
			return err
		}
	}
	return nil
}
