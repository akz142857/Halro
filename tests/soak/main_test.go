package main

import (
	"testing"
	"time"
)

func TestEvaluateDistinguishesSmokeFromReleaseAndEnforcesBounds(t *testing.T) {
	base := sample{RSSBytes: 100 << 20, Goroutines: 20, OpenFDs: 10, WALQueueCapacity: 100, RequestsSucceeded: 100}
	last := base
	maximum := base
	maximum.WALQueueDepth = 50
	report := evaluate(options{duration: time.Minute}, time.Now(), base, last, maximum, 2)
	if report.Status != "smoke_only" || !report.Passed {
		t.Fatalf("unexpected smoke report: %#v", report)
	}
	report = evaluate(options{duration: releaseDuration}, time.Now(), base, last, maximum, 2)
	if report.Status != "release_24h" || !report.Passed {
		t.Fatalf("unexpected release report: %#v", report)
	}
	last.Goroutines = 41
	report = evaluate(options{duration: releaseDuration}, time.Now(), base, last, maximum, 2)
	if report.Passed || len(report.Failures) == 0 {
		t.Fatal("goroutine leak passed release evaluation")
	}
}

func TestParseMetricsIgnoresLabels(t *testing.T) {
	metrics := parseMetrics("# HELP x x\nhalro_usage_queue_depth 3\nhalro_requests_total{status=\"ok\"} 7\n")
	if metrics["halro_usage_queue_depth"] != 3 || len(metrics) != 1 {
		t.Fatalf("unexpected metrics: %#v", metrics)
	}
}
