package app

import (
	"net/http"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

func matchesLegacyPriceRetry(writer http.ResponseWriter, digest string, exists bool, requested string) bool {
	switch {
	case !exists:
		writePriceError(writer, errPriceWriteRatesRequired)
	case digest != requested:
		writePriceError(writer, boltstore.ErrIdempotencyConflict)
	default:
		return true
	}
	return false
}

// These projections preserve the schema-42 field order and omission rules.
// Idempotency records store a hash of the exact JSON, including nested windows.
type legacyPriceWindowRequest struct {
	StartMinute                 int   `json:"start_minute"`
	EndMinute                   int   `json:"end_minute"`
	InputMicrosPerMillion       int64 `json:"input_micros_per_million"`
	CachedInputMicrosPerMillion int64 `json:"cached_input_micros_per_million"`
	OutputMicrosPerMillion      int64 `json:"output_micros_per_million"`
	FixedRequestMicrosUSD       int64 `json:"fixed_request_micros_usd"`
}

type legacyPriceScheduleRequest struct {
	Timezone string                     `json:"timezone"`
	Windows  []legacyPriceWindowRequest `json:"windows"`
}

func legacyDeploymentPriceRequest(price domain.DeploymentPriceVersion) any {
	var schedule *legacyPriceScheduleRequest
	if price.Schedule != nil {
		schedule = &legacyPriceScheduleRequest{Timezone: price.Schedule.Timezone}
		for _, window := range price.Schedule.Windows {
			schedule.Windows = append(schedule.Windows, legacyPriceWindowRequest{
				StartMinute: window.StartMinute, EndMinute: window.EndMinute,
				InputMicrosPerMillion: window.InputMicrosPerMillion, CachedInputMicrosPerMillion: window.CachedInputMicrosPerMillion,
				OutputMicrosPerMillion: window.OutputMicrosPerMillion, FixedRequestMicrosUSD: window.FixedRequestMicrosUSD,
			})
		}
	}
	return struct {
		ID                          string                      `json:"id"`
		DeploymentID                string                      `json:"deployment_id"`
		Version                     uint64                      `json:"version"`
		BillingMode                 domain.BillingMode          `json:"billing_mode"`
		Currency                    string                      `json:"currency"`
		FormulaVersion              domain.PriceFormulaVersion  `json:"formula_version"`
		InputMicrosPerMillion       int64                       `json:"input_micros_per_million"`
		CachedInputMicrosPerMillion int64                       `json:"cached_input_micros_per_million"`
		OutputMicrosPerMillion      int64                       `json:"output_micros_per_million"`
		FixedRequestMicrosUSD       int64                       `json:"fixed_request_micros_usd"`
		Schedule                    *legacyPriceScheduleRequest `json:"schedule,omitempty"`
		EffectiveFrom               time.Time                   `json:"effective_from"`
		Source                      domain.PriceSource          `json:"source"`
		CreatedBy                   string                      `json:"created_by"`
		CreatedAt                   time.Time                   `json:"created_at"`
		CancelledBy                 string                      `json:"cancelled_by,omitempty"`
		CancelledAt                 *time.Time                  `json:"cancelled_at,omitempty"`
		Revision                    uint64                      `json:"revision"`
	}{
		ID: price.ID, DeploymentID: price.DeploymentID, Version: price.Version,
		BillingMode: price.BillingMode, Currency: price.Currency, FormulaVersion: price.FormulaVersion,
		InputMicrosPerMillion: price.InputMicrosPerMillion, CachedInputMicrosPerMillion: price.CachedInputMicrosPerMillion,
		OutputMicrosPerMillion: price.OutputMicrosPerMillion, FixedRequestMicrosUSD: price.FixedRequestMicrosUSD,
		Schedule: schedule, EffectiveFrom: price.EffectiveFrom, Source: price.Source,
		CreatedBy: price.CreatedBy, CreatedAt: price.CreatedAt, CancelledBy: price.CancelledBy,
		CancelledAt: price.CancelledAt, Revision: price.Revision,
	}
}
