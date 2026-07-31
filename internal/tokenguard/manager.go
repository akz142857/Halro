package tokenguard

import (
	"encoding/json"
	"errors"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/akz142857/Heimdall/internal/domain"
)

type Status string

const (
	StatusNormal             Status = "normal"
	StatusSuspicious         Status = "suspicious"
	StatusTemporarilyBlocked Status = "temporarily_blocked"
)

type Input struct {
	PolicyID               string
	ProjectID              string
	KeyID                  string
	EstimatedTokens        int64
	EstimatedCostMicrosUSD int64
	Concurrency            int64
	SourceHash             [32]byte
	HasSource              bool
	Now                    time.Time
}

type Decision struct {
	Allowed   bool
	Status    Status
	Reason    string
	ExpiresAt time.Time
}

type PreviewWindow struct {
	Requests      int64 `json:"requests"`
	Tokens        int64 `json:"tokens"`
	CostMicrosUSD int64 `json:"cost_micros_usd"`
	Errors        int64 `json:"errors"`
	UniqueIPs     int64 `json:"unique_ips"`
}

type PreviewResult struct {
	Violated bool   `json:"violated"`
	Reason   string `json:"reason,omitempty"`
	Action   string `json:"action"`
}

func Preview(policy domain.TokenGuardPolicy, input Input, window PreviewWindow) (PreviewResult, error) {
	if err := policy.Validate(); err != nil {
		return PreviewResult{}, err
	}
	stats := windowStats{
		requests: window.Requests, tokens: window.Tokens, cost: window.CostMicrosUSD,
		errors: window.Errors, sources: window.UniqueIPs, sourceSet: make(map[[32]byte]struct{}),
	}
	if input.HasSource && window.UniqueIPs > 0 {
		stats.sourceSet[input.SourceHash] = struct{}{}
	}
	reason := violationReason(policy, input, stats)
	return PreviewResult{Violated: reason != "", Reason: reason, Action: policy.Action}, nil
}

type Event struct {
	Type      string    `json:"type"`
	Severity  string    `json:"severity"`
	DedupKey  string    `json:"dedup_key"`
	ProjectID string    `json:"project_id"`
	KeyID     string    `json:"key_id"`
	Reason    string    `json:"reason"`
	Status    Status    `json:"status"`
	Timestamp time.Time `json:"timestamp"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

type bucket struct {
	start            time.Time
	requests         int64
	tokens           int64
	cost             int64
	errors           int64
	sources          map[[32]byte]struct{}
	baselineRequests int64
	baselineTokens   int64
	baselineCost     int64
	baselineTainted  bool
}

type subject struct {
	policyID       string
	policyRevision uint64
	projectID      string
	keyID          string
	status         Status
	violations     int64
	lastViolation  time.Time
	blockedUntil   time.Time
	buckets        map[int64]*bucket
	lastEvent      map[string]time.Time
	baseline       ewmaBaseline
}

type ewmaBaseline struct {
	StartedAt        time.Time `json:"started_at"`
	WindowStart      time.Time `json:"window_start"`
	Samples          int64     `json:"samples"`
	RPM              float64   `json:"rpm"`
	TPM              float64   `json:"tpm"`
	TokensPerRequest float64   `json:"tokens_per_request"`
	CostPerMinute    float64   `json:"cost_per_minute"`
	Initialized      bool      `json:"initialized"`
	FrozenUntil      time.Time `json:"frozen_until,omitempty"`
}

type baselineCheckpoint struct {
	PolicyID       string       `json:"policy_id"`
	PolicyRevision uint64       `json:"policy_revision"`
	ProjectID      string       `json:"project_id"`
	KeyID          string       `json:"key_id"`
	Baseline       ewmaBaseline `json:"baseline"`
}

type managerCheckpoint struct {
	Version  int                  `json:"version"`
	Subjects []baselineCheckpoint `json:"subjects"`
}

const managerCheckpointVersion = 1

type Manager struct {
	mu       sync.Mutex
	policies map[string]domain.TokenGuardPolicy
	subjects map[string]*subject
	events   chan Event
}

func New(policies []domain.TokenGuardPolicy) (*Manager, error) {
	manager := &Manager{
		policies: make(map[string]domain.TokenGuardPolicy),
		subjects: make(map[string]*subject),
		events:   make(chan Event, 256),
	}
	if err := manager.ReplacePolicies(policies); err != nil {
		return nil, err
	}
	return manager, nil
}

func (m *Manager) ReplacePolicies(policies []domain.TokenGuardPolicy) error {
	next := make(map[string]domain.TokenGuardPolicy, len(policies))
	for _, policy := range policies {
		if policy.DeletedAt != nil || !policy.Enabled {
			continue
		}
		if err := policy.Validate(); err != nil {
			return err
		}
		if _, exists := next[policy.ID]; exists {
			return errors.New("duplicate token guard policy id")
		}
		next[policy.ID] = policy
	}
	m.mu.Lock()
	m.policies = next
	for key, current := range m.subjects {
		policy, exists := next[current.policyID]
		if !exists || policy.Revision != current.policyRevision {
			delete(m.subjects, key)
		}
	}
	m.mu.Unlock()
	return nil
}

func (m *Manager) Events() <-chan Event {
	return m.events
}

func (m *Manager) HasPolicy(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.policies[id]
	return ok
}

func (m *Manager) Admit(input Input) Decision {
	if input.Now.IsZero() {
		input.Now = time.Now()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	policy, ok := m.policies[input.PolicyID]
	if !ok {
		return Decision{Allowed: true, Status: StatusNormal}
	}
	key := input.ProjectID + ":" + input.KeyID
	current := m.subjects[key]
	if current == nil || current.policyID != policy.ID || current.policyRevision != policy.Revision {
		current = &subject{
			policyID: policy.ID, policyRevision: policy.Revision,
			projectID: input.ProjectID, keyID: input.KeyID,
			status: StatusNormal, buckets: make(map[int64]*bucket),
			lastEvent: make(map[string]time.Time),
		}
		m.subjects[key] = current
	}
	m.expireBuckets(current, input.Now)
	ewmaReason := m.advanceEWMA(current, policy, input.Now)
	if current.status == StatusTemporarilyBlocked {
		if input.Now.Before(current.blockedUntil) {
			return Decision{
				Allowed: false, Status: current.status, Reason: "temporary_block",
				ExpiresAt: current.blockedUntil,
			}
		}
		current.status = StatusNormal
		current.violations = 0
		current.blockedUntil = time.Time{}
		m.emit(current, policy, input, "token_guard_recovered", "info", "block_expired")
	}
	stats := aggregate(current, input.Now.Add(-time.Minute))
	reason := violationReason(policy, input, stats)
	if reason != "" {
		switch policy.Action {
		case "temporary_block":
			if current.lastViolation.IsZero() || input.Now.Sub(current.lastViolation) > policy.Cooldown {
				current.violations = 1
			} else {
				current.violations++
			}
			current.lastViolation = input.Now
			minimum := policy.MinimumSamples
			if minimum < 2 {
				minimum = 2
			}
			if current.violations >= policy.ViolationsBeforeBlock && stats.requests+1 >= minimum {
				current.status = StatusTemporarilyBlocked
				current.blockedUntil = input.Now.Add(policy.BlockTTL)
				m.emit(current, policy, input, "token_guard_blocked", "critical", reason)
				return Decision{
					Allowed: false, Status: current.status, Reason: reason,
					ExpiresAt: current.blockedUntil,
				}
			}
			current.status = StatusSuspicious
			m.emit(current, policy, input, "token_guard_suspicious", "warning", reason)
		case "alert":
			m.emit(current, policy, input, "token_guard_alert", "warning", reason)
		case "observe":
			m.emit(current, policy, input, "token_guard_observed", "info", reason)
		}
	} else if current.status == StatusSuspicious &&
		!current.lastViolation.IsZero() && input.Now.Sub(current.lastViolation) > policy.Cooldown {
		current.status = StatusNormal
		current.violations = 0
	}
	if reason == "" && ewmaReason != "" {
		current.baseline.FrozenUntil = input.Now.Add(policy.EWMACooldown)
		m.emitWithStatus(current, input, "token_guard_ewma_detected", "warning", ewmaReason, StatusSuspicious, policy.EWMACooldown, current.baseline.FrozenUntil)
		recordAccepted(current, input, false)
		return Decision{Allowed: true, Status: StatusSuspicious, Reason: ewmaReason}
	}
	recordAccepted(current, input, reason == "" && (current.baseline.FrozenUntil.IsZero() || !input.Now.Before(current.baseline.FrozenUntil)))
	return Decision{Allowed: true, Status: current.status, Reason: reason}
}

func (m *Manager) MarshalCheckpoint() ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	items := make([]baselineCheckpoint, 0, len(m.subjects))
	for _, current := range m.subjects {
		policy, ok := m.policies[current.policyID]
		if !ok || !policy.EWMAEnabled || !current.baseline.Initialized {
			continue
		}
		items = append(items, baselineCheckpoint{
			PolicyID: current.policyID, PolicyRevision: current.policyRevision,
			ProjectID: current.projectID, KeyID: current.keyID, Baseline: current.baseline,
		})
	}
	sort.Slice(items, func(left, right int) bool {
		if items[left].ProjectID != items[right].ProjectID {
			return items[left].ProjectID < items[right].ProjectID
		}
		return items[left].KeyID < items[right].KeyID
	})
	return json.Marshal(managerCheckpoint{Version: managerCheckpointVersion, Subjects: items})
}

func (m *Manager) RestoreCheckpoint(payload []byte) error {
	if len(payload) == 0 {
		return nil
	}
	var saved managerCheckpoint
	if err := json.Unmarshal(payload, &saved); err != nil {
		return errors.New("decode Token Guard checkpoint")
	}
	if saved.Version != managerCheckpointVersion || len(saved.Subjects) > 1_000_000 {
		return errors.New("unsupported Token Guard checkpoint")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	next := make(map[string]*subject, len(saved.Subjects))
	for _, item := range saved.Subjects {
		if item.ProjectID == "" || item.KeyID == "" || item.PolicyID == "" || !validBaseline(item.Baseline) {
			return errors.New("invalid Token Guard checkpoint")
		}
		policy, ok := m.policies[item.PolicyID]
		if !ok || !policy.EWMAEnabled || policy.Revision != item.PolicyRevision {
			continue
		}
		if item.Baseline.StartedAt.After(item.Baseline.WindowStart.Add(policy.EWMAEvaluationWindow)) ||
			(!item.Baseline.FrozenUntil.IsZero() &&
				item.Baseline.FrozenUntil.After(item.Baseline.WindowStart.Add(policy.EWMAEvaluationWindow+policy.EWMACooldown))) {
			return errors.New("invalid Token Guard checkpoint timeline")
		}
		key := item.ProjectID + ":" + item.KeyID
		if _, duplicate := next[key]; duplicate {
			return errors.New("duplicate Token Guard checkpoint subject")
		}
		next[key] = &subject{
			policyID: item.PolicyID, policyRevision: item.PolicyRevision,
			projectID: item.ProjectID, keyID: item.KeyID,
			status: StatusNormal, buckets: make(map[int64]*bucket),
			lastEvent: make(map[string]time.Time), baseline: item.Baseline,
		}
	}
	for key, current := range next {
		m.subjects[key] = current
	}
	return nil
}

type ewmaWindow struct {
	requests         int64
	rpm              float64
	tpm              float64
	tokensPerRequest float64
	costPerMinute    float64
	tainted          bool
}

func (m *Manager) advanceEWMA(current *subject, policy domain.TokenGuardPolicy, now time.Time) string {
	if !policy.EWMAEnabled {
		return ""
	}
	window := policy.EWMAEvaluationWindow
	currentStart := now.Truncate(window)
	baseline := &current.baseline
	if baseline.WindowStart.IsZero() {
		baseline.WindowStart = currentStart
		baseline.StartedAt = now
		return ""
	}
	// Buckets retain one hour. After a longer idle/restart gap there is no
	// trustworthy suffix to evaluate, while the persisted baseline remains safe.
	if baseline.WindowStart.Before(currentStart.Add(-time.Hour)) {
		baseline.WindowStart = currentStart
		return ""
	}
	var detected string
	for start := baseline.WindowStart; start.Before(currentStart); start = start.Add(window) {
		metrics := ewmaWindowFor(current, start, start.Add(window), window)
		if metrics.requests == 0 {
			continue
		}
		if metrics.tainted {
			continue
		}
		reason := ewmaViolationReason(policy, *baseline, metrics, start.Add(window))
		if reason != "" {
			if detected == "" {
				detected = reason
			}
			continue // freeze the baseline for anomalous windows
		}
		updateEWMA(baseline, policy.EWMAAlpha, metrics)
	}
	baseline.WindowStart = currentStart
	return detected
}

func ewmaWindowFor(current *subject, start, end time.Time, duration time.Duration) ewmaWindow {
	var requests, tokens, cost int64
	result := ewmaWindow{}
	for _, value := range current.buckets {
		if value.start.Before(start) || !value.start.Before(end) {
			continue
		}
		requests = saturatingAdd(requests, value.baselineRequests)
		tokens = saturatingAdd(tokens, value.baselineTokens)
		cost = saturatingAdd(cost, value.baselineCost)
		if value.baselineTainted {
			result.tainted = true
		}
	}
	perMinute := float64(time.Minute) / float64(duration)
	result.requests = requests
	result.rpm = float64(requests) * perMinute
	result.tpm = float64(tokens) * perMinute
	result.costPerMinute = float64(cost) * perMinute
	if requests > 0 {
		result.tokensPerRequest = float64(tokens) / float64(requests)
	}
	return result
}

func ewmaViolationReason(policy domain.TokenGuardPolicy, baseline ewmaBaseline, current ewmaWindow, endedAt time.Time) string {
	if !baseline.Initialized || baseline.Samples < policy.EWMAMinimumSamples || endedAt.Sub(baseline.StartedAt) < policy.EWMAWarmup {
		return ""
	}
	multiplier := policy.EWMAMultiplier
	switch {
	case policy.EWMAAbsoluteRPM > 0 && current.rpm > float64(policy.EWMAAbsoluteRPM) && current.rpm > baseline.RPM*multiplier:
		return "ewma_rpm"
	case policy.EWMAAbsoluteTPM > 0 && current.tpm > float64(policy.EWMAAbsoluteTPM) && current.tpm > baseline.TPM*multiplier:
		return "ewma_tpm"
	case policy.EWMAAbsoluteTokensPerRequest > 0 && current.tokensPerRequest > policy.EWMAAbsoluteTokensPerRequest && current.tokensPerRequest > baseline.TokensPerRequest*multiplier:
		return "ewma_tokens_per_request"
	case policy.EWMAAbsoluteCostMicrosPerMinute > 0 && current.costPerMinute > float64(policy.EWMAAbsoluteCostMicrosPerMinute) && current.costPerMinute > baseline.CostPerMinute*multiplier:
		return "ewma_cost_rate"
	default:
		return ""
	}
}

func updateEWMA(baseline *ewmaBaseline, alpha float64, current ewmaWindow) {
	if !baseline.Initialized {
		baseline.RPM = current.rpm
		baseline.TPM = current.tpm
		baseline.TokensPerRequest = current.tokensPerRequest
		baseline.CostPerMinute = current.costPerMinute
		baseline.Initialized = true
	} else {
		baseline.RPM = alpha*current.rpm + (1-alpha)*baseline.RPM
		baseline.TPM = alpha*current.tpm + (1-alpha)*baseline.TPM
		baseline.TokensPerRequest = alpha*current.tokensPerRequest + (1-alpha)*baseline.TokensPerRequest
		baseline.CostPerMinute = alpha*current.costPerMinute + (1-alpha)*baseline.CostPerMinute
	}
	baseline.Samples = saturatingAdd(baseline.Samples, current.requests)
}

func validBaseline(value ewmaBaseline) bool {
	if !value.Initialized || value.StartedAt.IsZero() || value.WindowStart.IsZero() || value.Samples < 0 {
		return false
	}
	for _, metric := range []float64{value.RPM, value.TPM, value.TokensPerRequest, value.CostPerMinute} {
		if metric < 0 || math.IsNaN(metric) || math.IsInf(metric, 0) {
			return false
		}
	}
	return true
}

func (m *Manager) Complete(policyID, projectID, keyID string, now time.Time, failed bool) {
	if !failed {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.policies[policyID]; !exists {
		return
	}
	current := m.subjects[projectID+":"+keyID]
	if current == nil {
		return
	}
	start := now.Truncate(10 * time.Second)
	value := current.buckets[start.Unix()]
	if value == nil {
		value = &bucket{start: start, sources: make(map[[32]byte]struct{})}
		current.buckets[start.Unix()] = value
	}
	value.errors = saturatingAdd(value.errors, 1)
}

func (m *Manager) Unblock(projectID, keyID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if current := m.subjects[projectID+":"+keyID]; current != nil {
		current.status = StatusNormal
		current.violations = 0
		current.blockedUntil = time.Time{}
	}
}

func (m *Manager) UnblockProject(projectID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	count := 0
	for _, current := range m.subjects {
		if current.projectID != projectID || current.status == StatusNormal {
			continue
		}
		current.status = StatusNormal
		current.violations = 0
		current.blockedUntil = time.Time{}
		count++
	}
	return count
}

func (m *Manager) expireBuckets(current *subject, now time.Time) {
	cutoff := now.Add(-time.Hour).Unix()
	for key, value := range current.buckets {
		if value.start.Unix() < cutoff {
			delete(current.buckets, key)
		}
	}
}

func recordAccepted(current *subject, input Input, baselineEligible bool) {
	start := input.Now.Truncate(10 * time.Second)
	value := current.buckets[start.Unix()]
	if value == nil {
		value = &bucket{start: start, sources: make(map[[32]byte]struct{})}
		current.buckets[start.Unix()] = value
	}
	value.requests = saturatingAdd(value.requests, 1)
	value.tokens = saturatingAdd(value.tokens, input.EstimatedTokens)
	value.cost = saturatingAdd(value.cost, input.EstimatedCostMicrosUSD)
	if baselineEligible {
		value.baselineRequests = saturatingAdd(value.baselineRequests, 1)
		value.baselineTokens = saturatingAdd(value.baselineTokens, input.EstimatedTokens)
		value.baselineCost = saturatingAdd(value.baselineCost, input.EstimatedCostMicrosUSD)
	} else {
		value.baselineTainted = true
	}
	if input.HasSource {
		value.sources[input.SourceHash] = struct{}{}
	}
}

type windowStats struct {
	requests  int64
	tokens    int64
	cost      int64
	errors    int64
	sources   int64
	sourceSet map[[32]byte]struct{}
}

func aggregate(current *subject, cutoff time.Time) windowStats {
	var result windowStats
	sources := make(map[[32]byte]struct{})
	for _, value := range current.buckets {
		if value.start.Before(cutoff) {
			continue
		}
		result.requests = saturatingAdd(result.requests, value.requests)
		result.tokens = saturatingAdd(result.tokens, value.tokens)
		result.cost = saturatingAdd(result.cost, value.cost)
		result.errors = saturatingAdd(result.errors, value.errors)
		for source := range value.sources {
			sources[source] = struct{}{}
		}
	}
	result.sources = int64(len(sources))
	result.sourceSet = sources
	return result
}

func violationReason(policy domain.TokenGuardPolicy, input Input, stats windowStats) string {
	switch {
	case policy.RequestTokens > 0 && input.EstimatedTokens > policy.RequestTokens:
		return "request_tokens"
	case policy.TokensPerMinute > 0 && stats.tokens+input.EstimatedTokens > policy.TokensPerMinute:
		return "tokens_per_minute"
	case policy.CostMicrosPerMinute > 0 && stats.cost+input.EstimatedCostMicrosUSD > policy.CostMicrosPerMinute:
		return "cost_per_minute"
	case policy.Concurrency > 0 && input.Concurrency > policy.Concurrency:
		return "concurrency"
	case policy.UniqueIPsPerMinute > 0 && input.HasSource &&
		uniqueSourceCount(stats, input.SourceHash) > policy.UniqueIPsPerMinute:
		return "unique_ip"
	case policy.ErrorRate > 0 && stats.requests >= policy.MinimumSamples &&
		stats.requests > 0 && float64(stats.errors)/float64(stats.requests) > policy.ErrorRate:
		return "error_rate"
	default:
		return ""
	}
}

func uniqueSourceCount(stats windowStats, source [32]byte) int64 {
	if _, exists := stats.sourceSet[source]; exists {
		return stats.sources
	}
	return saturatingAdd(stats.sources, 1)
}

func saturatingAdd(left, right int64) int64 {
	if right > 0 && left > math.MaxInt64-right {
		return math.MaxInt64
	}
	if right < 0 && left < math.MinInt64-right {
		return math.MinInt64
	}
	return left + right
}

func (m *Manager) emit(
	current *subject,
	policy domain.TokenGuardPolicy,
	input Input,
	eventType, severity, reason string,
) {
	m.emitWithStatus(current, input, eventType, severity, reason, current.status, policy.Cooldown, current.blockedUntil)
}

func (m *Manager) emitWithStatus(
	current *subject,
	input Input,
	eventType, severity, reason string,
	status Status,
	cooldown time.Duration,
	expiresAt time.Time,
) {
	dedupKey := input.ProjectID + ":" + input.KeyID + ":" + reason + ":" + eventType
	if last := current.lastEvent[dedupKey]; !last.IsZero() && input.Now.Sub(last) < cooldown {
		return
	}
	current.lastEvent[dedupKey] = input.Now
	event := Event{
		Type: eventType, Severity: severity, DedupKey: dedupKey,
		ProjectID: input.ProjectID, KeyID: input.KeyID, Reason: reason,
		Status: status, Timestamp: input.Now, ExpiresAt: expiresAt,
	}
	select {
	case m.events <- event:
	default:
	}
}
