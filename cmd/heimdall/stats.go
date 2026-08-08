package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// heimdall stats answers "what is this instance doing right now" without an
// operator standing up Prometheus first — the reason Redis ships INFO. It reads
// the same metrics endpoint Prometheus would, and reduces it to the handful of
// means that explain this process's throughput ceilings.
//
// The counters behind it are cumulative since start, so a single snapshot is a
// lifetime average and dilutes a recent slowdown. Pass an interval to sample
// twice and report the window instead; the output says which one it is, because
// the difference decides whether the number means anything during an incident.

type statsSample map[string]float64

type statsFetcher func(context.Context) (statsSample, error)

func runStats(fetch statsFetcher, interval time.Duration, out io.Writer) error {
	if interval < 0 || interval > time.Minute {
		return errors.New("stats interval must be between zero and one minute")
	}
	ctx := context.Background()
	first, err := fetch(ctx)
	if err != nil {
		return err
	}
	window := "since start"
	sample := first
	if interval > 0 {
		timer := time.NewTimer(interval)
		defer timer.Stop()
		<-timer.C
		second, err := fetch(ctx)
		if err != nil {
			return err
		}
		sample = deltaSample(first, second)
		window = "over " + interval.String()
	}
	writeStatsReport(out, sample, window)
	return nil
}

// deltaSample subtracts two snapshots so the report describes a window rather
// than the process's whole life. Gauges are not counters, so a value that went
// down is taken from the newer snapshot rather than reported as negative work.
func deltaSample(first, second statsSample) statsSample {
	delta := make(statsSample, len(second))
	for name, newer := range second {
		older, seen := first[name]
		if !seen || newer < older {
			delta[name] = newer
			continue
		}
		delta[name] = newer - older
	}
	return delta
}

func writeStatsReport(out io.Writer, sample statsSample, window string) {
	writer := bufio.NewWriter(out)
	defer writer.Flush()

	syncs := sample["heimdall_wal_sync_seconds_count"]
	records := sample["heimdall_wal_append_records_total"]
	batches := sample["heimdall_wal_append_batches_total"]
	acquisitions := sample["heimdall_accounting_project_lock_acquisitions_total"]
	calls := sample["heimdall_metadata_batch_calls_total"]
	transactions := sample["heimdall_metadata_batch_transactions_total"]

	fmt.Fprintf(writer, "Durable write path (%s)\n", window)
	fmt.Fprintf(writer, "  Ledger fsync          %s   over %s barriers\n",
		statsMillis(sample["heimdall_wal_sync_seconds_sum"], syncs), statsCount(syncs))
	batchSize := statsRatio(records, batches)
	fmt.Fprintf(writer, "  Records per fsync     %s   over %s records\n",
		statsFactor(batchSize), statsCount(records))
	fmt.Fprintf(writer, "  WAL queue             %s / %s\n",
		statsCount(sample["heimdall_usage_queue_depth"]), statsCount(sample["heimdall_usage_queue_capacity"]))

	// One request lifecycle is five accounting records (ADR 0018), so the
	// per-project ceiling is reported in requests as well: that is the unit an
	// operator plans in. Approximate — a request that retries spends more than
	// five, so its real ceiling is lower.
	const recordsPerRequest = 5
	held := statsMean(sample["heimdall_accounting_project_lock_held_seconds_sum"], acquisitions)
	fmt.Fprintf(writer, "  Project lock wait     %s\n",
		statsMillis(sample["heimdall_accounting_project_lock_wait_seconds_sum"], acquisitions))
	fmt.Fprintf(writer, "  Project lock hold     %s", statsMillis(sample["heimdall_accounting_project_lock_held_seconds_sum"], acquisitions))
	if held > 0 {
		// One project's accounting writes serialize on this lock, so its
		// reciprocal is that project's ceiling however many requests it offers.
		fmt.Fprintf(writer, "   → one project ≈ %s requests/s", statsFactor(1/held/recordsPerRequest))
	}
	fmt.Fprintln(writer)
	fmt.Fprintf(writer, "  Metadata coalescing   %s   calls per write transaction\n",
		statsFactor(statsRatio(calls, transactions)))

	if errorCount := sample["heimdall_wal_append_errors_total"]; errorCount > 0 {
		fmt.Fprintf(writer, "\n  %s Ledger append errors — accounting is degraded\n", statsCount(errorCount))
	}
	// The two readings that look alike and are not: a full-looking batch means the
	// disk is the limit, a batch of one means nothing is arriving concurrently
	// enough to share a barrier. Only say so once there is enough traffic for the
	// distinction to be real.
	if batches >= 20 && batchSize > 0 && batchSize < 1.2 {
		fmt.Fprintln(writer, "\n  Appends are not coalescing. At this batch size the limit is offered")
		fmt.Fprintln(writer, "  concurrency or upstream serialization, not disk speed.")
	}
}

func statsMean(total, operations float64) float64 {
	if operations <= 0 {
		return 0
	}
	return total / operations
}

func statsRatio(numerator, denominator float64) float64 {
	if denominator <= 0 {
		return 0
	}
	return numerator / denominator
}

// statsMillis keeps three orders of magnitude readable in one column: a
// sub-millisecond NVMe fsync and a tens-of-milliseconds laptop fsync are both
// normal, and rounding either to "0 ms" would hide the number being asked for.
func statsMillis(total, operations float64) string {
	mean := statsMean(total, operations) * 1000
	switch {
	case mean == 0:
		return fmt.Sprintf("%9s", "—")
	case mean < 1:
		return fmt.Sprintf("%6.3f ms", mean)
	case mean < 10:
		return fmt.Sprintf("%6.2f ms", mean)
	default:
		return fmt.Sprintf("%6.1f ms", mean)
	}
}

func statsFactor(value float64) string {
	if value == 0 {
		return fmt.Sprintf("%9s", "—")
	}
	if value < 10 {
		return fmt.Sprintf("%9.2f", value)
	}
	return fmt.Sprintf("%9.1f", value)
}

func statsCount(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

// metricsSampleFetcher reads unlabelled samples from a running instance's
// metrics endpoint. Loopback only, no proxy and no redirects, matching
// healthcheck: this carries a bearer token, so it must not be talked into
// sending it somewhere else.
func metricsSampleFetcher(rawURL string, token []byte, timeout time.Duration) (statsFetcher, error) {
	if timeout <= 0 || timeout > 30*time.Second {
		return nil, errors.New("stats timeout must be between zero and 30 seconds")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("stats URL must be a plain HTTP(S) loopback URL")
	}
	hostname := parsed.Hostname()
	loopback := hostname == "localhost"
	if address, parseErr := netip.ParseAddr(hostname); parseErr == nil {
		loopback = address.IsLoopback()
	}
	if !loopback {
		return nil, errors.New("stats URL must use a loopback host")
	}
	client := &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("stats redirects are disabled")
		},
	}
	return func(ctx context.Context) (statsSample, error) {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
		if err != nil {
			return nil, errors.New("create stats request")
		}
		request.Header.Set("Authorization", "Bearer "+string(token))
		response, err := client.Do(request)
		if err != nil {
			// Naming the endpoint matters more here than the usual terseness:
			// this command reads a *running* instance, and the failure an
			// operator actually hits is that it is not up. "unavailable" alone
			// sends them looking at credentials and listeners instead. The URL
			// is loopback and operator-supplied, and userinfo is rejected above,
			// so there is nothing here to leak.
			return nil, fmt.Errorf("metrics endpoint %s is unavailable; is Heimdall running with metrics enabled?", parsed.Redacted())
		}
		defer response.Body.Close()
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
			return nil, errors.New("metrics endpoint rejected the derived token")
		}
		if response.StatusCode != http.StatusOK {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
			return nil, fmt.Errorf("metrics endpoint returned status %d", response.StatusCode)
		}
		return parseUnlabelledSamples(io.LimitReader(response.Body, 4<<20))
	}, nil
}

// parseUnlabelledSamples reads the Prometheus text exposition and keeps the
// samples that carry no labels, which is every series this report uses. Labelled
// series are skipped rather than summed: silently aggregating across labels would
// invent a number the endpoint never reported.
func parseUnlabelledSamples(reader io.Reader) (statsSample, error) {
	sample := make(statsSample)
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, rest, found := strings.Cut(line, " ")
		if !found || strings.ContainsAny(name, "{}") {
			continue
		}
		value, err := strconv.ParseFloat(strings.TrimSpace(rest), 64)
		if err != nil {
			continue
		}
		sample[name] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(sample) == 0 {
		return nil, errors.New("metrics endpoint returned no unlabelled samples")
	}
	return sample, nil
}

// statsMetricsURL derives the endpoint from the configured metrics listener so
// the usual case needs no flag. A listener bound to every interface is reached
// over loopback, which is the only host the fetcher will talk to anyway.
func statsMetricsURL(listen string, tlsEnabled bool) (string, error) {
	if strings.TrimSpace(listen) == "" {
		return "", errors.New("metrics listener is not configured; pass -url")
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("metrics listener %q is not host:port", listen)
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	scheme := "http"
	if tlsEnabled {
		scheme = "https"
	}
	return scheme + "://" + net.JoinHostPort(host, port) + "/metrics", nil
}
