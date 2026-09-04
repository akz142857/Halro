package domain

import (
	"errors"
	"math"
	"slices"
	"strings"
	"time"
)

const MaxGovernanceIDLength = 64

func ValidRunID(value string) bool      { return validGovernanceID(value, "run_") }
func ValidWorkUnitID(value string) bool { return validGovernanceID(value, "wku_") }

func validGovernanceID(value, prefix string) bool {
	if len(value) <= len(prefix) || len(value) > MaxGovernanceIDLength || !strings.HasPrefix(value, prefix) {
		return false
	}
	for _, character := range value[len(prefix):] {
		if character >= '0' && character <= '9' || character >= 'a' && character <= 'z' {
			continue
		}
		return false
	}
	return true
}

type GatewayScope string

const (
	GatewayScopeInference      GatewayScope = "inference"
	GatewayScopeWorkUnitCreate GatewayScope = "work_unit:create"
	GatewayScopeRunCreate      GatewayScope = "run:create"
	GatewayScopeRunAttach      GatewayScope = "run:attach"
	GatewayScopeGovernanceRead GatewayScope = "governance:read"
	GatewayScopeOutcomeWrite   GatewayScope = "outcome:write"
)

var gatewayScopes = []GatewayScope{
	GatewayScopeInference,
	GatewayScopeWorkUnitCreate,
	GatewayScopeRunCreate,
	GatewayScopeRunAttach,
	GatewayScopeGovernanceRead,
	GatewayScopeOutcomeWrite,
}

func ValidGatewayScope(scope GatewayScope) bool { return slices.Contains(gatewayScopes, scope) }

func EffectiveGatewayScopes(scopes []GatewayScope) []GatewayScope {
	if len(scopes) == 0 {
		return []GatewayScope{GatewayScopeInference}
	}
	return slices.Clone(scopes)
}

func HasGatewayScope(scopes []GatewayScope, scope GatewayScope) bool {
	return slices.Contains(EffectiveGatewayScopes(scopes), scope)
}

type RunGovernanceConfig struct {
	Enabled                   bool  `json:"enabled"`
	DefaultRunBudgetMicrosUSD int64 `json:"default_run_budget_micros_usd"`
	MaxRunBudgetMicrosUSD     int64 `json:"max_run_budget_micros_usd"`
	DefaultRunTTLSeconds      int64 `json:"default_run_ttl_seconds"`
	MaxRunTTLSeconds          int64 `json:"max_run_ttl_seconds"`
	MaxActiveRuns             int64 `json:"max_active_runs"`
	MaxOpenWorkUnits          int64 `json:"max_open_work_units"`
}

const (
	DefaultRunTTLSeconds = int64((24 * time.Hour) / time.Second)
	MaxRunTTLSeconds     = int64((30 * 24 * time.Hour) / time.Second)
	MaxActiveRuns        = int64(1_000)
	MaxOpenWorkUnits     = int64(1_000)
	MaxRunsPerWorkUnit   = 32
)

func (c RunGovernanceConfig) Validate() error {
	if !c.Enabled {
		if c != (RunGovernanceConfig{}) {
			return errors.New("disabled run governance fields must be zero")
		}
		return nil
	}
	if c.DefaultRunBudgetMicrosUSD <= 0 || c.MaxRunBudgetMicrosUSD <= 0 ||
		c.DefaultRunBudgetMicrosUSD > c.MaxRunBudgetMicrosUSD {
		return errors.New("run governance budget defaults are invalid")
	}
	if c.DefaultRunTTLSeconds <= 0 || c.MaxRunTTLSeconds <= 0 ||
		c.DefaultRunTTLSeconds > c.MaxRunTTLSeconds || c.MaxRunTTLSeconds > MaxRunTTLSeconds {
		return errors.New("run governance TTL is invalid")
	}
	if c.MaxActiveRuns <= 0 || c.MaxActiveRuns > MaxActiveRuns ||
		c.MaxOpenWorkUnits <= 0 || c.MaxOpenWorkUnits > MaxOpenWorkUnits {
		return errors.New("run governance active limits are invalid")
	}
	return nil
}

type WorkUnitStatus string

const (
	WorkUnitOpen   WorkUnitStatus = "open"
	WorkUnitClosed WorkUnitStatus = "closed"
)

type WorkUnit struct {
	ID                    string                 `json:"id"`
	ProjectID             string                 `json:"project_id"`
	Status                WorkUnitStatus         `json:"status"`
	CreatedByKeyID        string                 `json:"created_by_key_id"`
	CreatedAt             time.Time              `json:"created_at"`
	ClosedAt              *time.Time             `json:"closed_at,omitempty"`
	PeriodID              string                 `json:"period_id"`
	PeriodTimezoneVersion uint64                 `json:"period_timezone_version"`
	OutcomeDefinitions    []OutcomeDefinitionRef `json:"outcome_definitions,omitempty"`
}

func (w WorkUnit) Validate() error {
	if !ValidWorkUnitID(w.ID) || w.ProjectID == "" || w.CreatedByKeyID == "" ||
		w.CreatedAt.IsZero() || w.PeriodID == "" {
		return errors.New("work unit identity is invalid")
	}
	switch w.Status {
	case WorkUnitOpen:
		if w.ClosedAt != nil {
			return errors.New("open work unit cannot have closed_at")
		}
	case WorkUnitClosed:
		if w.ClosedAt == nil || w.ClosedAt.IsZero() {
			return errors.New("closed work unit requires closed_at")
		}
	default:
		return errors.New("work unit status is invalid")
	}
	if len(w.OutcomeDefinitions) > MaxDefinitionsPerWorkUnit {
		return errors.New("work unit outcome definition limit exceeded")
	}
	seenDefinitions := map[string]struct{}{}
	for _, reference := range w.OutcomeDefinitions {
		if !ValidOutcomeDefinitionID(reference.ID) || reference.Version == 0 {
			return errors.New("work unit outcome definition is invalid")
		}
		if _, exists := seenDefinitions[reference.ID]; exists {
			return errors.New("work unit outcome definitions must be unique")
		}
		seenDefinitions[reference.ID] = struct{}{}
	}
	return nil
}

type RunStatus string

type RunBudgetState string

const (
	RunActive  RunStatus = "active"
	RunClosed  RunStatus = "closed"
	RunExpired RunStatus = "expired"

	RunBudgetAvailable     RunBudgetState = "available"
	RunBudgetFullyReserved RunBudgetState = "fully_reserved"
	RunBudgetDepleted      RunBudgetState = "depleted"
)

// EffectiveRunStatus derives expiry from the reader's clock without inventing
// a lifecycle event. The Ledger keeps the explicit active/closed transition;
// every API still reports an elapsed active Run as expired.
func EffectiveRunStatus(run Run, now time.Time) RunStatus {
	if run.Status == RunActive && !now.Before(run.ExpiresAt) {
		return RunExpired
	}
	return run.Status
}

type Run struct {
	ID                 string         `json:"id"`
	ProjectID          string         `json:"project_id"`
	WorkUnitID         string         `json:"work_unit_id"`
	BudgetMicrosUSD    int64          `json:"budget_micros_usd"`
	CommittedMicrosUSD int64          `json:"committed_micros_usd"`
	ReservedMicrosUSD  int64          `json:"reserved_micros_usd"`
	RemainingMicrosUSD int64          `json:"remaining_micros_usd"`
	BudgetState        RunBudgetState `json:"budget_state"`
	UnknownAttempts    int64          `json:"unknown_attempts"`
	Status             RunStatus      `json:"status"`
	CreatedByKeyID     string         `json:"created_by_key_id"`
	CreatedAt          time.Time      `json:"created_at"`
	ExpiresAt          time.Time      `json:"expires_at"`
	ClosedAt           *time.Time     `json:"closed_at,omitempty"`
	CloseReason        string         `json:"close_reason,omitempty"`
}

// WithRunBudgetState adds the live, non-authoritative budget projection used by
// the public and Admin APIs. pendingMicrosUSD is admitted spend whose durable
// reservation has not reached the Ledger state yet. It is never persisted in a
// Run event and therefore cannot become a second accounting authority.
func WithRunBudgetState(run Run, pendingMicrosUSD int64) Run {
	if pendingMicrosUSD < 0 {
		pendingMicrosUSD = 0
	}
	durable := int64(math.MaxInt64)
	if run.CommittedMicrosUSD <= math.MaxInt64-run.ReservedMicrosUSD {
		durable = run.CommittedMicrosUSD + run.ReservedMicrosUSD
	}
	used := int64(math.MaxInt64)
	if durable <= math.MaxInt64-pendingMicrosUSD {
		used = durable + pendingMicrosUSD
	}
	if used >= run.BudgetMicrosUSD {
		run.RemainingMicrosUSD = 0
	} else {
		run.RemainingMicrosUSD = run.BudgetMicrosUSD - used
	}
	switch {
	case durable >= run.BudgetMicrosUSD:
		run.BudgetState = RunBudgetDepleted
	case used >= run.BudgetMicrosUSD:
		run.BudgetState = RunBudgetFullyReserved
	default:
		run.BudgetState = RunBudgetAvailable
	}
	return run
}

func (r Run) Validate() error {
	if !ValidRunID(r.ID) || r.ProjectID == "" || !ValidWorkUnitID(r.WorkUnitID) ||
		r.BudgetMicrosUSD <= 0 || r.CommittedMicrosUSD < 0 || r.ReservedMicrosUSD < 0 || r.UnknownAttempts < 0 ||
		r.CreatedByKeyID == "" || r.CreatedAt.IsZero() ||
		r.ExpiresAt.IsZero() || !r.ExpiresAt.After(r.CreatedAt) {
		return errors.New("run identity or bounds are invalid")
	}
	switch r.Status {
	case RunActive:
		if r.ClosedAt != nil || r.CloseReason != "" {
			return errors.New("active run cannot carry close fields")
		}
	case RunClosed:
		if r.ClosedAt == nil || r.ClosedAt.IsZero() || strings.TrimSpace(r.CloseReason) == "" || len(r.CloseReason) > 64 {
			return errors.New("closed run requires a bounded close reason")
		}
	default:
		return errors.New("run status is invalid")
	}
	return nil
}
