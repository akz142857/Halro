package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

const adminAuditMaxEntries = 64
const adminAuditMaxBytes = 4 << 20

// Includes both active and waiting events, codec/replication copies and result
// storage. Structural working envelope; target RSS calibration remains required.
const adminAuditBatchRAMBytes int64 = 32 << 20

var errAdminAuditBatchUnavailable = errors.New("admin audit batch is closed or at capacity")

type adminAuditBatchOwner struct {
	admitted         int
	mu               sync.Mutex
	pending          []adminAuditRequest
	count, bytes     int // includes the batch currently inside native IO/checkpoint
	running, retired bool
	paused           *adminAuditPause
	done             chan struct{}
	permit           resourcebudget.Permit
	owned            bool
	closeErr         error
}

func (r *Runtime) enqueueAdminAudit(event audit.Event) error {
	// This probe invokes no encoding callback and retains no borrowed graph.
	if err := event.Preflight(64 << 10); err != nil {
		return err
	}
	owner := &r.auditBatches
	owner.mu.Lock()
	if owner.retired || owner.paused != nil || owner.count >= adminAuditMaxEntries || (owner.running && owner.admitted >= adminAuditMaxEntries) {
		owner.mu.Unlock()
		return errAdminAuditBatchUnavailable
	}
	leader := !owner.running
	if leader {
		if manager := r.resourcePressure.logicalBudget(); manager != nil {
			permit, err := manager.TryAcquire(resourcebudget.IO, resourcebudget.Cost{RAM: adminAuditBatchRAMBytes})
			if err != nil {
				owner.mu.Unlock()
				return err
			}
			owner.permit, owner.owned = permit, true
		}
	}
	fail := func(err error) error {
		if leader && owner.owned {
			releaseErr := owner.permit.Release()
			err = errors.Join(err, releaseErr)
			if releaseErr != nil {
				owner.retired = true
				owner.closeErr = errors.Join(owner.closeErr, releaseErr)
			} else {
				owner.owned = false
				owner.permit = resourcebudget.Permit{}
			}
		}
		owner.mu.Unlock()
		return err
	}
	// The job envelope is held before canonical encoding, validation or copying.
	// Nonleaders borrow this same envelope while the lock protects retirement.
	if err := event.Validate(); err != nil {
		return fail(err)
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		return fail(err)
	}
	if len(encoded) > 64<<10 {
		return fail(audit.ErrBatchLimit)
	}
	if len(encoded) > adminAuditMaxBytes-owner.bytes {
		return fail(errAdminAuditBatchUnavailable)
	}
	var owned audit.Event
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if err := decoder.Decode(&owned); err != nil {
		return fail(err)
	}
	if leader {
		owner.admitted = 0
		owner.running, owner.done = true, make(chan struct{})
	}
	request := adminAuditRequest{event: owned, result: make(chan error, 1), bytes: len(encoded)}
	owner.admitted++
	owner.count++
	owner.bytes += request.bytes
	owner.pending = append(owner.pending, request)
	owner.mu.Unlock()
	if leader {
		r.flushAdminAuditBatches()
	}
	return <-request.result
}

func (r *Runtime) flushAdminAuditBatches() {
	owner := &r.auditBatches
	for {
		time.Sleep(time.Millisecond)
		owner.mu.Lock()
		batch := owner.pending
		owner.pending = nil
		if len(batch) == 0 {
			if owner.owned && owner.closeErr == nil {
				owner.closeErr = errors.Join(owner.closeErr, owner.permit.Release())
				if owner.closeErr == nil {
					owner.owned = false
					owner.permit = resourcebudget.Permit{}
				}
			}
			owner.running = false
			owner.admitted = 0
			if owner.closeErr != nil {
				owner.retired = true
			}
			close(owner.done)
			owner.mu.Unlock()
			return
		}
		owner.mu.Unlock()
		events := make([]audit.Event, len(batch))
		for index := range batch {
			events[index] = batch[index].event
		}
		err := r.appendOwnedAdminAuditBatch(events)
		// Native persistence/confirmation and Metadata checkpoint are finished.
		// Release graph references before credit and before a future batch starts.
		clear(events)
		owner.mu.Lock()
		for index := range batch {
			owner.count--
			owner.bytes -= batch[index].bytes
			batch[index].result <- err
			batch[index] = adminAuditRequest{}
		}
		owner.mu.Unlock()
	}
}

func (r *Runtime) closeAdminAuditBatches() error {
	owner := &r.auditBatches
	owner.mu.Lock()
	owner.retired = true
	done, running := owner.done, owner.running
	owner.mu.Unlock()
	if running {
		<-done
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	return owner.closeErr
}
