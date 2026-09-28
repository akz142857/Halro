package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"time"
)

type stageLatency struct {
	Instance    string    `json:"instance"`
	Stage       string    `json:"stage"`
	P95Seconds  float64   `json:"p95_seconds"`
	EvaluatedAt time.Time `json:"evaluated_at"`
	Window      string    `json:"window"`
}

var latencyStages = [...]struct{ name, metric string }{
	{"durable_to_confirm", "halro_replication_durable_to_confirm_seconds_bucket"},
	{"sink_persist", "halro_replication_sink_persist_seconds_bucket"},
	{"apply_batch", "halro_replication_apply_batch_seconds_bucket"},
}

func (s *server) stageLatencies(ctx context.Context) ([]stageLatency, error) {
	type queryResult struct {
		stage string
		data  prometheusResult
		err   error
	}
	results := make(chan queryResult, len(latencyStages))
	for _, stage := range latencyStages {
		stage := stage
		go func() {
			expression := "histogram_quantile(0.95, sum by (instance,le) (rate(" + s.memberSelector(stage.metric) + "[5m])))"
			data, err := s.query(ctx, expression, 0)
			results <- queryResult{stage.name, data, err}
		}()
	}
	known := make(map[string]bool, len(s.members))
	for _, member := range s.members {
		known[member] = true
	}
	latencies := make([]stageLatency, 0, len(latencyStages)*len(s.members))
	seen := make(map[string]bool)
	for range latencyStages {
		result := <-results
		if result.err != nil {
			return nil, errors.New("stage latency query unavailable")
		}
		for _, item := range result.data.Data.Result {
			instance := item.Metric["instance"]
			value, at, ok := sample(item.Value)
			if !known[instance] || !ok || value < 0 || time.Since(at) < 0 || time.Since(at) > 30*time.Second {
				continue
			}
			key := result.stage + ":" + instance
			if seen[key] {
				return nil, errors.New("duplicate stage latency series")
			}
			seen[key] = true
			latencies = append(latencies, stageLatency{Instance: instance, Stage: result.stage, P95Seconds: value, EvaluatedAt: at, Window: "5m"})
		}
	}
	sort.Slice(latencies, func(i, j int) bool {
		if latencies[i].Instance != latencies[j].Instance {
			return latencies[i].Instance < latencies[j].Instance
		}
		return latencies[i].Stage < latencies[j].Stage
	})
	return latencies, nil
}

func (s *server) latency(w http.ResponseWriter, r *http.Request) {
	values, err := s.stageLatencies(r.Context())
	if err != nil {
		http.Error(w, "stage latency unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(values)
}
