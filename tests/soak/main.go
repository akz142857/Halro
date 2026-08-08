// Command soak runs an auditable long-duration Gateway workload and records
// resource/accounting samples without ever writing authentication secrets.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const releaseDuration = 24 * time.Hour

type options struct {
	gatewayURL      string
	metricsURL      string
	model           string
	commit          string
	outputDir       string
	pid             int
	duration        time.Duration
	sampleInterval  time.Duration
	requestInterval time.Duration
}

type sample struct {
	Time              time.Time `json:"time"`
	RSSBytes          uint64    `json:"rss_bytes"`
	Goroutines        int64     `json:"goroutines"`
	OpenFDs           int       `json:"open_fds"`
	WALQueueDepth     int64     `json:"wal_queue_depth"`
	WALQueueCapacity  int64     `json:"wal_queue_capacity"`
	WALAppendErrors   int64     `json:"wal_append_errors_total"`
	AnalyticsQueue    int64     `json:"analytics_queue_depth"`
	AnalyticsLagging  int64     `json:"analytics_lagging"`
	RequestsSucceeded uint64    `json:"requests_succeeded"`
	RequestsFailed    uint64    `json:"requests_failed"`
}

type limits struct {
	RSSGrowthBytes    uint64 `json:"rss_growth_bytes"`
	GoroutineGrowth   int64  `json:"goroutine_growth"`
	FDGrowth          int    `json:"fd_growth"`
	WALQueuePercent   int64  `json:"wal_queue_percent"`
	RequestFailurePct int64  `json:"request_failure_percent"`
}

type summary struct {
	Status     string        `json:"status"`
	Commit     string        `json:"commit"`
	StartedAt  time.Time     `json:"started_at"`
	FinishedAt time.Time     `json:"finished_at"`
	Duration   time.Duration `json:"duration_ns"`
	Samples    int           `json:"samples"`
	Start      sample        `json:"start"`
	End        sample        `json:"end"`
	Maximum    sample        `json:"maximum"`
	Limits     limits        `json:"limits"`
	Passed     bool          `json:"passed"`
	Failures   []string      `json:"failures,omitempty"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "soak:", err)
		os.Exit(1)
	}
}

func run() error {
	var opts options
	flag.StringVar(&opts.gatewayURL, "gateway-url", "http://127.0.0.1:8080/v1/chat/completions", "Gateway chat-completions URL")
	flag.StringVar(&opts.metricsURL, "metrics-url", "http://127.0.0.1:9090/metrics", "Metrics URL")
	flag.StringVar(&opts.model, "model", "chat", "public route alias")
	flag.StringVar(&opts.commit, "commit", "", "exact RC commit under test (required for a 24h release run)")
	flag.StringVar(&opts.outputDir, "output", "soak-artifacts", "new artifact directory")
	flag.IntVar(&opts.pid, "pid", 0, "Halro process PID")
	flag.DurationVar(&opts.duration, "duration", releaseDuration, "test duration")
	flag.DurationVar(&opts.sampleInterval, "sample-interval", time.Minute, "resource sample interval")
	flag.DurationVar(&opts.requestInterval, "request-interval", 10*time.Second, "interval between requests")
	flag.Parse()
	if opts.pid <= 0 || opts.duration <= 0 || opts.sampleInterval <= 0 || opts.requestInterval <= 0 {
		return errors.New("pid, duration, sample-interval, and request-interval must be positive")
	}
	if opts.duration >= releaseDuration && strings.TrimSpace(opts.commit) == "" {
		return errors.New("-commit is required for a 24-hour release run")
	}
	gatewayKey := os.Getenv("HALRO_GATEWAY_KEY")
	if gatewayKey == "" {
		return errors.New("HALRO_GATEWAY_KEY is required")
	}
	metricsToken := os.Getenv("HALRO_METRICS_TOKEN")
	if err := os.Mkdir(opts.outputDir, 0o700); err != nil {
		return fmt.Errorf("create fresh output directory: %w", err)
	}
	samplesFile, err := os.OpenFile(filepath.Join(opts.outputDir, "samples.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer samplesFile.Close()
	requestsFile, err := os.OpenFile(filepath.Join(opts.outputDir, "requests.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer requestsFile.Close()

	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), opts.duration)
	defer cancel()
	var succeeded, failed atomic.Uint64
	loadDone := make(chan struct{})
	go func() {
		defer close(loadDone)
		driveLoad(ctx, client, opts, gatewayKey, requestsFile, &succeeded, &failed)
	}()
	defer func() {
		cancel()
		<-loadDone
	}()

	started := time.Now().UTC()
	first, err := takeSample(ctx, client, opts, metricsToken, succeeded.Load(), failed.Load())
	if err != nil {
		return err
	}
	maximum := first
	if err := writeJSONLine(samplesFile, first); err != nil {
		return err
	}
	sampleCount := 1
	ticker := time.NewTicker(opts.sampleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			goto finished
		case <-ticker.C:
			current, sampleErr := takeSample(ctx, client, opts, metricsToken, succeeded.Load(), failed.Load())
			if sampleErr != nil {
				return sampleErr
			}
			updateMaximum(&maximum, current)
			if err := writeJSONLine(samplesFile, current); err != nil {
				return err
			}
			sampleCount++
		}
	}

finished:
	<-loadDone
	// Use a fresh context for the final sample after the duration context expires.
	finalCtx, finalCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer finalCancel()
	// Give cancellation settlement and the bounded Ledger batcher a quiet period
	// before evaluating the final queue depth.
	select {
	case <-time.After(5 * time.Second):
	case <-finalCtx.Done():
		return finalCtx.Err()
	}
	last, err := takeSample(finalCtx, client, opts, metricsToken, succeeded.Load(), failed.Load())
	if err != nil {
		return err
	}
	updateMaximum(&maximum, last)
	if err := writeJSONLine(samplesFile, last); err != nil {
		return err
	}
	sampleCount++
	report := evaluate(opts, started, first, last, maximum, sampleCount)
	reportBytes, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(opts.outputDir, "summary.json"), append(reportBytes, '\n'), 0o600); err != nil {
		return err
	}
	fmt.Printf("soak status=%s passed=%t samples=%d artifacts=%s\n", report.Status, report.Passed, report.Samples, opts.outputDir)
	if report.Status == "release_24h" && !report.Passed {
		return errors.New("24-hour release soak thresholds failed")
	}
	return nil
}

func driveLoad(ctx context.Context, client *http.Client, opts options, key string, output io.Writer, succeeded, failed *atomic.Uint64) {
	payload, _ := json.Marshal(map[string]any{
		"model":      opts.model,
		"messages":   []map[string]string{{"role": "user", "content": "Reply with the single word OK."}},
		"max_tokens": 8,
	})
	ticker := time.NewTicker(opts.requestInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case at := <-ticker.C:
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, opts.gatewayURL, bytes.NewReader(payload))
			if err != nil {
				failed.Add(1)
			} else {
				request.Header.Set("Authorization", "Bearer "+key)
				request.Header.Set("Content-Type", "application/json")
				response, requestErr := client.Do(request)
				if requestErr == nil {
					_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
					_ = response.Body.Close()
					if response.StatusCode == http.StatusOK {
						succeeded.Add(1)
					} else {
						failed.Add(1)
						err = errors.New("Gateway returned a non-200 status")
					}
				} else {
					failed.Add(1)
					err = requestErr
				}
			}
			_ = writeJSONLine(output, map[string]any{"time": at.UTC(), "ok": err == nil, "succeeded": succeeded.Load(), "failed": failed.Load()})
		}
	}
}

func takeSample(ctx context.Context, client *http.Client, opts options, token string, succeeded, failed uint64) (sample, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, opts.metricsURL, nil)
	if err != nil {
		return sample{}, err
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := client.Do(request)
	if err != nil {
		return sample{}, fmt.Errorf("read metrics: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return sample{}, fmt.Errorf("metrics returned status %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return sample{}, err
	}
	metrics := parseMetrics(string(body))
	for _, name := range []string{
		"halro_process_goroutines",
		"halro_usage_queue_depth",
		"halro_usage_queue_capacity",
		"halro_wal_append_errors_total",
		"halro_usage_analytics_queue_depth",
		"halro_usage_analytics_lagging",
	} {
		if _, ok := metrics[name]; !ok {
			return sample{}, fmt.Errorf("required metric %s is absent", name)
		}
	}
	rss, err := processRSS(opts.pid)
	if err != nil {
		return sample{}, err
	}
	fds, err := processFDs(opts.pid)
	if err != nil {
		return sample{}, err
	}
	return sample{
		Time: time.Now().UTC(), RSSBytes: rss, OpenFDs: fds,
		Goroutines:        metric(metrics, "halro_process_goroutines"),
		WALQueueDepth:     metric(metrics, "halro_usage_queue_depth"),
		WALQueueCapacity:  metric(metrics, "halro_usage_queue_capacity"),
		WALAppendErrors:   metric(metrics, "halro_wal_append_errors_total"),
		AnalyticsQueue:    metric(metrics, "halro_usage_analytics_queue_depth"),
		AnalyticsLagging:  metric(metrics, "halro_usage_analytics_lagging"),
		RequestsSucceeded: succeeded, RequestsFailed: failed,
	}, nil
}

func parseMetrics(body string) map[string]int64 {
	values := make(map[string]int64)
	for _, line := range strings.Split(body, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || strings.HasPrefix(fields[0], "#") || strings.Contains(fields[0], "{") {
			continue
		}
		value, err := strconv.ParseFloat(fields[1], 64)
		if err == nil {
			values[fields[0]] = int64(value)
		}
	}
	return values
}

func metric(values map[string]int64, name string) int64 { return values[name] }

func processRSS(pid int) (uint64, error) {
	if runtime.GOOS == "linux" {
		body, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
		if err == nil {
			for _, line := range strings.Split(string(body), "\n") {
				var kib uint64
				if _, scanErr := fmt.Sscanf(line, "VmRSS: %d kB", &kib); scanErr == nil {
					return kib * 1024, nil
				}
			}
		}
	}
	output, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, fmt.Errorf("read process RSS: %w", err)
	}
	kib, err := strconv.ParseUint(strings.TrimSpace(string(output)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse process RSS: %w", err)
	}
	return kib * 1024, nil
}

func processFDs(pid int) (int, error) {
	entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
	if err == nil {
		return len(entries), nil
	}
	output, err := exec.Command("lsof", "-a", "-p", strconv.Itoa(pid), "-Fn").Output()
	if err != nil {
		return 0, fmt.Errorf("count process file descriptors: %w", err)
	}
	count := 0
	for _, line := range strings.Split(string(output), "\n") {
		if len(line) > 1 && line[0] == 'f' && line[1] >= '0' && line[1] <= '9' {
			count++
		}
	}
	if count == 0 {
		return 0, errors.New("process has no countable file descriptors")
	}
	return count, nil
}

func evaluate(opts options, started time.Time, first, last, maximum sample, count int) summary {
	status := "smoke_only"
	if opts.duration >= releaseDuration {
		status = "release_24h"
	}
	rssAllowance := uint64(64 << 20)
	if quarter := first.RSSBytes / 4; quarter > rssAllowance {
		rssAllowance = quarter
	}
	thresholds := limits{RSSGrowthBytes: rssAllowance, GoroutineGrowth: 20, FDGrowth: 20, WALQueuePercent: 75, RequestFailurePct: 1}
	failures := make([]string, 0)
	if last.RSSBytes > first.RSSBytes+thresholds.RSSGrowthBytes {
		failures = append(failures, "RSS growth exceeded max(64 MiB, 25% of start)")
	}
	if last.Goroutines > first.Goroutines+thresholds.GoroutineGrowth {
		failures = append(failures, "goroutine growth exceeded 20")
	}
	if last.OpenFDs > first.OpenFDs+thresholds.FDGrowth {
		failures = append(failures, "open FD growth exceeded 20")
	}
	if last.WALQueueDepth != 0 {
		failures = append(failures, "final WAL queue is not empty")
	}
	if maximum.WALQueueCapacity <= 0 || maximum.WALQueueDepth*100 > maximum.WALQueueCapacity*thresholds.WALQueuePercent {
		failures = append(failures, "WAL queue exceeded 75% capacity")
	}
	if last.WALAppendErrors != first.WALAppendErrors {
		failures = append(failures, "WAL append error counter increased")
	}
	if last.AnalyticsLagging != 0 {
		failures = append(failures, "analytics remained lagging")
	}
	total := last.RequestsSucceeded + last.RequestsFailed
	if total == 0 || last.RequestsFailed*100 > total*uint64(thresholds.RequestFailurePct) {
		failures = append(failures, "request failure rate exceeded 1% or no request completed")
	}
	return summary{Status: status, Commit: opts.commit, StartedAt: started, FinishedAt: time.Now().UTC(), Duration: opts.duration, Samples: count, Start: first, End: last, Maximum: maximum, Limits: thresholds, Passed: len(failures) == 0, Failures: failures}
}

func updateMaximum(maximum *sample, current sample) {
	if current.RSSBytes > maximum.RSSBytes {
		maximum.RSSBytes = current.RSSBytes
	}
	if current.Goroutines > maximum.Goroutines {
		maximum.Goroutines = current.Goroutines
	}
	if current.OpenFDs > maximum.OpenFDs {
		maximum.OpenFDs = current.OpenFDs
	}
	if current.WALQueueDepth > maximum.WALQueueDepth {
		maximum.WALQueueDepth = current.WALQueueDepth
	}
	if current.WALQueueCapacity > maximum.WALQueueCapacity {
		maximum.WALQueueCapacity = current.WALQueueCapacity
	}
	if current.WALAppendErrors > maximum.WALAppendErrors {
		maximum.WALAppendErrors = current.WALAppendErrors
	}
	if current.AnalyticsQueue > maximum.AnalyticsQueue {
		maximum.AnalyticsQueue = current.AnalyticsQueue
	}
	if current.AnalyticsLagging > maximum.AnalyticsLagging {
		maximum.AnalyticsLagging = current.AnalyticsLagging
	}
	maximum.RequestsSucceeded = current.RequestsSucceeded
	maximum.RequestsFailed = current.RequestsFailed
}

func writeJSONLine(writer io.Writer, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	_, err = writer.Write(encoded)
	return err
}
