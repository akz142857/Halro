package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var impactOutcomes = [...]string{
	"http_2xx", "http_4xx", "not_primary", "http_503", "timeout", "http_5xx", "canceled", "write_error",
}

type requestImpact struct {
	Window      string             `json:"window"`
	Observed    bool               `json:"observed"`
	Counts      map[string]float64 `json:"counts,omitempty"`
	EvaluatedAt time.Time          `json:"evaluated_at"`
}

func (s *server) requestImpact(ctx context.Context) (requestImpact, error) {
	result := requestImpact{Window: "5m", EvaluatedAt: time.Now().UTC().Truncate(time.Millisecond)}
	metric := "halro_ha_client_write_http_responses_total"
	quotedMembers := make([]string, 0, len(s.members))
	for _, member := range s.members {
		quotedMembers = append(quotedMembers, regexp.QuoteMeta(member))
	}
	memberLabel := `instance=~` + strconv.Quote(`^(`+strings.Join(quotedMembers, `|`)+`)$`)
	coverage, err := s.currentMetrics(ctx, s.memberSelectorWith(metric, memberLabel)+"[45s]", result.EvaluatedAt)
	if err != nil {
		return result, errors.New("request impact coverage unavailable")
	}
	type memberOutcome struct{ instance, outcome string }
	seen := make(map[memberOutcome]bool, len(s.members)*len(impactOutcomes))
	expected := make(map[string]bool, len(s.members))
	validOutcomes := make(map[string]bool, len(impactOutcomes))
	for _, member := range s.members {
		expected[member] = true
	}
	for _, outcome := range impactOutcomes {
		validOutcomes[outcome] = true
	}
	for _, item := range coverage.Data.Result {
		instance := item.Metric["instance"]
		outcome := item.Metric["outcome"]
		key := memberOutcome{instance, outcome}
		value, at, ok := sample(item.Value)
		if !expected[instance] || !validOutcomes[outcome] || !ok || value < 0 || at.After(result.EvaluatedAt) ||
			result.EvaluatedAt.Sub(at) > 30*time.Second || seen[key] {
			return result, nil
		}
		seen[key] = true
	}
	for _, member := range s.members {
		for _, outcome := range impactOutcomes {
			if !seen[memberOutcome{member, outcome}] {
				return result, nil
			}
		}
	}
	expression := `increase(` + s.memberSelectorWith(metric, memberLabel) + `[5m])`
	data, err := s.query(ctx, expression, 0, result.EvaluatedAt)
	if err != nil {
		return result, errors.New("request impact query unavailable")
	}
	counts := make(map[string]float64, len(impactOutcomes))
	rangeSeen := make(map[memberOutcome]bool, len(seen))
	for _, item := range data.Data.Result {
		instance := item.Metric["instance"]
		outcome := item.Metric["outcome"]
		key := memberOutcome{instance, outcome}
		value, at, ok := sample(item.Value)
		if !expected[instance] || !validOutcomes[outcome] || !ok || value < 0 || at.After(result.EvaluatedAt) ||
			result.EvaluatedAt.Sub(at) > 30*time.Second || rangeSeen[key] {
			return result, nil
		}
		rangeSeen[key] = true
		counts[outcome] += value
	}
	if len(rangeSeen) != len(seen) || len(counts) != len(impactOutcomes) {
		return result, nil
	}
	result.Observed, result.Counts = true, counts
	return result, nil
}

func (s *server) impact(w http.ResponseWriter, r *http.Request) {
	result, err := s.requestImpact(r.Context())
	if err != nil {
		http.Error(w, "request impact unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(result)
}
