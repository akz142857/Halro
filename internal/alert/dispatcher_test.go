package alert

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestDispatcherRetriesAndKeepsSecretOutOfPayload(t *testing.T) {
	var attempts atomic.Int64
	delivered := make(chan struct{}, 1)
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(request.Body)
		if strings.Contains(string(body), "webhook-secret") {
			t.Fatal("webhook secret leaked into payload")
		}
		if request.Header.Get("Authorization") != "webhook-secret" {
			t.Fatal("secret header missing")
		}
		status := http.StatusInternalServerError
		if attempts.Add(1) >= 3 {
			status = http.StatusNoContent
			delivered <- struct{}{}
		}
		return &http.Response{
			StatusCode: status, Body: io.NopCloser(strings.NewReader("response")),
			Header: make(http.Header), Request: request,
		}, nil
	})}
	endpointURL, _ := url.Parse("https://hooks.example/alert")
	endpoint, err := NewEndpoint("webhook_1", endpointURL, "Authorization", []byte("webhook-secret"), client)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := New(Config{
		QueueCapacity: 4, Workers: 1, Timeout: time.Second, MaxAttempts: 3,
		BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond, DedupCooldown: time.Minute,
	}, []Endpoint{endpoint})
	if err != nil {
		t.Fatal(err)
	}
	if stats := dispatcher.Stats(); stats.UnknownEndpoints != 1 || stats.UnhealthyEndpoints != 0 {
		t.Fatalf("unobserved endpoint health stats=%#v", stats)
	}
	results := make(chan DeliveryResult, 1)
	dispatcher.SetObserver(func(result DeliveryResult) { results <- result })
	dispatcher.Start()
	if !dispatcher.Submit(Event{ID: "alert_1", DedupKey: "same", Type: "test", Timestamp: time.Now()}) {
		t.Fatal("event was not queued")
	}
	select {
	case <-delivered:
	case <-time.After(time.Second):
		t.Fatal("delivery timed out")
	}
	select {
	case result := <-results:
		if result.EventID != "alert_1" || result.EndpointID != "webhook_1" ||
			result.Outcome != "success" || result.Reason != "delivered" ||
			result.Attempts != 3 {
			t.Fatalf("unexpected delivery result: %#v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("delivery audit result timed out")
	}
	dispatcher.Close()
	if attempts.Load() != 3 || dispatcher.Stats().Delivered != 1 {
		t.Fatalf("attempts=%d stats=%#v", attempts.Load(), dispatcher.Stats())
	}
}

func TestDispatcherDeduplicatesAndDropsWhenQueueIsFull(t *testing.T) {
	endpointURL, _ := url.Parse("https://hooks.example/alert")
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	})}
	endpoint, _ := NewEndpoint("webhook_1", endpointURL, "", nil, client)
	dispatcher, _ := New(Config{
		QueueCapacity: 1, Workers: 1, Timeout: time.Millisecond, MaxAttempts: 1,
		BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, DedupCooldown: time.Minute,
	}, []Endpoint{endpoint})
	results := make(chan DeliveryResult, 1)
	dispatcher.SetObserver(func(result DeliveryResult) { results <- result })
	now := time.Now()
	first := dispatcher.SubmitWithResult(Event{DedupKey: "one", Timestamp: now})
	if !first.Accepted || first.Status != SubmissionQueued {
		t.Fatal("first event was rejected")
	}
	duplicate := dispatcher.SubmitWithResult(Event{DedupKey: "one", Timestamp: now.Add(time.Second)})
	if !duplicate.Accepted || duplicate.Status != SubmissionDeduplicated {
		t.Fatal("duplicate should be treated as handled")
	}
	dropped := dispatcher.SubmitWithResult(Event{DedupKey: "two", Timestamp: now})
	if dropped.Accepted || dropped.Status != SubmissionDropped {
		t.Fatal("full queue accepted another event")
	}
	if dispatcher.Stats().Dropped != 1 {
		t.Fatalf("stats=%#v", dispatcher.Stats())
	}
	dispatcher.Start()
	dispatcher.Close()
	select {
	case result := <-results:
		// An unreachable host is reported as a transport failure, not as an exhausted
		// retry budget: the operator has to be able to tell those apart.
		if result.Outcome != "failure" || result.Reason != "transport_error" ||
			result.Attempts != 1 {
			t.Fatalf("unexpected failure result: %#v", result)
		}
	default:
		t.Fatal("failed delivery did not produce an audit result")
	}
}

func TestDispatcherReplacesEndpointsAndTestsOneDestination(t *testing.T) {
	var oldCalls atomic.Int64
	var newCalls atomic.Int64
	makeEndpoint := func(id string, calls *atomic.Int64) Endpoint {
		endpointURL, _ := url.Parse("https://hooks.example/" + id)
		endpoint, err := NewEndpoint(id, endpointURL, "", nil, &http.Client{
			Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				calls.Add(1)
				return &http.Response{
					StatusCode: http.StatusNoContent,
					Body:       io.NopCloser(strings.NewReader("")),
					Header:     make(http.Header),
					Request:    request,
				}, nil
			}),
		})
		if err != nil {
			t.Fatal(err)
		}
		return endpoint
	}
	oldEndpoint := makeEndpoint("old", &oldCalls)
	dispatcher, err := New(Config{
		QueueCapacity: 1, Workers: 1, Timeout: time.Second, MaxAttempts: 1,
		BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, DedupCooldown: time.Minute,
	}, []Endpoint{oldEndpoint})
	if err != nil {
		t.Fatal(err)
	}
	newEndpoint := makeEndpoint("new", &newCalls)
	retired := dispatcher.ReplaceEndpoints([]Endpoint{newEndpoint})
	if len(retired) != 1 || retired[0].ID != "old" {
		t.Fatalf("unexpected retired endpoints: %#v", retired)
	}
	result, err := dispatcher.TestEndpoint("new", Event{ID: "test", Type: "admin_test"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Reason != "delivered" {
		t.Fatalf("test delivery reason=%q", result.Reason)
	}
	if newCalls.Load() != 1 || oldCalls.Load() != 0 {
		t.Fatalf("old=%d new=%d", oldCalls.Load(), newCalls.Load())
	}
	stats := dispatcher.Stats()
	if stats.Endpoints != 1 || stats.UnhealthyEndpoints != 0 || stats.UnknownEndpoints != 0 || stats.QueueCapacity != 1 || stats.LastDeliveredAt == nil {
		t.Fatalf("delivery health stats=%#v", stats)
	}
	retired[0].Close()
	dispatcher.Close()
}

func TestEndpointHealthTracksCurrentOutcomeInsteadOfLifetimeFailures(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	endpointURL, _ := url.Parse("https://hooks.example/current-health")
	endpoint, err := NewEndpoint("current", endpointURL, "", nil, &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		status := http.StatusNoContent
		if fail.Load() {
			status = http.StatusInternalServerError
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header), Request: request}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := New(Config{QueueCapacity: 1, Workers: 1, Timeout: time.Second, MaxAttempts: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, DedupCooldown: time.Minute}, []Endpoint{endpoint})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dispatcher.TestEndpoint("current", Event{ID: "failed"}); err == nil {
		t.Fatal("failed delivery unexpectedly succeeded")
	}
	if stats := dispatcher.Stats(); stats.UnhealthyEndpoints != 1 || stats.UnknownEndpoints != 0 || stats.LastFailedAt == nil {
		t.Fatalf("failed health stats=%#v", stats)
	}
	fail.Store(false)
	if _, err := dispatcher.TestEndpoint("current", Event{ID: "recovered"}); err != nil {
		t.Fatal(err)
	}
	if stats := dispatcher.Stats(); stats.UnhealthyEndpoints != 0 || stats.LastDeliveredAt == nil {
		t.Fatalf("recovered health stats=%#v", stats)
	}
	dispatcher.Close()
}

func TestDispatcherConcurrentSubmitAndCloseNeverPanics(t *testing.T) {
	dispatcher, err := New(Config{
		QueueCapacity: 32, Workers: 2, Timeout: time.Second, MaxAttempts: 1,
		BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, DedupCooldown: time.Minute,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher.Start()
	var wait sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			for index := 0; index < 100; index++ {
				dispatcher.SubmitWithResult(Event{
					ID: "event", DedupKey: "", Type: "test", Timestamp: time.Now().UTC(),
				})
			}
		}(worker)
	}
	dispatcher.Close()
	wait.Wait()
	if result := dispatcher.SubmitWithResult(Event{ID: "after-close"}); result.Status != SubmissionClosed {
		t.Fatalf("post-close submission result=%#v", result)
	}
}

// Fan-out exists so one stalled endpoint cannot hold up the others. Unbounded,
// it turns an event addressed to many endpoints into as many simultaneous
// goroutines and sockets, each holding a full retry budget's worth of time.
func TestFanOutIsBounded(t *testing.T) {
	var inFlight, peak atomic.Int64
	release := make(chan struct{})
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		current := inFlight.Add(1)
		for {
			observed := peak.Load()
			if current <= observed || peak.CompareAndSwap(observed, current) {
				break
			}
		}
		<-release
		inFlight.Add(-1)
		return &http.Response{
			StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader("")),
			Header: make(http.Header), Request: request,
		}, nil
	})}
	endpointURL, _ := url.Parse("https://hooks.example/alert")
	endpoints := make([]Endpoint, 0, 12)
	for index := 0; index < 12; index++ {
		endpoint, err := NewEndpoint(fmt.Sprintf("webhook_%d", index), endpointURL, "", nil, client)
		if err != nil {
			t.Fatal(err)
		}
		endpoints = append(endpoints, endpoint)
	}
	dispatcher, err := New(Config{
		QueueCapacity: 4, Workers: 1, Timeout: time.Second, MaxAttempts: 1,
		BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond, DedupCooldown: time.Minute,
		MaxConcurrentDeliveries: 3,
	}, endpoints)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher.Start()
	if !dispatcher.Submit(Event{ID: "alert_fanout", Type: "test", Timestamp: time.Now()}) {
		t.Fatal("event was not queued")
	}
	deadline := time.After(2 * time.Second)
	for peak.Load() < 3 {
		select {
		case <-deadline:
			t.Fatalf("fan-out never reached the bound: peak=%d", peak.Load())
		default:
		}
		time.Sleep(time.Millisecond)
	}
	// Held long enough that anything unbounded would have piled up by now.
	time.Sleep(20 * time.Millisecond)
	if observed := peak.Load(); observed > 3 {
		t.Fatalf("fan-out exceeded its bound: peak=%d", observed)
	}
	close(release)
	dispatcher.Close()
	if delivered := dispatcher.Stats().Delivered; delivered != 12 {
		t.Fatalf("bounded fan-out lost deliveries: delivered=%d", delivered)
	}
}

// Close used to wait out every in-flight retry budget, because the backoff slept
// on a bare timer and reported success unconditionally — which also left
// retry_interrupted unreachable. Shutdown now cuts the backoff short.
func TestCloseInterruptsRetryBackoff(t *testing.T) {
	var attempts atomic.Int64
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		attempts.Add(1)
		return &http.Response{
			StatusCode: http.StatusInternalServerError, Body: io.NopCloser(strings.NewReader("nope")),
			Header: make(http.Header), Request: request,
		}, nil
	})}
	endpointURL, _ := url.Parse("https://hooks.example/alert")
	endpoint, err := NewEndpoint("webhook_slow", endpointURL, "", nil, client)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := New(Config{
		QueueCapacity: 4, Workers: 1, Timeout: time.Second, MaxAttempts: 5,
		BaseDelay: 30 * time.Second, MaxDelay: time.Minute, DedupCooldown: time.Minute,
	}, []Endpoint{endpoint})
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan DeliveryResult, 1)
	dispatcher.SetObserver(func(result DeliveryResult) { results <- result })
	dispatcher.Start()
	if !dispatcher.Submit(Event{ID: "alert_slow", Type: "test", Timestamp: time.Now()}) {
		t.Fatal("event was not queued")
	}
	for attempts.Load() == 0 {
		time.Sleep(time.Millisecond)
	}

	closed := make(chan struct{})
	go func() { dispatcher.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close waited for the retry backoff")
	}
	select {
	case result := <-results:
		if result.Reason != "retry_interrupted" {
			t.Fatalf("unexpected shutdown reason: %#v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("no delivery result was reported for the interrupted retry")
	}
	if delivered := attempts.Load(); delivered != 1 {
		t.Fatalf("the backoff was not cut short: attempts=%d", delivered)
	}
}
