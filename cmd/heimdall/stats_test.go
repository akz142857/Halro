package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const statsExposition = `# HELP heimdall_wal_sync_seconds Cumulative time spent in Ledger durability barriers.
# TYPE heimdall_wal_sync_seconds summary
heimdall_wal_sync_seconds_sum 0.430000
heimdall_wal_sync_seconds_count 100
# TYPE heimdall_wal_append_records_total counter
heimdall_wal_append_records_total 825
heimdall_wal_append_batches_total 100
heimdall_usage_queue_depth 3
heimdall_usage_queue_capacity 4096
heimdall_accounting_project_lock_acquisitions_total 500
heimdall_accounting_project_lock_wait_seconds_sum 0.050000
heimdall_accounting_project_lock_held_seconds_sum 11.050000
heimdall_metadata_batch_calls_total 64
heimdall_metadata_batch_transactions_total 20
heimdall_requests_total{status="success"} 400
`

func fixedFetcher(sample statsSample) statsFetcher {
	return func(context.Context) (statsSample, error) { return sample, nil }
}

func TestStatsReportsTheDerivedMeans(t *testing.T) {
	sample, err := parseUnlabelledSamples(strings.NewReader(statsExposition))
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := runStats(fixedFetcher(sample), 0, &out); err != nil {
		t.Fatal(err)
	}
	report := out.String()
	for _, want := range []string{
		// 0.43s over 100 barriers.
		"4.30 ms",
		// 825 records in 100 batches.
		"8.25",
		// 11.05s held over 500 acquisitions, so one project tops out near 45/s.
		"22.1 ms",
		"45.2",
		// 64 batched calls coalesced into 20 transactions.
		"3.20",
		"3 / 4096",
		"since start",
	} {
		if !strings.Contains(report, want) {
			t.Fatalf("report is missing %q:\n%s", want, report)
		}
	}
}

// A labelled series must not be folded into an unlabelled name: summing across
// labels would report a number the endpoint never published.
func TestStatsIgnoresLabelledSeries(t *testing.T) {
	sample, err := parseUnlabelledSamples(strings.NewReader(statsExposition))
	if err != nil {
		t.Fatal(err)
	}
	if _, present := sample["heimdall_requests_total"]; present {
		t.Fatal("a labelled series was recorded under its bare family name")
	}
}

// An instance that has served nothing must read as idle rather than as a division
// by zero.
func TestStatsOnAnIdleInstance(t *testing.T) {
	var out strings.Builder
	if err := runStats(fixedFetcher(statsSample{"heimdall_usage_queue_capacity": 4096}), 0, &out); err != nil {
		t.Fatal(err)
	}
	report := out.String()
	if strings.Contains(report, "NaN") || strings.Contains(report, "+Inf") {
		t.Fatalf("idle instance produced a non-finite number:\n%s", report)
	}
	if !strings.Contains(report, "—") {
		t.Fatalf("idle instance did not report unavailable means:\n%s", report)
	}
}

// The window form is the one that means anything during an incident: cumulative
// counters dilute a slowdown that started a minute ago.
func TestStatsWindowSubtractsTheEarlierSample(t *testing.T) {
	first := statsSample{
		"heimdall_wal_sync_seconds_sum":   1,
		"heimdall_wal_sync_seconds_count": 100,
		"heimdall_usage_queue_depth":      7,
	}
	second := statsSample{
		// 10 further barriers costing 1s in total: 100 ms each, far worse than
		// the 10 ms lifetime average would suggest.
		"heimdall_wal_sync_seconds_sum":   2,
		"heimdall_wal_sync_seconds_count": 110,
		"heimdall_usage_queue_depth":      3,
	}
	delta := deltaSample(first, second)
	if delta["heimdall_wal_sync_seconds_count"] != 10 {
		t.Fatalf("counter delta=%v, want 10", delta["heimdall_wal_sync_seconds_count"])
	}
	// A gauge that fell is a level, not negative work.
	if delta["heimdall_usage_queue_depth"] != 3 {
		t.Fatalf("gauge delta=%v, want the newer level 3", delta["heimdall_usage_queue_depth"])
	}

	var out strings.Builder
	calls := 0
	fetch := func(context.Context) (statsSample, error) {
		calls++
		if calls == 1 {
			return first, nil
		}
		return second, nil
	}
	if err := runStats(fetch, time.Millisecond, &out); err != nil {
		t.Fatal(err)
	}
	if report := out.String(); !strings.Contains(report, "100.0 ms") || !strings.Contains(report, "over 1ms") {
		t.Fatalf("window report did not describe the window:\n%s", report)
	}
}

// Batch size near 1.0 and a saturated disk present identically as "slower
// requests"; the report has to say which one it is looking at.
func TestStatsCallsOutAppendsThatAreNotCoalescing(t *testing.T) {
	var out strings.Builder
	if err := runStats(fixedFetcher(statsSample{
		"heimdall_wal_append_records_total": 100,
		"heimdall_wal_append_batches_total": 100,
		"heimdall_wal_sync_seconds_sum":     1,
		"heimdall_wal_sync_seconds_count":   100,
	}), 0, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "not coalescing") {
		t.Fatalf("a batch size of 1.0 was not called out:\n%s", out.String())
	}

	// And it must stay quiet when appends are coalescing normally.
	var healthy strings.Builder
	if err := runStats(fixedFetcher(statsSample{
		"heimdall_wal_append_records_total": 800,
		"heimdall_wal_append_batches_total": 100,
	}), 0, &healthy); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(healthy.String(), "not coalescing") {
		t.Fatalf("a healthy batch size was called out:\n%s", healthy.String())
	}
}

func TestStatsRejectsIntervalsOutsideTheAllowedRange(t *testing.T) {
	for _, interval := range []time.Duration{-time.Second, 2 * time.Minute} {
		if err := runStats(fixedFetcher(statsSample{"x": 1}), interval, &strings.Builder{}); err == nil {
			t.Fatalf("interval %s was accepted", interval)
		}
	}
}

// The fetcher carries a bearer token, so it must refuse to send it anywhere but
// loopback — the same rule healthcheck follows.
func TestStatsFetcherRefusesNonLoopbackTargets(t *testing.T) {
	for _, endpoint := range []string{
		"http://example.com/metrics",
		"http://10.0.0.5:9090/metrics",
		"ftp://127.0.0.1/metrics",
		"http://user:pass@127.0.0.1:9090/metrics",
		"http://127.0.0.1:9090/metrics?token=leak",
	} {
		if _, err := metricsSampleFetcher(endpoint, []byte("token"), time.Second); err == nil {
			t.Fatalf("%q was accepted", endpoint)
		}
	}
	if _, err := metricsSampleFetcher("http://127.0.0.1:9090/metrics", []byte("token"), time.Second); err != nil {
		t.Fatalf("a loopback endpoint was rejected: %v", err)
	}
	if _, err := metricsSampleFetcher("http://127.0.0.1:9090/metrics", []byte("token"), time.Hour); err == nil {
		t.Fatal("an unbounded timeout was accepted")
	}
}

func TestStatsFetcherSendsTheTokenAndParsesTheBody(t *testing.T) {
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		authorization = request.Header.Get("Authorization")
		_, _ = writer.Write([]byte(statsExposition))
	}))
	defer server.Close()

	fetch, err := metricsSampleFetcher(server.URL+"/metrics", []byte("derived-token"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	sample, err := fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if authorization != "Bearer derived-token" {
		t.Fatalf("authorization header was %q", authorization)
	}
	if sample["heimdall_wal_append_records_total"] != 825 {
		t.Fatalf("parsed sample = %v", sample["heimdall_wal_append_records_total"])
	}
}

func TestStatsFetcherReportsRejectedCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	fetch, err := metricsSampleFetcher(server.URL+"/metrics", []byte("wrong"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fetch(context.Background()); err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("unauthorized was reported as %v", err)
	}
}

// The failure an operator actually hits is "the instance is not up", and the
// error has to point there. A bare "unavailable" sends them to check
// credentials and listeners first — which is what happened the first time this
// command was run for real.
func TestStatsFetcherNamesTheEndpointWhenNothingAnswers(t *testing.T) {
	// A port that accepted a connection and then closed, so nothing is listening.
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	endpoint := server.URL + "/metrics"
	server.Close()

	fetch, err := metricsSampleFetcher(endpoint, []byte("token"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, err = fetch(context.Background())
	if err == nil {
		t.Fatal("a closed endpoint was reported as reachable")
	}
	if !strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("error does not name the endpoint it tried: %v", err)
	}
	if !strings.Contains(err.Error(), "running") {
		t.Fatalf("error does not point at the likely cause: %v", err)
	}
	// The bearer token must not appear in an error an operator will paste.
	if strings.Contains(err.Error(), "token") {
		t.Fatalf("error leaked the credential: %v", err)
	}
}

func TestStatsMetricsURLFromListener(t *testing.T) {
	for _, testCase := range []struct {
		listen string
		tls    bool
		want   string
	}{
		{listen: "127.0.0.1:9090", want: "http://127.0.0.1:9090/metrics"},
		// A listener bound to every interface is still reached over loopback.
		{listen: "0.0.0.0:9090", want: "http://127.0.0.1:9090/metrics"},
		{listen: "[::]:9090", want: "http://127.0.0.1:9090/metrics"},
		{listen: "127.0.0.1:9090", tls: true, want: "https://127.0.0.1:9090/metrics"},
	} {
		got, err := statsMetricsURL(testCase.listen, testCase.tls)
		if err != nil {
			t.Fatalf("%s: %v", testCase.listen, err)
		}
		if got != testCase.want {
			t.Fatalf("%s -> %s, want %s", testCase.listen, got, testCase.want)
		}
	}
	for _, listen := range []string{"", "   ", "127.0.0.1"} {
		if _, err := statsMetricsURL(listen, false); err == nil {
			t.Fatalf("listener %q was accepted", listen)
		}
	}
}

func TestParseUnlabelledSamplesRejectsAnEmptyBody(t *testing.T) {
	if _, err := parseUnlabelledSamples(strings.NewReader("# HELP only comments\n")); err == nil {
		t.Fatal("a body with no samples was accepted")
	}
}

func TestStatsPropagatesFetchFailures(t *testing.T) {
	failure := errors.New("metrics endpoint is unavailable")
	fetch := func(context.Context) (statsSample, error) { return nil, failure }
	if err := runStats(fetch, 0, &strings.Builder{}); !errors.Is(err, failure) {
		t.Fatalf("error = %v", err)
	}
}
