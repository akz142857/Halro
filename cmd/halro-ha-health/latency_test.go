package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestStageLatencyUsesFixedClusterScopedQueriesAndKeepsMissingUnknown(t *testing.T) {
	queries := make(chan string, 3)
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		expression := request.URL.Query().Get("query")
		queries <- expression
		result := []any{}
		if strings.Contains(expression, "durable_to_confirm_seconds_bucket") {
			result = append(result, map[string]any{"metric": map[string]string{"instance": "one"}, "value": []any{time.Now().Unix(), "0.04"}})
		}
		body, _ := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": result}})
		return jsonResponse(string(body)), nil
	})}
	base, _ := url.Parse("http://127.0.0.1:9091")
	s := &server{client: client, base: base, environment: "test", cluster: "ha", members: []string{"one", "two"}}
	response := httptest.NewRecorder()
	s.latency(response, httptest.NewRequest(http.MethodGet, "/api/latency?query=evil", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var values []stageLatency
	if err := json.Unmarshal(response.Body.Bytes(), &values); err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values[0].Instance != "one" || values[0].Stage != "durable_to_confirm" || values[0].P95Seconds != 0.04 || values[0].Window != "5m" {
		t.Fatalf("wrong or fabricated latency observations: %+v", values)
	}
	for range 3 {
		expression := <-queries
		if strings.Contains(expression, "evil") || !strings.Contains(expression, `environment="test",cluster="ha"`) ||
			!strings.Contains(expression, "histogram_quantile(0.95") || !strings.Contains(expression, "[5m]") {
			t.Fatalf("unscoped latency expression: %s", expression)
		}
	}
}
