package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Heimdall/internal/auth"
	"github.com/akz142857/Heimdall/internal/budget"
	openaiwire "github.com/akz142857/Heimdall/internal/compatibility/openai"
	"github.com/akz142857/Heimdall/internal/domain"
	"github.com/akz142857/Heimdall/internal/ledger"
	"github.com/akz142857/Heimdall/internal/limiter"
	"github.com/akz142857/Heimdall/internal/openaiapi"
	"github.com/akz142857/Heimdall/internal/provider"
	"github.com/akz142857/Heimdall/internal/redaction"
	"github.com/akz142857/Heimdall/internal/requestmeta"
	"github.com/akz142857/Heimdall/internal/semantic"
	"github.com/akz142857/Heimdall/internal/tokenguard"
)

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
}

func (s *fakePricePinStore) SelectDeploymentPriceVersion(ctx context.Context, deploymentID string, selectedAt time.Time) (domain.DeploymentPriceVersion, error) {
	price := s.admissionPrice
	if price.ID == "" {
		price = s.price
	}
	return staticPriceSelector{price: price}.SelectDeploymentPriceVersion(ctx, deploymentID, selectedAt)
}

func (s *fakePricePinStore) LockDeploymentPricing(string) func() {
	s.mu.Lock()
	return s.mu.Unlock
}

func (s *fakePricePinStore) PrepareDeploymentPricePin(_ context.Context, deploymentID, attemptID string, selectedAt time.Time, _, _ time.Duration) (domain.DeploymentPriceVersion, domain.PriceSnapshot, domain.PricePinIntent, error) {
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
	balance := f.state.Balance(f.project.ID, now.Format("2006-01-02"))
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

func (a *fakeAdapter) Type() string { return "fake" }
func (a *fakeAdapter) Close()       {}

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
		AllowedRoutes:        []string{"chat"},
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
	log, err := ledger.OpenWithOptions(filepath.Join(t.TempDir(), "usage.wal"), status, options)
	if err != nil {
		t.Fatal(err)
	}
	state := ledger.NewState()
	accounting, err := budget.NewWithOptions(log, state, time.UTC, budget.Options{Now: clock})
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
		ID:                     "target_1",
		PublicModel:            "chat",
		ProviderModel:          "provider-model",
		Adapter:                adapter,
		InputMicrosPerMillion:  1_000_000,
		OutputMicrosPerMillion: 2_000_000,
	}); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(snapshot, registry, accounting)
	if err != nil {
		t.Fatal(err)
	}
	if clock != nil {
		service.now = clock
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
	estimated := estimateInputTokens(request.EstimatedInputBytes()) + maxTokens
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
		ID: "target_2", PublicModel: "chat", ProviderModel: "provider-model",
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
		ID: "target_2", PublicModel: "chat", ProviderModel: "provider-model",
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
		ID: "target_2", PublicModel: "chat", ProviderModel: "provider-model",
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
	balance := f.state.Balance(f.project.ID, period)
	if balance.ReservedMicrosUSD != 0 || balance.CommittedMicrosUSD != 20 ||
		balance.InputTokens != 10 || balance.OutputTokens != 5 {
		t.Fatalf("unexpected balance: %#v", balance)
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
		ID: "target_2", PublicModel: "chat", ProviderModel: "provider-model",
		Adapter: fallback, Priority: 1,
	}); err != nil {
		t.Fatal(err)
	}
	_, err := f.service.Chat(context.Background(), f.plaintext, chatRequest())
	if err == nil {
		t.Fatal("expected provider error")
	}
	period := time.Now().UTC().Format("2006-01-02")
	balance := f.state.Balance(f.project.ID, period)
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
	balance := f.state.Balance(f.project.ID, period)
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
		ID: "target_1", PublicModel: "chat", ProviderModel: "provider-model",
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
		Chat: true, Streaming: true, Tools: true, Vision: true, JSONMode: true,
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

func TestSemanticCapabilityFilterRejectsOptionalSemanticsForUnprofiledLegacyTarget(t *testing.T) {
	target := provider.Target{
		ID: "legacy", LegacyUnprofiled: true,
		Capabilities: provider.Capabilities{
			Chat: true, Streaming: true, Embeddings: true, Tools: true, Vision: true,
			JSONMode: true, DeveloperRole: true, Reasoning: true, StreamUsage: true,
		},
	}
	requirements := []semantic.Requirements{
		{Tools: true}, {ParallelTools: true}, {InputImage: true}, {StructuredJSON: true},
		{DeveloperRole: true}, {Reasoning: true}, {StreamUsage: true}, {Seed: true},
		{MultipleCandidates: true}, {EndUserReference: true},
	}
	for _, requirement := range requirements {
		if filtered := filterSemanticCapabilities([]provider.Target{target}, requirement); len(filtered) != 0 {
			t.Fatalf("legacy target accepted optional requirements %#v", requirement)
		}
	}
	if filtered := filterSemanticCapabilities([]provider.Target{target}, semantic.Requirements{Streaming: true}); len(filtered) != 1 {
		t.Fatal("legacy target lost basic streaming compatibility")
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
		ID: "target_1", ProviderID: "provider_1", PublicModel: "chat",
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
			ID: "target_1", ProviderID: "provider_1", PublicModel: "chat",
			ProviderModel: "provider-model", Adapter: f.adapter,
			InputMicrosPerMillion: 1_000_000, OutputMicrosPerMillion: 2_000_000,
			MaxConcurrency: 1, Priority: 0,
		},
		{
			ID: "target_2", ProviderID: "provider_2", PublicModel: "chat",
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
		ID: "target_2", PublicModel: "chat", ProviderModel: "provider-model",
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
	balance := f.state.Balance(f.project.ID, period)
	if balance.ReservedMicrosUSD != 0 || balance.CommittedMicrosUSD != 20 ||
		balance.InputTokens != 10 || balance.OutputTokens != 5 {
		t.Fatalf("unexpected stream balance: %#v", balance)
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
		ID: "target_2", PublicModel: "chat", ProviderModel: "provider-model",
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
		ID: "target_2", PublicModel: "chat", ProviderModel: "provider-model",
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
		ID:                     "target_1",
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
	run, err := f.service.beginRequestRun(ctx, principal, "chat", targets, 15, 10, 5)
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
