package bolt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

var ErrProposalIndexConflict = errors.New("pricing proposal index differs from its facts")

// The secondary order and count are authoritative projection state. Every
// mutation is recorded beside the proposal, never rebuilt independently on a
// Replica. Migration publishes the whole projection as a new journal epoch.
func migratePricingProposalOrder(tx *Tx, step func(string) error) error {
	if err := migrationStep(step, "before_proposal_order"); err != nil {
		return err
	}
	for _, name := range [][]byte{bucketPricingProposalOrder, bucketPricingProposalCount} {
		if _, err := tx.CreateBucketIfNotExists(name); err != nil {
			return err
		}
	}
	// The authenticated old journal may contain durable, unapplied proposals.
	// Reserve the schema here; the existing durable epoch protocol reconciles
	// that tail before bounded index construction and epoch publication.
	control := tx.Bucket(bucketOutcomeDefinitionExportMeta)
	if control == nil {
		return ErrProposalIndexConflict
	}
	// An earlier migration may already have prepared/published its epoch file.
	// Preserve that durable proof, especially the file-before-position window.
	if control.Get(definitionExportPendingKey) == nil {
		if err := control.Put(definitionExportPendingKey, []byte{1}); err != nil {
			return err
		}
	}

	return migrationStep(step, "after_proposal_order")
}

var ErrProposalIndexIncomplete = errors.New("pricing proposal index awaits authenticated epoch publication")

func proposalIndexReadyTx(tx *Tx) error {
	control := tx.Bucket(bucketOutcomeDefinitionExportMeta)
	if control == nil || control.Get(definitionExportPendingKey) != nil {
		return ErrProposalIndexIncomplete
	}
	return nil
}

func proposalCountTx(tx *Tx, dep string) (uint64, error) {
	b := tx.Bucket(bucketPricingProposalCount)
	if b == nil {
		return 0, ErrProposalIndexConflict
	}
	raw := b.Get([]byte(dep))
	if raw == nil {
		return 0, nil
	}
	count, ok := decodeUint64(raw)
	if !ok {
		return 0, ErrProposalIndexConflict
	}
	return count, nil
}

// Decode only the order identity here. Business writers validate the complete
// proposal; cold reads independently validate all digests, including off-page
// rows. A corrupted historical digest must remain visible to that audit.
func proposalIndexIdentity(key, raw []byte) (domain.DeploymentPriceProposal, error) {
	var p struct {
		ID           string    `json:"id"`
		DeploymentID string    `json:"deployment_id"`
		CreatedAt    time.Time `json:"created_at"`
	}
	if len(key) > pricingRecoveryWideRowBytes || len(raw) > pricingRecoveryWideRowBytes-len(key) {
		return domain.DeploymentPriceProposal{}, ErrPricingRecoveryLimit
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return domain.DeploymentPriceProposal{}, err
	}
	if p.ID != string(key) || strings.TrimSpace(p.DeploymentID) == "" || p.CreatedAt.IsZero() {
		return domain.DeploymentPriceProposal{}, ErrProposalIndexConflict
	}
	return domain.DeploymentPriceProposal{ID: p.ID, DeploymentID: p.DeploymentID, CreatedAt: p.CreatedAt}, nil
}

func (tx *Tx) updateProposalOrder(key, previous, value []byte) error {
	if previous == nil && value == nil {
		return nil
	}
	if tx.recorder != nil {
		tx.recorder.updatingProposalOrder = true
		defer func() { tx.recorder.updatingProposalOrder = false }()
	}
	root, counts := tx.Bucket(bucketPricingProposalOrder), tx.Bucket(bucketPricingProposalCount)
	if root == nil || counts == nil {
		return ErrProposalIndexConflict
	}
	if previous != nil {
		old, err := proposalIndexIdentity(key, previous)
		if err != nil {
			return err
		}
		order := root.Bucket([]byte(old.DeploymentID))
		pos := proposalOrder(old)
		if order == nil || !bytes.Equal(order.Get(pos[:]), key) {
			return ErrProposalIndexConflict
		}
		count, err := proposalCountTx(tx, old.DeploymentID)
		if err != nil {
			return err
		}
		if count == 0 {
			return ErrProposalIndexConflict
		}
		if err := order.Delete(pos[:]); err != nil {
			return err
		}
		if err := counts.Put([]byte(old.DeploymentID), encodeUint64(count-1)); err != nil {
			return err
		}
	}
	if value != nil {
		p, err := proposalIndexIdentity(key, value)
		if err != nil {
			return err
		}
		order, err := root.CreateBucketIfNotExists([]byte(p.DeploymentID))
		if err != nil {
			return err
		}
		pos := proposalOrder(p)
		if order.Get(pos[:]) != nil {
			return ErrProposalIndexConflict
		}
		count, err := proposalCountTx(tx, p.DeploymentID)
		if err != nil {
			return err
		}
		if count == math.MaxUint64 {
			return ErrProposalIndexConflict
		}
		if err := order.Put(pos[:], key); err != nil {
			return err
		}
		if err := counts.Put([]byte(p.DeploymentID), encodeUint64(count+1)); err != nil {
			return err
		}
	}
	return nil
}

func checkProposalRevision(ctx context.Context, tx *Tx, dep string, revision uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := proposalRevisionTx(tx, dep)
	if err != nil {
		return err
	}
	if current != revision {
		return ErrPricingPageChanged
	}
	return nil
}

type proposalIndexedRow struct {
	pos     proposalPosition
	id, raw []byte
}
type proposalIndexedPage struct {
	rows []proposalIndexedRow
	done bool
}

// The native index is ordered ascending. Seek the exact preceding position,
// then walk backwards, copying a finite owned page before releasing the reader.
// The index row and the corresponding primary record share the same view.
func (s *Store) readProposalOrderPage(ctx context.Context, dep string, after *proposalPosition, revision uint64, limit int) (page proposalIndexedPage, resultErr error) {
	err := s.view(func(tx *Tx) error {
		if err := checkProposalRevision(ctx, tx, dep, revision); err != nil {
			return err
		}
		root := tx.Bucket(bucketPricingProposalOrder)
		if root == nil {
			return ErrProposalIndexConflict
		}
		order := root.Bucket([]byte(dep))
		if order == nil {
			if after != nil {
				return ErrPricingPageChanged
			}
			page.done = true
			return nil
		}
		cursor := order.Cursor()
		key, id := cursor.Last()
		if after != nil {
			key, id = cursor.Seek(after[:])
			if !bytes.Equal(key, after[:]) {
				return ErrPricingPageChanged
			}
			key, id = cursor.Prev()
		}
		used := 0
		for key != nil {
			if err := ctx.Err(); err != nil {
				return err
			}
			if len(key) != len(proposalPosition{}) || id == nil {
				return ErrProposalIndexConflict
			}
			raw := tx.Bucket(bucketDeploymentPriceProposals).Get(id)
			if raw == nil {
				return ErrProposalIndexConflict
			}
			if len(id) > pricingRecoveryWideRowBytes || len(raw) > pricingRecoveryWideRowBytes-len(id) {
				return ErrPricingRecoveryLimit
			}
			size := len(raw) + len(id)
			if len(page.rows) == limit || len(page.rows) > 0 && size > pricingRecoveryBytes-used {
				break
			}
			var pos proposalPosition
			copy(pos[:], key)
			page.rows = append(page.rows, proposalIndexedRow{pos: pos, id: bytes.Clone(id), raw: bytes.Clone(raw)})
			used += size
			key, id = cursor.Prev()
		}
		page.done = key == nil
		return nil
	})
	if err != nil {
		return proposalIndexedPage{}, err
	}
	return page, nil
}

func decodeIndexedProposal(dep string, row proposalIndexedRow) (domain.DeploymentPriceProposal, error) {
	var p domain.DeploymentPriceProposal
	if err := json.Unmarshal(row.raw, &p); err != nil {
		return p, err
	}
	if p.ID != string(row.id) || p.DeploymentID != dep || proposalOrder(p) != row.pos {
		return p, ErrProposalIndexConflict
	}
	return p, p.Validate()
}

// Full cold validation detects both omitted facts and spurious index entries.
// Readers end at each 64-row/1-MiB boundary. The fixed process marker is only a
// revision cache; native store authentication owns integrity after publication.
func (s *Store) auditProposalOrder(ctx context.Context, dep string, revision uint64) error {
	var upper []byte
	var expected uint64
	check := func(tx *Tx) error { return checkProposalRevision(ctx, tx, dep, revision) }
	if err := s.view(func(tx *Tx) error {
		if err := check(tx); err != nil {
			return err
		}
		var err error
		expected, err = proposalCountTx(tx, dep)
		if err != nil {
			return err
		}
		upper, _ = tx.Bucket(bucketDeploymentPriceProposals).Cursor().Last()
		upper = bytes.Clone(upper)
		if upper == nil {
			upper = []byte{}
		}
		return nil
	}); err != nil {
		return err
	}
	var count uint64
	var after []byte
	for {
		page, err := s.readPricingRecoveryPageChecked(ctx, bucketDeploymentPriceProposals, after, upper, check)
		if err != nil {
			return err
		}
		var identities [pricingRecoveryRows]domain.DeploymentPriceProposal
		n := 0
		for _, row := range page.rows {
			if err := ctx.Err(); err != nil {
				return err
			}
			var p domain.DeploymentPriceProposal
			if err := json.Unmarshal(row.raw, &p); err != nil {
				return err
			}
			if p.ID != string(row.key) {
				return ErrProposalIndexConflict
			}
			if p.DeploymentID != dep {
				continue
			}
			if err := p.Validate(); err != nil {
				return err
			}
			identities[n] = domain.DeploymentPriceProposal{ID: p.ID, DeploymentID: p.DeploymentID, CreatedAt: p.CreatedAt}
			n++
		}
		if err := s.view(func(tx *Tx) error {
			if err := check(tx); err != nil {
				return err
			}
			root := tx.Bucket(bucketPricingProposalOrder)
			if root == nil {
				return ErrProposalIndexConflict
			}
			order := root.Bucket([]byte(dep))
			for i := 0; i < n; i++ {
				pos := proposalOrder(identities[i])
				if order == nil || !bytes.Equal(order.Get(pos[:]), []byte(identities[i].ID)) {
					return ErrProposalIndexConflict
				}
			}
			return nil
		}); err != nil {
			return err
		}
		if uint64(n) > math.MaxUint64-count {
			return ErrProposalIndexConflict
		}
		count += uint64(n)
		if page.done {
			break
		}
		after = page.after
	}
	if count != expected {
		return ErrProposalIndexConflict
	}
	var indexed uint64
	var position *proposalPosition
	for {
		page, err := s.readProposalOrderPage(ctx, dep, position, revision, pricingRecoveryRows)
		if err != nil {
			return err
		}
		for _, row := range page.rows {
			if err := ctx.Err(); err != nil {
				return err
			}
			if _, err := decodeIndexedProposal(dep, row); err != nil {
				return err
			}
		}
		if uint64(len(page.rows)) > math.MaxUint64-indexed {
			return ErrProposalIndexConflict
		}
		indexed += uint64(len(page.rows))
		if page.done {
			break
		}
		last := page.rows[len(page.rows)-1].pos
		position = &last
	}
	if indexed != count {
		return ErrProposalIndexConflict
	}
	return s.view(check)
}
