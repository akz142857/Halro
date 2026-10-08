package bolt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/metadatajournal"
)

const priceAuditPageRows = 64
const priceAuditPageBytes = 1 << 20

type priceAuditRow struct {
	key [12]byte
	id  string
	raw []byte
}
type priceAuditPage struct {
	rows     []priceAuditRow
	revision uint64
	done     bool
}

func (s *Store) priceAuditPage(ctx context.Context, deployment string, after []byte, expectedRevision *uint64) (page priceAuditPage, err error) {
	err = s.view(func(tx *Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		page.revision, err = deploymentPriceRevisionTx(tx, deployment)
		if err != nil {
			return err
		}
		if expectedRevision != nil && page.revision != *expectedRevision {
			return ErrPricingRecoveryChanged
		}
		timeline := tx.Bucket(bucketDeploymentPriceTimeline).Bucket([]byte(deployment))
		if timeline == nil {
			if tx.Bucket(bucketDeployments).Get([]byte(deployment)) == nil {
				return ErrNotFound
			}
			page.done = true
			return nil
		}
		cursor := timeline.Cursor()
		key, id := cursor.First()
		if len(after) > 0 {
			key, id = cursor.Seek(after)
			if bytes.Equal(key, after) {
				key, id = cursor.Next()
			}
		}
		total := 0
		for key != nil {
			if err := ctx.Err(); err != nil {
				return err
			}
			if len(key) != 12 || id == nil {
				return domain.ErrPriceTimelineConflict
			}
			raw := tx.Bucket(bucketDeploymentPriceVersions).Get(id)
			if raw == nil {
				return errors.New("deployment price timeline references a missing record")
			}
			if len(id) > metadatajournal.MaxPayloadSize || len(raw) > metadatajournal.MaxPayloadSize-len(id) {
				return ErrPricingAuditLimit
			}
			size := len(raw) + len(id)
			if len(page.rows) == priceAuditPageRows || len(page.rows) > 0 && total+size > priceAuditPageBytes {
				break
			}
			row := priceAuditRow{id: string(id), raw: bytes.Clone(raw)}
			copy(row.key[:], key)
			page.rows = append(page.rows, row)
			total += size
			key, id = cursor.Next()
			if total >= priceAuditPageBytes {
				break
			}
		}
		page.done = key == nil
		return nil
	})
	if err != nil {
		return priceAuditPage{}, err
	}
	return page, nil
}

// Audit pages release the metadata reader before decoding/index IO. Deployment
// revision is checked on every page and completion, including inserts behind
// the cursor. Unrelated metadata and other deployments may continue writing.
func (s *Store) auditDeploymentTimeline(ctx context.Context, deployment string) (revision uint64, resultErr error) {
	leave, err := s.enter()
	if err != nil {
		return 0, err
	}
	defer leave()
	operation, err := s.acquirePricingIO()
	if err != nil {
		return 0, err
	}
	defer func() { resultErr = errors.Join(resultErr, operation.release()) }()
	return s.auditDeploymentTimelineWithReservation(ctx, deployment, operation)
}

func (s *Store) auditDeploymentTimelineWithReservation(ctx context.Context, deployment string, operation *pricingIOReservation) (revision uint64, resultErr error) {
	parent := filepath.Dir(s.path)
	if s.db != nil && s.db.IsReadOnly() {
		parent = ""
	}
	dir, err := os.MkdirTemp(parent, ".halro-price-audit-")
	if err != nil {
		return 0, err
	}
	defer func() { resultErr = errors.Join(resultErr, os.Remove(dir)) }()
	x, err := newPriceAuditIndexWithRetirement(filepath.Join(dir, "versions"), &operation.retired)
	if err != nil {
		return 0, err
	}
	defer func() { resultErr = errors.Join(resultErr, x.close()) }()
	var after []byte
	var expectedRevision *uint64
	for {
		page, err := s.priceAuditPage(ctx, deployment, after, expectedRevision)
		if err != nil {
			return 0, err
		}
		revision = page.revision
		expectedRevision = &revision
		for i := range page.rows {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			row := &page.rows[i]
			var price domain.DeploymentPriceVersion
			if err := json.Unmarshal(row.raw, &price); err != nil {
				return 0, err
			}
			if err := price.Validate(); err != nil {
				return 0, err
			}
			if price.ID != row.id || price.DeploymentID != deployment || !bytes.Equal(row.key[:], deploymentPriceTimelineKey(price.EffectiveFrom)) {
				return 0, domain.ErrPriceTimelineConflict
			}
			if err := x.add(ctx, price.Version); err != nil {
				return 0, err
			}
		}
		if page.done {
			break
		}
		after = bytes.Clone(page.rows[len(page.rows)-1].key[:])
	}
	resultErr = s.view(func(tx *Tx) error {
		current, err := deploymentPriceRevisionTx(tx, deployment)
		if err != nil {
			return err
		}
		if current != revision {
			return ErrPricingRecoveryChanged
		}
		return ctx.Err()
	})
	return revision, resultErr
}
