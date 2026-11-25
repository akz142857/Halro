package gateway

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/semantic"
)

func configureJobPrice(t *testing.T, f inferenceResourcesServiceFixture, input, output, fixed int64) *fakePricePinStore {
	t.Helper()
	now := time.Now().UTC().Add(-time.Hour)
	prices := &fakePricePinStore{price: domain.DeploymentPriceVersion{
		ID: "price_jobs", DeploymentID: "inferenceResources-deployment", Version: 1, Revision: 1,
		BillingMode: domain.BillingModeMetered, Currency: "USD", FormulaVersion: domain.PriceFormulaUSDTokensV1,
		InputMicrosPerMillion: input, OutputMicrosPerMillion: output, FixedRequestMicrosUSD: fixed,
		EffectiveFrom: now, CreatedAt: now, CreatedBy: "test",
		Source: domain.PriceSource{Type: domain.PriceSourceManual, Assurance: domain.PriceAssuranceAsserted,
			ReceivedAt: now, Reference: "test prices", AssertedWithoutArchive: true},
	}}
	f.service.pricing = prices
	return prices
}

func currentJobBalance(f inferenceResourcesServiceFixture) ledger.Balance {
	return f.state.Balance(f.project.ID, time.Now().UTC().Format("2006-01-02"), testTimezoneVersion)
}

func TestResourcePollWaivesPinnedFixedFee(t *testing.T) {
	ctx := context.Background()
	adapter := &inferenceResourcesAdapter{providerType: string(domain.ProviderAnthropic), batch: provider.BatchObject{
		ID: "msgbatch_1", Status: "in_progress",
	}}
	f := newInferenceResourcesServiceFixture(t, domain.ProfileAnthropicMessages, adapter, inferenceResourcesTargetFor("resources", adapter), nil)
	defer f.close()
	configureJobPrice(t, f, 1_000_000, 0, 201)
	now := time.Now().UTC()
	resource := domain.ProviderResource{
		ID: "batch-legacy", Kind: domain.ResourceBatch, ProjectID: f.project.ID,
		ProviderID: "inferenceResources-provider", DeploymentID: "inferenceResources-deployment",
		PublicModel: "resources", ProfileID: domain.ProfileAnthropicMessages, Region: "us-east-1",
		UpstreamID: "msgbatch_1", CreationStatus: creationCompleted, Status: "in_progress",
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour), Revision: 1,
	}
	f.store.resources[resource.ID] = resource
	if _, err := f.service.GetBatch(ctx, f.plaintext, resource.ID); err != nil {
		t.Fatal(err)
	}
	if got := currentJobBalance(f); got.CommittedMicrosUSD != 1 || got.ReservedMicrosUSD != 0 {
		t.Fatalf("legacy poll balance = %+v", got)
	}
}

type billedBatchAdapter struct {
	batchResultsAdapter
	creates int
}

func (a *billedBatchAdapter) CreateBatch(context.Context, provider.BatchCreateCall) (provider.BatchObject, error) {
	a.creates++
	return provider.BatchObject{ID: "msgbatch_1", Status: "in_progress"}, nil
}

func TestBatchBillingHoldsBudgetUntilTerminalUsageAndSettlesOnce(t *testing.T) {
	ctx := context.Background()
	adapter := &billedBatchAdapter{batchResultsAdapter: batchResultsAdapter{
		inferenceResourcesAdapter: inferenceResourcesAdapter{providerType: string(domain.ProviderAnthropic)},
		results: []byte("{\"custom_id\":\"b\",\"error\":{\"code\":\"expired\"}}\n" +
			"{\"custom_id\":\"a\",\"response\":{\"status_code\":200,\"body\":{\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":2,\"total_tokens\":6}}}}\n"),
	}}
	f := newInferenceResourcesServiceFixture(t, domain.ProfileAnthropicMessages, adapter, inferenceResourcesTargetFor("resources", adapter), nil)
	defer f.close()
	price := configureJobPrice(t, f, 1_000_000, 2_000_000, 200)
	file, err := f.service.CreateFile(ctx, f.plaintext, "resources", "upload-batch", provider.FileCreateCall{
		Filename: "input.jsonl", Purpose: "batch", ContentType: "application/jsonl",
		Data: []byte("{\"custom_id\":\"a\",\"method\":\"POST\",\"url\":\"/v1/chat/completions\",\"body\":{\"model\":\"resources\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}],\"max_tokens\":10}}\n" +
			"{\"custom_id\":\"b\",\"method\":\"POST\",\"url\":\"/v1/chat/completions\",\"body\":{\"model\":\"resources\",\"messages\":[{\"role\":\"user\",\"content\":\"world\"}],\"max_tokens\":12}}\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	baseline := currentJobBalance(f).CommittedMicrosUSD
	batch, err := f.service.CreateBatch(ctx, f.plaintext, "create-batch", provider.BatchCreateCall{
		InputFileID: file.ID, Endpoint: "/v1/chat/completions", CompletionWindow: "24h",
	})
	if err != nil {
		t.Fatal(err)
	}
	pending := currentJobBalance(f)
	if pending.ReservedMicrosUSD <= 244 || pending.CommittedMicrosUSD != baseline || adapter.creates != 1 {
		t.Fatalf("submitted batch balance = %+v, creates = %d", pending, adapter.creates)
	}

	// Replay durable events into a new accounting manager the way startup does.
	f.state = ledger.NewState()
	if _, err := f.log.Replay(ledger.Watermark{}, f.state.Apply); err != nil {
		t.Fatal(err)
	}
	manager, err := budget.New(f.log, f.state, mustResolver(t, "UTC"))
	if err != nil {
		t.Fatal(err)
	}
	f.service.accounting = manager
	if err := manager.RecoverPendingLeasesWithResources(ctx, func(event ledger.Event) (budget.ResourceLeaseRecovery, error) {
		resource, err := f.store.ProviderResource(ctx, event.ProjectID, event.ProviderResourceID)
		if err == nil && resource.AttemptID == event.AttemptID && resource.UpstreamID != "" {
			return budget.ResourceLeaseRetain, nil
		}
		return budget.ResourceLeaseOutcomeUnknown, err
	}); err != nil {
		t.Fatal(err)
	}
	if got := currentJobBalance(f); got.ReservedMicrosUSD != pending.ReservedMicrosUSD || got.CommittedMicrosUSD != baseline {
		t.Fatalf("restart balance = %+v", got)
	}

	price.price.InputMicrosPerMillion = 9_000_000 // The job keeps the submitted price.
	adapter.batch = provider.BatchObject{ID: "msgbatch_1", Status: "completed", ResultsURL: "https://api.anthropic.com/result"}
	for i := 0; i < 2; i++ {
		if _, err := f.service.GetBatch(ctx, f.plaintext, batch.ID); err != nil {
			t.Fatal(err)
		}
	}
	if got := currentJobBalance(f); got.ReservedMicrosUSD != 0 || got.CommittedMicrosUSD != baseline+208 || adapter.fetchCalls != 1 {
		t.Fatalf("settled batch balance = %+v, result downloads = %d", got, adapter.fetchCalls)
	}
	aggregate := newUsageAggregateForTest(t)
	if _, err := f.log.Replay(ledger.Watermark{}, aggregate.Apply); err != nil {
		t.Fatal(err)
	}
	if got := aggregate.Metrics().ActiveRequests; got != 0 {
		t.Fatalf("completed batch left %d open requests", got)
	}
}

type billedVideoAdapter struct {
	inferenceResourcesAdapter
	starts int
	state  string
}

func (a *billedVideoAdapter) StartAsyncInvoke(context.Context, provider.AsyncInvokeCall) (provider.AsyncInvokeObject, error) {
	a.starts++
	return provider.AsyncInvokeObject{InvocationARN: "arn:aws:bedrock:test", Status: "InProgress"}, nil
}

func (a *billedVideoAdapter) GetAsyncInvoke(context.Context, string, string) (provider.AsyncInvokeObject, error) {
	return provider.AsyncInvokeObject{InvocationARN: "arn:aws:bedrock:test", Status: a.state}, nil
}

func (a *billedVideoAdapter) Rerank(context.Context, provider.RerankCall) (provider.RerankResult, error) {
	return provider.RerankResult{}, fmt.Errorf("unexpected rerank")
}

func (a *billedVideoAdapter) GenerateBedrockImage(context.Context, provider.ImageCall) (provider.ImageResult, error) {
	return provider.ImageResult{}, fmt.Errorf("unexpected image request")
}

func TestVideoBillingReservesDurationWithoutBlockingPoll(t *testing.T) {
	ctx := context.Background()
	adapter := &billedVideoAdapter{inferenceResourcesAdapter: inferenceResourcesAdapter{providerType: string(domain.ProviderBedrock)}, state: "InProgress"}
	target := inferenceResourcesTargetFor("video", adapter)
	target.ProviderModel = "amazon.nova-reel-v1:0"
	f := newInferenceResourcesServiceFixture(t, domain.ProfileBedrockAsyncNovaReel, adapter, target, nil)
	defer f.close()
	configureJobPrice(t, f, 0, 100_000_000_000, 400_000)

	req := openaiapi.AsyncInvokeRequest{Model: "video", Prompt: "clouds", S3OutputURI: "s3://out/video", DurationSeconds: 6}
	job, err := f.service.StartAsyncInvoke(ctx, f.plaintext, "start-video", req)
	if err != nil {
		t.Fatal(err)
	}
	if got := currentJobBalance(f); got.ReservedMicrosUSD != 1_000_000 || got.CommittedMicrosUSD != 0 {
		t.Fatalf("video reservation = %+v", got)
	}
	if requests, jobs, age := f.state.PendingLeaseStats(time.Now().Add(time.Hour)); requests != 0 || jobs != 1 || age != 0 {
		t.Fatalf("long-running job entered the interactive expiry alert: requests=%d jobs=%d age=%v", requests, jobs, age)
	}
	_, err = f.service.StartAsyncInvoke(ctx, f.plaintext, "video-too-many", req)
	if err == nil || adapter.starts != 1 {
		t.Fatalf("budget refusal = %v, starts = %d", err, adapter.starts)
	}
	if _, err := f.service.GetAsyncInvoke(ctx, f.plaintext, job.InvocationARN); err != nil {
		t.Fatalf("poll while budget is fully reserved: %v", err)
	}

	adapter.state = "Completed"
	f.service.ReconcileProviderResourceBilling(ctx)
	for i := 0; i < 2; i++ {
		if _, err := f.service.GetAsyncInvoke(ctx, f.plaintext, job.InvocationARN); err != nil {
			t.Fatal(err)
		}
	}
	if got := currentJobBalance(f); got.ReservedMicrosUSD != 0 || got.CommittedMicrosUSD != 1_000_000 || got.OutputTokens != 0 || adapter.starts != 1 {
		t.Fatalf("settled video balance = %+v, starts = %d", got, adapter.starts)
	}
	var video ledger.Event
	if _, err := f.log.Replay(ledger.Watermark{}, func(record ledger.Record) error {
		if record.Event.VideoSeconds > 0 {
			video = record.Event
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if video.VideoSeconds != 6 || !video.CostEstimated || !video.CompleteResourceRequest {
		t.Fatalf("video billing event = %+v", video)
	}
}

func TestInterruptedStreamChargesOnlyMissingOutputAsEstimated(t *testing.T) {
	usage := &semantic.Usage{InputTokens: 10, CachedInputTokens: 5, OutputTokens: 2, TotalTokens: 12, OutputIncomplete: true, Source: semantic.UsageProviderReported}
	target := provider.Target{InputMicrosPerMillion: 1_000_000, CachedInputMicrosPerMillion: 200_000, OutputMicrosPerMillion: 2_000_000}
	err := &provider.Error{Class: provider.ErrorMalformed, Ambiguous: true}
	partial := streamSettlement(usage, err, true, 20, 8, 20, target, 40)
	if partial.ProviderInputTokens != 10 || partial.ProviderCachedInputTokens != 5 || partial.ProviderOutputTokens != 8 ||
		partial.EstimatedOutputTokens != 6 || !partial.CostEstimated || partial.TokenEstimated || partial.CommittedMicrosUSD != 22 {
		t.Fatalf("partial settlement = %+v", partial)
	}
	usage.OutputIncomplete = false
	final := streamSettlement(usage, err, true, 20, 8, 20, target, 40)
	if final.ProviderOutputTokens != 2 || final.EstimatedOutputTokens != 0 || final.CostEstimated || final.CommittedMicrosUSD != 10 {
		t.Fatalf("final usage settlement = %+v", final)
	}
}

func TestConcurrentVideoSettlementCommitsOnce(t *testing.T) {
	ctx := context.Background()
	adapter := &billedVideoAdapter{inferenceResourcesAdapter: inferenceResourcesAdapter{providerType: string(domain.ProviderBedrock)}, state: "Completed"}
	target := inferenceResourcesTargetFor("video", adapter)
	target.ProviderModel = "amazon.nova-reel-v1:0"
	f := newInferenceResourcesServiceFixture(t, domain.ProfileBedrockAsyncNovaReel, adapter, target, nil)
	defer f.close()
	configureJobPrice(t, f, 0, 100_000_000_000, 0)
	job, err := f.service.StartAsyncInvoke(ctx, f.plaintext, "video-concurrent", openaiapi.AsyncInvokeRequest{
		Model: "video", Prompt: "clouds", S3OutputURI: "s3://out/video", DurationSeconds: 6,
	})
	if err != nil {
		t.Fatal(err)
	}
	resource, err := f.store.ProviderResource(ctx, f.project.ID, job.InvocationARN)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	const workers = 8
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- f.service.settleAsyncStatus(ctx, resource, "Completed")
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := currentJobBalance(f); got.ReservedMicrosUSD != 0 || got.CommittedMicrosUSD != 600_000 {
		t.Fatalf("concurrent settlement balance = %+v", got)
	}
}
