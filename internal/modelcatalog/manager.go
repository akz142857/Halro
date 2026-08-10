package modelcatalog

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/akz142857/Halro/internal/safetransport"
)

const (
	RemoteCatalogURL  = "https://raw.githubusercontent.com/akz142857/Halro/main/catalog/model-catalog-v1.json"
	remoteCatalogHost = "raw.githubusercontent.com"
	LastKnownGoodFile = "model-catalog-lkg.json"
	SequenceFile      = "model-catalog-sequence.json"

	DefaultMaxDownloadBytes    int64 = 1 << 20
	DefaultMaxDecodedBytes     int64 = 4 << 20
	DefaultMaxCompressionRatio       = 20
	DefaultMaxEntries                = 10_000
)

type ManagerOptions struct {
	Enabled             bool
	PinnedRevision      string
	RefreshInterval     time.Duration
	DataDir             string
	TrustRoots          []TrustRoot
	ConnectTimeout      time.Duration
	ResponseTimeout     time.Duration
	MaxDownloadBytes    int64
	MaxDecodedBytes     int64
	MaxCompressionRatio int64
	MaxEntries          int
	Now                 func() time.Time
	Observer            func(RefreshEvent)

	// Test-only injection. Production callers leave these empty so both the URL
	// and SafeTransport policy remain compiled into the binary.
	HTTPClient *http.Client
	Endpoint   string
}

type CatalogState string

const (
	CatalogStateDisabled    CatalogState = "disabled"
	CatalogStateCurrent     CatalogState = "current"
	CatalogStateDegraded    CatalogState = "degraded"
	CatalogStateUnavailable CatalogState = "catalog_unavailable"
)

type ManagerStatus struct {
	Enabled         bool         `json:"enabled"`
	State           CatalogState `json:"state"`
	Source          Source       `json:"source"`
	Revision        string       `json:"revision"`
	Sequence        uint64       `json:"sequence,omitempty"`
	PinnedRevision  string       `json:"pinned_revision,omitempty"`
	LastAttemptAt   *time.Time   `json:"last_attempt_at,omitempty"`
	LastSuccessAt   *time.Time   `json:"last_success_at,omitempty"`
	DegradedSince   *time.Time   `json:"degraded_since,omitempty"`
	ErrorClass      string       `json:"error_class,omitempty"`
	ExpiresAt       *time.Time   `json:"expires_at,omitempty"`
	UsingExpiredLKG bool         `json:"using_expired_last_known_good,omitempty"`
}

type RefreshEvent struct {
	Status           ManagerStatus
	PreviousRevision string
	Outcome          string
	ErrorClass       string
	Recovered        bool
}

type Manager struct {
	options           ManagerOptions
	client            *http.Client
	endpoint          string
	lkgPath           string
	sequencePath      string
	active            atomic.Pointer[Catalog]
	refreshMu         sync.Mutex
	stateMu           sync.Mutex
	statusMu          sync.RWMutex
	status            ManagerStatus
	snapshot          Snapshot
	checkpoint        catalogCheckpoint
	checkpointErr     error
	observerMu        sync.RWMutex
	activationMu      sync.RWMutex
	prepareActivation func(*Catalog) (func(bool), error)
}

type catalogCheckpoint struct {
	Sequence uint64 `json:"sequence"`
	Revision string `json:"revision"`
}

func NewManager(options ManagerOptions) (*Manager, error) {
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.RefreshInterval <= 0 {
		options.RefreshInterval = 6 * time.Hour
	}
	if options.MaxDownloadBytes <= 0 {
		options.MaxDownloadBytes = DefaultMaxDownloadBytes
	}
	if options.MaxDecodedBytes <= 0 {
		options.MaxDecodedBytes = DefaultMaxDecodedBytes
	}
	if options.MaxCompressionRatio <= 0 {
		options.MaxCompressionRatio = DefaultMaxCompressionRatio
	}
	if options.MaxEntries <= 0 {
		options.MaxEntries = DefaultMaxEntries
	}
	if options.ConnectTimeout <= 0 {
		options.ConnectTimeout = 5 * time.Second
	}
	if options.ResponseTimeout <= 0 {
		options.ResponseTimeout = 30 * time.Second
	}
	if options.DataDir == "" {
		return nil, errors.New("model catalog data directory is required")
	}
	endpoint := options.Endpoint
	client := options.HTTPClient
	if client == nil {
		endpoint = RemoteCatalogURL
		policy := safetransport.Policy{RequireHTTPS: true, AllowedHosts: []string{remoteCatalogHost}}
		parsed, err := safetransport.ValidateURL(endpoint, policy)
		if err != nil {
			return nil, fmt.Errorf("validate compiled model catalog endpoint: %w", err)
		}
		endpoint = parsed.String()
		client, err = safetransport.NewClient(safetransport.Options{
			Policy: policy, ConnectTimeout: options.ConnectTimeout,
			ResponseHeaderTimeout: options.ResponseTimeout, MaxConnsPerHost: 1,
		})
		if err != nil {
			return nil, fmt.Errorf("build model catalog transport: %w", err)
		}
	}
	if strings.TrimSpace(endpoint) == "" {
		return nil, errors.New("model catalog endpoint is required")
	}
	manager := &Manager{
		options: options, client: client, endpoint: endpoint,
		lkgPath: filepath.Join(options.DataDir, LastKnownGoodFile), sequencePath: filepath.Join(options.DataDir, SequenceFile),
	}
	manager.active.Store(Builtin())
	bundledRevision := BundledSnapshot().CatalogRevision
	state := CatalogStateDisabled
	if options.Enabled {
		state = CatalogStateUnavailable
	}
	manager.status = ManagerStatus{
		Enabled: options.Enabled, State: state, Source: SourceBuiltin,
		Revision: bundledRevision, PinnedRevision: options.PinnedRevision,
	}
	if options.Enabled {
		manager.loadCheckpoint()
		manager.loadLastKnownGood()
	}
	return manager, nil
}

func (m *Manager) Current() *Catalog {
	m.expireIfNeeded()
	return m.active.Load()
}

func (m *Manager) SetObserver(observer func(RefreshEvent)) {
	m.observerMu.Lock()
	m.options.Observer = observer
	m.observerMu.Unlock()
}

// SetActivationPreparer installs a two-phase integration hook. The prepare
// phase must build all dependent state without mutating live traffic; its
// returned commit function is called only after LKG/checkpoint persistence and
// must not fail.
func (m *Manager) SetActivationPreparer(preparer func(*Catalog) (func(bool), error)) {
	m.activationMu.Lock()
	m.prepareActivation = preparer
	m.activationMu.Unlock()
}

func (m *Manager) Status() ManagerStatus {
	m.expireIfNeeded()
	m.statusMu.RLock()
	defer m.statusMu.RUnlock()
	return m.status
}

func (m *Manager) Run(ctx context.Context) {
	if !m.options.Enabled {
		return
	}
	_ = m.Refresh(ctx)
	ticker := time.NewTicker(m.options.RefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = m.Refresh(ctx)
		}
	}
}

func (m *Manager) Refresh(ctx context.Context) error {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	if !m.options.Enabled {
		return errors.New("dynamic model catalog updates are disabled")
	}
	now := m.options.Now().UTC()
	m.markAttempt(now)
	payload, err := m.download(ctx)
	if err != nil {
		return m.reject(now, "download", err)
	}
	snapshot, _, err := DecodeAndVerifySignedSnapshot(payload, VerifyOptions{
		Now: now, TrustRoots: m.options.TrustRoots, PinnedRevision: m.options.PinnedRevision,
		MaxEntries: m.options.MaxEntries,
	})
	if err != nil {
		return m.reject(now, classifyCatalogError(err), err)
	}
	m.statusMu.RLock()
	currentSequence, currentRevision := m.checkpoint.Sequence, m.checkpoint.Revision
	m.statusMu.RUnlock()
	if m.checkpointErr != nil {
		return m.reject(now, "rollback", fmt.Errorf("catalog sequence checkpoint is unavailable: %w", m.checkpointErr))
	}
	if currentSequence > 0 && snapshot.Sequence < currentSequence {
		return m.reject(now, "rollback", errors.New("catalog sequence rollback refused"))
	}
	if currentSequence == snapshot.Sequence && currentRevision != snapshot.CatalogRevision {
		return m.reject(now, "rollback", errors.New("catalog sequence was reused for different content"))
	}
	effective, err := MergeSnapshots(snapshot)
	if err != nil {
		return m.reject(now, "validation", err)
	}
	var finalize func(bool)
	m.activationMu.RLock()
	preparer := m.prepareActivation
	m.activationMu.RUnlock()
	if preparer != nil {
		finalize, err = preparer(effective)
		if err != nil {
			return m.reject(now, "activation", err)
		}
	}
	// Preparing dependent state may wait behind an Admin topology mutation.
	// Compare-and-swap the sequence/revision observed above before publishing so
	// a prepared candidate can never commit over a newer activation.
	now = m.options.Now().UTC()
	m.stateMu.Lock()
	m.statusMu.RLock()
	checkpointUnchanged := m.checkpoint == (catalogCheckpoint{Sequence: currentSequence, Revision: currentRevision})
	m.statusMu.RUnlock()
	if !checkpointUnchanged {
		m.stateMu.Unlock()
		if finalize != nil {
			finalize(false)
		}
		return m.reject(now, "activation", errors.New("catalog activation became stale"))
	}
	if !now.Before(snapshot.ExpiresAt) {
		m.stateMu.Unlock()
		if finalize != nil {
			finalize(false)
		}
		return m.reject(now, "expired", errors.New("catalog expired before activation commit"))
	}
	if err := writeAtomic(m.lkgPath, payload); err != nil {
		m.stateMu.Unlock()
		if finalize != nil {
			finalize(false)
		}
		return m.reject(now, "persistence", err)
	}
	checkpoint := catalogCheckpoint{Sequence: snapshot.Sequence, Revision: snapshot.CatalogRevision}
	if err := m.writeCheckpoint(checkpoint); err != nil {
		m.stateMu.Unlock()
		if finalize != nil {
			finalize(false)
		}
		return m.reject(now, "persistence", err)
	}
	if finalize != nil {
		finalize(true)
	}
	previous := currentRevision
	m.active.Store(effective)
	m.statusMu.Lock()
	recovered := m.status.State == CatalogStateDegraded || m.status.State == CatalogStateUnavailable
	expires := snapshot.ExpiresAt
	m.snapshot = snapshot
	m.checkpoint = checkpoint
	m.status.State = CatalogStateCurrent
	m.status.Source = SourceSignedCatalog
	m.status.Revision = snapshot.CatalogRevision
	m.status.Sequence = snapshot.Sequence
	m.status.LastSuccessAt = timePointer(now)
	m.status.DegradedSince = nil
	m.status.ErrorClass = ""
	m.status.ExpiresAt = &expires
	m.status.UsingExpiredLKG = false
	status := m.status
	m.statusMu.Unlock()
	m.stateMu.Unlock()
	m.observe(RefreshEvent{Status: status, PreviousRevision: previous, Outcome: "success", Recovered: recovered})
	return nil
}

func (m *Manager) download(ctx context.Context) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, m.endpoint, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	response, err := m.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("catalog endpoint returned HTTP %d", response.StatusCode)
	}
	if response.ContentLength > m.options.MaxDownloadBytes {
		return nil, errors.New("catalog compressed response exceeds byte limit")
	}
	compressed, err := readBounded(response.Body, m.options.MaxDownloadBytes)
	if err != nil {
		return nil, err
	}
	encoding := strings.ToLower(strings.TrimSpace(response.Header.Get("Content-Encoding")))
	switch encoding {
	case "", "identity":
		if int64(len(compressed)) > m.options.MaxDecodedBytes {
			return nil, errors.New("catalog decoded response exceeds byte limit")
		}
		return compressed, nil
	case "gzip":
		reader, err := gzip.NewReader(bytes.NewReader(compressed))
		if err != nil {
			return nil, errors.New("catalog gzip stream is invalid")
		}
		defer reader.Close()
		decoded, err := readBounded(reader, m.options.MaxDecodedBytes)
		if err != nil {
			return nil, err
		}
		if len(compressed) == 0 || int64(len(decoded)) > int64(len(compressed))*m.options.MaxCompressionRatio {
			return nil, errors.New("catalog gzip compression ratio exceeds limit")
		}
		return decoded, nil
	default:
		return nil, fmt.Errorf("catalog content encoding %q is not supported", encoding)
	}
}

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	payload, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(payload)) > limit {
		return nil, errors.New("catalog response exceeds byte limit")
	}
	return payload, nil
}

func (m *Manager) loadLastKnownGood() {
	file, err := os.Open(m.lkgPath)
	if err != nil {
		return
	}
	defer file.Close()
	payload, err := readBounded(file, m.options.MaxDecodedBytes)
	if err != nil {
		m.markDegraded(m.options.Now().UTC(), "size_limit")
		return
	}
	now := m.options.Now().UTC()
	snapshot, _, err := DecodeAndVerifySignedSnapshot(payload, VerifyOptions{
		Now: now, TrustRoots: m.options.TrustRoots, PinnedRevision: m.options.PinnedRevision,
		MaxEntries: m.options.MaxEntries, AllowExpired: true,
	})
	if err != nil {
		m.markDegraded(now, classifyCatalogError(err))
		return
	}
	effective, err := MergeSnapshots(snapshot)
	if err != nil {
		m.markDegraded(now, "validation")
		return
	}
	if m.checkpointErr != nil {
		m.markDegraded(now, "rollback")
		return
	}
	if m.checkpoint.Sequence > snapshot.Sequence || m.checkpoint.Sequence == snapshot.Sequence && m.checkpoint.Revision != "" && m.checkpoint.Revision != snapshot.CatalogRevision {
		m.markDegraded(now, "rollback")
		return
	}
	if m.checkpoint.Sequence < snapshot.Sequence || m.checkpoint.Revision == "" {
		checkpoint := catalogCheckpoint{Sequence: snapshot.Sequence, Revision: snapshot.CatalogRevision}
		if err := m.writeCheckpoint(checkpoint); err != nil {
			m.markDegraded(now, "persistence")
			return
		}
		m.checkpoint = checkpoint
	}
	expires := snapshot.ExpiresAt
	m.statusMu.Lock()
	m.snapshot = snapshot
	m.status.Source = SourceSignedCatalog
	m.status.Revision = snapshot.CatalogRevision
	m.status.Sequence = snapshot.Sequence
	m.status.ExpiresAt = &expires
	if now.Before(snapshot.ExpiresAt) {
		m.active.Store(effective)
		if m.options.Enabled {
			m.status.State = CatalogStateCurrent
		}
	} else {
		m.status.State = CatalogStateDegraded
		m.status.ErrorClass = "expired"
		m.status.DegradedSince = timePointer(now)
		m.status.UsingExpiredLKG = true
	}
	m.statusMu.Unlock()
}

func (m *Manager) loadCheckpoint() {
	payload, err := os.ReadFile(m.sequencePath)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		m.checkpointErr = err
		return
	}
	if len(payload) == 0 || len(payload) > 4096 {
		m.checkpointErr = errors.New("catalog sequence checkpoint size is invalid")
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var checkpoint catalogCheckpoint
	if err := decoder.Decode(&checkpoint); err != nil || checkpoint.Sequence == 0 || !strings.HasPrefix(checkpoint.Revision, "sha256:") {
		m.checkpointErr = errors.New("catalog sequence checkpoint is invalid")
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		m.checkpointErr = errors.New("catalog sequence checkpoint has trailing JSON")
		return
	}
	m.checkpoint = checkpoint
}

func (m *Manager) writeCheckpoint(checkpoint catalogCheckpoint) error {
	payload, err := json.Marshal(checkpoint)
	if err != nil {
		return err
	}
	return writeAtomic(m.sequencePath, payload)
}

func (m *Manager) expireIfNeeded() {
	if !m.options.Enabled {
		return
	}
	m.stateMu.Lock()
	now := m.options.Now().UTC()
	var event *RefreshEvent
	m.statusMu.Lock()
	if m.snapshot.Sequence > 0 && !now.Before(m.snapshot.ExpiresAt) && !m.status.UsingExpiredLKG {
		m.active.Store(Builtin())
		if m.status.DegradedSince == nil {
			m.status.DegradedSince = timePointer(now)
		}
		m.status.State = CatalogStateDegraded
		m.status.ErrorClass = "expired"
		m.status.UsingExpiredLKG = true
		status := m.status
		event = &RefreshEvent{Status: status, PreviousRevision: status.Revision, Outcome: "failure", ErrorClass: "expired"}
	}
	m.statusMu.Unlock()
	m.stateMu.Unlock()
	if event != nil {
		m.observe(*event)
	}
}

func (m *Manager) markAttempt(now time.Time) {
	m.statusMu.Lock()
	m.status.LastAttemptAt = timePointer(now)
	m.statusMu.Unlock()
}

func (m *Manager) reject(now time.Time, class string, err error) error {
	m.stateMu.Lock()
	m.statusMu.Lock()
	if m.status.DegradedSince == nil {
		m.status.DegradedSince = timePointer(now)
	}
	if m.snapshot.Sequence > 0 {
		m.status.State = CatalogStateDegraded
		// A failed refresh makes remote facts unavailable for new resolutions.
		// Keep the verified LKG metadata/checkpoint for recovery and rollback
		// protection, but return the effective catalog to reviewed bundled facts.
		m.active.Store(Builtin())
	} else {
		m.status.State = CatalogStateUnavailable
	}
	m.status.ErrorClass = class
	status := m.status
	m.statusMu.Unlock()
	m.stateMu.Unlock()
	m.observe(RefreshEvent{Status: status, PreviousRevision: status.Revision, Outcome: "failure", ErrorClass: class})
	return err
}

func (m *Manager) markDegraded(now time.Time, class string) {
	m.statusMu.Lock()
	m.status.State = CatalogStateUnavailable
	m.status.ErrorClass = class
	m.status.DegradedSince = timePointer(now)
	m.statusMu.Unlock()
}

func (m *Manager) observe(event RefreshEvent) {
	m.observerMu.RLock()
	observer := m.options.Observer
	m.observerMu.RUnlock()
	if observer != nil {
		observer(event)
	}
}

func classifyCatalogError(err error) string {
	message := err.Error()
	switch {
	case strings.Contains(message, "signature"):
		return "signature"
	case strings.Contains(message, "schema") || strings.Contains(message, "dictionary"):
		return "schema"
	case strings.Contains(message, "expired"):
		return "expired"
	case strings.Contains(message, "pinned"):
		return "pinned_revision"
	case strings.Contains(message, "byte limit") || strings.Contains(message, "compression ratio") || strings.Contains(message, "entries"):
		return "size_limit"
	default:
		return "validation"
	}
}

func writeAtomic(path string, payload []byte) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".model-catalog-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	_, writeErr := temporary.Write(payload)
	syncErr := temporary.Sync()
	closeErr := temporary.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	directoryHandle, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer directoryHandle.Close()
	return directoryHandle.Sync()
}

func timePointer(value time.Time) *time.Time { return &value }
