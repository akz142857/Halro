package app

import (
	"net/http"
	"time"
)

// timeContext travels with every response that reports a figure scoped to a
// day.
//
// "Today" is a server-side judgement: it depends on the accounting timezone,
// which the browser has no way to know. Without this the console fell back to
// the viewer's own zone for chart axes while the totals beside them were
// summed over the server's day, so the two disagreed by the offset between
// them. Clients render against these bounds and never derive a day boundary of
// their own.
type timeContext struct {
	AccountingTimezone string    `json:"accounting_timezone"`
	TimezoneVersion    uint64    `json:"timezone_version"`
	PeriodID           string    `json:"period_id"`
	PeriodStart        time.Time `json:"period_start"`
	PeriodEnd          time.Time `json:"period_end"`
	// PendingTimezone and PendingEffectiveAt describe a scheduled change that
	// has not taken effect yet. Absent when none is scheduled.
	PendingTimezone    string     `json:"pending_timezone,omitempty"`
	PendingEffectiveAt *time.Time `json:"pending_effective_at,omitempty"`
	GeneratedAt        time.Time  `json:"generated_at"`
}

// timeContextAt describes the accounting period containing now.
func (r *Runtime) timeContextAt(now time.Time) (timeContext, error) {
	period, err := r.periods.PeriodAt(now)
	if err != nil {
		return timeContext{}, err
	}
	context := timeContext{
		AccountingTimezone: period.Timezone,
		TimezoneVersion:    period.TimezoneVersion,
		PeriodID:           period.ID,
		PeriodStart:        period.Start,
		PeriodEnd:          period.End,
		GeneratedAt:        now.UTC(),
	}
	// Surfaced so the console can warn before the boundary moves rather than
	// after an administrator notices a day changed length.
	if settings := r.periods.Settings(); settings.HasPendingChange() && now.Before(*settings.PendingEffectiveAt) {
		context.PendingTimezone = settings.PendingTimezone
		context.PendingEffectiveAt = settings.PendingEffectiveAt
	}
	return context, nil
}

// writeTimeContext resolves the context or fails the request. An unusable
// accounting period is not a partial answer to degrade past: every daily figure
// in the response would be scoped to a window nobody can reproduce.
func (r *Runtime) writeTimeContext(writer http.ResponseWriter, now time.Time) (timeContext, bool) {
	context, err := r.timeContextAt(now)
	if err != nil {
		r.logger.Error("resolve accounting period", "error", err)
		writeJSON(writer, http.StatusInternalServerError, map[string]string{"error": "accounting period is unavailable"})
		return timeContext{}, false
	}
	return context, true
}
