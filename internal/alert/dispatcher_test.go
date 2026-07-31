package alert

import (
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
		if result.Outcome != "failure" || result.Reason != "retry_exhausted" ||
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
	if err := dispatcher.TestEndpoint("new", Event{ID: "test", Type: "admin_test"}); err != nil {
		t.Fatal(err)
	}
	if newCalls.Load() != 1 || oldCalls.Load() != 0 {
		t.Fatalf("old=%d new=%d", oldCalls.Load(), newCalls.Load())
	}
	retired[0].Close()
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
