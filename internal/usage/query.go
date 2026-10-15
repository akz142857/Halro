package usage

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"sort"
	"time"

	bbolt "go.etcd.io/bbolt"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/provider"
)

type AttemptQuery struct {
	BeforeSequence        uint64
	Limit                 int
	ProjectID             string
	WorkUnitID            string
	RunID                 string
	ProviderID            string
	OfferingID            domain.ProviderOfferingID
	AccountRegionID       domain.ProviderRegionID
	ProviderFailureReason provider.FailureReason
	DeploymentID          string
	RequestID             string
	RequestedModel        string
	ProviderModel         string
	Status                string
	Start                 time.Time
	End                   time.Time
}

type AttemptPage struct {
	Attempts   []AttemptEvent
	NextCursor string
}

type RequestDetail struct {
	Summary  RequestSummary `json:"summary"`
	Attempts []AttemptEvent `json:"attempts"`
}

type Dashboard struct {
	Today           Bucket                            `json:"today"`
	Hourly          []Bucket                          `json:"hourly"`
	Active          uint64                            `json:"active_requests"`
	Watermark       uint64                            `json:"watermark_sequence"`
	Breakdowns      map[string]map[string][]Breakdown `json:"breakdowns"`
	RecentAnomalies []Anomaly                         `json:"recent_anomalies"`
}

type Breakdown struct {
	Key                 string `json:"key"`
	Calls               int64  `json:"calls"`
	InputTokens         int64  `json:"input_tokens"`
	OutputTokens        int64  `json:"output_tokens"`
	CostMicrosUSD       int64  `json:"cost_micros_usd"`
	EstimatedCostMicros int64  `json:"estimated_cost_micros_usd,omitempty"`
	UnknownAttempts     int64  `json:"unknown_attempts"`
	Errors              int64  `json:"errors"`
}

type Anomaly struct {
	RequestID      string    `json:"request_id"`
	AttemptID      string    `json:"attempt_id"`
	CompletedAt    time.Time `json:"completed_at"`
	ProjectID      string    `json:"project_id"`
	DeploymentID   string    `json:"deployment_id,omitempty"`
	ProviderID     string    `json:"provider_id,omitempty"`
	RequestedModel string    `json:"requested_model,omitempty"`
	ProviderModel  string    `json:"provider_model,omitempty"`
	Status         string    `json:"status"`
	ErrorClass     string    `json:"error_class,omitempty"`
	HTTPStatus     int       `json:"http_status,omitempty"`
	RetryCount     int       `json:"retry_count"`
	FallbackCount  int       `json:"fallback_count"`
}

// ErrInvalidQuery marks a query the caller got wrong, as opposed to a read
// the history index failed.
var ErrInvalidQuery = errors.New("invalid usage query")

type invalidQueryError string

func (e invalidQueryError) Error() string        { return string(e) }
func (e invalidQueryError) Is(target error) bool { return target == ErrInvalidQuery }

func invalidQuery(message string) error { return invalidQueryError(message) }

func (a *Aggregate) QueryAttempts(query AttemptQuery) (AttemptPage, error) {
	if query.Limit < 1 || query.Limit > 100 {
		return AttemptPage{}, invalidQuery("usage page limit must be between 1 and 100")
	}
	if query.AccountRegionID != domain.RegionNone && query.AccountRegionID != domain.RegionCN && query.AccountRegionID != domain.RegionGlobal {
		return AttemptPage{}, invalidQuery("usage account region is invalid")
	}
	if !query.ProviderFailureReason.Valid() {
		return AttemptPage{}, invalidQuery("usage provider failure reason is invalid")
	}
	if err := a.Flush(); err != nil {
		return AttemptPage{}, err
	}
	page := AttemptPage{Attempts: make([]AttemptEvent, 0, query.Limit+1)}
	err := a.history.db.View(func(tx *bbolt.Tx) error {
		scanned := 0
		consider := func(attempt AttemptEvent) error {
			scanned++
			if pastStart(attempt.CompletedAt, query.Start) {
				return errStopWalk
			}
			if attemptMatches(attempt, query) {
				page.Attempts = append(page.Attempts, attempt)
				if len(page.Attempts) == query.Limit+1 {
					page.Attempts = page.Attempts[:query.Limit]
					page.NextCursor = EncodeCursor(page.Attempts[len(page.Attempts)-1].Sequence)
					return errStopWalk
				}
			}
			// The budget is spent: hand back what this page found and where
			// the walk stopped, rather than reading on through the history.
			if scanned >= historyScanBudget {
				page.NextCursor = EncodeCursor(attempt.Sequence)
				return errStopWalk
			}
			return nil
		}
		// The narrowest index the query names decides which entries are
		// walked; every filter is still applied to each attempt.
		// An index entry carries no MAC of its own, so the authenticated row
		// it names must agree with it.
		byIndex := func(bucket []byte, prefix string, field func(AttemptEvent) string) error {
			return walkPrefixBackward(tx.Bucket(bucket), prefix, query.BeforeSequence, func(sequence uint64) error {
				attempt, err := a.history.attemptAt(tx, sequence)
				if err != nil {
					return err
				}
				if field(attempt) != prefix {
					return a.history.tampered()
				}
				return consider(attempt)
			})
		}
		switch {
		case query.RequestID != "":
			return byIndex(historyRequestAttempts, query.RequestID, func(attempt AttemptEvent) string { return attempt.RequestID })
		case query.ProjectID != "":
			return byIndex(historyProjectAttempts, query.ProjectID, func(attempt AttemptEvent) string { return attempt.ProjectID })
		default:
			return walkSequencesBackward(tx.Bucket(historyAttempts), query.BeforeSequence, func(sequence uint64, raw []byte) error {
				attempt, err := a.history.decodeAttempt(sequenceKey(sequence), raw)
				if err != nil {
					return err
				}
				return consider(attempt)
			})
		}
	})
	if err != nil {
		return AttemptPage{}, err
	}
	return page, nil
}

// startSlack is how far before a query's start a backward walk keeps reading.
// Rows are walked in ledger order, which follows completion time closely but
// not exactly — two requests finishing together can be appended in either
// order — so the walk stops only once it is well past the start.
const startSlack = time.Hour

func pastStart(completed, start time.Time) bool {
	return !start.IsZero() && completed.Before(start.Add(-startSlack))
}

func attemptMatches(attempt AttemptEvent, query AttemptQuery) bool {
	return !(query.ProjectID != "" && attempt.ProjectID != query.ProjectID ||
		query.WorkUnitID != "" && attempt.WorkUnitID != query.WorkUnitID ||
		query.RunID != "" && attempt.RunID != query.RunID ||
		query.ProviderID != "" && attempt.ProviderID != query.ProviderID ||
		query.OfferingID != "" && attempt.OfferingID != query.OfferingID ||
		query.AccountRegionID != "" && attempt.AccountRegionID != query.AccountRegionID ||
		query.ProviderFailureReason != "" && attempt.ProviderFailureReason != query.ProviderFailureReason ||
		query.DeploymentID != "" && attempt.DeploymentID != query.DeploymentID ||
		query.RequestID != "" && attempt.RequestID != query.RequestID ||
		query.RequestedModel != "" && attempt.RequestedModel != query.RequestedModel ||
		query.ProviderModel != "" && attempt.ProviderModel != query.ProviderModel ||
		query.Status != "" && attempt.Status != query.Status ||
		!query.Start.IsZero() && attempt.CompletedAt.Before(query.Start) ||
		!query.End.IsZero() && !attempt.CompletedAt.Before(query.End))
}

// RequestDetail is one request and its attempts. A request still in flight is
// answered from memory, with the attempts that have already settled.
func (a *Aggregate) RequestDetail(requestID string) (RequestDetail, bool, error) {
	if err := a.Flush(); err != nil {
		return RequestDetail{}, false, err
	}
	var result RequestDetail
	found := false
	if summary, ok := a.activeRequest(requestID); ok {
		result.Summary, found = summary, true
	}
	err := a.history.db.View(func(tx *bbolt.Tx) error {
		if !found {
			raw := tx.Bucket(historyRequestIndex).Get([]byte(requestID))
			if raw == nil {
				return nil
			}
			if len(raw) != 8 {
				return a.history.tampered()
			}
			summary, err := a.history.summaryAt(tx, binary.BigEndian.Uint64(raw))
			if err != nil {
				return err
			}
			if summary.RequestID != requestID {
				return a.history.tampered()
			}
			result.Summary, found = summary, true
		}
		attempts, err := a.history.requestAttempts(tx, requestID)
		result.Attempts = attempts
		return err
	})
	if err != nil {
		return RequestDetail{}, false, err
	}
	if !found {
		return RequestDetail{}, false, nil
	}
	return result, true, nil
}

// Period is the accounting day a dashboard reports on, as a half-open UTC
// interval.
//
// It is supplied by the caller rather than derived here. The boundary is a
// governed setting that only the caller can resolve, and the figures must cover
// exactly the interval the response advertises alongside them — deriving it
// twice is how the totals and the interval drift apart.
// Period is the accounting day a dashboard reports on. It carries the period's
// identity, not just its interval, because membership is decided by the stamp
// an event was admitted with rather than by when it finished — a request
// accepted at 23:59 and settled at 00:02 is charged to the day it was admitted
// on, and the summary rollup keys on that same stamp.
type Period struct {
	ID              string
	TimezoneVersion uint64
}

// Includes reports whether work stamped with this period identity belongs to
// it. There is deliberately no instant here: membership is the stamp, and a
// second interval-based rule beside it is how the dashboard and the summary
// would start disagreeing about which day a call belongs to.
func (p Period) Includes(periodID string, timezoneVersion uint64) bool {
	return periodID == p.ID && timezoneVersion == p.TimezoneVersion
}

// Dashboard is one accounting day's figures, the last seven days by hour, and
// the day's most recent anomalies.
//
// The day's totals and breakdowns are read from the daily rollup, and the
// hours from the hourly rows: both are stored increments, so the cost of a
// dashboard does not depend on how much traffic the day or the week carried.
// Request latency percentiles are therefore read off a histogram and report the
// upper bound of the bucket they fall in, the same answer the summary view
// gives for the same day.
func (a *Aggregate) Dashboard(now time.Time, period Period) (Dashboard, error) {
	if err := a.Flush(); err != nil {
		return Dashboard{}, err
	}
	since := now.Add(-7 * 24 * time.Hour).UTC().Truncate(time.Hour)
	result := Dashboard{
		Active: a.activeCount(), Watermark: a.Watermark().Sequence,
		Hourly:          make([]Bucket, 0, 7*24),
		RecentAnomalies: make([]Anomaly, 0, 5),
		Breakdowns: map[string]map[string][]Breakdown{
			"project": {}, "provider": {}, "offering": {}, "requested_model": {}, "provider_model": {},
		},
	}
	breakdowns := map[string]map[string]*Breakdown{
		"project": {}, "provider": {}, "offering": {}, "requested_model": {}, "provider_model": {},
	}
	err := a.history.db.View(func(tx *bbolt.Tx) error {
		if err := a.history.dayRollups(tx, period.ID, period.TimezoneVersion, func(key domain.RollupKey, row domain.DailyRollup) error {
			if key.Dimension == domain.RollupDimensionTotal {
				result.Today = rollupBucket(row)
				return nil
			}
			if groups, ok := breakdowns[key.Dimension]; ok {
				groups[key.DimensionKey] = &Breakdown{
					Key: key.DimensionKey, Calls: row.Attempts, InputTokens: row.InputTokens,
					OutputTokens: row.OutputTokens, CostMicrosUSD: row.CostMicrosUSD,
					EstimatedCostMicros: row.EstimatedCostMicrosUSD, UnknownAttempts: row.UnknownAttempts,
					Errors: row.Errors,
				}
			}
			return nil
		}); err != nil {
			return err
		}
		hours, err := a.history.hoursSince(tx, since)
		if err != nil {
			return err
		}
		for _, row := range hours {
			bucket := row.Bucket
			bucket.RequestLatencyP50Millis, _ = ApproximateLatencyPercentile(row.RequestLatencyBuckets, row.RequestLatencyOverflow, 50)
			bucket.RequestLatencyP95Millis, _ = ApproximateLatencyPercentile(row.RequestLatencyBuckets, row.RequestLatencyOverflow, 95)
			result.Hourly = append(result.Hourly, bucket)
		}
		scanned := 0
		return walkSequencesBackward(tx.Bucket(historyAnomalies), 0, func(sequence uint64, _ []byte) error {
			scanned++
			attempt, err := a.history.attemptAt(tx, sequence)
			if err != nil {
				return err
			}
			if !isAnomaly(attempt) {
				return a.history.tampered()
			}
			if period.Includes(attempt.PeriodID, attempt.PeriodTimezoneVersion) {
				result.RecentAnomalies = append(result.RecentAnomalies, Anomaly{
					RequestID: attempt.RequestID, AttemptID: attempt.AttemptID,
					CompletedAt: attempt.CompletedAt, ProjectID: attempt.ProjectID,
					DeploymentID: attempt.DeploymentID,
					ProviderID:   attempt.ProviderID, RequestedModel: attempt.RequestedModel,
					ProviderModel: attempt.ProviderModel, Status: attempt.Status,
					ErrorClass: attempt.ErrorClass, HTTPStatus: attempt.HTTPStatus,
					RetryCount: attempt.RetryCount, FallbackCount: attempt.FallbackCount,
				})
			}
			if len(result.RecentAnomalies) == 5 || scanned >= historyScanBudget {
				return errStopWalk
			}
			return nil
		})
	})
	if err != nil {
		return Dashboard{}, err
	}
	for dimension, groups := range breakdowns {
		result.Breakdowns[dimension] = map[string][]Breakdown{
			"calls":  topBreakdowns(groups, 5, "calls"),
			"cost":   topBreakdowns(groups, 5, "cost"),
			"errors": topBreakdowns(groups, 5, "errors"),
		}
	}
	return result, nil
}

// rollupBucket presents one day's total row as a dashboard bucket.
func rollupBucket(row domain.DailyRollup) Bucket {
	bucket := Bucket{
		Requests: row.Requests, RequestErrors: row.RequestErrors,
		RequestLatencySamples: row.RequestLatencySamples,
		Attempts:              row.Attempts, InputTokens: row.InputTokens, OutputTokens: row.OutputTokens,
		EstimatedInputTokens: row.EstimatedInputTokens, EstimatedOutputTokens: row.EstimatedOutputTokens,
		CostMicrosUSD: row.CostMicrosUSD, EstimatedCostMicrosUSD: row.EstimatedCostMicrosUSD,
		UnknownAttempts: row.UnknownAttempts, Errors: row.Errors, LatencyMillis: row.LatencyMillis,
	}
	bucket.RequestLatencyP50Millis, _ = ApproximateLatencyPercentile(row.RequestLatencyBuckets, row.RequestLatencyOverflow, 50)
	bucket.RequestLatencyP95Millis, _ = ApproximateLatencyPercentile(row.RequestLatencyBuckets, row.RequestLatencyOverflow, 95)
	return bucket
}

func topBreakdowns(groups map[string]*Breakdown, limit int, metric string) []Breakdown {
	items := make([]Breakdown, 0, len(groups))
	for _, item := range groups {
		items = append(items, *item)
	}
	sort.Slice(items, func(i, j int) bool {
		left, right := items[i].Calls, items[j].Calls
		switch metric {
		case "cost":
			left, right = items[i].CostMicrosUSD, items[j].CostMicrosUSD
		case "errors":
			left, right = items[i].Errors, items[j].Errors
		}
		if left != right {
			return left > right
		}
		return items[i].Key < items[j].Key
	})
	if len(items) > limit {
		items = items[:limit]
	}
	return items
}

func EncodeCursor(sequence uint64) string {
	var raw [8]byte
	binary.BigEndian.PutUint64(raw[:], sequence)
	return base64.RawURLEncoding.EncodeToString(raw[:])
}

func DecodeCursor(value string) (uint64, error) {
	if value == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) != 8 {
		return 0, errors.New("invalid usage cursor")
	}
	sequence := binary.BigEndian.Uint64(raw)
	if sequence == 0 {
		return 0, errors.New("invalid usage cursor")
	}
	return sequence, nil
}
