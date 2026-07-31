package usage

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"time"
)

type AttemptQuery struct {
	BeforeSequence uint64
	Limit          int
	ProjectID      string
	ProviderID     string
	RequestedModel string
	Status         string
	Start          time.Time
	End            time.Time
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
	Today     Bucket   `json:"today"`
	Hourly    []Bucket `json:"hourly"`
	Active    uint64   `json:"active_requests"`
	Watermark uint64   `json:"watermark_sequence"`
}

func (a *Aggregate) QueryAttempts(query AttemptQuery) (AttemptPage, error) {
	if query.Limit < 1 || query.Limit > 100 {
		return AttemptPage{}, errors.New("usage page limit must be between 1 and 100")
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	page := AttemptPage{Attempts: make([]AttemptEvent, 0, query.Limit+1)}
	for index := len(a.attempts) - 1; index >= 0; index-- {
		attempt := a.attempts[index]
		if query.BeforeSequence > 0 && attempt.Sequence >= query.BeforeSequence {
			continue
		}
		if query.ProjectID != "" && attempt.ProjectID != query.ProjectID ||
			query.ProviderID != "" && attempt.ProviderID != query.ProviderID ||
			query.RequestedModel != "" && attempt.RequestedModel != query.RequestedModel ||
			query.Status != "" && attempt.Status != query.Status ||
			!query.Start.IsZero() && attempt.CompletedAt.Before(query.Start) ||
			!query.End.IsZero() && !attempt.CompletedAt.Before(query.End) {
			continue
		}
		page.Attempts = append(page.Attempts, attempt)
		if len(page.Attempts) == query.Limit+1 {
			page.Attempts = page.Attempts[:query.Limit]
			page.NextCursor = EncodeCursor(page.Attempts[len(page.Attempts)-1].Sequence)
			break
		}
	}
	return page, nil
}

func (a *Aggregate) RequestDetail(requestID string) (RequestDetail, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	var result RequestDetail
	found := false
	for index := len(a.summaries) - 1; index >= 0; index-- {
		if a.summaries[index].RequestID == requestID {
			result.Summary = a.summaries[index]
			found = true
			break
		}
	}
	if active := a.requests[requestID]; active != nil {
		result.Summary = active.summary
		found = true
	}
	if !found {
		return RequestDetail{}, false
	}
	for _, attempt := range a.attempts {
		if attempt.RequestID == requestID {
			result.Attempts = append(result.Attempts, attempt)
		}
	}
	return result, true
}

func (a *Aggregate) Dashboard(now time.Time, location *time.Location) Dashboard {
	a.mu.RLock()
	defer a.mu.RUnlock()
	now = now.In(location)
	todayYear, todayDay := now.Year(), now.YearDay()
	since := now.Add(-7 * 24 * time.Hour).UTC().Truncate(time.Hour)
	result := Dashboard{
		Active: uint64(len(a.requests)), Watermark: a.watermark.Sequence,
		Hourly: make([]Bucket, 0, 7*24),
	}
	hourIndexes := make(map[int64]int, len(a.hourly))
	for _, bucket := range a.hourly {
		if !bucket.Hour.Before(since) {
			hourIndexes[bucket.Hour.UTC().Truncate(time.Hour).Unix()] = len(result.Hourly)
			result.Hourly = append(result.Hourly, bucket)
		}
		localHour := bucket.Hour.In(location)
		if localHour.Year() == todayYear && localHour.YearDay() == todayDay {
			result.Today.Requests += bucket.Requests
			result.Today.Attempts += bucket.Attempts
			result.Today.InputTokens += bucket.InputTokens
			result.Today.OutputTokens += bucket.OutputTokens
			result.Today.CostMicrosUSD += bucket.CostMicrosUSD
			result.Today.Errors += bucket.Errors
			result.Today.LatencyMillis += bucket.LatencyMillis
		}
	}
	// Provider usage can be unavailable after an ambiguous failure. Such attempts
	// are deliberately settled against a conservative token upper bound. Preserve
	// that accounting total while exposing the estimated portion separately so
	// operators do not mistake an upper bound for Provider-reported consumption.
	for _, attempt := range a.attempts {
		if !attempt.TokensEstimated {
			continue
		}
		hour := attempt.CompletedAt.UTC().Truncate(time.Hour)
		if index, ok := hourIndexes[hour.Unix()]; ok {
			result.Hourly[index].EstimatedInputTokens += attempt.ProviderInputTokens
			result.Hourly[index].EstimatedOutputTokens += attempt.ProviderOutputTokens
		}
		localHour := attempt.CompletedAt.In(location)
		if localHour.Year() == todayYear && localHour.YearDay() == todayDay {
			result.Today.EstimatedInputTokens += attempt.ProviderInputTokens
			result.Today.EstimatedOutputTokens += attempt.ProviderOutputTokens
		}
	}
	sortBuckets(result.Hourly)
	return result
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

func sortBuckets(buckets []Bucket) {
	for index := 1; index < len(buckets); index++ {
		for current := index; current > 0 &&
			buckets[current].Hour.Before(buckets[current-1].Hour); current-- {
			buckets[current], buckets[current-1] = buckets[current-1], buckets[current]
		}
	}
}
