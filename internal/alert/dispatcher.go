package alert

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Event struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	Severity   string         `json:"severity"`
	DedupKey   string         `json:"dedup_key"`
	Summary    string         `json:"summary"`
	ProjectID  string         `json:"project_id,omitempty"`
	ProviderID string         `json:"provider_id,omitempty"`
	Timestamp  time.Time      `json:"timestamp"`
	Details    map[string]any `json:"details,omitempty"`
}

type Endpoint struct {
	ID         string
	URL        *url.URL
	HeaderName string
	secret     []byte
	client     *http.Client
}

func NewEndpoint(id string, endpointURL *url.URL, headerName string, secret []byte, client *http.Client) (Endpoint, error) {
	if id == "" || endpointURL == nil || client == nil {
		return Endpoint{}, errors.New("endpoint id, URL, and client are required")
	}
	if len(secret) > 0 && headerName == "" {
		return Endpoint{}, errors.New("secret header name is required")
	}
	return Endpoint{
		ID: id, URL: endpointURL, HeaderName: headerName,
		secret: bytes.Clone(secret), client: client,
	}, nil
}

func (e *Endpoint) Close() {
	if e == nil {
		return
	}
	clear(e.secret)
	e.secret = nil
	e.client.CloseIdleConnections()
}

type Config struct {
	QueueCapacity int
	Workers       int
	Timeout       time.Duration
	MaxAttempts   int
	BaseDelay     time.Duration
	MaxDelay      time.Duration
	DedupCooldown time.Duration

	// MaxConcurrentDeliveries bounds in-flight endpoint deliveries across the
	// whole dispatcher. Zero selects defaultMaxConcurrentDeliveries.
	MaxConcurrentDeliveries int
}

// defaultMaxConcurrentDeliveries bounds the fan-out below. Fan-out exists so one
// stalled endpoint cannot hold up the others, but unbounded it turns a burst of
// events across many endpoints into an unbounded goroutine and socket count,
// each holding an entire retry budget's worth of time. The bound is on in-flight
// deliveries rather than per worker, because that is the resource.
const defaultMaxConcurrentDeliveries = 32

type Dispatcher struct {
	config      Config
	endpointsMu sync.RWMutex
	endpoints   []Endpoint
	queue       chan Event
	// shutdown is closed by Close before it waits for workers, so a delivery
	// sitting in its retry backoff gives up instead of holding shutdown for the
	// rest of its budget.
	shutdown chan struct{}
	// deliverySlots is the fan-out bound; a token is held for the whole of one
	// endpoint delivery, retries included.
	deliverySlots chan struct{}
	submitMu      sync.RWMutex
	closed        bool
	mu            sync.Mutex
	lastSent      map[string]time.Time
	startOnce     sync.Once
	closeOnce     sync.Once
	wait          sync.WaitGroup
	accepted      atomic.Uint64
	delivered     atomic.Uint64
	failed        atomic.Uint64
	dropped       atomic.Uint64
	healthMu      sync.RWMutex
	unhealthy     map[string]bool
	lastDelivered time.Time
	lastFailed    time.Time
	observerMu    sync.RWMutex
	observer      func(DeliveryResult)
}

type Stats struct {
	Accepted           uint64
	Delivered          uint64
	Failed             uint64
	Dropped            uint64
	Queued             int
	QueueCapacity      int
	Endpoints          int
	UnhealthyEndpoints int
	UnknownEndpoints   int
	LastDeliveredAt    *time.Time `json:",omitempty"`
	LastFailedAt       *time.Time `json:",omitempty"`
}

type SubmissionStatus string

const (
	SubmissionQueued       SubmissionStatus = "queued"
	SubmissionDeduplicated SubmissionStatus = "deduplicated"
	SubmissionDropped      SubmissionStatus = "dropped"
	SubmissionClosed       SubmissionStatus = "closed"
)

type SubmissionResult struct {
	Accepted bool
	Status   SubmissionStatus
}

type DeliveryResult struct {
	EventID    string
	EventType  string
	ProjectID  string
	EndpointID string
	Outcome    string
	Reason     string
	Attempts   int
	OccurredAt time.Time
	// StatusCode and ResponseSnippet describe what the endpoint actually answered. Several
	// chat platforms accept the request with 200 and reject the payload in the body, so a
	// transport-level success alone cannot tell the operator the alert was received.
	StatusCode      int
	ResponseSnippet string
	LatencyMillis   int64
}

// responseSnippetLimit keeps enough of the reply to carry an error code without turning
// the audit trail or the console into a mirror for arbitrary remote content.
const responseSnippetLimit = 300

func (d *Dispatcher) SetObserver(observer func(DeliveryResult)) {
	d.observerMu.Lock()
	d.observer = observer
	d.observerMu.Unlock()
}

// TestEndpoint reports the delivery reason alongside the error: an operator debugging a
// webhook needs to tell "host unreachable" from "endpoint rejected the credential".
func (d *Dispatcher) TestEndpoint(id string, event Event) (DeliveryResult, error) {
	d.endpointsMu.RLock()
	var selected *Endpoint
	for index := range d.endpoints {
		if d.endpoints[index].ID == id {
			copy := d.endpoints[index]
			selected = &copy
			break
		}
	}
	d.endpointsMu.RUnlock()
	if selected == nil {
		return DeliveryResult{Outcome: "failure", Reason: "endpoint_inactive"},
			errors.New("alert endpoint is not active")
	}
	result := d.deliver(event, selected)
	d.recordDeliveryState(result)
	d.notify(result)
	if result.Outcome != "success" {
		return result, errors.New("alert endpoint delivery failed")
	}
	return result, nil
}

func New(config Config, endpoints []Endpoint) (*Dispatcher, error) {
	if config.QueueCapacity < 1 || config.Workers < 1 || config.Timeout <= 0 ||
		config.MaxAttempts < 1 || config.BaseDelay <= 0 ||
		config.MaxDelay < config.BaseDelay || config.DedupCooldown <= 0 {
		return nil, errors.New("invalid alert dispatcher configuration")
	}
	if config.MaxConcurrentDeliveries < 1 {
		config.MaxConcurrentDeliveries = defaultMaxConcurrentDeliveries
	}
	return &Dispatcher{
		config: config, endpoints: endpoints,
		queue:         make(chan Event, config.QueueCapacity),
		shutdown:      make(chan struct{}),
		deliverySlots: make(chan struct{}, config.MaxConcurrentDeliveries),
		lastSent:      make(map[string]time.Time),
		unhealthy:     make(map[string]bool),
	}, nil
}

func (d *Dispatcher) Start() {
	d.startOnce.Do(func() {
		for index := 0; index < d.config.Workers; index++ {
			d.wait.Add(1)
			go d.worker()
		}
	})
}

func (d *Dispatcher) Submit(event Event) bool {
	return d.SubmitWithResult(event).Accepted
}

func (d *Dispatcher) SubmitWithResult(event Event) SubmissionResult {
	d.submitMu.RLock()
	defer d.submitMu.RUnlock()
	if d.closed {
		return SubmissionResult{Status: SubmissionClosed}
	}
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now().UTC()
	}
	if event.DedupKey != "" {
		d.mu.Lock()
		last := d.lastSent[event.DedupKey]
		if !last.IsZero() && event.Timestamp.Sub(last) < d.config.DedupCooldown {
			d.mu.Unlock()
			return SubmissionResult{Accepted: true, Status: SubmissionDeduplicated}
		}
		d.lastSent[event.DedupKey] = event.Timestamp
		d.mu.Unlock()
	}
	select {
	case d.queue <- event:
		d.accepted.Add(1)
		return SubmissionResult{Accepted: true, Status: SubmissionQueued}
	default:
		if event.DedupKey != "" {
			d.mu.Lock()
			if d.lastSent[event.DedupKey].Equal(event.Timestamp) {
				delete(d.lastSent, event.DedupKey)
			}
			d.mu.Unlock()
		}
		d.dropped.Add(1)
		return SubmissionResult{Status: SubmissionDropped}
	}
}

func (d *Dispatcher) Close() {
	d.closeOnce.Do(func() {
		d.submitMu.Lock()
		d.closed = true
		close(d.queue)
		d.submitMu.Unlock()
		close(d.shutdown)
		d.wait.Wait()
		d.endpointsMu.Lock()
		endpoints := d.endpoints
		d.endpoints = nil
		d.endpointsMu.Unlock()
		for index := range endpoints {
			endpoints[index].Close()
		}
	})
}

// ReplaceEndpoints atomically changes alert destinations. The returned
// endpoints may still be referenced by an in-flight delivery and must only be
// closed after the caller's delivery grace period.
func (d *Dispatcher) ReplaceEndpoints(endpoints []Endpoint) []Endpoint {
	d.endpointsMu.Lock()
	previous := d.endpoints
	d.endpoints = endpoints
	d.endpointsMu.Unlock()
	active := make(map[string]struct{}, len(endpoints))
	for index := range endpoints {
		active[endpoints[index].ID] = struct{}{}
	}
	d.healthMu.Lock()
	for id := range d.unhealthy {
		if _, exists := active[id]; !exists {
			delete(d.unhealthy, id)
		}
	}
	d.healthMu.Unlock()
	return previous
}

func (d *Dispatcher) Stats() Stats {
	d.endpointsMu.RLock()
	endpointIDs := make([]string, 0, len(d.endpoints))
	for index := range d.endpoints {
		endpointIDs = append(endpointIDs, d.endpoints[index].ID)
	}
	d.endpointsMu.RUnlock()
	d.healthMu.RLock()
	unhealthyCount := 0
	observedCount := 0
	for _, id := range endpointIDs {
		if unhealthy, observed := d.unhealthy[id]; observed {
			observedCount++
			if unhealthy {
				unhealthyCount++
			}
		}
	}
	var lastDeliveredAt, lastFailedAt *time.Time
	if !d.lastDelivered.IsZero() {
		value := d.lastDelivered
		lastDeliveredAt = &value
	}
	if !d.lastFailed.IsZero() {
		value := d.lastFailed
		lastFailedAt = &value
	}
	d.healthMu.RUnlock()
	return Stats{
		Accepted: d.accepted.Load(), Delivered: d.delivered.Load(),
		Failed: d.failed.Load(), Dropped: d.dropped.Load(), Queued: len(d.queue),
		QueueCapacity: d.config.QueueCapacity, Endpoints: len(endpointIDs),
		UnhealthyEndpoints: unhealthyCount, UnknownEndpoints: len(endpointIDs) - observedCount,
		LastDeliveredAt: lastDeliveredAt, LastFailedAt: lastFailedAt,
	}
}

func (d *Dispatcher) recordDeliveryState(result DeliveryResult) {
	if result.EndpointID == "" {
		return
	}
	occurredAt := result.OccurredAt
	if occurredAt.IsZero() {
		occurredAt = time.Now().UTC()
	}
	d.healthMu.Lock()
	if result.Outcome == "success" {
		d.unhealthy[result.EndpointID] = false
		d.lastDelivered = occurredAt
	} else {
		d.unhealthy[result.EndpointID] = true
		d.lastFailed = occurredAt
	}
	d.healthMu.Unlock()
}

func (d *Dispatcher) worker() {
	defer d.wait.Done()
	for event := range d.queue {
		d.endpointsMu.RLock()
		endpoints := append([]Endpoint(nil), d.endpoints...)
		d.endpointsMu.RUnlock()
		// Fan out rather than looping: delivered serially, one endpoint that stalls until
		// its retry budget expires holds up every healthy endpoint behind it and blocks
		// this worker from draining the queue, so real alerts get dropped on overflow.
		var fanOut sync.WaitGroup
		for index := range endpoints {
			fanOut.Add(1)
			// Acquired before the goroutine starts, so a saturated dispatcher
			// makes this worker wait rather than pile up goroutines that will.
			d.deliverySlots <- struct{}{}
			go func(endpoint *Endpoint) {
				defer fanOut.Done()
				defer func() { <-d.deliverySlots }()
				result := d.deliver(event, endpoint)
				d.recordDeliveryState(result)
				if result.Outcome == "success" {
					d.delivered.Add(1)
				} else {
					d.failed.Add(1)
				}
				d.notify(result)
			}(&endpoints[index])
		}
		fanOut.Wait()
	}
}

func (d *Dispatcher) deliver(event Event, endpoint *Endpoint) DeliveryResult {
	result := DeliveryResult{
		EventID: event.ID, EventType: event.Type, ProjectID: event.ProjectID,
		EndpointID: endpoint.ID, Outcome: "failure", Reason: "retry_exhausted",
		OccurredAt: time.Now().UTC(),
	}
	payload, err := json.Marshal(event)
	if err != nil {
		result.Reason = "encode_error"
		return result
	}
	started := time.Now()
	for attempt := 0; attempt < d.config.MaxAttempts; attempt++ {
		result.Attempts = attempt + 1
		ctx, cancel := context.WithTimeout(context.Background(), d.config.Timeout)
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.URL.String(), bytes.NewReader(payload))
		if err == nil {
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("User-Agent", "halro-alert/1")
			if len(endpoint.secret) > 0 {
				request.Header.Set(endpoint.HeaderName, string(endpoint.secret))
			}
			var response *http.Response
			response, err = endpoint.client.Do(request)
			if err != nil {
				// Separated from retry_exhausted so an unreachable host reads differently
				// from an endpoint that answered and kept failing.
				result.Reason = "transport_error"
			}
			if response != nil {
				body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
				_, _ = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
				result.StatusCode = response.StatusCode
				result.ResponseSnippet = snippet(body)
				if response.StatusCode >= 200 && response.StatusCode < 300 {
					cancel()
					result.Outcome = "success"
					result.Reason = "delivered"
					result.OccurredAt = time.Now().UTC()
					result.LatencyMillis = time.Since(started).Milliseconds()
					return result
				}
				if response.StatusCode != http.StatusRequestTimeout &&
					response.StatusCode != http.StatusTooManyRequests &&
					response.StatusCode < 500 {
					cancel()
					result.Reason = "http_client_error"
					result.OccurredAt = time.Now().UTC()
					result.LatencyMillis = time.Since(started).Milliseconds()
					return result
				}
			}
		}
		cancel()
		if attempt+1 < d.config.MaxAttempts && !d.sleepJitter(d.config.BaseDelay, d.config.MaxDelay, attempt) {
			result.Reason = "retry_interrupted"
			result.OccurredAt = time.Now().UTC()
			result.LatencyMillis = time.Since(started).Milliseconds()
			return result
		}
	}
	result.OccurredAt = time.Now().UTC()
	result.LatencyMillis = time.Since(started).Milliseconds()
	return result
}

// snippet trims a reply to one printable line so it can be shown next to a test result.
func snippet(body []byte) string {
	text := strings.TrimSpace(string(body))
	text = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, text)
	if len(text) > responseSnippetLimit {
		return text[:responseSnippetLimit] + "…"
	}
	return text
}

func (d *Dispatcher) notify(result DeliveryResult) {
	d.observerMu.RLock()
	observer := d.observer
	d.observerMu.RUnlock()
	if observer != nil {
		observer(result)
	}
}

// sleepJitter reports whether the backoff completed. It returns false when the
// dispatcher is shutting down, which is what makes retry_interrupted reachable:
// the previous version always slept the full delay and always returned true, so
// Close waited out every in-flight retry budget and the reason was dead code.
func (d *Dispatcher) sleepJitter(base, maximum time.Duration, attempt int) bool {
	delay := base
	for index := 0; index < attempt && delay < maximum; index++ {
		if delay > maximum/2 {
			delay = maximum
			break
		}
		delay *= 2
	}
	if delay > maximum {
		delay = maximum
	}
	var random [1]byte
	if _, err := cryptorand.Read(random[:]); err == nil {
		delay = delay/2 + time.Duration(int64(delay/2)*int64(random[0])/255)
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-d.shutdown:
		return false
	}
}
