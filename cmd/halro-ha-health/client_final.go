package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

var clientFinalOutcomes = [...]string{
	"success", "rejected", "timeout", "transport_failure", "canceled", "incomplete_stream",
}

var clientFinalName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
var clientFinalAcceptanceID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9/_-]{0,127}$`)

const clientFinalWindow = 5 * time.Minute
const clientFinalGap = 30 * time.Second

type clientFinalObserver struct {
	Region   string `json:"region"`
	Instance string `json:"instance"`
	Owner    string `json:"owner"`
	Scope    string `json:"scope"`
}

type clientFinalManifest struct {
	Version          int                   `json:"version"`
	AcceptanceRecord string                `json:"acceptance_record"`
	Observers        []clientFinalObserver `json:"observers"`
	OperationClasses []string              `json:"operation_classes"`
}

type clientFinalResult struct {
	Window      string                        `json:"window"`
	Status      string                        `json:"status"`
	Reason      string                        `json:"reason,omitempty"`
	Acceptance  string                        `json:"acceptance_record,omitempty"`
	EvaluatedAt time.Time                     `json:"evaluated_at"`
	Counts      map[string]map[string]float64 `json:"counts,omitempty"`
}

type clientSeriesKey struct {
	region, instance, class, outcome string
}

func loadClientFinalManifest(path string) (*clientFinalManifest, error) {
	if path == "" {
		return nil, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect client-final manifest: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 || info.Size() < 1 || info.Size() > 64<<10 {
		return nil, errors.New("client-final manifest must be a nonempty regular file, not group/world writable, at most 64 KiB")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open client-final manifest: %w", err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() {
		return nil, errors.New("client-final manifest changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, 64<<10+1))
	if err != nil {
		return nil, fmt.Errorf("read client-final manifest: %w", err)
	}
	if len(data) < 1 || len(data) > 64<<10 {
		return nil, errors.New("client-final manifest exceeds 64 KiB or is empty")
	}
	var manifest clientFinalManifest
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("decode client-final manifest: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("client-final manifest has trailing data")
	}
	if manifest.Version != 1 || !clientFinalAcceptanceID.MatchString(manifest.AcceptanceRecord) ||
		len(manifest.Observers) == 0 || len(manifest.Observers) > 16 ||
		len(manifest.OperationClasses) == 0 || len(manifest.OperationClasses) > 16 {
		return nil, errors.New("client-final manifest version or inventory is invalid")
	}
	seenObservers := make(map[clientSeriesKey]bool, len(manifest.Observers))
	for _, observer := range manifest.Observers {
		key := clientSeriesKey{region: observer.Region, instance: observer.Instance}
		if observer.Region == "" || observer.Instance == "" || len(observer.Region) > 128 || len(observer.Instance) > 128 ||
			strings.TrimSpace(observer.Region) != observer.Region ||
			strings.TrimSpace(observer.Instance) != observer.Instance ||
			strings.TrimSpace(observer.Owner) == "" || len(observer.Owner) > 256 ||
			strings.TrimSpace(observer.Scope) == "" || len(observer.Scope) > 256 ||
			seenObservers[key] {
			return nil, errors.New("client-final observer identity, ownership or scope is invalid")
		}
		seenObservers[key] = true
	}
	seenClasses := make(map[string]bool, len(manifest.OperationClasses))
	for _, class := range manifest.OperationClasses {
		if !clientFinalName.MatchString(class) || seenClasses[class] {
			return nil, errors.New("client-final operation classes must be unique bounded names")
		}
		seenClasses[class] = true
	}
	return &manifest, nil
}

func (s *server) clientFinalSelector(metric string) string {
	return s.selectorWith(metric, `job="halro-client-observer",expected_client_observer="true"`)
}

func (s *server) clientFinalResults(ctx context.Context) clientFinalResult {
	result := clientFinalResult{Window: "5m", Status: "not_configured", EvaluatedAt: time.Now().UTC()}
	if s.clientFinal == nil {
		return result
	}
	result.Acceptance = s.clientFinal.AcceptanceRecord
	result.Status, result.Reason = "unobserved", "coverage_incomplete"
	expectedObserver := make(map[clientSeriesKey]bool, len(s.clientFinal.Observers))
	expectedCounter := make(map[clientSeriesKey]bool, len(s.clientFinal.Observers)*len(s.clientFinal.OperationClasses)*len(clientFinalOutcomes))
	for _, observer := range s.clientFinal.Observers {
		base := clientSeriesKey{region: observer.Region, instance: observer.Instance}
		expectedObserver[base] = true
		for _, class := range s.clientFinal.OperationClasses {
			for _, outcome := range clientFinalOutcomes {
				expectedCounter[clientSeriesKey{region: observer.Region, instance: observer.Instance, class: class, outcome: outcome}] = true
			}
		}
	}
	var upTimes map[clientSeriesKey][]time.Time
	sources := []struct {
		metric   string
		expected map[clientSeriesKey]bool
		want     float64
		counter  bool
	}{
		{"up", expectedObserver, 1, false},
		{"halro_ha_client_observer_ready", expectedObserver, 1, false},
		{"halro_ha_client_observer_unreconciled_operations", expectedObserver, 0, false},
		{"halro_ha_client_logical_operation_results_total", expectedCounter, 0, true},
	}
	rawExpressions := make([]string, len(sources))
	for index, source := range sources {
		rawExpressions[index] = s.clientFinalSelector(source.metric) + "[6m]"
	}
	raw, err := s.clientFinalQueries(ctx, result.EvaluatedAt, rawExpressions)
	if err != nil {
		result.Reason = "query_failed"
		return result
	}
	for index, source := range sources {
		times, valid := s.validClientFinalMatrix(raw[index], source.metric, source.expected, source.want, source.counter, result.EvaluatedAt, upTimes)
		if !valid {
			return result
		}
		if source.metric == "up" {
			upTimes = times
		}
	}
	metric := s.clientFinalSelector("halro_ha_client_logical_operation_results_total")
	vectors, err := s.clientFinalQueries(ctx, result.EvaluatedAt, []string{
		"resets(" + metric + "[5m])", "increase(" + metric + "[5m])",
	})
	if err != nil {
		result.Reason = "query_failed"
		return result
	}
	if _, ok := s.validClientFinalVector(vectors[0], expectedCounter, true, result.EvaluatedAt); !ok {
		return result
	}
	values, ok := s.validClientFinalVector(vectors[1], expectedCounter, false, result.EvaluatedAt)
	if !ok || time.Since(result.EvaluatedAt) > clientFinalGap {
		return result
	}
	counts := make(map[string]map[string]float64, len(s.clientFinal.OperationClasses))
	total := 0.0
	for key, value := range values {
		if counts[key.class] == nil {
			counts[key.class] = make(map[string]float64, len(clientFinalOutcomes))
		}
		counts[key.class][key.outcome] += value
		total += value
	}
	if total == 0 || math.IsInf(total, 0) || math.IsNaN(total) {
		result.Reason = "empty_window"
		return result
	}
	result.Status, result.Reason, result.Counts = "observed", "", counts
	return result
}

func (s *server) clientFinalQueries(ctx context.Context, at time.Time, expressions []string) ([]prometheusResult, error) {
	type queryResult struct {
		index int
		data  prometheusResult
		err   error
	}
	completed := make(chan queryResult, len(expressions))
	for index, expression := range expressions {
		go func() {
			data, err := s.query(ctx, expression, 0, at)
			completed <- queryResult{index: index, data: data, err: err}
		}()
	}
	results := make([]prometheusResult, len(expressions))
	var queryErr error
	for range expressions {
		item := <-completed
		results[item.index] = item.data
		if item.err != nil {
			queryErr = item.err
		}
	}
	return results, queryErr
}

func (s *server) clientFinalKey(labels map[string]string, metric string, counter bool) (clientSeriesKey, bool) {
	if labels["environment"] != s.environment || labels["cluster"] != s.cluster ||
		labels["job"] != "halro-client-observer" || labels["expected_client_observer"] != "true" ||
		labels["region"] == "" || labels["instance"] == "" {
		return clientSeriesKey{}, false
	}
	allowed := map[string]bool{"environment": true, "region": true, "cluster": true, "instance": true,
		"job": true, "expected_client_observer": true, "__name__": true}
	if counter {
		allowed["operation_class"], allowed["outcome"] = true, true
	} else if labels["operation_class"] != "" || labels["outcome"] != "" {
		return clientSeriesKey{}, false
	}
	for label := range labels {
		if !allowed[label] {
			return clientSeriesKey{}, false
		}
	}
	if metric != "" && labels["__name__"] != metric {
		return clientSeriesKey{}, false
	}
	key := clientSeriesKey{region: labels["region"], instance: labels["instance"]}
	if counter {
		key.class, key.outcome = labels["operation_class"], labels["outcome"]
	}
	return key, true
}

func (s *server) validClientFinalMatrix(data prometheusResult, metric string, expected map[clientSeriesKey]bool, want float64, counter bool, now time.Time, upTimes map[clientSeriesKey][]time.Time) (map[clientSeriesKey][]time.Time, bool) {
	if data.Data.ResultType != "matrix" || len(data.Data.Result) != len(expected) {
		return nil, false
	}
	seen := make(map[clientSeriesKey]bool, len(expected))
	allTimes := make(map[clientSeriesKey][]time.Time, len(expected))
	start := now.Add(-clientFinalWindow)
	for _, item := range data.Data.Result {
		key, ok := s.clientFinalKey(item.Metric, metric, counter)
		if !ok || !expected[key] || seen[key] {
			return nil, false
		}
		seen[key] = true
		var first, previous time.Time
		var previousValue float64
		var times []time.Time
		for _, point := range item.Values {
			value, at, valid := sample(point)
			if !valid || at.After(now.Add(time.Second)) || value < 0 {
				return nil, false
			}
			if at.Before(start.Add(-clientFinalGap)) {
				continue
			}
			if first.IsZero() {
				first = at
				if at.After(start) {
					return nil, false
				}
			} else if !at.After(previous) || at.Sub(previous) > clientFinalGap || counter && value < previousValue {
				return nil, false
			}
			if !counter && value != want {
				return nil, false
			}
			times = append(times, at)
			previous, previousValue = at, value
		}
		if first.IsZero() || now.Sub(previous) > clientFinalGap {
			return nil, false
		}
		if upTimes != nil {
			base := clientSeriesKey{region: key.region, instance: key.instance}
			expectedTimes := upTimes[base]
			if len(times) != len(expectedTimes) {
				return nil, false
			}
			for index := range times {
				if difference := times[index].Sub(expectedTimes[index]); difference < -time.Second || difference > time.Second {
					return nil, false
				}
			}
		}
		allTimes[key] = times
	}
	return allTimes, len(seen) == len(expected)
}

func (s *server) validClientFinalVector(data prometheusResult, expected map[clientSeriesKey]bool, zero bool, now time.Time) (map[clientSeriesKey]float64, bool) {
	if data.Data.ResultType != "vector" || len(data.Data.Result) != len(expected) {
		return nil, false
	}
	values := make(map[clientSeriesKey]float64, len(expected))
	for _, item := range data.Data.Result {
		key, ok := s.clientFinalKey(item.Metric, "", true)
		value, at, valid := sample(item.Value)
		if !ok || !expected[key] || !valid || value < 0 || at.After(now.Add(time.Second)) ||
			now.Sub(at) > clientFinalGap || zero && value != 0 {
			return nil, false
		}
		if _, exists := values[key]; exists {
			return nil, false
		}
		values[key] = value
	}
	return values, len(values) == len(expected)
}

func (s *server) clientFinalResponse(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(s.clientFinalResults(r.Context()))
}
