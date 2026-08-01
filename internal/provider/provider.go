package provider

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/akz142857/Heimdall/internal/anthropicapi"
	"github.com/akz142857/Heimdall/internal/domain"
	"github.com/akz142857/Heimdall/internal/openaiapi"
	"github.com/akz142857/Heimdall/internal/semantic"
)

type ErrorClass string

const (
	ErrorAuthentication ErrorClass = "authentication"
	ErrorRateLimit      ErrorClass = "rate_limit"
	ErrorTimeout        ErrorClass = "timeout"
	ErrorProvider5xx    ErrorClass = "provider_5xx"
	ErrorBadRequest     ErrorClass = "bad_request"
	ErrorConnect        ErrorClass = "connect"
	ErrorMalformed      ErrorClass = "malformed_response"
	ErrorUnknown        ErrorClass = "unknown"
)

type Error struct {
	Class             ErrorClass
	StatusCode        int
	Retryable         bool
	Ambiguous         bool
	Message           string
	ProviderRequestID string
	ProviderCode      string
	RetryAfter        time.Duration
	Cause             error
}

func (e *Error) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return string(e.Class)
}

func (e *Error) Unwrap() error {
	return e.Cause
}

type ChatCall struct {
	RequestID     string
	ProviderModel string
	Request       openaiapi.ChatCompletionRequest
}

type EmbeddingCall struct {
	RequestID     string
	ProviderModel string
	Request       openaiapi.EmbeddingRequest
}

type NativeMessageCall struct {
	RequestID     string
	ProviderModel string
	Version       string
	Payload       []byte
}

type NativeMessageResult struct {
	Payload           []byte
	ProviderRequestID string
	RetryAfter        time.Duration
}

type NativeMessagesAdapter interface {
	MessagesNative(context.Context, NativeMessageCall) (NativeMessageResult, error)
	MessagesNativeStream(context.Context, NativeMessageCall, func(anthropicapi.RawStreamEvent) error) (*anthropicapi.Usage, error)
}

type Adapter interface {
	Type() string
	Chat(context.Context, ChatCall) (openaiapi.ChatCompletionResponse, error)
	ChatStream(context.Context, ChatCall, func(semantic.Event) error) (*openaiapi.Usage, error)
	Embed(context.Context, EmbeddingCall) (openaiapi.EmbeddingResponse, error)
	Close()
}

type Capabilities struct {
	Chat             bool
	Streaming        bool
	Embeddings       bool
	Moderations      bool
	Images           bool
	Transcriptions   bool
	Speech           bool
	Files            bool
	Batches          bool
	Rerank           bool
	AsyncGenerate    bool
	Tools            bool
	Vision           bool
	JSONMode         bool
	DeveloperRole    bool
	Reasoning        bool
	StreamUsage      bool
	MaxContextTokens int64
	MaxOutputTokens  int64
}

type CapabilityReporter interface {
	Capabilities() Capabilities
}

type Prober interface {
	Probe(context.Context, string) error
}

type Operation string

const (
	OperationChat           Operation = "chat"
	OperationChatStream     Operation = "chat_stream"
	OperationEmbeddings     Operation = "embeddings"
	OperationMessages       Operation = "anthropic_messages"
	OperationMessagesStream Operation = "anthropic_messages_stream"
	OperationModerations    Operation = "moderations"
	OperationImages         Operation = "images"
	OperationTranscriptions Operation = "audio_transcriptions"
	OperationSpeech         Operation = "audio_speech"
	OperationFiles          Operation = "files"
	OperationBatches        Operation = "batches"
	OperationRerank         Operation = "rerank"
	OperationAsyncInvoke    Operation = "async_invoke"
)

type Target struct {
	ID                     string
	DeploymentID           string
	ProviderID             string
	PublicModel            string
	ProviderModel          string
	AccessSurface          domain.AccessSurface
	ProfileID              domain.ProviderProfileID
	Region                 string
	Adapter                Adapter
	InputMicrosPerMillion  int64
	OutputMicrosPerMillion int64
	FixedRequestMicrosUSD  int64
	Priority               int
	Strategy               string
	Capabilities           Capabilities
	CapabilityEvidence     domain.CapabilityEvidenceSet
	MaxConcurrency         int64
	DeploymentConcurrency  int64
	// LegacyUnprofiled marks adapters that have not crossed the versioned
	// ProfiledAdapter boundary. They remain usable for the original core
	// operations, but optional portable semantics must fail closed.
	LegacyUnprofiled bool
	operations       OperationRegistry
}

type Registry struct {
	mu       sync.RWMutex
	targets  map[string][]Target
	next     map[string]uint64
	adapters map[string]Adapter
	health   map[string]bool
}

func NewRegistry() *Registry {
	return &Registry{
		targets: make(map[string][]Target), next: make(map[string]uint64),
		adapters: make(map[string]Adapter),
		health:   make(map[string]bool),
	}
}

// RegisterAdapter makes an enabled provider independently addressable for
// health probes, even when it is not referenced by a route yet.
func (r *Registry) RegisterAdapter(providerID string, adapter Adapter) error {
	if providerID == "" || adapter == nil {
		return errors.New("provider id and adapter are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.adapters[providerID]; exists {
		return errors.New("provider adapter is already registered")
	}
	r.adapters[providerID] = adapter
	return nil
}

func (r *Registry) Register(target Target) error {
	if target.ID == "" || target.PublicModel == "" || target.ProviderModel == "" || target.Adapter == nil {
		return errors.New("target id, public model, provider model, and adapter are required")
	}
	if profiled, ok := target.Adapter.(ProfiledAdapter); ok {
		manifest := profiled.Profile()
		if target.ProfileID == "" {
			target.ProfileID = manifest.ID
		}
		if target.AccessSurface == "" {
			target.AccessSurface = manifest.AccessSurface
		}
		if target.ProfileID != manifest.ID || target.AccessSurface != manifest.AccessSurface {
			return errors.New("target profile does not match adapter profile")
		}
		if len(target.CapabilityEvidence) == 0 {
			target.CapabilityEvidence = profiled.CapabilityEvidence()
		}
		target.operations = profiled.Operations()
	} else {
		if target.ProfileID != "" || target.AccessSurface != "" {
			return errors.New("unprofiled adapter cannot claim a provider profile or access surface")
		}
		target.LegacyUnprofiled = true
	}
	if target.Strategy == "" {
		target.Strategy = "ordered"
	}
	if target.Strategy != "ordered" && target.Strategy != "round_robin" {
		return errors.New("route strategy must be ordered or round_robin")
	}
	if !target.Capabilities.Chat && !target.Capabilities.Embeddings {
		if reporter, ok := target.Adapter.(CapabilityReporter); ok {
			target.Capabilities = reporter.Capabilities()
		} else {
			// Preserve compatibility for built-in test and extension adapters.
			target.Capabilities = Capabilities{
				Chat: true, Streaming: true, Embeddings: true,
			}
		}
	}
	if target.LegacyUnprofiled {
		// A legacy adapter has no immutable profile contract proving optional
		// semantic support. Preserve its core operations, while preventing old
		// capability booleans from claiming richer portable semantics.
		target.Capabilities.Tools = false
		target.Capabilities.Vision = false
		target.Capabilities.JSONMode = false
		target.Capabilities.DeveloperRole = false
		target.Capabilities.Reasoning = false
		target.Capabilities.StreamUsage = false
	}
	if target.LegacyUnprofiled && len(target.CapabilityEvidence) == 0 {
		target.CapabilityEvidence = domain.EvidenceForCapabilities(
			domain.ProviderCapabilities{
				Chat: target.Capabilities.Chat, Streaming: target.Capabilities.Streaming,
				Embeddings: target.Capabilities.Embeddings,
			},
			domain.EvidenceLegacy,
		)
	}
	target = cloneTarget(target)
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, existing := range r.targets[target.PublicModel] {
		if existing.Strategy != target.Strategy {
			return errors.New("all targets for a public model must use the same strategy")
		}
		if existing.ID == target.ID {
			return errors.New("target id is already registered for public model")
		}
	}
	r.targets[target.PublicModel] = append(r.targets[target.PublicModel], target)
	slices.SortFunc(r.targets[target.PublicModel], func(left, right Target) int {
		if left.Priority != right.Priority {
			return left.Priority - right.Priority
		}
		if left.ID < right.ID {
			return -1
		}
		if left.ID > right.ID {
			return 1
		}
		return 0
	})
	return nil
}

func (r *Registry) Resolve(publicModel string) (Target, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	targets := r.targets[publicModel]
	if len(targets) == 0 {
		return Target{}, false
	}
	return cloneTarget(targets[0]), true
}

func (r *Registry) ResolveAll(publicModel string) []Target {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return cloneTargets(r.targets[publicModel])
}

func (r *Registry) ResolveCandidates(publicModel string) []Target {
	return r.ResolveCandidatesFor(publicModel, "")
}

func (r *Registry) ResolveCandidatesFor(publicModel string, operation Operation) []Target {
	return r.ResolveCandidatesForEvidence(publicModel, operation, "")
}

func (r *Registry) ResolveCandidatesForEvidence(publicModel string, operation Operation, minimum domain.CapabilityEvidence) []Target {
	r.mu.RLock()
	targets := r.resolveCandidatesLocked(publicModel, operation, minimum)
	r.mu.RUnlock()
	if len(targets) < 2 || targets[0].Strategy != "round_robin" {
		return targets
	}

	// Round-robin is the only resolution strategy that mutates registry state.
	// Re-resolve after upgrading the lock so a concurrent reload cannot rotate a
	// stale candidate snapshot.
	r.mu.Lock()
	defer r.mu.Unlock()
	targets = r.resolveCandidatesLocked(publicModel, operation, minimum)
	if len(targets) < 2 || targets[0].Strategy != "round_robin" {
		return targets
	}
	offset := int(r.next[publicModel] % uint64(len(targets)))
	r.next[publicModel]++
	return append(targets[offset:], targets[:offset]...)
}

func (r *Registry) resolveCandidatesLocked(publicModel string, operation Operation, minimum domain.CapabilityEvidence) []Target {
	targets := cloneTargets(r.targets[publicModel])
	targets = slices.DeleteFunc(targets, func(target Target) bool {
		healthy, probed := r.health[target.DeploymentID]
		return target.DeploymentID != "" && probed && !healthy
	})
	if operation != "" {
		targets = slices.DeleteFunc(targets, func(target Target) bool {
			if _, ok := target.ResolveOperation(operation); !ok {
				return true
			}
			capabilityName := ""
			switch operation {
			case OperationChat, OperationMessages:
				capabilityName = "chat"
			case OperationChatStream, OperationMessagesStream:
				if !target.Capabilities.Chat || !target.Capabilities.Streaming {
					return true
				}
				if minimum != "" && !target.CapabilityEvidence.Satisfies("chat", minimum) {
					return true
				}
				capabilityName = "streaming"
			case OperationEmbeddings:
				capabilityName = "embeddings"
			default:
				return true
			}
			if capabilityName == "chat" && !target.Capabilities.Chat ||
				capabilityName == "embeddings" && !target.Capabilities.Embeddings {
				return true
			}
			return minimum != "" && !target.CapabilityEvidence.Satisfies(capabilityName, minimum)
		})
	}
	return targets
}

func cloneTarget(target Target) Target {
	target.CapabilityEvidence = target.CapabilityEvidence.Clone()
	return target
}
func cloneTargets(targets []Target) []Target {
	result := make([]Target, len(targets))
	for index, target := range targets {
		result[index] = cloneTarget(target)
	}
	return result
}

// SetDeploymentHealthy updates active-probe health. Unknown deployments remain
// eligible so startup and transient probe scheduling cannot black-hole traffic.
func (r *Registry) SetDeploymentHealthy(deploymentID string, healthy bool) {
	if deploymentID == "" {
		return
	}
	r.mu.Lock()
	r.health[deploymentID] = healthy
	r.mu.Unlock()
}

func (r *Registry) DeploymentHealth() map[string]bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make(map[string]bool, len(r.health))
	for deploymentID, healthy := range r.health {
		result[deploymentID] = healthy
	}
	return result
}

func (r *Registry) ProviderTypes() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	seen := make(map[string]struct{})
	for _, adapter := range r.adapters {
		seen[adapter.Type()] = struct{}{}
	}
	for _, targets := range r.targets {
		for _, target := range targets {
			seen[target.Adapter.Type()] = struct{}{}
		}
	}
	result := make([]string, 0, len(seen))
	for providerType := range seen {
		result = append(result, providerType)
	}
	slices.Sort(result)
	return result
}

func (r *Registry) DeploymentIDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	set := make(map[string]struct{})
	for _, targets := range r.targets {
		for _, target := range targets {
			if target.DeploymentID != "" {
				set[target.DeploymentID] = struct{}{}
			}
		}
	}
	result := make([]string, 0, len(set))
	for deploymentID := range set {
		result = append(result, deploymentID)
	}
	slices.Sort(result)
	return result
}

func (r *Registry) ProviderIDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	seen := make(map[string]struct{})
	for providerID := range r.adapters {
		seen[providerID] = struct{}{}
	}
	for _, targets := range r.targets {
		for _, target := range targets {
			if target.ProviderID != "" {
				seen[target.ProviderID] = struct{}{}
			}
		}
	}
	result := make([]string, 0, len(seen))
	for providerID := range seen {
		result = append(result, providerID)
	}
	slices.Sort(result)
	return result
}

func (r *Registry) AdapterForProvider(providerID string) (Adapter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if adapter, ok := r.adapters[providerID]; ok {
		return adapter, true
	}
	for _, targets := range r.targets {
		for _, target := range targets {
			if target.ProviderID == providerID {
				return target.Adapter, true
			}
		}
	}
	return nil, false
}

// Replace atomically installs the targets from next and returns the adapters
// that are no longer reachable. Callers must close the returned adapters only
// after their maximum in-flight request duration has elapsed.
func (r *Registry) Replace(next *Registry) []Adapter {
	if next == nil || next == r {
		return nil
	}
	next.mu.Lock()
	replacementTargets := next.targets
	replacementNext := next.next
	replacementAdapters := next.adapters
	replacementHealth := next.health
	next.targets = make(map[string][]Target)
	next.next = make(map[string]uint64)
	next.adapters = make(map[string]Adapter)
	next.health = make(map[string]bool)
	next.mu.Unlock()

	r.mu.Lock()
	oldTargets := r.targets
	oldAdapters := r.adapters
	oldHealth := r.health
	r.targets = replacementTargets
	r.next = replacementNext
	r.adapters = replacementAdapters
	r.health = replacementHealth
	for deploymentID, healthy := range oldHealth {
		if _, exists := r.health[deploymentID]; !exists {
			r.health[deploymentID] = healthy
		}
	}
	r.mu.Unlock()

	active := make(map[Adapter]struct{})
	for _, adapter := range replacementAdapters {
		active[adapter] = struct{}{}
	}
	for _, targets := range replacementTargets {
		for _, target := range targets {
			active[target.Adapter] = struct{}{}
		}
	}
	retired := make(map[Adapter]struct{})
	for _, adapter := range oldAdapters {
		if _, stillActive := active[adapter]; !stillActive {
			retired[adapter] = struct{}{}
		}
	}
	for _, targets := range oldTargets {
		for _, target := range targets {
			if _, stillActive := active[target.Adapter]; !stillActive {
				retired[target.Adapter] = struct{}{}
			}
		}
	}
	result := make([]Adapter, 0, len(retired))
	for adapter := range retired {
		result = append(result, adapter)
	}
	return result
}

func (r *Registry) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	adapters := make(map[Adapter]struct{})
	for _, adapter := range r.adapters {
		adapters[adapter] = struct{}{}
	}
	for _, targets := range r.targets {
		for _, target := range targets {
			adapters[target.Adapter] = struct{}{}
		}
	}
	r.targets = make(map[string][]Target)
	r.next = make(map[string]uint64)
	r.adapters = make(map[string]Adapter)
	r.health = make(map[string]bool)
	for adapter := range adapters {
		adapter.Close()
	}
}
