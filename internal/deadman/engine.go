package deadman

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"
)

type Checker func(context.Context, Config, TargetConfig) (time.Duration, string)

type Engine struct {
	// tickMu serializes whole ticks; mu guards the state a tick and the delivery
	// worker share. Keeping them apart is the point: a probe is a network call
	// with a timeout, and holding the state lock across it stopped the delivery
	// worker from sending the heartbeat that says this probe is alive.
	tickMu        sync.Mutex
	mu            sync.Mutex
	cfg           Config
	state         persistedState
	check         Checker
	now           func() time.Time
	audit         auditSink
	anchor        anchorSink
	logger        *slog.Logger
	notify        *http.Client
	targetClients map[string]*http.Client
	wake          chan struct{}
	inflight      string
}

func New(cfg Config, logger *slog.Logger) (*Engine, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	state, err := loadState(cfg.StateFile, cfg.OutboxLimit)
	if err != nil {
		return nil, err
	}
	notificationClient, err := secureClient(cfg.Notification.TLS, cfg.Notification.Timeout.Value())
	if err != nil {
		return nil, fmt.Errorf("notification client: %w", err)
	}
	targetClients := make(map[string]*http.Client, len(cfg.Targets))
	for _, target := range cfg.Targets {
		client, err := secureClient(target.TLS, cfg.Timeout.Value())
		if err != nil {
			notificationClient.CloseIdleConnections()
			for _, initialized := range targetClients {
				initialized.CloseIdleConnections()
			}
			return nil, fmt.Errorf("target %q client: %w", target.ID, err)
		}
		targetClients[target.ID] = client
	}
	engine := &Engine{
		cfg:           cfg,
		state:         state,
		now:           func() time.Time { return time.Now().UTC() },
		audit:         &auditWriter{path: cfg.AuditFile},
		anchor:        &anchorWriter{path: cfg.AnchorFile},
		logger:        logger,
		notify:        notificationClient,
		targetClients: targetClients,
		wake:          make(chan struct{}, 1),
	}
	engine.check = engine.checkTarget
	return engine, nil
}

func (e *Engine) Run(ctx context.Context) error {
	defer e.closeIdleConnections()
	if err := e.audit.append(auditRecord{OccurredAt: e.now(), Action: "deadman.start", Outcome: "success"}); err != nil {
		return fmt.Errorf("audit startup: %w", err)
	}
	workerCtx, cancelWorker := context.WithCancel(ctx)
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		e.deliveryLoop(workerCtx)
	}()
	defer func() { cancelWorker(); workers.Wait() }()
	if err := e.Tick(ctx); err != nil {
		return err
	}
	ticker := time.NewTicker(e.cfg.Interval.Value())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := e.Tick(ctx); err != nil {
				e.logger.Error("dead-man tick failed", "error", err)
			}
		}
	}
}

func (e *Engine) checkTarget(ctx context.Context, cfg Config, target TargetConfig) (time.Duration, string) {
	return checkTargetWithClient(ctx, cfg, target, e.targetClients[target.ID])
}

func (e *Engine) closeIdleConnections() {
	if e.notify != nil {
		e.notify.CloseIdleConnections()
	}
	for _, client := range e.targetClients {
		client.CloseIdleConnections()
	}
}

type probeResult struct {
	latency time.Duration
	reason  string
}

func (e *Engine) Tick(ctx context.Context) error {
	// Serialized against other ticks, but not against delivery: the heartbeat is
	// queued and released immediately so the worker can send it while the probes
	// below are still waiting on the network.
	e.tickMu.Lock()
	defer e.tickMu.Unlock()
	now := e.now()
	e.mu.Lock()
	if err := e.enqueue("heartbeat", TargetConfig{}, "", "", 0, now); err != nil {
		e.logger.Error("dead-man heartbeat was not queued", "error", err)
	}
	e.mu.Unlock()
	select {
	case e.wake <- struct{}{}:
	default:
	}
	results := e.probeTargets(ctx)
	e.pullAnchors(ctx)
	e.mu.Lock()
	defer e.mu.Unlock()
	for index, target := range e.cfg.Targets {
		latency, reason := results[index].latency, results[index].reason
		success := reason == ""
		previous := e.state.Targets[target.ID]
		next, transition := transition(previous, success, reason, now, e.cfg.FailuresToDown, e.cfg.SuccessesToUp)
		outcome := "success"
		if !success {
			outcome = "failure"
		}
		if err := e.audit.append(auditRecord{OccurredAt: now, Action: "deadman.probe", Outcome: outcome, TargetID: target.ID, ReasonCode: reason}); err != nil {
			return err
		}
		if transition {
			if err := e.enqueue("state_transition", target, next.Phase, next.LastReason, latency, now); err != nil {
				e.logger.Error("dead-man transition was not queued; retaining prior state", "target_id", target.ID, "error", err)
				continue
			}
		}
		e.state.Targets[target.ID] = next
	}
	if err := saveState(e.cfg.StateFile, e.state); err != nil {
		return fmt.Errorf("persist probe results: %w", err)
	}
	select {
	case e.wake <- struct{}{}:
	default:
	}
	return nil
}

// probeTargets runs every configured probe without holding any lock. The config
// and the checker are fixed once the engine is built, so nothing here needs one.
func (e *Engine) probeTargets(ctx context.Context) []probeResult {
	results := make([]probeResult, len(e.cfg.Targets))
	var probes sync.WaitGroup
	for index, target := range e.cfg.Targets {
		probes.Add(1)
		go func() {
			defer probes.Done()
			results[index].latency, results[index].reason = e.check(ctx, e.cfg, target)
		}()
	}
	probes.Wait()
	return results
}

func transition(state TargetState, success bool, reason string, now time.Time, failures, successes int) (TargetState, bool) {
	stable := state.StablePhase
	if stable == "" && (state.Phase == PhaseUp || state.Phase == PhaseDown) {
		stable = state.Phase
	}
	state.LastCheckedAt = now
	state.LastReason = reason
	if success {
		state.ConsecutiveFailures = 0
		state.ConsecutiveSuccesses++
		if state.ConsecutiveSuccesses >= successes {
			state.Phase = PhaseUp
			changed := stable == PhaseDown
			state.StablePhase = PhaseUp
			return state, changed
		} else {
			state.Phase = PhasePendingUp
		}
	} else {
		state.ConsecutiveSuccesses = 0
		state.ConsecutiveFailures++
		if state.ConsecutiveFailures >= failures {
			state.Phase = PhaseDown
			changed := stable != PhaseDown
			state.StablePhase = PhaseDown
			state.EverDown = true
			return state, changed
		} else {
			state.Phase = PhasePendingDown
		}
	}
	state.StablePhase = stable
	return state, false
}

func (e *Engine) enqueue(kind string, target TargetConfig, state Phase, reason string, latency time.Duration, now time.Time) error {
	if kind == "heartbeat" {
		// Only the newest observed heartbeat can extend the receiver deadline.
		// Replayed stale heartbeats are actively harmful after an outage.
		filtered := e.state.Outbox[:0]
		for _, queued := range e.state.Outbox {
			if queued.Event.Kind != "heartbeat" || queued.Event.EventID == e.inflight {
				filtered = append(filtered, queued)
			}
		}
		e.state.Outbox = filtered
	}
	if len(e.state.Outbox) >= e.cfg.OutboxLimit {
		return fmt.Errorf("persistent outbox reached its limit of %d", e.cfg.OutboxLimit)
	}
	e.state.Sequence++
	event := Event{
		SchemaVersion: "heimdall.deadman.event/v1",
		EventID:       fmt.Sprintf("%s-%020d", e.cfg.ProbeID, e.state.Sequence),
		Sequence:      e.state.Sequence,
		ProbeID:       e.cfg.ProbeID,
		Environment:   e.cfg.Environment,
		Region:        e.cfg.Region,
		Cluster:       e.cfg.Cluster,
		Kind:          kind,
		TargetID:      target.ID,
		TargetKind:    target.Kind,
		State:         state,
		ObservedAt:    now,
		HeartbeatTTL:  e.cfg.HeartbeatTTL.Value().String(),
		ReasonCode:    reason,
		LatencyMS:     latency.Milliseconds(),
	}
	e.state.Outbox = append(e.state.Outbox, queuedEvent{Event: event, NextAttempt: now})
	return nil
}

func (e *Engine) deliveryLoop(ctx context.Context) {
	period := min(e.cfg.Interval.Value(), e.cfg.Notification.RetryMin.Value())
	ticker := time.NewTicker(period)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-e.wake:
		case <-ticker.C:
		}
		for count := 0; count < e.cfg.DeliveryBatch; count++ {
			progressed, err := e.drainOne(ctx)
			if err != nil {
				e.logger.Error("dead-man delivery worker failed", "error", err)
				break
			}
			if !progressed {
				break
			}
		}
	}
}

// drainOne preserves FIFO ordering: no later sequence is delivered until the
// oldest queued event is durably acknowledged and its local audit is synced.
func (e *Engine) drainOne(ctx context.Context) (bool, error) {
	e.mu.Lock()
	if len(e.state.Outbox) == 0 || e.state.Outbox[0].NextAttempt.After(e.now()) {
		e.mu.Unlock()
		return false, nil
	}
	queued := e.state.Outbox[0]
	queued.Event.Attempt++
	e.inflight = queued.Event.EventID
	e.mu.Unlock()

	now := e.now()
	deliveryErr := e.send(ctx, queued.Event)
	outcome, reasonCode := "success", ""
	if deliveryErr != nil {
		outcome, reasonCode = "failure", "delivery_failed"
	}
	auditErr := e.audit.append(auditRecord{OccurredAt: now, Action: "deadman.notify", Outcome: outcome, EventID: queued.Event.EventID, Sequence: queued.Event.Sequence, ReasonCode: reasonCode})
	e.mu.Lock()
	defer e.mu.Unlock()
	e.inflight = ""
	// In-flight events are protected from heartbeat coalescing. A changed head
	// therefore indicates state corruption or an unsupported concurrent writer.
	if len(e.state.Outbox) == 0 || e.state.Outbox[0].Event.EventID != queued.Event.EventID {
		return false, fmt.Errorf("FIFO head changed while delivering %s", queued.Event.EventID)
	}
	if deliveryErr != nil {
		queued.NextAttempt = now.Add(e.retryDelay(queued.Event.Attempt))
		e.state.Outbox[0] = queued
		if auditErr != nil {
			e.logger.Error("dead-man notification audit failed; event retained", "event_id", queued.Event.EventID, "error", auditErr)
		}
		return false, saveState(e.cfg.StateFile, e.state)
	}
	if auditErr != nil {
		queued.NextAttempt = now.Add(e.retryDelay(queued.Event.Attempt))
		e.state.Outbox[0] = queued
		if saveErr := saveState(e.cfg.StateFile, e.state); saveErr != nil {
			return false, fmt.Errorf("audit failed: %v; persist retained event: %w", auditErr, saveErr)
		}
		return false, fmt.Errorf("notification was acknowledged but audit failed; event retained: %w", auditErr)
	}
	e.state.Outbox = e.state.Outbox[1:]
	return true, saveState(e.cfg.StateFile, e.state)
}

func (e *Engine) retryDelay(attempt int) time.Duration {
	maximum := e.cfg.Notification.RetryMax.Value()
	delay := e.cfg.Notification.RetryMin.Value()
	for count := 1; count < attempt && delay < maximum/2; count++ {
		delay *= 2
	}
	if delay > maximum {
		delay = maximum
	}
	maximumJitter := min(delay/5, maximum-delay)
	if maximumJitter <= 0 {
		return delay
	}
	return delay + time.Duration(rand.Int64N(int64(maximumJitter)+1))
}

func (e *Engine) send(ctx context.Context, event Event) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	response, err := request(ctx, e.notify, http.MethodPost, e.cfg.Notification.URL, e.cfg.Notification.BearerTokenFile, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("notification returned status %d", response.StatusCode)
	}
	return nil
}
