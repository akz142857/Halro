package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/replication"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

// The fixed synchronous batch owns its own future ticket until native append,
// Metadata checkpoint and all original peer application have actually joined.
// It never borrows the cap of one of the callers whose events share this batch.
func (r *Runtime) appendOwnedAdminAuditBatch(events []audit.Event) error {
	ctx := context.Background()
	var q *haPeerPhysicalController
	if f := r.haSelectedState; f != nil && f.replication != nil {
		q = f.replication.peerPhysical.Load()
		if q != nil {
			ctx = f.ctx
		}
	}
	var ticket *haPeerPhysicalTicket
	if q != nil {
		if len(events) < 1 || len(events) > adminAuditMaxEntries {
			return audit.ErrBatchLimit
		}
		encoded, err := json.Marshal(events)
		if err != nil {
			return err
		}
		// Audit encoder accepts one legal batch; the finite queue is <=4MiB. Metadata
		// checkpoint is one actual transaction and frame, capped by its legal frame.
		cost := replication.PhysicalCreditCost{OrderingRecords: 2}
		cost.NativeBytes[1] = uint64(audit.PhysicalBatchBytes)
		cost.NativeRecords[1] = uint32(len(events))
		cost.NativeBytes[3] = uint64(ledger.HAPhysicalCreditAppendBytes)
		cost.NativeRecords[3], cost.COWRecords[3] = 1, 1
		var errReserve error
		ctx, ticket, errReserve = q.reserve(ctx, sha256.Sum256(encoded), cost)
		clear(encoded)
		if errReserve != nil {
			if ticket != nil {
				q.mu.Lock()
				unknown := q.live == ticket
				q.mu.Unlock()
				if unknown {
					r.retainAdminAuditFuture(errReserve)
					return errors.Join(ownedfile.ErrRetirement, errReserve)
				}
			}
			return errReserve
		}
	}
	_, err := r.audit.AppendBatch(ctx, events)
	if err == nil {
		summary := r.audit.Summary()
		err = r.store.PutAuditCheckpointContext(ctx, boltstore.AuditCheckpoint{Records: summary.Records, Bytes: summary.Bytes, LastHash: summary.LastHash})
		if err != nil {
			err = fmt.Errorf("checkpoint audit log: %w", err)
		}
	}
	if ticket != nil {
		// Both synchronous callbacks have returned; confirmation still must include
		// the trailing Metadata fact, all stores and every original peer.
		joined := q.finishTicket(ctx, ticket)
		if joined != nil {
			r.retainAdminAuditFuture(joined)
			return errors.Join(err, ownedfile.ErrRetirement, joined)
		}
	}
	return err
}
func (r *Runtime) retainAdminAuditFuture(err error) {
	owner := &r.auditBatches
	owner.mu.Lock()
	defer owner.mu.Unlock()
	owner.retired = true
	owner.closeErr = errors.Join(owner.closeErr, ownedfile.ErrRetirement, err)
}
