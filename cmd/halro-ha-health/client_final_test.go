package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestClientFinalManifestRequiresExplicitBoundedInventory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client-final.json")
	for _, tc := range []struct {
		name, document string
		valid          bool
	}{
		{"valid", `{"version":1,"acceptance_record":"ha-client-final-acceptance","observers":[{"region":"r1","instance":"client-a","owner":"gateway team","scope":"public write routes"}],"operation_classes":["write","stream"]}`, true},
		{"missing acceptance", `{"version":1,"observers":[{"region":"r1","instance":"client-a","owner":"team","scope":"write"}],"operation_classes":["write"]}`, false},
		{"duplicate observer", `{"version":1,"acceptance_record":"ha-client-final-acceptance","observers":[{"region":"r1","instance":"client-a","owner":"team","scope":"write"},{"region":"r1","instance":"client-a","owner":"team","scope":"stream"}],"operation_classes":["write"]}`, false},
		{"missing scope", `{"version":1,"acceptance_record":"ha-client-final-acceptance","observers":[{"region":"r1","instance":"client-a","owner":"team"}],"operation_classes":["write"]}`, false},
		{"duplicate class", `{"version":1,"acceptance_record":"ha-client-final-acceptance","observers":[{"region":"r1","instance":"client-a","owner":"team","scope":"write"}],"operation_classes":["write","write"]}`, false},
		{"unbounded class", `{"version":1,"acceptance_record":"ha-client-final-acceptance","observers":[{"region":"r1","instance":"client-a","owner":"team","scope":"write"}],"operation_classes":["/v1/chat/completions"]}`, false},
		{"unknown field", `{"version":1,"acceptance_record":"ha-client-final-acceptance","observers":[{"region":"r1","instance":"client-a","owner":"team","scope":"write"}],"operation_classes":["write"],"trust_me":true}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(tc.document), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := loadClientFinalManifest(path)
			if (err == nil) != tc.valid || tc.valid && (got == nil || len(got.OperationClasses) == 0) {
				t.Fatalf("manifest accepted=%v error=%v", got != nil, err)
			}
		})
	}
	valid := `{"version":1,"acceptance_record":"ha-client-final-acceptance","observers":[{"region":"r1","instance":"client-a","owner":"gateway team","scope":"public write routes"}],"operation_classes":["write"]}`
	if err := os.WriteFile(path, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	projected := filepath.Join(t.TempDir(), "projected-manifest.json")
	if err := os.Symlink(path, projected); err != nil {
		t.Fatal(err)
	}
	if _, err := loadClientFinalManifest(projected); err != nil {
		t.Fatalf("regular projected manifest rejected: %v", err)
	}
	if _, err := loadClientFinalManifest(t.TempDir()); err == nil {
		t.Fatal("directory accepted as client-final manifest")
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := loadClientFinalManifest(path); err == nil {
		t.Fatal("group/world writable client-final manifest accepted")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 64<<10+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadClientFinalManifest(path); err == nil {
		t.Fatal("oversized client-final manifest accepted")
	}
}

func TestClientFinalEndpointIsUnconfiguredWithoutAcceptedManifest(t *testing.T) {
	response := httptest.NewRecorder()
	(&server{}).routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/client-final", nil))
	var result clientFinalResult
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil ||
		result.Status != "not_configured" || result.Counts != nil || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("unconfigured endpoint exposed counts: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestClientFinalQueriesRunEachGateStageConcurrently(t *testing.T) {
	var mu sync.Mutex
	active, maximum := 0, 0
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		mu.Lock()
		active++
		if active > maximum {
			maximum = active
		}
		mu.Unlock()
		time.Sleep(30 * time.Millisecond)
		mu.Lock()
		active--
		mu.Unlock()
		return jsonResponse(`{"status":"success","data":{"resultType":"vector","result":[]}}`), nil
	})}
	base, _ := url.Parse("http://127.0.0.1:9091")
	s := &server{client: client, base: base}
	results, err := s.clientFinalQueries(context.Background(), time.Now(), []string{"up", "vector(1)", "vector(2)", "vector(3)"})
	if err != nil || len(results) != 4 || maximum < 2 {
		t.Fatalf("client-final gate queries were serialized: count=%d maximum=%d err=%v", len(results), maximum, err)
	}
}

func TestClientFinalResultsRequireFullWindowCoverage(t *testing.T) {
	mode := ""
	queries := 0
	evaluationTime := ""
	var queryMu sync.Mutex
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		queryMu.Lock()
		queries++
		if evaluationTime == "" {
			evaluationTime = request.URL.Query().Get("time")
		} else if request.URL.Query().Get("time") != evaluationTime {
			t.Error("client-final queries used different evaluation times")
		}
		queryMu.Unlock()
		expression := request.URL.Query().Get("query")
		if !strings.Contains(expression, `environment="test",cluster="ha",job="halro-client-observer",expected_client_observer="true"`) ||
			request.URL.Query().Get("time") == "" {
			t.Fatalf("client-final query lost scope or fixed evaluation time: %s", request.URL.String())
		}
		evaluated := time.UnixMilli(parsePrometheusTime(t, request.URL.Query().Get("time"))).UTC()
		vector := strings.HasPrefix(expression, "increase(") || strings.HasPrefix(expression, "resets(")
		metric := "halro_ha_client_logical_operation_results_total"
		switch {
		case strings.HasPrefix(expression, "up{"):
			metric = "up"
		case strings.HasPrefix(expression, "halro_ha_client_observer_ready{"):
			metric = "halro_ha_client_observer_ready"
		case strings.HasPrefix(expression, "halro_ha_client_observer_unreconciled_operations{"):
			metric = "halro_ha_client_observer_unreconciled_operations"
		}
		entries := make([]map[string]any, 0)
		for _, instance := range []string{"client-a", "client-b"} {
			if mode == "missing_observer" && instance == "client-b" {
				continue
			}
			outcomes := []string{""}
			if metric == "halro_ha_client_logical_operation_results_total" {
				outcomes = clientFinalOutcomes[:]
			}
			for _, outcome := range outcomes {
				if mode == "missing_outcome" && instance == "client-b" && outcome == "timeout" {
					continue
				}
				labels := map[string]string{
					"environment": "test", "region": "r1", "cluster": "ha", "instance": instance,
					"job": "halro-client-observer", "expected_client_observer": "true",
				}
				if !vector {
					labels["__name__"] = metric
				}
				if outcome != "" {
					labels["operation_class"], labels["outcome"] = "write", outcome
				}
				if mode == "unexpected_label" && instance == "client-b" && outcome == "timeout" {
					labels["request_id"] = "secret"
				}
				entry := map[string]any{"metric": labels}
				if vector {
					value := "0"
					if strings.HasPrefix(expression, "increase(") && outcome == "success" && mode != "empty_window" {
						value = "2"
					}
					if strings.HasPrefix(expression, "resets(") && mode == "counter_reset" && instance == "client-b" && outcome == "success" {
						value = "1"
					}
					if strings.HasPrefix(expression, "resets(") && mode == "prometheus_reset" && instance == "client-b" && outcome == "success" {
						value = "1"
					}
					entry["value"] = []any{float64(evaluated.Unix()), value}
				} else {
					points := make([]any, 0, 22)
					for at := evaluated.Add(-clientFinalWindow - 15*time.Second); !at.After(evaluated.Add(-5 * time.Second)); at = at.Add(15 * time.Second) {
						if mode == "outcome_scrape_missing" && metric == "halro_ha_client_logical_operation_results_total" &&
							instance == "client-b" && outcome == "timeout" && at.Equal(evaluated.Add(-2*time.Minute-15*time.Second)) {
							continue
						}
						if mode == "late_start" && instance == "client-b" && at.Before(evaluated.Add(-4*time.Minute)) {
							continue
						}
						if mode == "stale_last" && instance == "client-b" && at.After(evaluated.Add(-time.Minute)) {
							continue
						}
						if mode == "scrape_gap" && instance == "client-b" && at.After(evaluated.Add(-3*time.Minute)) && at.Before(evaluated.Add(-2*time.Minute)) {
							continue
						}
						value := "0"
						if metric == "up" || metric == "halro_ha_client_observer_ready" {
							value = "1"
						}
						if mode == "observer_down" && metric == "up" && instance == "client-b" && at.After(evaluated.Add(-time.Minute)) {
							value = "0"
						}
						if mode == "unreconciled" && metric == "halro_ha_client_observer_unreconciled_operations" && instance == "client-b" && at.After(evaluated.Add(-time.Minute)) {
							value = "1"
						}
						if mode == "counter_reset" && metric == "halro_ha_client_logical_operation_results_total" && instance == "client-b" && outcome == "success" && at.Before(evaluated.Add(-2*time.Minute)) {
							value = "3"
						}
						points = append(points, []any{float64(at.Unix()), value})
					}
					entry["values"] = points
				}
				entries = append(entries, entry)
				if mode == "duplicate_series" && metric == "up" && instance == "client-b" {
					entries = append(entries, entry)
				}
			}
		}
		resultType := "matrix"
		if vector {
			resultType = "vector"
		}
		body, _ := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": resultType, "result": entries}})
		return jsonResponse(string(body)), nil
	})}
	base, _ := url.Parse("http://127.0.0.1:9091")
	s := &server{client: client, base: base, environment: "test", cluster: "ha",
		clientFinal: &clientFinalManifest{Version: 1, AcceptanceRecord: "ha-client-final-acceptance", Observers: []clientFinalObserver{
			{Region: "r1", Instance: "client-a", Owner: "team", Scope: "write"},
			{Region: "r1", Instance: "client-b", Owner: "team", Scope: "write"},
		}, OperationClasses: []string{"write"}}}
	result := s.clientFinalResults(context.Background())
	if result.Status != "observed" || result.Acceptance != "ha-client-final-acceptance" || result.Counts["write"]["success"] != 4 || queries != 6 {
		t.Fatalf("complete client-final coverage: result=%+v queries=%d", result, queries)
	}
	for _, tc := range []struct {
		mode        string
		wantQueries int
	}{
		{"missing_observer", 4}, {"missing_outcome", 4}, {"unexpected_label", 4},
		{"scrape_gap", 4}, {"late_start", 4}, {"stale_last", 4}, {"duplicate_series", 4}, {"outcome_scrape_missing", 4},
		{"observer_down", 4}, {"unreconciled", 4}, {"counter_reset", 4}, {"prometheus_reset", 6}, {"empty_window", 6},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			mode, queries, evaluationTime = tc.mode, 0, ""
			result := s.clientFinalResults(context.Background())
			if result.Status != "unobserved" || result.Counts != nil || queries != tc.wantQueries {
				t.Fatalf("invalid client-final coverage became observed: result=%+v queries=%d", result, queries)
			}
		})
	}
	s.clientFinal = nil
	if result := s.clientFinalResults(context.Background()); result.Status != "not_configured" {
		t.Fatalf("absent producer manifest became observed: %+v", result)
	}
}

func parsePrometheusTime(t *testing.T, raw string) int64 {
	t.Helper()
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		t.Fatal(err)
	}
	return int64(value * 1000)
}
