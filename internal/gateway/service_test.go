package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/auth"
	"github.com/akz142857/Halro/internal/budget"
	openaiwire "github.com/akz142857/Halro/internal/compatibility/openai"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/limiter"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/redaction"
	"github.com/akz142857/Halro/internal/requestmeta"
	"github.com/akz142857/Halro/internal/semantic"
	"github.com/akz142857/Halro/internal/tokenguard"
)

// testChainKey is a fixed 32-byte Ledger HMAC key: every event the ledger
// package writes is promoted to epoch 4 (ADR 0016), which requires a key.
var testChainKey = bytes.Repeat([]byte{0x24}, 32)

type source struct {
	keys     []domain.GatewayKey
	projects []domain.Project
}

type staticPriceSelector struct {
	price domain.DeploymentPriceVersion
}

type fakePricePinStore struct {
	mu             sync.Mutex
	price          domain.DeploymentPriceVersion
	admissionPrice domain.DeploymentPriceVersion
	prepared       domain.PricePinIntent
	committed      domain.PricePinIntent
	prepares       atomic.Int64
	deletes        atomic.Int64
}

func (s *fakePricePinStore) SelectDeploymentPriceVersion(ctx context.Context, deploymentID string, selectedAt time.Time) (domain.DeploymentPriceVersion, error) {
	price := s.admissionPrice
	if price.ID == "" {
		price = s.price
	}
	return staticPriceSelector{price: price}.SelectDeploymentPriceVersion(ctx, deploymentID, selectedAt)
}

func (s *fakePricePinStore) LockDeploymentPricingShared(string) func() {
	s.mu.Lock()
	return s.mu.Unlock
}

func (s *fakePricePinStore) PrepareDeploymentPricePin(_ context.Context, deploymentID, attemptID string, selectedAt time.Time, _, _ time.Duration) (domain.DeploymentPriceVersion, domain.PriceSnapshot, domain.PricePinIntent, error) {
	s.prepares.Add(1)
	price, err := staticPriceSelector{price: s.price}.SelectDeploymentPriceVersion(context.Background(), deploymentID, selectedAt)
	if err != nil {
		return domain.DeploymentPriceVersion{}, domain.PriceSnapshot{}, domain.PricePinIntent{}, err
	}
	snapshot, err := domain.NewVersionedPriceSnapshot(price, selectedAt)
	if err != nil {
		return domain.DeploymentPriceVersion{}, domain.PriceSnapshot{}, domain.PricePinIntent{}, err
	}
	digest, _ := snapshot.Digest()
	s.prepared = domain.PricePinIntent{AttemptID: attemptID, DeploymentID: deploymentID, PriceVersionID: price.ID,
		PriceVersion: price.Version, SnapshotSHA256: digest, PricingSelectedAt: selectedAt,
		MetadataRevision: 1, State: domain.PricePinPrepared, CreatedAt: selectedAt}
	return price, snapshot, s.prepared, nil
}

func (s *fakePricePinStore) CommitDeploymentPricePin(_ context.Context, attemptID, digest string, sequence uint64, committedAt time.Time) (domain.PricePinIntent, error) {
	if s.prepared.AttemptID != attemptID || s.prepared.SnapshotSHA256 != digest || sequence == 0 {
		return domain.PricePinIntent{}, errors.New("invalid pin commit")
	}
	s.committed = s.prepared
	s.committed.State, s.committed.LedgerSequence, s.committed.CommittedAt = domain.PricePinCommitted, sequence, &committedAt
	return s.committed, nil
}

func (s *fakePricePinStore) DeletePreparedDeploymentPricePin(context.Context, string) error {
	s.deletes.Add(1)
	return nil
}

func (s staticPriceSelector) SelectDeploymentPriceVersion(_ context.Context, deploymentID string, selectedAt time.Time) (domain.DeploymentPriceVersion, error) {
	if s.price.DeploymentID != deploymentID || s.price.EffectiveFrom.After(selectedAt) {
		return domain.DeploymentPriceVersion{}, domain.ErrPriceUnavailable
	}
	return s.price, nil
}

func TestPrepareAccountingLeaseCapturesVersionedFreeSnapshotWithoutSyntheticReservation(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	price := domain.DeploymentPriceVersion{
		ID: "price_free", DeploymentID: "dep_free", Version: 1, Revision: 1,
		BillingMode: domain.BillingModeFree, Currency: "USD", FormulaVersion: domain.PriceFormulaUSDTokensV1,
		EffectiveFrom: now.Add(-time.Hour), CreatedBy: "test", CreatedAt: now.Add(-time.Hour),
		Source: domain.PriceSource{Type: domain.PriceSourceManual, Assurance: domain.PriceAssuranceAsserted,
			ReceivedAt: now.Add(-time.Hour), ContentSHA256: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Reference: "test", AssertedWithoutArchive: true},
	}
	service := &Service{pricing: staticPriceSelector{price: price}, now: func() time.Time { return now }}
	reservation, mode, snapshot, _, err := service.prepareAccountingLease(context.Background(), provider.Target{DeploymentID: "dep_free"}, 10, 20)
	if err != nil || reservation != 0 || mode != ledger.LeaseModeFree || snapshot == nil || snapshot.CostValueStatus != domain.CostValueKnown {
		t.Fatalf("reservation=%d mode=%q snapshot=%#v err=%v", reservation, mode, snapshot, err)
	}
}

func TestGatewayCommitsPricePinBeforeProviderAttempt(t *testing.T) {
	f := newFixture(t, 10_000)
	defer f.close()
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	priceStore := &fakePricePinStore{price: domain.DeploymentPriceVersion{
		ID: "price_gateway", DeploymentID: "dep_gateway", Version: 1, Revision: 1,
		BillingMode: domain.BillingModeMetered, Currency: "USD", FormulaVersion: domain.PriceFormulaUSDTokensV1,
		InputMicrosPerMillion: 1_000_000, OutputMicrosPerMillion: 2_000_000,
		EffectiveFrom: now.Add(-time.Hour), CreatedBy: "test", CreatedAt: now.Add(-time.Hour),
		Source: domain.PriceSource{Type: domain.PriceSourceManual, Assurance: domain.PriceAssuranceAsserted,
			ReceivedAt: now.Add(-time.Hour), ContentSHA256: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Reference: "test", AssertedWithoutArchive: true},
	}}
	registry := provider.NewRegistry()
	if err := registry.Register(provider.Target{ID: "target_pin", DeploymentID: "dep_gateway", PublicModel: "chat",
		ProviderModel: "provider-model", Adapter: f.adapter, InputMicrosPerMillion: 99, OutputMicrosPerMillion: 99}); err != nil {
		t.Fatal(err)
	}
	service, err := NewServiceWithOptions(f.service.auth, registry, f.accounting, ServiceOptions{Pricing: priceStore})
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now }
	ctx := requestmeta.WithRequestID(context.Background(), "req_developer_debug")
	if _, err := service.Chat(ctx, f.plaintext, chatRequest()); err != nil {
		t.Fatal(err)
	}
	if f.adapter.calls != 1 || priceStore.committed.State != domain.PricePinCommitted || priceStore.committed.LedgerSequence == 0 ||
		priceStore.committed.AttemptID != priceStore.prepared.AttemptID {
		t.Fatalf("provider calls=%d prepared=%#v committed=%#v", f.adapter.calls, priceStore.prepared, priceStore.committed)
	}
	lease, ok := f.state.AccountingLease(priceStore.prepared.AttemptID)
	if !ok || !domain.ValidSHA256Label(lease.Event.TokenGuardPricingViewDigest) || lease.Event.RequestID != "req_developer_debug" {
		t.Fatalf("accounting lease pricing view digest=%q exists=%t", lease.Event.TokenGuardPricingViewDigest, ok)
	}
}

func TestGatewayRechecksAttemptPriceAgainstTokenGuardBeforeProviderIO(t *testing.T) {
	f := newFixture(t, 10_000)
	defer f.close()
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	price := func(id string, version uint64, rate int64) domain.DeploymentPriceVersion {
		return domain.DeploymentPriceVersion{
			ID: id, DeploymentID: "dep_reprice", Version: version, Revision: 1,
			BillingMode: domain.BillingModeMetered, Currency: "USD", FormulaVersion: domain.PriceFormulaUSDTokensV1,
			InputMicrosPerMillion: rate, OutputMicrosPerMillion: rate,
			EffectiveFrom: now.Add(-time.Hour), CreatedBy: "test", CreatedAt: now.Add(-time.Hour),
			Source: domain.PriceSource{Type: domain.PriceSourceManual, Assurance: domain.PriceAssuranceAsserted,
				ReceivedAt: now.Add(-time.Hour), ContentSHA256: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				Reference: "test", AssertedWithoutArchive: true},
		}
	}
	prices := &fakePricePinStore{admissionPrice: price("price_low", 1, 1), price: price("price_high", 2, 1_000_000)}
	project := f.project
	project.TokenGuardPolicyID = "guard_cost"
	snapshot := auth.NewSnapshot()
	if err := snapshot.Refresh(context.Background(), source{keys: []domain.GatewayKey{f.key}, projects: []domain.Project{project}}); err != nil {
		t.Fatal(err)
	}
	guard, err := tokenguard.New([]domain.TokenGuardPolicy{{
		ID: "guard_cost", Name: "cost", Enabled: true, Action: "observe", CostMicrosPerMinute: 10, Revision: 1,
	}})
	if err != nil {
		t.Fatal(err)
	}
	registry := provider.NewRegistry()
	if err := registry.Register(provider.Target{ID: "target_reprice", DeploymentID: "dep_reprice", PublicModel: "chat", ProviderModel: "provider-model", Adapter: f.adapter}); err != nil {
		t.Fatal(err)
	}
	service, err := NewServiceWithOptions(snapshot, registry, f.accounting, ServiceOptions{Pricing: prices, TokenGuard: guard})
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now }
	_, err = service.Chat(context.Background(), f.plaintext, chatRequest())
	var gatewayErr *Error
	if !errors.As(err, &gatewayErr) || gatewayErr.Code != "token_guard_blocked" {
		t.Fatalf("error=%v", err)
	}
	if f.adapter.calls != 0 || f.state.PendingReservations() != 0 {
		t.Fatalf("provider calls=%d pending=%d", f.adapter.calls, f.state.PendingReservations())
	}
}

func TestGatewayUnknownPriceExplicitOptInPersistsUnknownCost(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	f := newFixtureAt(t, 0, ledger.Options{}, func() time.Time { return now })
	defer f.close()
	prices := &fakePricePinStore{}
	registry := provider.NewRegistry()
	if err := registry.Register(provider.Target{ID: "target_unknown", DeploymentID: "dep_unknown", PublicModel: "chat", ProviderModel: "provider-model", Adapter: f.adapter}); err != nil {
		t.Fatal(err)
	}
	service, err := NewServiceWithOptions(f.service.auth, registry, f.accounting, ServiceOptions{
		Pricing: prices, PricingUnknownPolicy: "allow_without_cost_governance",
	})
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now }
	if _, err := service.Chat(context.Background(), f.plaintext, chatRequest()); err != nil {
		t.Fatal(err)
	}
	balance := f.state.Balance(f.project.ID, now.Format("2006-01-02"), testTimezoneVersion)
	if f.adapter.calls != 1 || balance.CommittedMicrosUSD != 0 || balance.UnknownAttempts != 1 {
		t.Fatalf("provider calls=%d balance=%#v", f.adapter.calls, balance)
	}
}

func (s source) ListGatewayKeys(context.Context) ([]domain.GatewayKey, error) {
	return s.keys, nil
}

func (s source) ListProjects(context.Context) ([]domain.Project, error) {
	return s.projects, nil
}

type fakeAdapter struct {
	// profileID lets a test register the fake behind a profile other than the
	// default OpenAI one, so profile-scoped filtering can be exercised without
	// a second fake.
	profileID         domain.ProviderProfileID
	mu                sync.Mutex
	response          openaiapi.ChatCompletionResponse
	embeddingResponse openaiapi.EmbeddingResponse
	streamChunks      []openaiapi.ChatCompletionResponse
	streamUsage       *openaiapi.Usage
	err               error
	calls             int
	lastChatRequest   openaiapi.ChatCompletionRequest
	started           chan struct{}
	release           <-chan struct{}
}

// The registry only routes adapters that carry a profile contract, which is how
// production wires every adapter (internal/app wraps each one in
// LegacyAdapterBridge). The fake mirrors that instead of registering bare, so
// these tests exercise the same shape the gateway actually serves.
func (a *fakeAdapter) Type() string {
	if a.profileID != "" {
		if providerType, _, ok := domain.RegisteredProviderProfile(a.profileID); ok {
			return string(providerType)
		}
	}
	return "openai"
}

func (a *fakeAdapter) Profile() provider.ProfileManifest {
	profileID := a.profileID
	if profileID == "" {
		profileID = domain.ProfileOpenAIChatEmbeddings
	}
	manifest, _ := provider.BuiltinProfile(profileID)
	return manifest
}

func (a *fakeAdapter) Operations() provider.OperationRegistry {
	bridge, err := provider.NewLegacyAdapterBridge(adapterOnly{a}, a.Profile(), a.CapabilityEvidence())
	if err != nil {
		panic(err)
	}
	return bridge.Operations()
}

func (a *fakeAdapter) CapabilityEvidence() domain.CapabilityEvidenceSet {
	return domain.EvidenceForCapabilities(
		domain.ProviderCapabilities{Chat: true, Streaming: true, Embeddings: true},
		domain.EvidenceDeclared,
	)
}

// adapterOnly hides the profile methods so the bridge wraps the plain adapter
// rather than recursing into the fake's own Operations().
type adapterOnly struct{ provider.Adapter }

func (a *fakeAdapter) Close() {}

func (a *fakeAdapter) Chat(_ context.Context, call provider.ChatCall) (openaiapi.ChatCompletionResponse, error) {
	a.mu.Lock()
	a.calls++
	a.lastChatRequest = call.Request
	a.mu.Unlock()
	if a.started != nil {
		a.started <- struct{}{}
	}
	if a.release != nil {
		<-a.release
	}
	if call.ProviderModel != "provider-model" {
		return openaiapi.ChatCompletionResponse{}, errors.New("provider model was not mapped")
	}
	return a.response, a.err
}

func (a *fakeAdapter) ChatStream(
	_ context.Context,
	_ provider.ChatCall,
	emit func(semantic.Event) error,
) (*openaiapi.Usage, error) {
	a.mu.Lock()
	a.calls++
	a.mu.Unlock()
	for index, chunk := range a.streamChunks {
		if chunk.Model == "" {
			chunk.Model = "provider-model"
		}
		if index == len(a.streamChunks)-1 {
			for choiceIndex := range chunk.Choices {
				if chunk.Choices[choiceIndex].FinishReason == nil {
					finish := "stop"
					chunk.Choices[choiceIndex].FinishReason = &finish
				}
			}
		}
		event, err := openaiwire.DecodeEvent(chunk)
		if err != nil {
			return a.streamUsage, err
		}
		if err := emit(event); err != nil {
			return a.streamUsage, err
		}
	}
	return a.streamUsage, a.err
}

func (a *fakeAdapter) Embed(_ context.Context, call provider.EmbeddingCall) (openaiapi.EmbeddingResponse, error) {
	a.mu.Lock()
	a.calls++
	a.mu.Unlock()
	if call.ProviderModel != "provider-model" {
		return openaiapi.EmbeddingResponse{}, errors.New("provider model was not mapped")
	}
	return a.embeddingResponse, a.err
}

type fixture struct {
	service    *Service
	plaintext  string
	state      *ledger.State
	adapter    *fakeAdapter
	registry   *provider.Registry
	close      func()
	project    domain.Project
	key        domain.GatewayKey
	accounting *budget.Manager
	status     *ledger.Status
	log        *ledger.Log
}

func newFixture(t *testing.T, dailyBudget int64) fixture {
	return newFixtureWithLedgerOptions(t, dailyBudget, ledger.Options{})
}

func newFixtureWithLedgerOptions(t *testing.T, dailyBudget int64, options ledger.Options) fixture {
	return newFixtureAt(t, dailyBudget, options, nil)
}

// newFixtureAt wires one clock through both the accounting manager and the service. The
// manager buckets a request into its accounting period using its own clock, so a test that
// only overrode service.now would still write into the real current day and its assertion
// would start failing the day after it was written.
func newFixtureAt(
	t *testing.T,
	dailyBudget int64,
	options ledger.Options,
	clock func() time.Time,
) fixture {
	t.Helper()
	project := domain.Project{
		ID:                   "project_1",
		Name:                 "Project",
		Enabled:              true,
		AllowedModels:        []string{"chat"},
		DailyBudgetMicrosUSD: dailyBudget,
		MaxInputTokens:       10_000,
		MaxOutputTokens:      100,
	}
	plaintext, key, err := auth.GenerateGatewayKey(project.ID, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := auth.NewSnapshot()
	if err := snapshot.Refresh(context.Background(), source{
		keys:     []domain.GatewayKey{key},
		projects: []domain.Project{project},
	}); err != nil {
		t.Fatal(err)
	}
	status := ledger.NewStatus()
	// Every event the ledger package writes is promoted to epoch 4 (ADR
	// 0016), which requires a key to compute the frame MAC. Callers that
	// don't care about durability fault injection pass a zero-value
	// ledger.Options and rely on this default rather than repeating the key
	// at every call site.
	if len(options.ChainKey) == 0 {
		options.ChainKey = testChainKey
	}
	log, err := ledger.OpenWithOptions(filepath.Join(t.TempDir(), "usage.wal"), status, options)
	if err != nil {
		t.Fatal(err)
	}
	state := ledger.NewState()
	accounting, err := budget.NewWithOptions(log, state, mustResolver(t, "UTC"), budget.Options{Now: clock})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &fakeAdapter{response: openaiapi.ChatCompletionResponse{
		ID:      "chatcmpl_1",
		Object:  "chat.completion",
		Created: 1,
		Model:   "provider-model",
		Choices: []openaiapi.Choice{{Index: 0}},
		Usage:   &openaiapi.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}, embeddingResponse: openaiapi.EmbeddingResponse{
		Object: "list",
		Model:  "provider-model",
		Data:   []openaiapi.EmbeddingData{{Object: "embedding", Embedding: json.RawMessage(`[0.1,0.2]`), Index: 0}},
		Usage:  &openaiapi.Usage{PromptTokens: 4, TotalTokens: 4},
	}, streamChunks: []openaiapi.ChatCompletionResponse{{
		ID: "chunk_1", Object: "chat.completion.chunk", Model: "provider-model",
		Choices: []openaiapi.Choice{{Index: 0, Delta: &openaiapi.Message{
			Role: "assistant", Content: openaiapi.TextContent("hello"),
		}}},
	}}, streamUsage: &openaiapi.Usage{
		PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15,
	}}
	registry := provider.NewRegistry()
	if err := registry.Register(provider.Target{
		ID: "target_1", DeploymentID: "dep_target_1",
		PublicModel:            "chat",
		ProviderModel:          "provider-model",
		Adapter:                adapter,
		InputMicrosPerMillion:  1_000_000,
		OutputMicrosPerMillion: 2_000_000,
	}); err != nil {
		t.Fatal(err)
	}
	service, err := NewServiceWithOptions(snapshot, registry, accounting, ServiceOptions{Now: clock})
	if err != nil {
		t.Fatal(err)
	}
	return fixture{
		service:    service,
		plaintext:  plaintext,
		state:      state,
		adapter:    adapter,
		registry:   registry,
		close:      func() { _ = log.Close() },
		project:    project,
		key:        key,
		accounting: accounting,
		status:     status,
		log:        log,
	}
}

func TestGatewayReconcilesEstimatedTPMAgainstProviderUsage(t *testing.T) {
	f := newFixture(t, 10_000)
	defer f.close()
	maxTokens := int64(10)
	request := openaiapi.ChatCompletionRequest{
		Model: "chat", MaxCompletionTokens: &maxTokens,
		Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("hello")}},
	}
	// Measured on the semantic request, which is what the hot path reserves
	// against: every facade reaches it through the same estimate now.
	canonical, err := openaiwire.DecodeGenerate(request)
	if err != nil {
		t.Fatal(err)
	}
	estimated := estimateInputTokens(canonical.EstimatedInputBytes()) + maxTokens
	f.project.TPM = estimated + 3
	if err := f.service.auth.Refresh(context.Background(), source{
		keys: []domain.GatewayKey{f.key}, projects: []domain.Project{f.project},
	}); err != nil {
		t.Fatal(err)
	}
	f.adapter.response.Usage = &openaiapi.Usage{PromptTokens: 2, CompletionTokens: 1, TotalTokens: 3}
	for index := 0; index < 2; index++ {
		if _, err := f.service.Chat(context.Background(), f.plaintext, request); err != nil {
			t.Fatalf("request %d should use reconciled capacity: %v", index+1, err)
		}
	}
	if _, err := f.service.Chat(context.Background(), f.plaintext, request); !errors.Is(err, limiter.ErrTPM) {
		t.Fatalf("third request should exceed reconciled TPM: %v", err)
	}
	if f.adapter.calls != 2 {
		t.Fatalf("provider calls=%d", f.adapter.calls)
	}
}

type gatewayFaultDurability struct {
	file     *os.File
	writeErr error
	syncErr  error
}

func (d *gatewayFaultDurability) Write(payload []byte) (int, error) {
	if d.writeErr != nil {
		return 0, d.writeErr
	}
	return d.file.Write(payload)
}

func (d *gatewayFaultDurability) Sync() error {
	if d.syncErr != nil {
		return d.syncErr
	}
	return d.file.Sync()
}

func TestDurabilityFailurePreventsCurrentAndFutureProviderCalls(t *testing.T) {
	tests := []struct {
		name     string
		writeErr error
		syncErr  error
	}{
		{name: "ENOSPC write", writeErr: syscall.ENOSPC},
		{name: "EIO fsync", syncErr: syscall.EIO},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newFixtureWithLedgerOptions(t, 10_000, ledger.Options{
				MaxBatch: 1,
				WrapDurability: func(file *os.File) ledger.DurabilityWriter {
					return &gatewayFaultDurability{file: file, writeErr: test.writeErr, syncErr: test.syncErr}
				},
			})
			defer f.close()
			maxTokens := int64(10)
			request := openaiapi.ChatCompletionRequest{
				Model: "chat", MaxCompletionTokens: &maxTokens,
				Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("hello")}},
			}
			for attempt := 0; attempt < 2; attempt++ {
				_, err := f.service.Chat(context.Background(), f.plaintext, request)
				var gatewayErr *Error
				if !errors.As(err, &gatewayErr) || gatewayErr.Code != "accounting_unavailable" || gatewayErr.HTTPStatus != 503 {
					t.Fatalf("attempt=%d error=%#v", attempt, err)
				}
				if f.adapter.calls != 0 {
					t.Fatalf("attempt=%d provider calls=%d", attempt, f.adapter.calls)
				}
			}
			if f.status.Load() != ledger.AccountingUnavailable {
				t.Fatalf("accounting status=%v", f.status.Load())
			}
		})
	}
}

func TestChatRetriesThenFallsBackInPriorityOrder(t *testing.T) {
	f := newFixture(t, 10_000)
	defer f.close()
	f.adapter.err = &provider.Error{
		Class: provider.ErrorProvider5xx, Retryable: true, Message: "primary unavailable",
	}
	fallback := &fakeAdapter{response: openaiapi.ChatCompletionResponse{
		ID: "chatcmpl_fallback", Object: "chat.completion", Model: "fallback-provider-model",
		Choices: []openaiapi.Choice{{Index: 0}},
		Usage:   &openaiapi.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}}
	if err := f.registry.Register(provider.Target{
		ID: "target_2", DeploymentID: "dep_target_2", PublicModel: "chat", ProviderModel: "provider-model",
		Adapter: fallback, Priority: 1,
		InputMicrosPerMillion: 1_000_000, OutputMicrosPerMillion: 2_000_000,
	}); err != nil {
		t.Fatal(err)
	}
	response, err := f.service.Chat(context.Background(), f.plaintext, chatRequest())
	if err != nil {
		t.Fatal(err)
	}
	if response.ID != "chatcmpl_fallback" || f.adapter.calls != 2 || fallback.calls != 1 {
		t.Fatalf("response=%#v primary_calls=%d fallback_calls=%d", response, f.adapter.calls, fallback.calls)
	}
	if f.state.PendingReservations() != 0 {
		t.Fatal("fallback left pending reservations")
	}
}

func TestChatDoesNotFallbackForNonRetryableProviderError(t *testing.T) {
	f := newFixture(t, 10_000)
	defer f.close()
	f.adapter.err = &provider.Error{
		Class: provider.ErrorBadRequest, Retryable: false, Message: "invalid request",
	}
	fallback := &fakeAdapter{response: f.adapter.response}
	if err := f.registry.Register(provider.Target{
		ID: "target_2", DeploymentID: "dep_target_2", PublicModel: "chat", ProviderModel: "provider-model",
		Adapter: fallback, Priority: 1,
	}); err != nil {
		t.Fatal(err)
	}
	_, err := f.service.Chat(context.Background(), f.plaintext, chatRequest())
	if err == nil || f.adapter.calls != 1 || fallback.calls != 0 {
		t.Fatalf("err=%v primary_calls=%d fallback_calls=%d", err, f.adapter.calls, fallback.calls)
	}
}

func TestOpenCircuitSkipsFailedTarget(t *testing.T) {
	f := newFixture(t, 10_000)
	defer f.close()
	f.adapter.err = &provider.Error{Class: provider.ErrorProvider5xx, Retryable: true, Message: "down"}
	fallback := &fakeAdapter{response: f.adapter.response}
	if err := f.registry.Register(provider.Target{
		ID: "target_2", DeploymentID: "dep_target_2", PublicModel: "chat", ProviderModel: "provider-model",
		Adapter: fallback, Priority: 1,
		InputMicrosPerMillion: 1_000_000, OutputMicrosPerMillion: 2_000_000,
	}); err != nil {
		t.Fatal(err)
	}
	service, err := NewServiceWithOptions(f.service.auth, f.registry, f.accounting, ServiceOptions{
		MaxAttempts: 3, MaxAttemptsPerTarget: 2,
		RetryBaseDelay: time.Millisecond, RetryMaxDelay: time.Millisecond,
		CircuitFailureThreshold: 1, CircuitOpenDuration: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 2; index++ {
		if _, err := service.Chat(context.Background(), f.plaintext, chatRequest()); err != nil {
			t.Fatal(err)
		}
	}
	if f.adapter.calls != 1 || fallback.calls != 2 {
		t.Fatalf("primary_calls=%d fallback_calls=%d", f.adapter.calls, fallback.calls)
	}
}

func TestProjectRedactionPolicyTransformsInboundAndOutboundAndBlocksStrictStream(t *testing.T) {
	f := newFixture(t, 1_000_000)
	defer f.close()
	policy := domain.RedactionPolicy{
		ID: "redaction_1", Name: "PII", Enabled: true, Mode: "strict",
		Rules: []domain.RedactionRule{{
			ID: "phone", Name: "Phone", Kind: "builtin", Builtin: "china_phone",
			Scopes: []string{"inbound", "outbound"}, Action: "mask", Enabled: true,
		}},
	}
	engine, err := redaction.New([]domain.RedactionPolicy{policy})
	if err != nil {
		t.Fatal(err)
	}
	f.service.redactor = engine
	f.project.RedactionPolicyID = policy.ID
	plaintext, key, err := auth.GenerateGatewayKey(f.project.ID, "redaction", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.auth.Refresh(context.Background(), source{
		keys: []domain.GatewayKey{key}, projects: []domain.Project{f.project},
	}); err != nil {
		t.Fatal(err)
	}
	f.adapter.response.Choices = []openaiapi.Choice{{Message: &openaiapi.Message{
		Role: "assistant", Content: openaiapi.TextContent("call 13900139000"),
	}}}
	request := chatRequest()
	request.Messages[0].Content = openaiapi.TextContent("call 13800138000")
	response, err := f.service.Chat(context.Background(), plaintext, request)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(f.adapter.lastChatRequest.Messages[0].Content); !strings.Contains(got, "••••8000") ||
		strings.Contains(got, "13800138000") {
		t.Fatalf("inbound request was not masked: %s", got)
	}
	encoded, _ := json.Marshal(response)
	if strings.Contains(string(encoded), "13900139000") || !strings.Contains(string(encoded), "••••9000") {
		t.Fatalf("outbound response was not masked: %s", encoded)
	}
	request.Stream = true
	calls := f.adapter.calls
	err = f.service.ChatStream(context.Background(), plaintext, request, func(openaiapi.ChatCompletionResponse) error {
		return nil
	})
	var gatewayErr *Error
	if !errors.As(err, &gatewayErr) || gatewayErr.Code != "streaming_redaction_incompatible" {
		t.Fatalf("strict stream error=%v", err)
	}
	if f.adapter.calls != calls {
		t.Fatal("strict streaming request reached provider")
	}
}

func TestProjectBoundedRedactionMasksCrossChunkStreamBeforeDelivery(t *testing.T) {
	f := newFixture(t, 1_000_000)
	defer f.close()
	policy := domain.RedactionPolicy{
		ID: "redaction_stream", Name: "Stream PII", Enabled: true, Mode: "bounded_stream",
		Rules: []domain.RedactionRule{{
			ID: "phone", Name: "Phone", Kind: "builtin", Builtin: "china_phone",
			Scopes: []string{"outbound"}, Action: "mask", Enabled: true,
		}},
	}
	engine, err := redaction.New([]domain.RedactionPolicy{policy})
	if err != nil {
		t.Fatal(err)
	}
	f.service.redactor = engine
	f.project.RedactionPolicyID = policy.ID
	plaintext, key, err := auth.GenerateGatewayKey(f.project.ID, "stream-redaction", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.auth.Refresh(context.Background(), source{
		keys: []domain.GatewayKey{key}, projects: []domain.Project{f.project},
	}); err != nil {
		t.Fatal(err)
	}
	f.adapter.streamChunks = nil
	for _, value := range []string{"call ", "138", "0013", "8000", " now"} {
		f.adapter.streamChunks = append(f.adapter.streamChunks, openaiapi.ChatCompletionResponse{
			ID: "chunk", Object: "chat.completion.chunk", Model: "provider-model",
			Choices: []openaiapi.Choice{{Index: 0, Delta: &openaiapi.Message{
				Role: "assistant", Content: openaiapi.TextContent(value),
			}}},
		})
	}
	request := chatRequest()
	request.Stream = true
	var output strings.Builder
	err = f.service.ChatStream(context.Background(), plaintext, request, func(chunk openaiapi.ChatCompletionResponse) error {
		for _, choice := range chunk.Choices {
			if choice.Delta == nil {
				continue
			}
			if value, ok := openaiapi.DecodeTextContent(choice.Delta.Content); ok {
				output.WriteString(value)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := output.String(); got != "call ••••8000 now" ||
		strings.Contains(got, "13800138000") {
		t.Fatalf("unsafe bounded stream output: %q", got)
	}
}

func TestTokenGuardBlocksRepeatedAnomalousRequestsBeforeProvider(t *testing.T) {
	f := newFixture(t, 10_000)
	defer f.close()
	project := f.project
	project.TokenGuardPolicyID = "guard_1"
	plaintext, key, err := auth.GenerateGatewayKey(project.ID, "guarded", nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := auth.NewSnapshot()
	if err := snapshot.Refresh(context.Background(), source{
		keys: []domain.GatewayKey{key}, projects: []domain.Project{project},
	}); err != nil {
		t.Fatal(err)
	}
	guard, err := tokenguard.New([]domain.TokenGuardPolicy{{
		ID: "guard_1", Name: "strict", Enabled: true, Action: "temporary_block",
		RequestTokens: 10, MinimumSamples: 2, ViolationsBeforeBlock: 2,
		BlockTTL: time.Minute, Cooldown: 30 * time.Second,
	}})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewServiceWithOptions(snapshot, f.registry, f.accounting, ServiceOptions{
		TokenGuard: guard, RetryBaseDelay: time.Millisecond, RetryMaxDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Chat(context.Background(), plaintext, chatRequest()); err != nil {
		t.Fatal(err)
	}
	_, err = service.Chat(context.Background(), plaintext, chatRequest())
	var gatewayErr *Error
	if !errors.As(err, &gatewayErr) || gatewayErr.Code != "token_guard_blocked" {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.adapter.calls != 1 {
		t.Fatalf("blocked request reached provider; calls=%d", f.adapter.calls)
	}
}

func TestTokenGuardConcurrencyUsesHeldRequestLeases(t *testing.T) {
	f := newFixture(t, 10_000)
	defer f.close()
	project := f.project
	project.TokenGuardPolicyID = "guard_concurrency"
	plaintext, key, err := auth.GenerateGatewayKey(project.ID, "guarded-concurrency", nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := auth.NewSnapshot()
	if err := snapshot.Refresh(context.Background(), source{keys: []domain.GatewayKey{key}, projects: []domain.Project{project}}); err != nil {
		t.Fatal(err)
	}
	guard, err := tokenguard.New([]domain.TokenGuardPolicy{{
		ID: "guard_concurrency", Name: "concurrency", Enabled: true, Action: "temporary_block",
		Concurrency: 1, MinimumSamples: 2, ViolationsBeforeBlock: 2,
		BlockTTL: time.Minute, Cooldown: 30 * time.Second,
	}})
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	f.adapter.started, f.adapter.release = started, release
	service, err := NewServiceWithOptions(snapshot, f.registry, f.accounting, ServiceOptions{TokenGuard: guard})
	if err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 2)
	for index := 0; index < 2; index++ {
		go func() { _, callErr := service.Chat(context.Background(), plaintext, chatRequest()); errs <- callErr }()
		<-started
	}
	_, err = service.Chat(context.Background(), plaintext, chatRequest())
	var gatewayErr *Error
	if !errors.As(err, &gatewayErr) || gatewayErr.Code != "token_guard_blocked" {
		t.Fatalf("third request=%v", err)
	}
	close(release)
	for index := 0; index < 2; index++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

func TestProjectCIDRAuthorizationUsesTrustedSourceContext(t *testing.T) {
	project := domain.Project{AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("10.20.0.0/16")}}
	allowed := requestmeta.WithSourceIP(context.Background(), netip.MustParseAddr("10.20.1.2"))
	if err := authorizeSource(allowed, project); err != nil {
		t.Fatal(err)
	}
	denied := requestmeta.WithSourceIP(context.Background(), netip.MustParseAddr("10.21.1.2"))
	var gatewayErr *Error
	if err := authorizeSource(denied, project); !errors.As(err, &gatewayErr) ||
		gatewayErr.Code != "source_not_allowed" {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := authorizeSource(context.Background(), project); err == nil {
		t.Fatal("missing source bypassed CIDR policy")
	}
}

func chatRequest() openaiapi.ChatCompletionRequest {
	maxTokens := int64(20)
	return openaiapi.ChatCompletionRequest{
		Model:               "chat",
		Messages:            []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("hello")}},
		MaxCompletionTokens: &maxTokens,
	}
}

func TestChatAuthenticatesRoutesReservesAndSettles(t *testing.T) {
	f := newFixture(t, 1_000)
	defer f.close()
	response, err := f.service.Chat(context.Background(), f.plaintext, chatRequest())
	if err != nil {
		t.Fatal(err)
	}
	if response.Model != "chat" {
		t.Fatalf("public model was not restored: %q", response.Model)
	}
	if f.adapter.calls != 1 {
		t.Fatalf("calls=%d", f.adapter.calls)
	}
	period := time.Now().UTC().Format("2006-01-02")
	balance := f.state.Balance(f.project.ID, period, testTimezoneVersion)
	if balance.ReservedMicrosUSD != 0 || balance.CommittedMicrosUSD != 20 ||
		balance.InputTokens != 10 || balance.OutputTokens != 5 {
		t.Fatalf("unexpected balance: %#v", balance)
	}
}

// The Responses facade decodes its own wire form and enters the generation hot
// path directly. It used to write itself as a Chat request first, which is what
// carried it past the Project's redaction policy — so the thing worth pinning is
// that dropping the translation did not drop the inspection with it.
// A tool the upstream runs itself is egress the operator has to have accepted.
// The request names it, routing sees it as a requirement, and a connection that
// never declared the capability is not a candidate — so the refusal happens
// before any provider call and before any reservation.
func TestWebSearchIsRefusedByARouteThatNeverAcceptedUpstreamEgress(t *testing.T) {
	f := newFixture(t, 1_000)
	defer f.close()
	calls := f.adapter.calls
	_, err := f.service.Responses(context.Background(), f.plaintext, openaiapi.ResponseRequest{
		Model: "chat", Input: json.RawMessage(`"who won?"`),
		Tools: []openaiapi.ResponseTool{{Type: openaiapi.ProviderExecutedToolWebSearch}},
	})
	var gatewayErr *Error
	if !errors.As(err, &gatewayErr) || gatewayErr.Code != "unsupported_feature" {
		t.Fatalf("error=%v", err)
	}
	if !strings.Contains(gatewayErr.Message, "provider_executed_tools") &&
		!strings.Contains(err.Error(), "provider_executed_tools") {
		t.Fatalf("the refusal does not name what was missing: %v", err)
	}
	if f.adapter.calls != calls {
		t.Fatal("a request for upstream egress reached a provider anyway")
	}
}

func TestResponsesInputIsRedactedWithoutBecomingAChatRequest(t *testing.T) {
	f := newFixture(t, 1_000_000)
	defer f.close()
	policy := domain.RedactionPolicy{
		ID: "redaction_responses", Name: "PII", Enabled: true, Mode: "bounded_stream",
		Rules: []domain.RedactionRule{{
			ID: "phone", Name: "Phone", Kind: "builtin", Builtin: "china_phone",
			Scopes: []string{"inbound", "outbound"}, Action: "mask", Enabled: true,
		}},
	}
	engine, err := redaction.New([]domain.RedactionPolicy{policy})
	if err != nil {
		t.Fatal(err)
	}
	f.service.redactor = engine
	f.project.RedactionPolicyID = policy.ID
	plaintext, key, err := auth.GenerateGatewayKey(f.project.ID, "redaction", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.auth.Refresh(context.Background(), source{
		keys: []domain.GatewayKey{key}, projects: []domain.Project{f.project},
	}); err != nil {
		t.Fatal(err)
	}
	maxOutput := int64(20)
	if _, err := f.service.Responses(context.Background(), plaintext, openaiapi.ResponseRequest{
		Model: "chat", Input: json.RawMessage(`"call 13800138000"`), MaxOutputTokens: &maxOutput,
	}); err != nil {
		t.Fatal(err)
	}
	got := string(f.adapter.lastChatRequest.Messages[0].Content)
	if !strings.Contains(got, "••••8000") || strings.Contains(got, "13800138000") {
		t.Fatalf("a Responses input reached the provider unmasked: %s", got)
	}
}

// Capability filtering runs on the requirements the caller's request derived,
// and redaction runs after it. Masking inside a data URL turns bytes the request
// carried into an address somebody has to fetch, which is a capability the
// chosen target was never filtered for — so the request is refused rather than
// quietly executed against a route it no longer matches.
func TestRedactionThatChangesWhatARequestRequiresIsRefused(t *testing.T) {
	f := newFixture(t, 1_000_000)
	defer f.close()
	policy := domain.RedactionPolicy{
		ID: "redaction_image", Name: "URL", Enabled: true, Mode: "bounded_stream",
		Rules: []domain.RedactionRule{{
			ID: "scheme", Name: "Scheme", Kind: "regex", Pattern: "data:image/png;base64,",
			Scopes: []string{"inbound"}, Action: "mask", Enabled: true,
		}},
	}
	engine, err := redaction.New([]domain.RedactionPolicy{policy})
	if err != nil {
		t.Fatal(err)
	}
	f.service.redactor = engine
	f.project.RedactionPolicyID = policy.ID
	plaintext, key, err := auth.GenerateGatewayKey(f.project.ID, "redaction", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.auth.Refresh(context.Background(), source{
		keys: []domain.GatewayKey{key}, projects: []domain.Project{f.project},
	}); err != nil {
		t.Fatal(err)
	}
	// A target that can read an image it is handed and cannot go and get one:
	// the distinction the masked URL crosses.
	replacement := provider.NewRegistry()
	if err := replacement.Register(provider.Target{
		ID: "target_1", DeploymentID: "dep_target_1", PublicModel: "chat", ProviderModel: "provider-model",
		Adapter: f.adapter, Capabilities: provider.Capabilities{Chat: true, Vision: true},
	}); err != nil {
		t.Fatal(err)
	}
	f.registry.Replace(replacement)
	calls := f.adapter.calls
	request := chatRequest()
	request.Messages[0].Content = json.RawMessage(
		`[{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}]`)
	_, err = f.service.Chat(context.Background(), plaintext, request)
	var gatewayErr *Error
	if !errors.As(err, &gatewayErr) || gatewayErr.Code != "sensitive_data_detected" {
		t.Fatalf("error=%v", err)
	}
	if f.adapter.calls != calls {
		t.Fatal("the rerouted request reached a provider anyway")
	}
}

func TestResponsesUsesGenerationHotPathAndRestoresPublicModel(t *testing.T) {
	f := newFixture(t, 1_000)
	defer f.close()
	maxOutput := int64(20)
	response, err := f.service.Responses(context.Background(), f.plaintext, openaiapi.ResponseRequest{
		Model: "chat", Input: json.RawMessage(`"hello"`), MaxOutputTokens: &maxOutput,
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Model != "chat" || response.Object != "response" || response.ID != "resp_1" || f.adapter.calls != 1 {
		t.Fatalf("response=%#v calls=%d", response, f.adapter.calls)
	}
	if f.adapter.lastChatRequest.MaxCompletionTokens == nil || *f.adapter.lastChatRequest.MaxCompletionTokens != maxOutput {
		t.Fatalf("max output token mapping was lost: %#v", f.adapter.lastChatRequest)
	}
}

func TestResponsesRejectsStatefulRequestBeforeProvider(t *testing.T) {
	f := newFixture(t, 1_000)
	defer f.close()
	store := true
	_, err := f.service.Responses(context.Background(), f.plaintext, openaiapi.ResponseRequest{Model: "chat", Input: json.RawMessage(`"hello"`), Store: &store})
	if err == nil {
		t.Fatal("stateful Responses request was accepted")
	}
	if f.adapter.calls != 0 {
		t.Fatalf("stateful request reached provider: calls=%d", f.adapter.calls)
	}
}

func TestBudgetExceededBeforeProviderCall(t *testing.T) {
	f := newFixture(t, 10)
	defer f.close()
	_, err := f.service.Chat(context.Background(), f.plaintext, chatRequest())
	var gatewayErr *Error
	if !errors.As(err, &gatewayErr) || gatewayErr.Code != "budget_exceeded" {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.adapter.calls != 0 {
		t.Fatal("provider was called before budget rejection")
	}
	if got := f.service.RejectionMetrics().Budget; got != 1 {
		t.Fatalf("budget rejections=%d", got)
	}
}

func TestInvalidKeyDoesNotCreateReservation(t *testing.T) {
	f := newFixture(t, 1_000)
	defer f.close()
	_, err := f.service.Chat(context.Background(), "gw_invalid", chatRequest())
	var gatewayErr *Error
	if !errors.As(err, &gatewayErr) || gatewayErr.Code != "invalid_api_key" {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.state.PendingReservations() != 0 || f.adapter.calls != 0 {
		t.Fatal("invalid key changed accounting or called provider")
	}
}

func TestAmbiguousProviderFailureIsEstimatedAndSettled(t *testing.T) {
	f := newFixture(t, 1_000)
	defer f.close()
	f.adapter.err = &provider.Error{
		Class:     provider.ErrorConnect,
		Ambiguous: true,
		Retryable: true,
		Message:   "connection lost",
	}
	fallback := &fakeAdapter{response: f.adapter.response}
	if err := f.registry.Register(provider.Target{
		ID: "target_2", DeploymentID: "dep_target_2", PublicModel: "chat", ProviderModel: "provider-model",
		Adapter: fallback, Priority: 1,
	}); err != nil {
		t.Fatal(err)
	}
	_, err := f.service.Chat(context.Background(), f.plaintext, chatRequest())
	if err == nil {
		t.Fatal("expected provider error")
	}
	period := time.Now().UTC().Format("2006-01-02")
	balance := f.state.Balance(f.project.ID, period, testTimezoneVersion)
	if balance.ReservedMicrosUSD != 0 || balance.CommittedMicrosUSD == 0 {
		t.Fatalf("ambiguous attempt was not conservatively settled: %#v", balance)
	}
	if f.adapter.calls != 1 || fallback.calls != 0 {
		t.Fatalf("ambiguous execution was retried: primary=%d fallback=%d", f.adapter.calls, fallback.calls)
	}
}

func TestAllOperationsPreserveTerminalContextErrors(t *testing.T) {
	f := newFixture(t, 10_000)
	defer f.close()
	f.adapter.err = context.Canceled

	if _, err := f.service.Chat(context.Background(), f.plaintext, chatRequest()); !errors.Is(err, context.Canceled) {
		t.Fatalf("chat error=%v", err)
	}
	if _, err := f.service.Embeddings(context.Background(), f.plaintext, openaiapi.EmbeddingRequest{
		Model: "chat", Input: json.RawMessage(`["hello"]`),
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("embeddings error=%v", err)
	}
	streamRequest := chatRequest()
	streamRequest.Stream = true
	if err := f.service.ChatStream(context.Background(), f.plaintext, streamRequest, func(openaiapi.ChatCompletionResponse) error {
		return nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("stream error=%v", err)
	}
}

func TestEmbeddingsUsesSameAuthorizationAndAccounting(t *testing.T) {
	f := newFixture(t, 1_000)
	defer f.close()
	response, err := f.service.Embeddings(context.Background(), f.plaintext, openaiapi.EmbeddingRequest{
		Model: "chat",
		Input: json.RawMessage(`["hello"]`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Model != "chat" || f.adapter.calls != 1 {
		t.Fatalf("unexpected response or calls: %#v calls=%d", response, f.adapter.calls)
	}
	period := time.Now().UTC().Format("2006-01-02")
	balance := f.state.Balance(f.project.ID, period, testTimezoneVersion)
	if balance.ReservedMicrosUSD != 0 || balance.CommittedMicrosUSD != 4 ||
		balance.InputTokens != 4 {
		t.Fatalf("unexpected embedding balance: %#v", balance)
	}
}

func TestEmbeddingsRejectsRouteWithoutCapabilityBeforeProviderCall(t *testing.T) {
	f := newFixture(t, 1_000)
	defer f.close()
	replacement := provider.NewRegistry()
	if err := replacement.Register(provider.Target{
		ID: "target_1", DeploymentID: "dep_target_1", PublicModel: "chat", ProviderModel: "provider-model",
		Adapter: f.adapter, Capabilities: provider.Capabilities{Chat: true, Streaming: true},
	}); err != nil {
		t.Fatal(err)
	}
	f.registry.Replace(replacement)
	_, err := f.service.Embeddings(context.Background(), f.plaintext, openaiapi.EmbeddingRequest{
		Model: "chat", Input: json.RawMessage(`["hello"]`),
	})
	var gatewayErr *Error
	if !errors.As(err, &gatewayErr) || gatewayErr.Code != "unsupported_feature" || gatewayErr.HTTPStatus != 400 {
		t.Fatalf("unexpected error: %#v", err)
	}
	if f.adapter.calls != 0 {
		t.Fatalf("provider was called %d times", f.adapter.calls)
	}
}

func TestTitanEmbeddingProfileFiltersBatchBeforeProviderIO(t *testing.T) {
	request := semantic.EmbeddingRequest{
		Operation: semantic.OperationEmbed,
		Source:    semantic.Source{ProfileID: "openai.embeddings.v1", ProfileRevision: 1},
		Mode:      semantic.ModePortable, RequestedModel: "embedding", Input: json.RawMessage(`["one","two"]`),
	}
	request.Requirements = request.DeriveRequirements()
	targets := []provider.Target{{ProfileID: domain.ProfileBedrockInvokeTitanEmbedV2}}
	if compatible := filterEmbeddingProfileCompatibility(targets, request); len(compatible) != 0 {
		t.Fatalf("Titan batch request reached provider candidates: %#v", compatible)
	}
	request.Input = json.RawMessage(`"one"`)
	request.Requirements = request.DeriveRequirements()
	if compatible := filterEmbeddingProfileCompatibility(targets, request); len(compatible) != 1 {
		t.Fatalf("Titan single input was filtered: %#v", compatible)
	}
}

func TestChatRejectsUnsupportedSemanticCapabilityBeforeProviderCall(t *testing.T) {
	f := newFixture(t, 1_000)
	defer f.close()
	request := chatRequest()
	request.Tools = []openaiapi.Tool{{Type: "function", Function: openaiapi.ToolFunction{Name: "lookup"}}}
	_, err := f.service.Chat(context.Background(), f.plaintext, request)
	var gatewayErr *Error
	if !errors.As(err, &gatewayErr) || gatewayErr.Code != "unsupported_feature" || gatewayErr.HTTPStatus != 400 {
		t.Fatalf("unexpected error: %#v", err)
	}
	if f.adapter.calls != 0 {
		t.Fatalf("unsupported request reached provider; calls=%d", f.adapter.calls)
	}
}

func TestChatCapabilityFilterSelectsOnlyCompatibleFallback(t *testing.T) {
	all := provider.Capabilities{
		Chat: true, Streaming: true, Tools: true, Vision: true, FetchedImage: true,
		JSONObject: true, StructuredOutputs: true,
		DeveloperRole: true, Reasoning: true, StreamUsage: true,
	}
	targets := []provider.Target{
		{ID: "basic", Capabilities: provider.Capabilities{Chat: true, Streaming: true}},
		{ID: "semantic", Capabilities: all},
	}
	cases := map[string]openaiapi.ChatCompletionRequest{
		"tools": {
			Tools: []openaiapi.Tool{{Type: "function", Function: openaiapi.ToolFunction{Name: "lookup"}}},
		},
		"vision": {
			Messages: []openaiapi.Message{{Role: "user", Content: json.RawMessage(`[{"type":"image_url","image_url":{"url":"https://example.invalid/image.png"}}]`)}},
		},
		"json":      {ResponseFormat: json.RawMessage(`{"type":"json_object"}`)},
		"developer": {Messages: []openaiapi.Message{{Role: "developer", Content: openaiapi.TextContent("policy")}}},
		"reasoning": {ReasoningEffort: "high"},
		"usage":     {StreamOptions: &openaiapi.StreamOptions{IncludeUsage: true}},
	}
	for name, request := range cases {
		t.Run(name, func(t *testing.T) {
			request.Model = "chat"
			if len(request.Messages) == 0 {
				request.Messages = []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("hello")}}
			}
			canonical, err := openaiwire.DecodeGenerate(request)
			if err != nil {
				t.Fatal(err)
			}
			filtered := filterSemanticCapabilities(targets, canonical.Requirements)
			if len(filtered) != 1 || filtered[0].ID != "semantic" {
				t.Fatalf("unexpected targets: %#v", filtered)
			}
			if len(targets) != 2 {
				t.Fatal("capability filtering mutated registry candidates")
			}
		})
	}
}

func TestProfileCompatibilityFilterRejectsFieldsThatWouldBeDropped(t *testing.T) {
	seed := int64(7)
	request := chatRequest()
	request.Seed = &seed
	canonical, err := openaiwire.DecodeGenerate(request)
	if err != nil {
		t.Fatal(err)
	}
	targets := []provider.Target{
		{ID: "gemini", ProfileID: domain.ProfileGeminiText},
		{ID: "openai", ProfileID: domain.ProfileOpenAIChatEmbeddings},
	}
	filtered := filterGenerateProfileCompatibility(targets, canonical)
	if len(filtered) != 1 || filtered[0].ID != "openai" {
		t.Fatalf("unexpected compatible targets: %#v", filtered)
	}
	if len(targets) != 2 {
		t.Fatal("profile filtering mutated registry candidates")
	}
}

// The unprofiled-target branch this test used to cover is gone: an adapter
// without a profile contract can no longer be registered at all, so a target
// carrying capability booleans nothing proves is unrepresentable. Registry
// rejection is asserted in internal/provider (TestUnprofiledAdapterIsRejected).
//
// What remains here is the filter's own contract — that a requirement is only
// satisfied by a capability the target actually holds.
func TestSemanticCapabilityFilterRequiresTheCapabilityItFiltersOn(t *testing.T) {
	capable := provider.Target{ID: "capable", DeploymentID: "dep_capable", Capabilities: provider.Capabilities{
		Chat: true, Streaming: true, Tools: true, Vision: true,
		JSONObject: true, StructuredOutputs: true, DeveloperRole: true, Reasoning: true, StreamUsage: true,
	}}
	bare := provider.Target{ID: "bare", DeploymentID: "dep_bare", Capabilities: provider.Capabilities{Chat: true, Streaming: true}}

	for _, requirement := range []semantic.Requirements{
		{Tools: true}, {Vision: true}, {JSONObject: true}, {StructuredOutputs: true},
		{DeveloperRole: true}, {Reasoning: true}, {StreamUsage: true},
	} {
		filtered := filterSemanticCapabilities([]provider.Target{capable, bare}, requirement)
		if len(filtered) != 1 || filtered[0].ID != "capable" {
			t.Fatalf("requirement %#v kept %d target(s)", requirement, len(filtered))
		}
	}
	if filtered := filterSemanticCapabilities([]provider.Target{bare}, semantic.Requirements{Streaming: true}); len(filtered) != 1 {
		t.Fatal("a plain streaming requirement dropped a chat target")
	}
}

// A data URL is the picture itself in base64. Counting it at the text ratio put a
// modest photograph six figures of tokens over a project limit no provider would
// have applied, and that same estimate bounds the deployment context window, the
// TPM lease and the budget reservation.
func TestImageInputIsEstimatedAtItsCeilingNotItsEncodedLength(t *testing.T) {
	dataURL := "data:image/png;base64," + strings.Repeat("A", 400_000)
	withImage := semantic.GenerateRequest{Messages: []semantic.Message{{
		Role: semantic.RoleUser,
		Content: []semantic.Content{
			{Kind: semantic.ContentText, Text: "describe this"},
			{Kind: semantic.ContentInputImage, URL: dataURL},
		},
	}}}
	// The wire bytes the facade would report: the whole body, image included.
	wireBytes := int64(len(dataURL) + len("describe this") + 64)

	estimate := estimateGenerateInputTokens(wireBytes, withImage)
	if estimate > semantic.ImageInputTokenCeiling+64 {
		t.Fatalf("image estimated at %d tokens; the ceiling is %d", estimate, semantic.ImageInputTokenCeiling)
	}
	if estimate <= semantic.ImageInputTokenCeiling {
		t.Fatalf("image estimated at %d tokens, which does not charge the ceiling plus its prompt", estimate)
	}
	if plain := estimateInputTokens(wireBytes); plain <= estimate*10 {
		t.Fatalf("the text estimate %d was not the outsized one this replaces", plain)
	}

	// Two images cost two ceilings, and a request without one is untouched.
	twoImages := withImage
	twoImages.Messages = []semantic.Message{{Role: semantic.RoleUser, Content: append(
		append([]semantic.Content{}, withImage.Messages[0].Content...),
		semantic.Content{Kind: semantic.ContentInputImage, URL: dataURL},
	)}}
	if got := estimateGenerateInputTokens(wireBytes+int64(len(dataURL)), twoImages); got-estimate != semantic.ImageInputTokenCeiling {
		t.Fatalf("second image added %d tokens", got-estimate)
	}
	textOnly := semantic.GenerateRequest{Messages: []semantic.Message{{
		Role: semantic.RoleUser, Content: []semantic.Content{{Kind: semantic.ContentText, Text: "hello"}},
	}}}
	if got := estimateGenerateInputTokens(400, textOnly); got != estimateInputTokens(400) {
		t.Fatalf("a request without an image estimated %d, not %d", got, estimateInputTokens(400))
	}
}

// A remote URL is a few dozen bytes either way, but it is still not prose, and a
// request that carries one is still asking a model to look at a picture.
func TestRemoteImageURLAlsoChargesTheCeiling(t *testing.T) {
	request := semantic.GenerateRequest{Messages: []semantic.Message{{
		Role:    semantic.RoleUser,
		Content: []semantic.Content{{Kind: semantic.ContentInputImage, URL: "https://example.invalid/a.png"}},
	}}}
	if got := estimateGenerateInputTokens(120, request); got < semantic.ImageInputTokenCeiling {
		t.Fatalf("remote image estimated %d tokens", got)
	}
}

func TestTokenCapabilityFilterHonorsContextAndOutputLimits(t *testing.T) {
	targets := []provider.Target{
		{ID: "small", Capabilities: provider.Capabilities{MaxContextTokens: 100, MaxOutputTokens: 20}},
		{ID: "large", Capabilities: provider.Capabilities{MaxContextTokens: 1_000, MaxOutputTokens: 200}},
	}
	filtered := filterTokenCapabilities(targets, 90, 21)
	if len(filtered) != 1 || filtered[0].ID != "large" {
		t.Fatalf("unexpected targets: %#v", filtered)
	}
	filtered = filterTokenCapabilities(targets, 990, 5)
	if len(filtered) != 1 || filtered[0].ID != "large" {
		t.Fatalf("context filtering failed: %#v", filtered)
	}
	if len(targets) != 2 {
		t.Fatal("token filtering mutated registry candidates")
	}
}

func TestProviderConcurrencyRejectsBeforeReservationAndReleasesLease(t *testing.T) {
	f := newFixture(t, 10_000)
	defer f.close()
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	f.adapter.started = started
	f.adapter.release = release
	replacement := provider.NewRegistry()
	if err := replacement.Register(provider.Target{
		ID: "target_1", DeploymentID: "dep_target_1", ProviderID: "provider_1", PublicModel: "chat",
		ProviderModel: "provider-model", Adapter: f.adapter,
		InputMicrosPerMillion: 1_000_000, OutputMicrosPerMillion: 2_000_000,
		MaxConcurrency: 1,
	}); err != nil {
		t.Fatal(err)
	}
	f.registry.Replace(replacement)
	firstResult := make(chan error, 1)
	go func() {
		_, err := f.service.Chat(context.Background(), f.plaintext, chatRequest())
		firstResult <- err
	}()
	<-started

	_, err := f.service.Chat(context.Background(), f.plaintext, chatRequest())
	var gatewayErr *Error
	if !errors.As(err, &gatewayErr) ||
		gatewayErr.Code != "provider_concurrency_limit_exceeded" ||
		gatewayErr.HTTPStatus != 429 {
		t.Fatalf("unexpected rejection: %#v", err)
	}
	if got := f.service.RejectionMetrics().ProviderConcurrency; got != 1 {
		t.Fatalf("provider concurrency rejections=%d", got)
	}
	close(release)
	if err := <-firstResult; err != nil {
		t.Fatalf("admitted request failed: %v", err)
	}
	if active := f.service.ActiveProviderRequests(); len(active) != 0 {
		t.Fatalf("provider lease leaked: %#v", active)
	}
}

func TestProviderConcurrencyFallsBackToAvailableProvider(t *testing.T) {
	f := newFixture(t, 10_000)
	defer f.close()
	fallback := &fakeAdapter{response: f.adapter.response}
	replacement := provider.NewRegistry()
	for _, target := range []provider.Target{
		{
			ID: "target_1", DeploymentID: "dep_target_1", ProviderID: "provider_1", PublicModel: "chat",
			ProviderModel: "provider-model", Adapter: f.adapter,
			InputMicrosPerMillion: 1_000_000, OutputMicrosPerMillion: 2_000_000,
			MaxConcurrency: 1, Priority: 0,
		},
		{
			ID: "target_2", DeploymentID: "dep_target_2", ProviderID: "provider_2", PublicModel: "chat",
			ProviderModel: "provider-model", Adapter: fallback,
			InputMicrosPerMillion: 1_000_000, OutputMicrosPerMillion: 2_000_000,
			MaxConcurrency: 1, Priority: 1,
		},
	} {
		if err := replacement.Register(target); err != nil {
			t.Fatal(err)
		}
	}
	f.registry.Replace(replacement)
	occupied, err := f.service.providerConcurrency.Acquire("provider_1", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Release()
	response, err := f.service.Chat(context.Background(), f.plaintext, chatRequest())
	if err != nil {
		t.Fatal(err)
	}
	if response.ID == "" || f.adapter.calls != 0 || fallback.calls != 1 {
		t.Fatalf("primary_calls=%d fallback_calls=%d response=%#v", f.adapter.calls, fallback.calls, response)
	}
	if got := f.service.RejectionMetrics().ProviderConcurrency; got != 1 {
		t.Fatalf("provider concurrency rejections=%d", got)
	}
}

func TestDeploymentConcurrencyRejectsWithoutLeakingProviderLease(t *testing.T) {
	f := newFixture(t, 10_000)
	defer f.close()
	replacement := provider.NewRegistry()
	if err := replacement.Register(provider.Target{
		ID: "route_1", DeploymentID: "deployment_1", ProviderID: "provider_1",
		PublicModel: "chat", ProviderModel: "provider-model", Adapter: f.adapter,
		InputMicrosPerMillion: 1_000_000, OutputMicrosPerMillion: 2_000_000,
		DeploymentConcurrency: 1,
	}); err != nil {
		t.Fatal(err)
	}
	f.registry.Replace(replacement)
	occupied, err := f.service.deploymentConcurrency.Acquire("deployment_1", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Release()

	_, err = f.service.Chat(context.Background(), f.plaintext, chatRequest())
	var gatewayErr *Error
	if !errors.As(err, &gatewayErr) || gatewayErr.Code != "deployment_concurrency_limit_exceeded" {
		t.Fatalf("unexpected rejection: %#v", err)
	}
	if f.adapter.calls != 0 {
		t.Fatalf("provider was called %d times", f.adapter.calls)
	}
	if active := f.service.ActiveProviderRequests(); len(active) != 0 {
		t.Fatalf("provider lease leaked after deployment rejection: %#v", active)
	}
	if got := f.service.RejectionMetrics().DeploymentConcurrency; got != 1 {
		t.Fatalf("deployment concurrency rejections=%d", got)
	}
}

func TestProjectLimiterRejectionMetricsByReason(t *testing.T) {
	f := newFixture(t, 10_000)
	defer f.close()
	tests := []struct {
		err  error
		want func(RejectionMetrics) uint64
	}{
		{limiter.ErrRPM, func(metrics RejectionMetrics) uint64 { return metrics.RPM }},
		{limiter.ErrTPM, func(metrics RejectionMetrics) uint64 { return metrics.TPM }},
		{limiter.ErrConcurrency, func(metrics RejectionMetrics) uint64 {
			return metrics.ProjectConcurrency
		}},
	}
	for _, test := range tests {
		mapped := f.service.mapLimitError(test.err)
		var gatewayErr *Error
		if !errors.As(mapped, &gatewayErr) || gatewayErr.HTTPStatus != 429 {
			t.Fatalf("unexpected mapped error: %#v", mapped)
		}
		if got := test.want(f.service.RejectionMetrics()); got != 1 {
			t.Fatalf("rejection metric=%d for %v", got, test.err)
		}
	}
}

func TestEmbeddingsRetriesAndFallsBack(t *testing.T) {
	f := newFixture(t, 10_000)
	defer f.close()
	f.adapter.err = &provider.Error{Class: provider.ErrorProvider5xx, Retryable: true, Message: "unavailable"}
	fallback := &fakeAdapter{embeddingResponse: openaiapi.EmbeddingResponse{
		Object: "list", Model: "provider-model",
		Data:  []openaiapi.EmbeddingData{{Object: "embedding", Embedding: json.RawMessage(`[0.5]`)}},
		Usage: &openaiapi.Usage{PromptTokens: 4, TotalTokens: 4},
	}}
	if err := f.registry.Register(provider.Target{
		ID: "target_2", DeploymentID: "dep_target_2", PublicModel: "chat", ProviderModel: "provider-model",
		Adapter: fallback, Priority: 1, InputMicrosPerMillion: 1_000_000,
	}); err != nil {
		t.Fatal(err)
	}
	response, err := f.service.Embeddings(context.Background(), f.plaintext, openaiapi.EmbeddingRequest{
		Model: "chat", Input: json.RawMessage(`["hello"]`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Model != "chat" || f.adapter.calls != 2 || fallback.calls != 1 {
		t.Fatalf("response=%#v primary_calls=%d fallback_calls=%d", response, f.adapter.calls, fallback.calls)
	}
}

func TestChatStreamUsesPublicModelAndSettlesUsage(t *testing.T) {
	f := newFixture(t, 1_000)
	defer f.close()
	request := chatRequest()
	request.Stream = true
	var chunks []openaiapi.ChatCompletionResponse
	err := f.service.ChatStream(context.Background(), f.plaintext, request, func(chunk openaiapi.ChatCompletionResponse) error {
		chunks = append(chunks, chunk)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 2 || chunks[0].Model != "chat" || chunks[1].Model != "chat" || chunks[1].Choices[0].FinishReason == nil {
		t.Fatalf("unexpected chunks: %#v", chunks)
	}
	period := time.Now().UTC().Format("2006-01-02")
	balance := f.state.Balance(f.project.ID, period, testTimezoneVersion)
	if balance.ReservedMicrosUSD != 0 || balance.CommittedMicrosUSD != 20 ||
		balance.InputTokens != 10 || balance.OutputTokens != 5 {
		t.Fatalf("unexpected stream balance: %#v", balance)
	}
}

// A tool-forced turn can deliver its whole output as call arguments, which the
// delivered-byte count ignored. The estimate was then capped at the one-token
// floor, so a client that dropped the connection before the usage frame left the
// ledger recording a single token against everything the provider generated and
// billed — repeatable at will, and invisible to budget enforcement.
func TestAbortedToolArgumentStreamIsNotBilledAsOneToken(t *testing.T) {
	f := newFixture(t, 1_000)
	defer f.close()
	f.adapter.streamUsage = nil
	f.adapter.streamChunks = nil
	for index := range 4 {
		f.adapter.streamChunks = append(
			f.adapter.streamChunks, toolArgumentChunk(index, strings.Repeat("a", 2000)),
		)
	}
	request := chatRequest()
	request.Stream = true
	received := 0
	err := f.service.ChatStream(context.Background(), f.plaintext, request,
		func(openaiapi.ChatCompletionResponse) error {
			received++
			if received == 2 {
				return errors.New("client went away")
			}
			return nil
		})
	if err == nil {
		t.Fatal("an aborted stream reported success")
	}
	period := time.Now().UTC().Format("2006-01-02")
	balance := f.state.Balance(f.project.ID, period, testTimezoneVersion)
	// chatRequest asks for 20 output tokens and the delivered arguments exceed
	// that, so the estimate stands rather than being capped down to the floor.
	if balance.OutputTokens != 20 {
		t.Fatalf("tool-call output was not accounted: %#v", balance)
	}
}

func toolArgumentChunk(index int, arguments string) openaiapi.ChatCompletionResponse {
	callIndex := 0
	call := openaiapi.ToolCall{
		Index:    &callIndex,
		Function: openaiapi.ToolCallFunction{Arguments: arguments},
	}
	if index == 0 {
		call.ID, call.Type, call.Function.Name = "call_1", "function", "lookup"
	}
	return openaiapi.ChatCompletionResponse{
		ID: "chunk", Object: "chat.completion.chunk", Model: "provider-model",
		Choices: []openaiapi.Choice{{
			Index: 0, Delta: &openaiapi.Message{Role: "assistant", ToolCalls: []openaiapi.ToolCall{call}},
		}},
	}
}

func TestChatStreamFallsBackOnlyBeforeFirstPayload(t *testing.T) {
	f := newFixture(t, 10_000)
	defer f.close()
	f.adapter.streamChunks = nil
	f.adapter.streamUsage = nil
	f.adapter.err = &provider.Error{
		Class: provider.ErrorConnect, Retryable: true, Message: "connect failed before write",
	}
	fallback := &fakeAdapter{
		streamChunks: []openaiapi.ChatCompletionResponse{{
			ID: "fallback_chunk", Object: "chat.completion.chunk",
			Choices: []openaiapi.Choice{{Index: 0, Delta: &openaiapi.Message{
				Role: "assistant", Content: openaiapi.TextContent("fallback"),
			}}},
		}},
		streamUsage: &openaiapi.Usage{PromptTokens: 4, CompletionTokens: 2, TotalTokens: 6},
	}
	if err := f.registry.Register(provider.Target{
		ID: "target_2", DeploymentID: "dep_target_2", PublicModel: "chat", ProviderModel: "provider-model",
		Adapter: fallback, Priority: 1,
		InputMicrosPerMillion: 1_000_000, OutputMicrosPerMillion: 2_000_000,
	}); err != nil {
		t.Fatal(err)
	}
	request := chatRequest()
	request.Stream = true
	var chunks int
	if err := f.service.ChatStream(context.Background(), f.plaintext, request, func(openaiapi.ChatCompletionResponse) error {
		chunks++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if chunks != 2 || f.adapter.calls != 2 || fallback.calls != 1 {
		t.Fatalf("chunks=%d primary_calls=%d fallback_calls=%d", chunks, f.adapter.calls, fallback.calls)
	}
}

func TestChatStreamNeverFallsBackAfterPayload(t *testing.T) {
	f := newFixture(t, 10_000)
	defer f.close()
	f.adapter.err = &provider.Error{
		Class: provider.ErrorProvider5xx, Retryable: true, Ambiguous: true, Message: "mid-stream failure",
	}
	fallback := &fakeAdapter{streamChunks: f.adapter.streamChunks, streamUsage: f.adapter.streamUsage}
	if err := f.registry.Register(provider.Target{
		ID: "target_2", DeploymentID: "dep_target_2", PublicModel: "chat", ProviderModel: "provider-model",
		Adapter: fallback, Priority: 1,
	}); err != nil {
		t.Fatal(err)
	}
	request := chatRequest()
	request.Stream = true
	err := f.service.ChatStream(context.Background(), f.plaintext, request, func(openaiapi.ChatCompletionResponse) error {
		return nil
	})
	if err == nil || f.adapter.calls != 1 || fallback.calls != 0 {
		t.Fatalf("err=%v primary_calls=%d fallback_calls=%d", err, f.adapter.calls, fallback.calls)
	}
}

// A refusal that named a field has to reach the caller naming it. The upstream's
// own code stays behind — it identifies the provider sitting behind a public
// model alias — but the parameter is the caller's own field, and without it the
// only way to act on "provider rejected the request" is to bisect the payload.
func TestProviderRefusalNamesTheFieldButNotTheProvider(t *testing.T) {
	for _, testCase := range []struct {
		name         string
		providerCode string
		param        string
	}{
		{"joined path", "unsupported_parameter:messages[0].content[1].image_url", "messages[0].content[1].image_url"},
		{"code only", "invalid_request_error", ""},
		{"no code at all", "", ""},
		{"parameter outside the identifier set", "unsupported_parameter:a b", ""},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			mapped := mapProviderError(&provider.Error{
				Class: provider.ErrorBadRequest, StatusCode: 400,
				Message: "prose the upstream wrote about the request", ProviderCode: testCase.providerCode,
			})
			var gatewayErr *Error
			if !errors.As(mapped, &gatewayErr) {
				t.Fatalf("not a gateway error: %v", mapped)
			}
			if gatewayErr.Param != testCase.param {
				t.Fatalf("param=%q want %q", gatewayErr.Param, testCase.param)
			}
			if strings.Contains(gatewayErr.Code, "unsupported_parameter") ||
				strings.Contains(gatewayErr.Message, "unsupported_parameter") ||
				strings.Contains(gatewayErr.Message, "prose the upstream wrote") {
				t.Fatalf("upstream vocabulary reached the caller: code=%q message=%q", gatewayErr.Code, gatewayErr.Message)
			}
		})
	}
}

func TestProviderRetryAfterPropagatesAndDelaysRetry(t *testing.T) {
	upstream := &provider.Error{
		Class: provider.ErrorRateLimit, Retryable: true, RetryAfter: 50 * time.Millisecond,
	}
	mapped := mapProviderError(upstream)
	var gatewayErr *Error
	if !errors.As(mapped, &gatewayErr) || gatewayErr.RetryAfter != upstream.RetryAfter {
		t.Fatalf("mapped retry-after=%#v", gatewayErr)
	}
	service := &Service{retryBaseDelay: 0, retryMaxDelay: upstream.RetryAfter}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if err := service.waitRetry(ctx, 0, upstream); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("provider retry-after was ignored: %v", err)
	}
}

// A request that fails after its attempt exists but before the provider is
// called has to give back everything the attempt took. Every such path is a
// local failure behind adapter resolution, which no adapter in the tree can
// currently produce — so the helper they all share is tested directly, and a
// future adapter that makes one of those branches reachable inherits the
// cleanup instead of having to rediscover it.
func TestAbortReleasesEverythingTheAttemptTook(t *testing.T) {
	f := newFixture(t, 1_000_000)
	defer f.close()
	// One slot, so a lease that is not returned is the difference between the
	// next attempt starting and being turned away.
	limited := provider.NewRegistry()
	if err := limited.Register(provider.Target{
		ID: "target_1", DeploymentID: "dep_target_1",
		PublicModel:            "chat",
		ProviderModel:          "provider-model",
		Adapter:                f.adapter,
		MaxConcurrency:         1,
		InputMicrosPerMillion:  1_000_000,
		OutputMicrosPerMillion: 2_000_000,
	}); err != nil {
		t.Fatal(err)
	}
	f.registry.Replace(limited)

	ctx := context.Background()
	principal, targets, err := f.service.resolveRequest(ctx, f.plaintext, "chat", provider.OperationChat, "chat is unavailable")
	if err != nil {
		t.Fatal(err)
	}
	target := targets[0]
	run, err := f.service.beginRequestRun(ctx, principal, "chat", targets, 15, 10, 5, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer run.close()
	attempt, err := f.service.startAttempt(ctx, run, target, 10, 5, 0, 0, 1)
	if err != nil {
		t.Fatal(err)
	}

	second, err := f.service.startAttempt(ctx, run, target, 10, 5, 0, 0, 2)
	if err == nil {
		_ = second.abort("test_cleanup")
		t.Fatal("the fixture does not actually bound concurrency, so this proves nothing")
	}

	if abortErr := attempt.abort("unsupported_feature"); abortErr != nil {
		t.Fatalf("abort could not settle the attempt: %v", abortErr)
	}
	replacement, err := f.service.startAttempt(ctx, run, target, 10, 5, 0, 0, 3)
	if err != nil {
		t.Fatalf("the aborted attempt held its concurrency slot: %v", err)
	}
	if abortErr := replacement.abort("unsupported_feature"); abortErr != nil {
		t.Fatalf("second abort: %v", abortErr)
	}
}

// The gateway's clock decides authentication timestamps, price selection,
// rate-limit buckets and Token Guard windows. It was hard-wired to time.Now, so
// a test in any other package could pin the accounting clock and still have the
// gateway disagree with it about what day it is.
func TestGatewayClockComesFromOptions(t *testing.T) {
	fixed := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	f := newFixtureAt(t, 1_000_000, ledger.Options{}, func() time.Time { return fixed })
	defer f.close()
	if got := f.service.now(); !got.Equal(fixed) {
		t.Fatalf("the service kept its own clock: %s", got)
	}
	if _, err := f.service.Chat(context.Background(), f.plaintext, chatRequest()); err != nil {
		t.Fatalf("a fixed clock broke an ordinary request: %v", err)
	}
}

// The daily budget belongs to the project, so no other candidate can change the
// answer. Every extra candidate the gateway tried anyway took the pricing lock,
// selected a price, wrote a pin, failed the same check and deleted the pin —
// disk writes multiplied by the number of deployments, on every request, for as
// long as the budget stayed spent.
func TestBudgetExceededStopsAtTheFirstCandidate(t *testing.T) {
	f := newFixture(t, 10)
	defer f.close()
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	price := domain.DeploymentPriceVersion{
		ID: "price_budget", DeploymentID: "dep_budget", Version: 1, Revision: 1,
		BillingMode: domain.BillingModeMetered, Currency: "USD", FormulaVersion: domain.PriceFormulaUSDTokensV1,
		InputMicrosPerMillion: 1_000_000, OutputMicrosPerMillion: 2_000_000,
		EffectiveFrom: now.Add(-time.Hour), CreatedBy: "test", CreatedAt: now.Add(-time.Hour),
		Source: domain.PriceSource{Type: domain.PriceSourceManual, Assurance: domain.PriceAssuranceAsserted,
			ReceivedAt: now.Add(-time.Hour), ContentSHA256: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Reference: "test", AssertedWithoutArchive: true},
	}
	priceStore := &fakePricePinStore{price: price}
	registry := provider.NewRegistry()
	for index, id := range []string{"target_a", "target_b", "target_c"} {
		if err := registry.Register(provider.Target{
			ID: id, DeploymentID: "dep_budget", PublicModel: "chat", ProviderModel: "provider-model",
			Adapter: f.adapter, Priority: index, InputMicrosPerMillion: 99, OutputMicrosPerMillion: 99,
		}); err != nil {
			t.Fatal(err)
		}
	}
	service, err := NewServiceWithOptions(f.service.auth, registry, f.accounting, ServiceOptions{
		Pricing: priceStore,
		Now:     func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = service.Chat(context.Background(), f.plaintext, chatRequest())
	var gatewayErr *Error
	if !errors.As(err, &gatewayErr) || gatewayErr.Code != "budget_exceeded" {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.adapter.calls != 0 {
		t.Fatalf("provider was called for an over-budget request: calls=%d", f.adapter.calls)
	}
	if prepares := priceStore.prepares.Load(); prepares != 1 {
		t.Fatalf("the pricing dance ran once per candidate: prepares=%d", prepares)
	}
	if deletes := priceStore.deletes.Load(); deletes != 1 {
		t.Fatalf("pins written and deleted per candidate: deletes=%d", deletes)
	}
}

// A stream that breaks after a few tokens used to settle at the project's
// output ceiling, because that is what an unspecified max_tokens estimates to.
// The gateway knows how much it actually wrote out, and that is a real upper
// bound on what the provider produced.
func TestInterruptedStreamIsBilledForWhatItDelivered(t *testing.T) {
	billedFor := func(t *testing.T, text string) int64 {
		t.Helper()
		f := newFixture(t, 1_000_000)
		defer f.close()
		f.adapter.streamChunks = []openaiapi.ChatCompletionResponse{{
			ID: "chunk_1", Object: "chat.completion.chunk", Model: "provider-model",
			Choices: []openaiapi.Choice{{Index: 0, Delta: &openaiapi.Message{
				Role: "assistant", Content: openaiapi.TextContent(text),
			}}},
		}}
		// No usage from the provider and a stream that ends in failure: the
		// settlement has nothing to go on but the estimate.
		f.adapter.streamUsage = nil
		f.adapter.err = errors.New("upstream cut the stream")

		request := chatRequest()
		request.Stream = true
		request.MaxTokens = nil
		if err := f.service.ChatStream(context.Background(), f.plaintext, request, func(openaiapi.ChatCompletionResponse) error { return nil }); err == nil {
			t.Fatal("expected the interrupted stream to fail")
		}
		balance := f.state.Balance(f.project.ID, time.Now().UTC().Format("2006-01-02"), testTimezoneVersion)
		if balance.CommittedMicrosUSD <= 0 {
			t.Fatalf("nothing was settled: %#v", balance)
		}
		return balance.CommittedMicrosUSD
	}

	short := billedFor(t, "twenty bytes of text")
	long := billedFor(t, strings.Repeat("x", 200))
	if short >= long {
		t.Fatalf("the delivered amount did not reach the bill: short=%d long=%d", short, long)
	}
}

// testTimezoneVersion matches the version the fixtures' fixed period resolver
// stamps onto events. Balances are keyed by it, so a lookup that omitted it
// would read an empty balance and quietly assert nothing.
const testTimezoneVersion = 1

// An expired or wrong-project Bedrock API key answers 401 or 403, which every
// adapter classifies as ErrorAuthentication with Retryable false. That must
// stop the request rather than move it to a standby deployment: falling back
// would hide a credential the operator has to rotate, and spend the fallback's
// budget doing it.
func TestChatDoesNotFallbackForProviderAuthenticationFailure(t *testing.T) {
	for _, status := range []int{401, 403} {
		f := newFixture(t, 10_000)
		f.adapter.err = &provider.Error{
			Class: provider.ErrorAuthentication, StatusCode: status, Retryable: false, Message: "denied",
		}
		fallback := &fakeAdapter{response: f.adapter.response}
		if err := f.registry.Register(provider.Target{
			ID: "target_2", DeploymentID: "dep_target_2", PublicModel: "chat", ProviderModel: "provider-model",
			Adapter: fallback, Priority: 1,
		}); err != nil {
			t.Fatal(err)
		}
		_, err := f.service.Chat(context.Background(), f.plaintext, chatRequest())
		var classified *Error
		if !errors.As(err, &classified) {
			t.Fatalf("status %d produced an unclassified error: %v", status, err)
		}
		if classified.Code != "provider_authentication_error" {
			t.Fatalf("status %d surfaced as %q", status, classified.Code)
		}
		if f.adapter.calls != 1 || fallback.calls != 0 {
			t.Fatalf("status %d primary_calls=%d fallback_calls=%d", status, f.adapter.calls, fallback.calls)
		}
		f.close()
	}
}

// What the accounting record says about an authentication failure is what an
// operator sees days later. It must name the class — so an expired key is
// distinguishable from a rate limit or a provider outage — and it must not
// carry the provider's error text, which can quote the request.
func TestProviderAuthenticationFailureIsAccountedAsAuthenticationWithoutProviderText(t *testing.T) {
	f := newFixture(t, 10_000)
	defer f.close()
	f.adapter.err = &provider.Error{
		Class: provider.ErrorAuthentication, StatusCode: 403, Retryable: false,
		Message: "project proj_secret is archived; caller sk-leak has no access",
	}
	var records []ledger.Record
	f.accounting.AddObserver(func(record ledger.Record) {
		records = append(records, record)
	})

	if _, err := f.service.Chat(context.Background(), f.plaintext, chatRequest()); err == nil {
		t.Fatal("an authentication failure was reported as success")
	}

	classified := 0
	for _, record := range records {
		encoded, marshalErr := json.Marshal(record.Event)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		for _, leaked := range []string{"proj_secret", "sk-leak", "is archived"} {
			if strings.Contains(string(encoded), leaked) {
				t.Fatalf("the provider error text reached the ledger: %q", leaked)
			}
		}
		if record.Event.ErrorClass == "" {
			continue
		}
		if record.Event.ErrorClass != string(provider.ErrorAuthentication) {
			t.Fatalf("attempt recorded error_class=%q", record.Event.ErrorClass)
		}
		classified++
	}
	if classified == 0 {
		t.Fatalf("no accounting record classified the failure: %d records", len(records))
	}
}

// A Mantle target must refuse a request its profile cannot serve before any
// Provider I/O: no upstream call, no budget consumed.
//
// Two distinct filters can do that, and this pins both rather than assuming
// which one fires. reasoning_effort on the Responses profile is refused by the
// profile's field manifest — that profile cannot preserve reasoning items, so
// the field is unsupported regardless of what the target declares. An image
// input is refused by the capability filter, from the target's own declaration.
// Verified by inverting each: giving the Responses target Reasoning does not
// make the first pass, because the field manifest is what refuses it.
func TestMantleTargetRefusesUnservableRequestsBeforeProviderIO(t *testing.T) {
	f := newFixture(t, 10_000)
	defer f.close()
	ceiling := domain.DefaultProviderCapabilitiesForProfile(domain.ProviderBedrock, domain.ProfileBedrockMantleOpenAIResponses)
	f.registry = provider.NewRegistry()
	mantle := &fakeAdapter{response: f.adapter.response, profileID: domain.ProfileBedrockMantleOpenAIResponses}
	if err := f.registry.Register(provider.Target{
		ID: "target_mantle", DeploymentID: "dep_mantle", PublicModel: "chat", ProviderModel: "provider-model",
		Adapter: mantle, ProfileID: domain.ProfileBedrockMantleOpenAIResponses,
		// Narrowed below the ceiling on vision, which a deployment is allowed to
		// do and which is what makes the capability filter observable here.
		Capabilities: provider.Capabilities{
			Chat: ceiling.Chat, Streaming: ceiling.Streaming, Tools: ceiling.Tools, Vision: false,
			JSONObject: ceiling.JSONObject, StructuredOutputs: ceiling.StructuredOutputs,
			DeveloperRole: ceiling.DeveloperRole,
			Reasoning:     ceiling.Reasoning, StreamUsage: ceiling.StreamUsage,
		},
	}); err != nil {
		t.Fatal(err)
	}
	service, err := NewServiceWithOptions(f.service.auth, f.registry, f.accounting, ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}

	reasoning := chatRequest()
	reasoning.ReasoningEffort = "high"
	assertRefusedBeforeProviderIO(t, service, f.plaintext, reasoning, mantle, "reasoning_effort")

	vision := chatRequest()
	vision.Messages = []openaiapi.Message{{Role: "user", Content: json.RawMessage(
		`[{"type":"image_url","image_url":{"url":"https://example.invalid/image.png"}}]`,
	)}}
	assertRefusedBeforeProviderIO(t, service, f.plaintext, vision, mantle, "image input")
}

// The declaration only matters if it stops the request here. A Mantle target that
// declares vision still cannot fetch a picture, so the remote address is refused
// with no provider call and no budget spent — while the same target serves the
// same image inlined, which is the shape the console produces for a local file.
func TestMantleRefusesAnImageItWouldHaveToFetchAndServesAnInlinedOne(t *testing.T) {
	f := newFixture(t, 10_000)
	defer f.close()
	ceiling := domain.DefaultProviderCapabilitiesForProfile(domain.ProviderBedrock, domain.ProfileBedrockMantleOpenAIChat)
	f.registry = provider.NewRegistry()
	mantle := &fakeAdapter{response: f.adapter.response, profileID: domain.ProfileBedrockMantleOpenAIChat}
	if err := f.registry.Register(provider.Target{
		ID: "target_mantle", DeploymentID: "dep_mantle", PublicModel: "chat", ProviderModel: "provider-model",
		Adapter: mantle, ProfileID: domain.ProfileBedrockMantleOpenAIChat,
		// Vision at the profile ceiling: the target can see an image. What it
		// cannot do is go and get one.
		Capabilities: provider.Capabilities{
			Chat: ceiling.Chat, Streaming: ceiling.Streaming, Tools: ceiling.Tools, Vision: ceiling.Vision,
			JSONObject: ceiling.JSONObject, StructuredOutputs: ceiling.StructuredOutputs,
			DeveloperRole: ceiling.DeveloperRole,
			Reasoning:     ceiling.Reasoning, StreamUsage: ceiling.StreamUsage,
		},
	}); err != nil {
		t.Fatal(err)
	}
	service, err := NewServiceWithOptions(f.service.auth, f.registry, f.accounting, ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}

	remote := chatRequest()
	remote.Messages = []openaiapi.Message{{Role: "user", Content: json.RawMessage(
		`[{"type":"image_url","image_url":{"url":"https://example.invalid/photo.jpg"}}]`,
	)}}
	assertRefusedBeforeProviderIO(t, service, f.plaintext, remote, mantle, "a remote image URL")

	inline := chatRequest()
	inline.Messages = []openaiapi.Message{{Role: "user", Content: json.RawMessage(
		`[{"type":"image_url","image_url":{"url":"data:image/png;base64,aGk="}}]`,
	)}}
	if _, err := service.Chat(context.Background(), f.plaintext, inline); err != nil {
		t.Fatalf("an inlined image was refused: %v", err)
	}
	if mantle.calls != 1 {
		t.Fatalf("the inlined image reached the provider %d time(s)", mantle.calls)
	}
}

// A refusal that does not name its reason sends the operator to bisect a request
// body. Every reason is already computed by the filters, and each one is an
// identifier — a capability key the console shows, or a request field the
// published manifest lists — so the refusal can carry them.
//
// Both reasons here are capability keys, which is what moving the fetch limit
// out of the field layer bought: the operator is told "fetched_image, vision"
// and can find both as ticks on the deployment form, instead of being handed one
// endpoint's spelling of a member and left to work out which box it belongs to.
func TestUnservableRouteNamesTheCapabilityAndFieldItRefused(t *testing.T) {
	f := newFixture(t, 10_000)
	defer f.close()
	f.registry = provider.NewRegistry()
	// One target that can see an image but cannot fetch one, and one that can
	// fetch but was never given vision. Neither can serve the request, and an
	// operator sent to fix only one of them fixes nothing.
	mantle := &fakeAdapter{response: f.adapter.response, profileID: domain.ProfileBedrockMantleOpenAIChat}
	if err := f.registry.Register(provider.Target{
		ID: "target_mantle", DeploymentID: "dep_mantle", PublicModel: "chat", ProviderModel: "provider-model",
		Adapter: mantle, ProfileID: domain.ProfileBedrockMantleOpenAIChat,
		Capabilities: provider.Capabilities{Chat: true, Streaming: true, Vision: true},
	}); err != nil {
		t.Fatal(err)
	}
	blind := &fakeAdapter{response: f.adapter.response, profileID: domain.ProfileOpenAIChatEmbeddings}
	if err := f.registry.Register(provider.Target{
		ID: "target_blind", DeploymentID: "dep_blind", PublicModel: "chat", ProviderModel: "provider-model",
		Adapter: blind, ProfileID: domain.ProfileOpenAIChatEmbeddings, Priority: 1,
		Capabilities: provider.Capabilities{Chat: true, Streaming: true},
	}); err != nil {
		t.Fatal(err)
	}
	service, err := NewServiceWithOptions(f.service.auth, f.registry, f.accounting, ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}

	request := chatRequest()
	request.Messages = []openaiapi.Message{{Role: "user", Content: json.RawMessage(
		`[{"type":"image_url","image_url":{"url":"https://example.invalid/photo.jpg"}}]`,
	)}}
	_, err = service.Chat(context.Background(), f.plaintext, request)
	var classified *Error
	if !errors.As(err, &classified) {
		t.Fatalf("the route did not refuse the request: %v", err)
	}
	if classified.Code != "unsupported_feature" {
		t.Fatalf("code = %q", classified.Code)
	}
	for _, named := range []string{"fetched_image", "vision"} {
		if !strings.Contains(classified.Message, named) {
			t.Fatalf("the refusal does not name %s: %q", named, classified.Message)
		}
	}
	if mantle.calls != 0 || blind.calls != 0 {
		t.Fatal("a target the route could not serve was called anyway")
	}

	// A route that can serve the request still answers it, and the reason list
	// only appears on a refusal.
	if _, err := service.Chat(context.Background(), f.plaintext, chatRequest()); err != nil {
		t.Fatalf("a servable request was refused: %v", err)
	}
}

func assertRefusedBeforeProviderIO(
	t *testing.T,
	service *Service,
	plaintext string,
	request openaiapi.ChatCompletionRequest,
	adapter *fakeAdapter,
	what string,
) {
	t.Helper()
	before := adapter.calls
	_, err := service.Chat(context.Background(), plaintext, request)
	var classified *Error
	if !errors.As(err, &classified) {
		t.Fatalf("%s was not refused: %v", what, err)
	}
	if classified.Code != "unsupported_feature" || classified.HTTPStatus != 400 {
		t.Fatalf("%s refused as %q with status %d", what, classified.Code, classified.HTTPStatus)
	}
	if adapter.calls != before {
		t.Fatalf("%s reached the provider: %d calls", what, adapter.calls-before)
	}
}

// The provider reports how much of the prompt it served from its own cache, and
// the Deployment now carries a rate for exactly that span. Settling the whole
// prompt at the ordinary input rate — which is what happened before the rate
// existed — over-charges by the difference between the two rates, here tenfold
// on eight of ten prompt tokens.
func TestChatSettlesCachedPromptTokensAtTheCacheReadRate(t *testing.T) {
	f := newFixture(t, 1_000)
	defer f.close()
	if err := f.registry.Register(provider.Target{
		ID: "target_cached", DeploymentID: "dep_target_cached",
		PublicModel: "chat-cached", ProviderModel: "provider-model", Adapter: f.adapter,
		InputMicrosPerMillion:       1_000_000,
		CachedInputMicrosPerMillion: 100_000,
		OutputMicrosPerMillion:      2_000_000,
	}); err != nil {
		t.Fatal(err)
	}
	f.project.AllowedModels = append(f.project.AllowedModels, "chat-cached")
	if err := f.service.auth.Refresh(context.Background(), source{
		keys: []domain.GatewayKey{f.key}, projects: []domain.Project{f.project},
	}); err != nil {
		t.Fatal(err)
	}
	usage := &openaiapi.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}
	usage.SetCachedPromptTokens(8)
	f.adapter.response.Usage = usage
	request := chatRequest()
	request.Model = "chat-cached"
	if _, err := f.service.Chat(context.Background(), f.plaintext, request); err != nil {
		t.Fatal(err)
	}
	period := time.Now().UTC().Format("2006-01-02")
	balance := f.state.Balance(f.project.ID, period, testTimezoneVersion)
	// 2 uncached prompt tokens at $1/M, 8 cached at $0.10/M rounded up to one
	// micro-USD, and 5 output tokens at $2/M. The same call with nothing cached
	// commits 20.
	if balance.ReservedMicrosUSD != 0 || balance.CommittedMicrosUSD != 13 ||
		balance.InputTokens != 10 || balance.OutputTokens != 5 {
		t.Fatalf("unexpected balance: %#v", balance)
	}
}

// DeepSeek reports the same split under its own two keys, and the rate it earns
// is the same rate. Reading only OpenAI's spelling left the hit span at zero, so
// every cached DeepSeek prompt settled at the miss rate — the whole distance
// between 13 and 20 here, and a factor of thirty on DeepSeek's own table.
func TestChatSettlesDeepSeekCachePromptCountersAtTheCacheReadRate(t *testing.T) {
	f := newFixture(t, 1_000)
	defer f.close()
	if err := f.registry.Register(provider.Target{
		ID: "target_deepseek", DeploymentID: "dep_target_deepseek",
		PublicModel: "chat-deepseek", ProviderModel: "provider-model", Adapter: f.adapter,
		InputMicrosPerMillion:       1_000_000,
		CachedInputMicrosPerMillion: 100_000,
		OutputMicrosPerMillion:      2_000_000,
	}); err != nil {
		t.Fatal(err)
	}
	f.project.AllowedModels = append(f.project.AllowedModels, "chat-deepseek")
	if err := f.service.auth.Refresh(context.Background(), source{
		keys: []domain.GatewayKey{f.key}, projects: []domain.Project{f.project},
	}); err != nil {
		t.Fatal(err)
	}
	f.adapter.response.Usage = &openaiapi.Usage{
		PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15,
		PromptCacheHitTokens: 8, PromptCacheMissTokens: 2,
	}
	request := chatRequest()
	request.Model = "chat-deepseek"
	if _, err := f.service.Chat(context.Background(), f.plaintext, request); err != nil {
		t.Fatal(err)
	}
	period := time.Now().UTC().Format("2006-01-02")
	balance := f.state.Balance(f.project.ID, period, testTimezoneVersion)
	if balance.ReservedMicrosUSD != 0 || balance.InputTokens != 10 || balance.OutputTokens != 5 {
		t.Fatalf("unexpected balance: %#v", balance)
	}
	// The same 2 + 8 + 5 arithmetic as the test above. Stated as a bound as well
	// as an exact figure because the bound is the defect: 20 is what settling
	// every prompt token at the miss rate costs.
	if balance.CommittedMicrosUSD != 13 {
		t.Fatalf("committed %d micro-USD, want 13", balance.CommittedMicrosUSD)
	}
	if balance.CommittedMicrosUSD >= 20 {
		t.Fatalf("committed %d micro-USD, the whole prompt at the miss rate", balance.CommittedMicrosUSD)
	}
}

// A caller's own cancel must not count against the deployment's availability:
// classified as "connect" it fed the circuit breaker, so a client that hung up
// early could mark a healthy upstream unhealthy for everyone else.
func TestACallerCancelIsNotAnAvailabilityFailure(t *testing.T) {
	canceled := &provider.Error{
		Class:     provider.ErrorCanceled,
		Ambiguous: true,
		Cause:     context.Canceled,
	}
	if err := availabilityFailure(canceled); err != nil {
		t.Fatalf("a caller cancel was counted as an availability failure: %v", err)
	}
	connect := &provider.Error{Class: provider.ErrorConnect, Retryable: true}
	if err := availabilityFailure(connect); err == nil {
		t.Fatal("a genuine connect failure must still count")
	}
	if retryable(canceled) {
		t.Fatal("a canceled request must not be retried")
	}
}
