package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/akz142857/Halro/internal/anthropicapi"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/safetransport"
	"github.com/akz142857/Halro/internal/semantic"
)

func CapabilityClaimRevision(parts ...string) string {
	hash := sha256.New()
	for _, part := range parts {
		_, _ = hash.Write([]byte(part))
		_, _ = hash.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

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
	// ErrorCanceled is the caller abandoning the request, not the upstream
	// failing it. The string matches what the gateway already records for a
	// cancellation it classifies itself.
	ErrorCanceled ErrorClass = "canceled"
)

// TransportClass names the failure of a request attempt that produced no HTTP
// response. Cancellation gets its own class: reporting it as a connection
// failure sent operators chasing network problems the upstream never had, and
// let a caller's own cancel count against the deployment's availability
// breaker. It says nothing about ambiguity — a cancel mid-response may still
// have been served upstream, so callers keep deriving that from Unsent.
func TransportClass(err error) ErrorClass {
	switch {
	case errors.Is(err, context.Canceled):
		return ErrorCanceled
	case errors.Is(err, context.DeadlineExceeded):
		return ErrorTimeout
	default:
		return ErrorConnect
	}
}

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

// Unsent reports whether a transport error happened before any byte of the
// request could have reached the provider.
//
// Ambiguity and retryability answer different questions, and connection setup
// is where they come apart. Name resolution and dialling fail with the provider
// never having seen the request: nothing ran, nothing is owed, and another
// deployment can serve it. Everything after that point — including a timeout
// waiting for the response — may already have been executed upstream, so it
// stays ambiguous and neither retries nor settles as free.
//
// The test is deliberately narrow. Treating an ambiguous failure as unsent
// would retry a completion the provider already billed, so anything this
// cannot positively identify as a setup failure keeps the conservative answer.
func Unsent(err error) bool {
	if err == nil {
		return false
	}
	// A refusal SafeTransport made itself, before a connection existed. This one
	// is not an inference from an error type the network happened to produce: the
	// gateway declined to dial, so nothing was sent. Left out, it was the single
	// case of total certainty being settled as ambiguous.
	if errors.Is(err, safetransport.ErrRefusedBeforeSend) {
		return true
	}
	var resolution *net.DNSError
	if errors.As(err, &resolution) {
		return true
	}
	var operation *net.OpError
	return errors.As(err, &operation) && operation.Op == "dial"
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
	Betas         []string
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

// NativeTokenCountAdapter is separate from NativeMessagesAdapter because the
// operation is separate: an adapter can speak the Messages wire format without
// exposing count_tokens, and an adapter that cannot must fail closed rather than
// have the call fall through to something that looks similar.
type NativeTokenCountAdapter interface {
	CountTokensNative(context.Context, NativeMessageCall) (NativeMessageResult, error)
}

type Adapter interface {
	Type() string
	Chat(context.Context, ChatCall) (openaiapi.ChatCompletionResponse, error)
	ChatStream(context.Context, ChatCall, func(semantic.Event) error) (*openaiapi.Usage, error)
	Embed(context.Context, EmbeddingCall) (openaiapi.EmbeddingResponse, error)
	Close()
}

type Capabilities struct {
	Chat           bool
	Streaming      bool
	Embeddings     bool
	Moderations    bool
	Images         bool
	Transcriptions bool
	Speech         bool
	Files          bool
	Batches        bool
	Rerank         bool
	AsyncGenerate  bool
	Tools          bool
	Vision         bool
	FetchedImage   bool
	JSONMode       bool
	DeveloperRole  bool
	Reasoning      bool
	StreamUsage    bool
	// ProviderExecutedTools admits tools the upstream runs itself. It is the one
	// capability here that describes egress rather than formatting: the provider
	// makes its own network calls, outside SafeTransport's host allowlist.
	ProviderExecutedTools bool
	MaxContextTokens      int64
	MaxOutputTokens       int64
}

func (c Capabilities) AnyOperation() bool {
	return c.Chat || c.Streaming || c.Embeddings || c.Moderations || c.Images ||
		c.Transcriptions || c.Speech || c.Files || c.Batches || c.Rerank || c.AsyncGenerate
}

type CapabilityReporter interface {
	Capabilities() Capabilities
}

type Prober interface {
	Probe(context.Context, string) error
}

// ProbeModelRequirer is a Prober whose probe is a request about one model, so a
// connection with no enabled deployment has nothing to name in it.
//
// It exists because the adapters that need a model reported its absence as a
// malformed value — "invalid Bedrock model id" for an id that was never
// supplied — which sends the operator to look at a model they never chose. The
// requirement is the adapter's own knowledge (Azure needs a deployment route,
// plain OpenAI lists models instead, two Bedrock profiles probe a collection),
// so it is stated here rather than guessed from the profile by the caller.
type ProbeModelRequirer interface {
	Prober
	ProbeRequiresModel() bool
}

// InvocationTargetLister discovers real upstream invocation identities using
// the adapter's bound credential and endpoint. Discovery establishes
// availability only; capability claims are produced separately by an audited
// ProviderMetadataMapper.
type InvocationTargetLister interface {
	ListInvocationTargets(context.Context, domain.TargetQuery) ([]domain.InvocationTargetDescriptor, error)
}

type InvocationTargetDescriber interface {
	DescribeInvocationTarget(context.Context, domain.InvocationTargetDescriptor) (domain.InvocationTargetDescriptor, error)
}

type ProviderMetadataMapper interface {
	MapCapabilityClaims(domain.InvocationTargetDescriptor, domain.InvocationTargetScopeKey, time.Time) []domain.CapabilityClaim
}

type InvocationTargetDiscoveryReporter interface {
	InvocationTargetDiscovery() domain.InvocationTargetDiscoveryCapabilities
}

// AdapterUnwrapper exposes an adapter wrapped only to add profile metadata.
// Optional invocation-target extension interfaces remain discoverable without
// making every wrapper claim support for them.
type AdapterUnwrapper interface {
	UnwrapAdapter() Adapter
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
	ID                          string
	DeploymentID                string
	ProviderID                  string
	BindingID                   string
	PublicModel                 string
	ProviderModel               string
	AccessSurface               domain.AccessSurface
	ProfileID                   domain.ProviderProfileID
	Region                      string
	Adapter                     Adapter
	InputMicrosPerMillion       int64
	CachedInputMicrosPerMillion int64
	OutputMicrosPerMillion      int64
	FixedRequestMicrosUSD       int64
	Priority                    int
	Strategy                    string
	Capabilities                Capabilities
	CapabilityEvidence          domain.CapabilityEvidenceSet
	// AllowedAnthropicBetas is the set of anthropic-beta tokens this connection
	// may forward. Routing checks a request's tokens against it before any
	// provider work, so an unaccepted beta fails closed rather than reaching the
	// upstream and changing what the response means.
	AllowedAnthropicBetas []string
	MaxConcurrency        int64
	DeploymentConcurrency int64
	operations            OperationRegistry
}

type Registry struct {
	mu      sync.RWMutex
	targets map[string][]Target
	// next holds one rotation counter per public model, created when the alias
	// is first registered so the resolve path never has to insert into the map.
	next map[string]*atomic.Uint64
	// adapters are keyed by binding identity. Legacy registrations use the
	// Provider ID as their binding identity.
	adapters         map[string]Adapter
	providerBindings map[string][]string
	health           map[string]bool
	// Why a provider or binding has no adapter here, keyed by binding identity
	// and by Provider ID for a provider-wide exclusion. It lives beside the
	// adapters rather than next to the load report so it swaps with them: a
	// caller that finds no adapter and then reads a reason must not be told why
	// a registry that is no longer the live one left something out.
	unavailable map[string]string
}

func NewRegistry() *Registry {
	return &Registry{
		targets: make(map[string][]Target), next: make(map[string]*atomic.Uint64),
		adapters:         make(map[string]Adapter),
		providerBindings: make(map[string][]string),
		health:           make(map[string]bool),
		unavailable:      make(map[string]string),
	}
}

// RecordUnavailable states why this registry has no adapter for a provider or
// one of its bindings. Recorded by the load beside the exclusion it reports, so
// the two always say the same thing.
func (r *Registry) RecordUnavailable(providerID, bindingID, reason string) {
	key := providerID
	if bindingID != "" {
		key = bindingID
	}
	if key == "" || reason == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.unavailable[key] = reason
}

// UnavailableReason answers why a binding has no adapter. It returns "" when
// this registry excluded nothing that covers it: a caller finding no adapter and
// no reason is looking at a state the load cannot explain, and keeps its own
// fail-closed answer.
func (r *Registry) UnavailableReason(providerID, bindingID string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if bindingID != "" {
		if reason, ok := r.unavailable[bindingID]; ok {
			return reason
		}
	}
	return r.unavailable[providerID]
}

// RegisterAdapter makes an enabled provider independently addressable for
// health probes, even when it is not referenced by a route yet.
func (r *Registry) RegisterAdapter(providerID string, adapter Adapter) error {
	return r.RegisterBindingAdapter(providerID, providerID, adapter)
}

// RegisterBindingAdapter makes one immutable provider profile binding
// independently addressable. Multiple bindings may share a Provider ID while
// retaining distinct adapters and protocol contracts.
func (r *Registry) RegisterBindingAdapter(providerID, bindingID string, adapter Adapter) error {
	if providerID == "" || bindingID == "" || adapter == nil {
		return errors.New("provider id, binding id, and adapter are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.adapters[bindingID]; exists {
		return errors.New("provider binding adapter is already registered")
	}
	r.adapters[bindingID] = adapter
	r.providerBindings[providerID] = append(r.providerBindings[providerID], bindingID)
	slices.Sort(r.providerBindings[providerID])
	return nil
}

func (r *Registry) Register(target Target) error {
	// DeploymentID is required. Everything governed about a target — its
	// versioned price, health probe, capability snapshot and concurrency limit
	// — is keyed on the deployment, so a target without one silently opts out
	// of all four. Requiring it here makes that unrepresentable rather than
	// leaving each consumer to remember a fallback.
	if target.ID == "" || target.PublicModel == "" || target.ProviderModel == "" ||
		target.DeploymentID == "" || target.Adapter == nil {
		return errors.New("target id, deployment id, public model, provider model, and adapter are required")
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
		// Registering an adapter with no immutable profile contract used to be
		// allowed, with its optional semantics scrubbed to false afterwards.
		// That made the fail-closed behaviour a property of a later branch
		// rather than of the type, so anything that skipped the branch — a new
		// requirement the scrub did not cover, a new call site — silently
		// became fail-open. The state is now unrepresentable instead.
		return errors.New("adapter must implement ProfiledAdapter; an unprofiled adapter has no capability contract to route on")
	}
	if target.Strategy == "" {
		target.Strategy = "ordered"
	}
	if target.Strategy != "ordered" && target.Strategy != "round_robin" {
		return errors.New("route strategy must be ordered or round_robin")
	}
	if !target.Capabilities.AnyOperation() {
		if _, reports := target.Adapter.(CapabilityReporter); reports {
			// An adapter that reports its own capabilities is registered by a
			// caller that has already intersected them with what the deployment
			// declared, so an empty set here means the intersection came out
			// empty — a deployment that can serve nothing. Adopting the adapter's
			// full set instead read "empty" as "unspecified" and granted every
			// capability the adapter has, including ones the deployment never
			// declared. Refusing keeps the target out of the registry rather than
			// widening it, and the caller records it as a withheld reference.
			return errors.New("target declares no operation capability; the deployment and the adapter have none in common")
		}
		// Built-in test and extension adapters predate capability reporting and
		// have nothing to intersect against, so their targets still get the
		// portable core.
		target.Capabilities = Capabilities{
			Chat: true, Streaming: true, Embeddings: true,
		}
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
	if r.next[target.PublicModel] == nil {
		r.next[target.PublicModel] = new(atomic.Uint64)
	}
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
	defer r.mu.RUnlock()
	targets := r.resolveCandidatesLocked(publicModel, operation, minimum)
	if len(targets) < 2 || targets[0].Strategy != "round_robin" {
		return targets
	}
	// The rotation counter is atomic and the counters map is only written while
	// the write lock is held, so the snapshot and the offset it is rotated by
	// come from one read-locked pass. This used to resolve a second time under
	// the write lock — the counter was plain registry state, and rotating a
	// snapshot taken before the upgrade could have rotated one a reload had
	// already replaced. The re-resolve doubled the cost of every round-robin
	// request and serialised them behind the write lock; taking the counter out
	// of the locked state removes the reason for both.
	counter := r.next[publicModel]
	if counter == nil {
		return targets
	}
	offset := int((counter.Add(1) - 1) % uint64(len(targets)))
	return append(targets[offset:], targets[:offset]...)
}

func (r *Registry) resolveCandidatesLocked(publicModel string, operation Operation, minimum domain.CapabilityEvidence) []Target {
	targets := cloneTargets(r.targets[publicModel])
	targets = slices.DeleteFunc(targets, func(target Target) bool {
		healthy, probed := r.health[target.DeploymentID]
		return target.DeploymentID != "" && probed && !healthy
	})
	return filterByOperation(targets, operation, minimum)
}

// SupportsOperation reports whether any registered target for the alias could
// serve the operation, ignoring probe health. Candidate resolution removes
// unhealthy targets before the operation filter runs, so an alias whose every
// deployment is probe-failed resolves to zero candidates for every operation —
// indistinguishable, from the outside, from an operation the route never
// supported. This answers which of the two it was, so the caller can say
// "upstream is unhealthy" instead of blaming the request.
func (r *Registry) SupportsOperation(publicModel string, operation Operation, minimum domain.CapabilityEvidence) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(filterByOperation(cloneTargets(r.targets[publicModel]), operation, minimum)) > 0
}

func filterByOperation(targets []Target, operation Operation, minimum domain.CapabilityEvidence) []Target {
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
			case OperationModerations:
				capabilityName = "moderations"
			case OperationImages:
				capabilityName = "images"
			case OperationTranscriptions:
				capabilityName = "transcriptions"
			case OperationSpeech:
				capabilityName = "speech"
			case OperationFiles:
				capabilityName = "files"
			case OperationBatches:
				capabilityName = "batches"
			case OperationRerank:
				capabilityName = "rerank"
			case OperationAsyncInvoke:
				capabilityName = "async_generate"
			default:
				return true
			}
			if !targetCapabilityEnabled(target.Capabilities, capabilityName) {
				return true
			}
			return minimum != "" && !target.CapabilityEvidence.Satisfies(capabilityName, minimum)
		})
	}
	return targets
}

func targetCapabilityEnabled(capabilities Capabilities, name string) bool {
	switch name {
	case "chat":
		return capabilities.Chat
	case "streaming":
		return capabilities.Streaming
	case "embeddings":
		return capabilities.Embeddings
	case "moderations":
		return capabilities.Moderations
	case "images":
		return capabilities.Images
	case "transcriptions":
		return capabilities.Transcriptions
	case "speech":
		return capabilities.Speech
	case "files":
		return capabilities.Files
	case "batches":
		return capabilities.Batches
	case "rerank":
		return capabilities.Rerank
	case "async_generate":
		return capabilities.AsyncGenerate
	default:
		return false
	}
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

func (r *Registry) DeploymentConcurrencyLimits() map[string]int64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make(map[string]int64)
	for _, targets := range r.targets {
		for _, target := range targets {
			if target.DeploymentID != "" && target.DeploymentConcurrency > result[target.DeploymentID] {
				result[target.DeploymentID] = target.DeploymentConcurrency
			}
		}
	}
	return result
}

func (r *Registry) ProviderConcurrencyLimits() map[string]int64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make(map[string]int64)
	for _, targets := range r.targets {
		for _, target := range targets {
			if target.ProviderID != "" && target.MaxConcurrency > result[target.ProviderID] {
				result[target.ProviderID] = target.MaxConcurrency
			}
		}
	}
	return result
}

func (r *Registry) ProviderIDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	seen := make(map[string]struct{})
	for providerID := range r.providerBindings {
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
	bindings := r.providerBindings[providerID]
	if len(bindings) == 1 {
		adapter, ok := r.adapters[bindings[0]]
		return adapter, ok
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

func (r *Registry) AdapterForBinding(providerID, bindingID string) (Adapter, bool) {
	if providerID == "" || bindingID == "" {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !slices.Contains(r.providerBindings[providerID], bindingID) {
		return nil, false
	}
	adapter, ok := r.adapters[bindingID]
	return adapter, ok
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
	replacementProviderBindings := next.providerBindings
	replacementHealth := next.health
	replacementUnavailable := next.unavailable
	next.targets = make(map[string][]Target)
	next.next = make(map[string]*atomic.Uint64)
	next.adapters = make(map[string]Adapter)
	next.providerBindings = make(map[string][]string)
	next.health = make(map[string]bool)
	next.unavailable = make(map[string]string)
	next.mu.Unlock()

	r.mu.Lock()
	oldTargets := r.targets
	oldAdapters := r.adapters
	oldHealth := r.health
	r.targets = replacementTargets
	r.next = replacementNext
	r.adapters = replacementAdapters
	r.providerBindings = replacementProviderBindings
	r.health = replacementHealth
	r.unavailable = replacementUnavailable
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
	r.next = make(map[string]*atomic.Uint64)
	r.adapters = make(map[string]Adapter)
	r.providerBindings = make(map[string][]string)
	r.health = make(map[string]bool)
	for adapter := range adapters {
		adapter.Close()
	}
}
