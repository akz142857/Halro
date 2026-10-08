package app

import (
	"context"
	"errors"
	"math"
	"net/http"
	"sync"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/statecut"
)

type runtimeCheckpointPause struct {
	done   chan struct{}
	joined bool
}

// The ticket spans the full HTTP handler and its deferred terminal cleanup.
// Health remains callable while maintenance sheds new business requests.
type runtimeLiveCheckpoint struct {
	job    sync.Mutex
	mu     sync.Mutex
	active uint64
	paused *runtimeCheckpointPause
}

func (r *Runtime) withMemoryCheckpointRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if r.selectedState == nil && r.haSelectedState == nil || request.URL.Path == "/health/live" || request.URL.Path == "/health/ready" {
			next.ServeHTTP(w, request)
			return
		}
		l := &r.liveCheckpoint
		l.mu.Lock()
		if l.paused != nil {
			l.mu.Unlock()
			w.Header().Set("Retry-After", "1")
			http.Error(w, "memory state checkpoint is in progress", http.StatusServiceUnavailable)
			return
		}
		l.active++
		l.mu.Unlock()
		defer func() {
			l.mu.Lock()
			l.active--
			if l.active == 0 && l.paused != nil && !l.paused.joined {
				close(l.paused.done)
				l.paused.joined = true
			}
			l.mu.Unlock()
		}()
		next.ServeHTTP(w, request)
	})
}

// CheckpointMemoryState publishes all seven authenticated roots while keeping
// the same listeners, managers and worker owners. It is also the scheduled live
// checkpoint path. This method must not be called from a business HTTP handler
// (which owns a ticket that the checkpoint must join), or from owner callbacks.
func (r *Runtime) CheckpointMemoryState(ctx context.Context) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.selectedState == nil && (r.haSelectedState == nil || r.config.ResourceBudget == nil || !r.config.ResourceBudget.PhysicalTerminalReserve) {
		return authenticatedstate.ErrSealIneligible
	}
	l := &r.liveCheckpoint
	if !l.job.TryLock() {
		return authenticatedstate.ErrBusy
	}
	defer l.job.Unlock()
	if r.draining.Load() || r.backgroundWait.drainState.Load() != businessDrainUnstarted {
		return authenticatedstate.ErrSealIneligible
	}
	if r.selectedState != nil {
		if err := r.selectedState.generation.WaitStartupAudit(ctx); err != nil {
			return err
		}
	}
	l.mu.Lock()
	pause := &runtimeCheckpointPause{done: make(chan struct{})}
	l.paused = pause
	if l.active == 0 {
		close(pause.done)
		pause.joined = true
	}
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		if l.paused == pause {
			l.paused = nil
		}
		l.mu.Unlock()
	}()
	select {
	case <-pause.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	resumeDeferred, err := r.gatewayService.PauseDeferredResponses(ctx)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, resumeDeferred()) }()
	if r.haSelectedState != nil {
		resumeAudit, err := r.pauseAdminAuditBatches(ctx)
		if err != nil {
			return err
		}
		defer func() { resultErr = errors.Join(resultErr, resumeAudit()) }()
		return r.haSelectedState.checkpointHAPhysicalOnline(ctx)
	}
	s := r.selectedState
	if _, err := s.repackDisk.Cost(); err != nil {
		limits := r.config.ResourceBudget
		if limits == nil {
			return authenticatedstate.ErrLimit
		}
		var bytes int64
		for _, ceiling := range [5]int64{limits.LedgerProjectionDiskBytes, limits.MetadataStoreDiskBytes, limits.GovernanceProjectionDiskBytes, limits.UsageProjectionDiskBytes, limits.LedgerSegmentIndexDiskBytes} {
			reservationBytes, sizeErr := authenticatedstate.RepackReservationBytes(ceiling)
			if sizeErr != nil || reservationBytes > math.MaxInt64-bytes {
				return authenticatedstate.ErrLimit
			}
			bytes += reservationBytes
		}
		s.repackDisk, err = s.generation.Owners.Budget.TryAcquireContext(ctx, resourcebudget.Background, resourcebudget.Cost{Disk: bytes})
		if err != nil {
			// Publication can still advance the common cut without replacing any
			// page pack. Capacity refusal must not retire healthy native writers.
			r.logger.Warn("memory page reclamation reservation unavailable", "error", err)
		} else {
			s.fees = append(s.fees, s.repackDisk)
		}
	}
	_, err = s.generation.CheckpointLiveSelectedServicesWithRepack(ctx, func(ctx context.Context) (statecut.StartupCacheCandidates, error) {
		return prepareSelectedCheckpointCaches(ctx, r.config, r.selectedState)
	}, s.repackDisk)
	if errors.Is(err, ownedfile.ErrRetirement) || errors.Is(err, ledger.ErrDirectoryPartialAdvance) || errors.Is(err, authenticatedstate.ErrRecovery) {
		r.status.RequireRecovery()
	}
	return err
}
