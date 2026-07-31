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
}

type Dispatcher struct {
	config      Config
	endpointsMu sync.RWMutex
	endpoints   []Endpoint
	queue       chan Event
	submitMu    sync.RWMutex
	closed      bool
	mu          sync.Mutex
	lastSent    map[string]time.Time
	startOnce   sync.Once
	closeOnce   sync.Once
	wait        sync.WaitGroup
	accepted    atomic.Uint64
	delivered   atomic.Uint64
	failed      atomic.Uint64
	dropped     atomic.Uint64
	observerMu  sync.RWMutex
	observer    func(DeliveryResult)
}

type Stats struct {
	Accepted  uint64
	Delivered uint64
	Failed    uint64
	Dropped   uint64
	Queued    int
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
}

func (d *Dispatcher) SetObserver(observer func(DeliveryResult)) {
	d.observerMu.Lock()
	d.observer = observer
	d.observerMu.Unlock()
}

func (d *Dispatcher) TestEndpoint(id string, event Event) error {
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
		return errors.New("alert endpoint is not active")
	}
	result := d.deliver(event, selected)
	d.notify(result)
	if result.Outcome != "success" {
		return errors.New("alert endpoint delivery failed")
	}
	return nil
}

func New(config Config, endpoints []Endpoint) (*Dispatcher, error) {
	if config.QueueCapacity < 1 || config.Workers < 1 || config.Timeout <= 0 ||
		config.MaxAttempts < 1 || config.BaseDelay <= 0 ||
		config.MaxDelay < config.BaseDelay || config.DedupCooldown <= 0 {
		return nil, errors.New("invalid alert dispatcher configuration")
	}
	return &Dispatcher{
		config: config, endpoints: endpoints,
		queue: make(chan Event, config.QueueCapacity), lastSent: make(map[string]time.Time),
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
	return previous
}

func (d *Dispatcher) Stats() Stats {
	return Stats{
		Accepted: d.accepted.Load(), Delivered: d.delivered.Load(),
		Failed: d.failed.Load(), Dropped: d.dropped.Load(), Queued: len(d.queue),
	}
}

func (d *Dispatcher) worker() {
	defer d.wait.Done()
	for event := range d.queue {
		d.endpointsMu.RLock()
		endpoints := append([]Endpoint(nil), d.endpoints...)
		d.endpointsMu.RUnlock()
		for index := range endpoints {
			result := d.deliver(event, &endpoints[index])
			if result.Outcome == "success" {
				d.delivered.Add(1)
			} else {
				d.failed.Add(1)
			}
			d.notify(result)
		}
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
	for attempt := 0; attempt < d.config.MaxAttempts; attempt++ {
		result.Attempts = attempt + 1
		ctx, cancel := context.WithTimeout(context.Background(), d.config.Timeout)
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.URL.String(), bytes.NewReader(payload))
		if err == nil {
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("User-Agent", "heimdall-alert/1")
			if len(endpoint.secret) > 0 {
				request.Header.Set(endpoint.HeaderName, string(endpoint.secret))
			}
			var response *http.Response
			response, err = endpoint.client.Do(request)
			if response != nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
				_ = response.Body.Close()
				if response.StatusCode >= 200 && response.StatusCode < 300 {
					cancel()
					result.Outcome = "success"
					result.Reason = "delivered"
					result.OccurredAt = time.Now().UTC()
					return result
				}
				if response.StatusCode != http.StatusRequestTimeout &&
					response.StatusCode != http.StatusTooManyRequests &&
					response.StatusCode < 500 {
					cancel()
					result.Reason = "http_client_error"
					result.OccurredAt = time.Now().UTC()
					return result
				}
			}
		}
		cancel()
		if attempt+1 < d.config.MaxAttempts && !sleepJitter(d.config.BaseDelay, d.config.MaxDelay, attempt) {
			result.Reason = "retry_interrupted"
			result.OccurredAt = time.Now().UTC()
			return result
		}
	}
	result.OccurredAt = time.Now().UTC()
	return result
}

func (d *Dispatcher) notify(result DeliveryResult) {
	d.observerMu.RLock()
	observer := d.observer
	d.observerMu.RUnlock()
	if observer != nil {
		observer(result)
	}
}

func sleepJitter(base, maximum time.Duration, attempt int) bool {
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
	<-timer.C
	return true
}
