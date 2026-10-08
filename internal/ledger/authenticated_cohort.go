package ledger

import (
	"bytes"
	"context"

	"github.com/akz142857/Halro/internal/domain"
)

// Visitors run outside owner/State locks and DB transactions, while one fixed
// authenticated pin and workspace remain owned through the entire scope. A
// callback may Close its component: physical pins close, the callback fee
// remains owned until return, and the result fails. Any error invalidates all
// visited inputs. Recovery and cohort share a single zero-queue admission slot.
func (p *AuthenticatedLedgerProjection) VisitGovernanceCohort(ctx context.Context, q GovernanceCohortQuery, visit func(GovernanceCohortRow) error) (Watermark, error) {
	if err := validateGovernanceCohort(&q, visit); err != nil {
		return Watermark{}, err
	}
	var head Watermark
	err := p.withAuthenticatedPinnedView(ctx, func(v *authenticatedRecoveryView) error {
		head = v.head.Head
		after, eligible := "", 0
		for {
			batch, last, done, err := authenticatedCohortBatch(ctx, v, q, after, &eligible)
			if err != nil {
				return err
			}
			for _, row := range batch {
				if err := ctx.Err(); err != nil {
					return err
				}
				v.mu.Lock()
				err := v.check(ctx)
				v.mu.Unlock()
				if err != nil {
					return err
				}
				if err := visit(row); err != nil {
					return err
				}
			}
			if done {
				return nil
			}
			if last == after {
				return ErrProjectionCorrupt
			}
			after = last
		}
	}, false)
	if err != nil {
		return Watermark{}, err
	}
	return head, nil
}

func authenticatedCohortBatch(ctx context.Context, v *authenticatedRecoveryView, q GovernanceCohortQuery, after string, eligible *int) (batch []GovernanceCohortRow, last string, done bool, resultErr error) {
	c, end, err := v.begin(ctx)
	if err != nil {
		return nil, "", false, err
	}
	defer end()
	rows := authenticatedReadRows{c, v.snapshot}
	last, done = after, true
	readBytes, scanned := 0, 0
	resultErr = authenticatedIndexWalk(c, v.snapshot, "cohort_units", projectionTuple(q.ProjectID), projectionListKey(q.ProjectID, after), func(k, raw []byte) (bool, error) {
		if after != "" && bytes.Equal(k, projectionListKey(q.ProjectID, after)) {
			return false, nil
		}
		if scanned == q.Limits.Rows {
			done = false
			return true, nil
		}
		if len(k) <= 32 || len(k) > 32+domain.MaxGovernanceIDLength || len(raw) == 0 || len(raw) > projectionMaxValue {
			return false, ErrProjectionCorrupt
		}
		if len(raw) > q.Limits.Bytes-readBytes {
			if scanned == 0 {
				return false, ErrProjectionPageLimit
			}
			done = false
			return true, nil
		}
		id := string(k[32:])
		identity, err := recoveryDecode[cohortIdentity](raw)
		if err != nil {
			return false, err
		}
		if identity.Sequence == 0 || identity.Sequence > v.head.Head.Sequence || identity.WorkUnit.ID != id || identity.WorkUnit.ProjectID != q.ProjectID || identity.WorkUnit.Status != domain.WorkUnitOpen || identity.WorkUnit.Validate() != nil {
			return false, ErrProjectionCorrupt
		}
		rowBytes := len(raw)
		if cohortEligible(identity.WorkUnit, q) {
			versionRaw, seq, err := cohortVersionRowsRaw(rows, id, v.head.Head.Sequence)
			if err != nil {
				return false, err
			}
			if seq < identity.Sequence {
				return false, ErrProjectionCorrupt
			}
			if len(versionRaw) > q.Limits.Bytes-readBytes-rowBytes {
				if scanned == 0 {
					return false, ErrProjectionPageLimit
				}
				done = false
				return true, nil
			}
			rowBytes += len(versionRaw)
			version, err := readCohortRowsVersion(rows, id, seq)
			if err != nil {
				return false, err
			}
			row, err := version.row()
			if err != nil {
				return false, err
			}
			if *eligible >= q.MaxWorkUnits {
				return false, ErrGovernanceCohortLimit
			}
			*eligible++
			batch = append(batch, row)
		}
		readBytes += rowBytes
		scanned++
		last = id
		return false, nil
	})
	resultErr = v.finish(c, resultErr)
	if resultErr != nil {
		return nil, "", false, resultErr
	}
	return batch, last, done, nil
}
