package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func TestRequestImpactRequiresFreshCompleteMemberCoverage(t *testing.T) {
	missingMember, missingOutcome, missingIncreaseOutcome := "", "", ""
	duplicateOutcome, staleOutcome := "", ""
	duplicateIncreaseOutcome, staleIncreaseOutcome := "", ""
	queries := 0
	var firstQueryTime string
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		queries++
		queryTime := request.URL.Query().Get("time")
		if queryTime == "" {
			t.Fatal("impact query did not pin its Prometheus evaluation time")
		}
		if queries == 1 {
			firstQueryTime = queryTime
		} else if queryTime != firstQueryTime {
			t.Fatalf("impact queries used different evaluation times: %s != %s", queryTime, firstQueryTime)
		}
		now, err := strconv.ParseFloat(queryTime, 64)
		if err != nil {
			t.Fatalf("invalid impact query time %q: %v", queryTime, err)
		}
		expression := request.URL.Query().Get("query")
		if !strings.Contains(expression, `environment="test",cluster="ha",job="halro",expected_target="true"`) {
			t.Fatalf("impact query escaped configured scope: %s", expression)
		}
		items := make([]map[string]any, 0)
		if strings.Contains(expression, "increase(") {
			if strings.Contains(expression, "sum by") || !strings.Contains(expression, `instance=~`) {
				t.Fatalf("impact query hid per-member range coverage: %s", expression)
			}
			for _, member := range []string{"halro-0", "halro-1"} {
				for _, outcome := range impactOutcomes {
					if member == "halro-1" && outcome == missingIncreaseOutcome {
						continue
					}
					at := now
					if member == "halro-1" && outcome == staleIncreaseOutcome {
						at -= 60
					}
					item := map[string]any{"metric": map[string]string{"instance": member, "outcome": outcome}, "value": []any{at, "2"}}
					items = append(items, item)
					if member == "halro-1" && outcome == duplicateIncreaseOutcome {
						items = append(items, item)
					}
				}
			}
		} else {
			if strings.Contains(expression, `outcome="http_2xx"`) || !strings.Contains(expression, `instance=~`) || !strings.HasSuffix(expression, "[45s]") {
				t.Fatalf("coverage query did not inspect all fixed member outcomes: %s", expression)
			}
			for _, member := range []string{"halro-0", "halro-1"} {
				if missingMember == member {
					continue
				}
				for _, outcome := range impactOutcomes {
					if member == "halro-1" && outcome == missingOutcome {
						continue
					}
					at := now
					if member == "halro-1" && outcome == staleOutcome {
						at -= 60
					}
					item := map[string]any{"metric": map[string]string{"instance": member, "outcome": outcome}, "values": []any{[]any{at, "0"}}}
					items = append(items, item)
					if member == "halro-1" && outcome == duplicateOutcome {
						items = append(items, item)
					}
				}
			}
		}
		resultType := "vector"
		if !strings.Contains(expression, "increase(") {
			resultType = "matrix"
		}
		body, _ := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": resultType, "result": items}})
		return jsonResponse(string(body)), nil
	})}
	base, _ := url.Parse("http://127.0.0.1:9091")
	s := &server{client: client, base: base, environment: "test", cluster: "ha", members: []string{"halro-0", "halro-1"}}
	result, err := s.requestImpact(context.Background())
	if err != nil || !result.Observed || len(result.Counts) != len(impactOutcomes) || result.Counts["timeout"] != 4 || queries != 2 {
		t.Fatalf("complete coverage result=%+v err=%v queries=%d", result, err, queries)
	}
	for _, tc := range []struct {
		name, member, missing, duplicate, stale, missingIncrease string
		wantQueries                                              int
	}{
		{"missing member", "halro-1", "", "", "", "", 1},
		{"member missing one outcome", "", "timeout", "", "", "", 1},
		{"member has duplicate outcome", "", "", "timeout", "", "", 1},
		{"member has stale outcome", "", "", "", "timeout", "", 1},
		{"member has current counter but no range increase", "", "", "", "", "timeout", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			missingMember, missingOutcome, duplicateOutcome, staleOutcome, missingIncreaseOutcome = tc.member, tc.missing, tc.duplicate, tc.stale, tc.missingIncrease
			queries = 0
			result, err = s.requestImpact(context.Background())
			if err != nil || result.Observed || result.Counts != nil || queries != tc.wantQueries {
				t.Fatalf("partial member rollout became observed: result=%+v err=%v queries=%d", result, err, queries)
			}
		})
	}
	missingMember, missingOutcome, duplicateOutcome, staleOutcome, missingIncreaseOutcome = "", "", "", "", ""
	for _, mode := range []string{"duplicate", "stale"} {
		t.Run(mode+" range increase", func(t *testing.T) {
			duplicateIncreaseOutcome, staleIncreaseOutcome = "", ""
			if mode == "duplicate" {
				duplicateIncreaseOutcome = "timeout"
			} else {
				staleIncreaseOutcome = "timeout"
			}
			queries = 0
			result, err = s.requestImpact(context.Background())
			if err != nil || result.Observed || result.Counts != nil || queries != 2 {
				t.Fatalf("invalid member range result became observed: result=%+v err=%v queries=%d", result, err, queries)
			}
		})
	}
}
