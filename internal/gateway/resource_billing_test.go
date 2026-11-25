package gateway

import (
	"context"
	"crypto/sha256"
	"errors"
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
	starts   int
	state    string
	startErr error
}

func (a *billedVideoAdapter) StartAsyncInvoke(context.Context, provider.AsyncInvokeCall) (provider.AsyncInvokeObject, error) {
	a.starts++
	if a.startErr != nil {
		return provider.AsyncInvokeObject{}, a.startErr
	}
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

func TestResourcePreDispatchWriteFailureAllowsRetry(t *testing.T) {
	ctx := context.Background()
	adapter := &billedVideoAdapter{inferenceResourcesAdapter: inferenceResourcesAdapter{providerType: string(domain.ProviderBedrock)}, state: "Completed"}
	target := inferenceResourcesTargetFor("video", adapter)
	target.ProviderModel = "amazon.nova-reel-v1:0"
	f := newInferenceResourcesServiceFixture(t, domain.ProfileBedrockAsyncNovaReel, adapter, target, nil)
	defer f.close()
	configureJobPrice(t, f, 0, 100_000_000_000, 0)
	f.store.failInFlightWrite = true
	request := openaiapi.AsyncInvokeRequest{Model: "video", Prompt: "clouds", S3OutputURI: "s3://out/video", DurationSeconds: 6}
	const key = "retry-undispatched-video"
	if _, err := f.service.StartAsyncInvoke(ctx, f.plaintext, key, request); err == nil || adapter.starts != 0 {
		t.Fatalf("pre-dispatch write error = %v, upstream starts = %d", err, adapter.starts)
	}
	resource, ok, err := f.store.ProviderResourceByIdempotency(ctx, f.project.ID, domain.ResourceAsyncInvoke, sha256.Sum256([]byte(key)))
	if err != nil || !ok || resource.CreationStatus != creationReserved || resource.ReservedBy != "" || resource.AttemptID != "" {
		t.Fatalf("pre-dispatch refusal stranded reservation: %+v, found %t, err %v", resource, ok, err)
	}
	if got := currentJobBalance(f); got.ReservedMicrosUSD != 0 || got.CommittedMicrosUSD != 0 {
		t.Fatalf("pre-dispatch refusal charged or held funds: %+v", got)
	}
	job, err := f.service.StartAsyncInvoke(ctx, f.plaintext, key, request)
	if err != nil || job.InvocationARN != resource.ID || adapter.starts != 1 {
		t.Fatalf("retry job = %+v, error = %v, upstream starts = %d", job, err, adapter.starts)
	}
}

func TestRefusedResourceIsReclaimableAndFailedJobSettles(t *testing.T) {
	ctx := context.Background()
	adapter := &billedVideoAdapter{
		inferenceResourcesAdapter: inferenceResourcesAdapter{providerType: string(domain.ProviderBedrock)},
		state:                     "Failed", startErr: &provider.Error{Class: provider.ErrorBadRequest, StatusCode: 400},
	}
	target := inferenceResourcesTargetFor("video", adapter)
	target.ProviderModel = "amazon.nova-reel-v1:0"
	f := newInferenceResourcesServiceFixture(t, domain.ProfileBedrockAsyncNovaReel, adapter, target, nil)
	defer f.close()
	configureJobPrice(t, f, 0, 100_000_000_000, 0)
	request := openaiapi.AsyncInvokeRequest{Model: "video", Prompt: "clouds", S3OutputURI: "s3://out/video", DurationSeconds: 6}
	const key = "retry-refused-video"
	_, err := f.service.StartAsyncInvoke(ctx, f.plaintext, key, request)
	var gatewayFailure *Error
	if !errors.As(err, &gatewayFailure) || gatewayFailure.HTTPStatus != 400 || adapter.starts != 1 {
		t.Fatalf("provider refusal = %v, starts = %d", err, adapter.starts)
	}
	if got := currentJobBalance(f); got.ReservedMicrosUSD != 0 || got.CommittedMicrosUSD != 0 {
		t.Fatalf("unserved refusal charged or held funds: %+v", got)
	}
	adapter.startErr = nil
	job, err := f.service.StartAsyncInvoke(ctx, f.plaintext, key, request)
	if err != nil || adapter.starts != 2 {
		t.Fatalf("retry = %+v, starts = %d, error = %v", job, adapter.starts, err)
	}
	if status, err := f.service.GetAsyncInvoke(ctx, f.plaintext, job.InvocationARN); err != nil || status.Status != "Failed" {
		t.Fatalf("terminal provider failure = %+v, error = %v", status, err)
	}
	if got := currentJobBalance(f); got.ReservedMicrosUSD != 0 || got.CommittedMicrosUSD != 600_000 {
		t.Fatalf("terminal provider failure did not settle: %+v", got)
	}
}

type jobResourceWriteFaultStore struct {
	*inferenceResourcesMemoryStore
	commitBeforeError bool
	failed            bool
}

func (s *jobResourceWriteFaultStore) PutProviderResource(ctx context.Context, resource domain.ProviderResource, revision uint64) (domain.ProviderResource, error) {
	if resource.Kind == domain.ResourceAsyncInvoke && resource.CreationStatus == creationCompleted && !s.failed {
		s.failed = true
		if s.commitBeforeError {
			if _, err := s.inferenceResourcesMemoryStore.PutProviderResource(ctx, resource, revision); err != nil {
				return domain.ProviderResource{}, err
			}
		}
		return domain.ProviderResource{}, errors.New("injected job identity write failure")
	}
	return s.inferenceResourcesMemoryStore.PutProviderResource(ctx, resource, revision)
}

func TestAcceptedResourceIdentityWriteFailureReconcilesOriginalJob(t *testing.T) {
	for name, persisted := range map[string]bool{"write_failed": false, "confirmation_failed": true} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			adapter := &billedVideoAdapter{inferenceResourcesAdapter: inferenceResourcesAdapter{providerType: string(domain.ProviderBedrock)}, state: "Completed"}
			target := inferenceResourcesTargetFor("video", adapter)
			target.ProviderModel = "amazon.nova-reel-v1:0"
			f := newInferenceResourcesServiceFixture(t, domain.ProfileBedrockAsyncNovaReel, adapter, target, nil)
			defer f.close()
			configureJobPrice(t, f, 0, 100_000_000_000, 0)
			f.service.resources = &jobResourceWriteFaultStore{inferenceResourcesMemoryStore: f.store, commitBeforeError: persisted}
			request := openaiapi.AsyncInvokeRequest{Model: "video", Prompt: "clouds", S3OutputURI: "s3://out/video", DurationSeconds: 6}
			const key = "recover-accepted-video"
			if _, err := f.service.StartAsyncInvoke(ctx, f.plaintext, key, request); err == nil || adapter.starts != 1 {
				t.Fatalf("lost identity write error = %v, upstream starts = %d", err, adapter.starts)
			}
			if got := currentJobBalance(f); got.ReservedMicrosUSD != 600_000 || got.CommittedMicrosUSD != 0 {
				t.Fatalf("lost identity write changed pending hold: %+v", got)
			}
			f.service.ReconcileProviderResourceBilling(ctx)
			job, err := f.service.StartAsyncInvoke(ctx, f.plaintext, key, request)
			if err != nil || job.InvocationARN == "" || job.Status != "Completed" || adapter.starts != 1 {
				t.Fatalf("recovered job = %+v, err = %v, upstream starts = %d", job, err, adapter.starts)
			}
			if got := currentJobBalance(f); got.ReservedMicrosUSD != 0 || got.CommittedMicrosUSD != 600_000 {
				t.Fatalf("recovered job was not settled once: %+v", got)
			}
		})
	}
}

func TestUnknownResourceReconcilesFailedSettlement(t *testing.T) {
	ctx := context.Background()
	adapter := &billedVideoAdapter{inferenceResourcesAdapter: inferenceResourcesAdapter{providerType: string(domain.ProviderBedrock)}, state: "InProgress"}
	target := inferenceResourcesTargetFor("video", adapter)
	target.ProviderModel = "amazon.nova-reel-v1:0"
	f := newInferenceResourcesServiceFixture(t, domain.ProfileBedrockAsyncNovaReel, adapter, target, nil)
	defer f.close()
	configureJobPrice(t, f, 0, 100_000_000_000, 0)
	job, err := f.service.StartAsyncInvoke(ctx, f.plaintext, "unknown-video", openaiapi.AsyncInvokeRequest{
		Model: "video", Prompt: "clouds", S3OutputURI: "s3://out/video", DurationSeconds: 6,
	})
	if err != nil {
		t.Fatal(err)
	}
	resource, err := f.store.ProviderResource(ctx, f.project.ID, job.InvocationARN)
	if err != nil {
		t.Fatal(err)
	}
	resource.CreationStatus, resource.UpstreamID = creationUnknown, ""
	resource, err = f.store.PutProviderResource(ctx, resource, resource.Revision)
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := f.service.settleUnconfirmedResource(canceled, resource); !errors.Is(err, context.Canceled) {
		t.Fatalf("initial settlement error = %v", err)
	}
	f.service.ReconcileProviderResourceBilling(ctx)
	f.service.ReconcileProviderResourceBilling(ctx)
	if got := currentJobBalance(f); got.ReservedMicrosUSD != 0 || got.CommittedMicrosUSD != 600_000 {
		t.Fatalf("unknown job did not recover once: %+v", got)
	}
}

type blockedVideoSubmission struct {
	billedVideoAdapter
	entered chan struct{}
	release chan struct{}
}

func (a *blockedVideoSubmission) StartAsyncInvoke(ctx context.Context, call provider.AsyncInvokeCall) (provider.AsyncInvokeObject, error) {
	close(a.entered)
	select {
	case <-a.release:
		return a.billedVideoAdapter.StartAsyncInvoke(ctx, call)
	case <-ctx.Done():
		return provider.AsyncInvokeObject{}, ctx.Err()
	}
}

func TestResourceReconciliationLeavesActiveSubmissionReserved(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	adapter := &blockedVideoSubmission{
		billedVideoAdapter: billedVideoAdapter{inferenceResourcesAdapter: inferenceResourcesAdapter{providerType: string(domain.ProviderBedrock)}, state: "InProgress"},
		entered:            make(chan struct{}), release: make(chan struct{}),
	}
	target := inferenceResourcesTargetFor("video", adapter)
	target.ProviderModel = "amazon.nova-reel-v1:0"
	f := newInferenceResourcesServiceFixture(t, domain.ProfileBedrockAsyncNovaReel, adapter, target, nil)
	defer f.close()
	configureJobPrice(t, f, 0, 100_000_000_000, 0)
	done := make(chan error, 1)
	go func() {
		_, err := f.service.StartAsyncInvoke(ctx, f.plaintext, "held-active-video", openaiapi.AsyncInvokeRequest{
			Model: "video", Prompt: "clouds", S3OutputURI: "s3://out/video", DurationSeconds: 6,
		})
		done <- err
	}()
	select {
	case <-adapter.entered:
	case err := <-done:
		t.Fatalf("submission never called upstream: %v", err)
	}
	f.service.ReconcileProviderResourceBilling(ctx)
	got := currentJobBalance(f)
	close(adapter.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got.ReservedMicrosUSD != 600_000 || got.CommittedMicrosUSD != 0 {
		t.Fatalf("reconciliation settled an active submission: %+v", got)
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

func TestStreamSettlementEstimatesMissingInputAndRetainsReportedOutput(t *testing.T) {
	target := provider.Target{InputMicrosPerMillion: 1_000_000, OutputMicrosPerMillion: 2_000_000,
		CachedInputMicrosPerMillion: 500_000}
	usage := &semantic.Usage{
		InputIncomplete: true, InputTokens: 4, CachedInputTokens: 4,
		OutputTokens: 7, TotalTokens: 11, Source: semantic.UsageProviderReported,
	}
	settlement := streamSettlement(usage, nil, true, 12, 100, 6, target, 212)
	if settlement.ProviderInputTokens != 12 || settlement.EstimatedInputTokens != 8 ||
		settlement.ProviderCachedInputTokens != 4 || settlement.ProviderOutputTokens != 7 ||
		settlement.EstimatedOutputTokens != 0 || settlement.TokenEstimated || !settlement.CostEstimated ||
		settlement.CommittedMicrosUSD != 24 {
		t.Fatalf("mixed settlement = %+v", settlement)
	}
}

func TestBatchSettlementRetainsReportedUsageWhenAResultIsMissing(t *testing.T) {
	ctx := context.Background()
	adapter := &billedBatchAdapter{batchResultsAdapter: batchResultsAdapter{
		inferenceResourcesAdapter: inferenceResourcesAdapter{providerType: string(domain.ProviderAnthropic)},
		results:                   []byte("{\"custom_id\":\"reported\",\"response\":{\"status_code\":200,\"body\":{\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":2,\"total_tokens\":6}}}}\n"),
	}}
	f := newInferenceResourcesServiceFixture(t, domain.ProfileAnthropicMessages, adapter, inferenceResourcesTargetFor("resources", adapter), nil)
	defer f.close()
	configureJobPrice(t, f, 1_000_000, 2_000_000, 200)
	file, err := f.service.CreateFile(ctx, f.plaintext, "resources", "mixed-batch-input", provider.FileCreateCall{
		Filename: "batch.jsonl", Purpose: "batch", ContentType: "application/jsonl",
		Data: []byte("{\"custom_id\":\"reported\",\"method\":\"POST\",\"url\":\"/v1/chat/completions\",\"body\":{\"model\":\"resources\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}],\"max_tokens\":10}}\n" +
			"{\"custom_id\":\"missing\",\"method\":\"POST\",\"url\":\"/v1/chat/completions\",\"body\":{\"model\":\"resources\",\"messages\":[{\"role\":\"user\",\"content\":\"world\"}],\"max_tokens\":12}}\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	batch, err := f.service.CreateBatch(ctx, f.plaintext, "mixed-batch", provider.BatchCreateCall{
		InputFileID: file.ID, Endpoint: "/v1/chat/completions", CompletionWindow: "24h",
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter.batch = provider.BatchObject{ID: "msgbatch_1", Status: "completed", ResultsURL: "https://api.anthropic.com/result"}
	if _, err := f.service.GetBatch(ctx, f.plaintext, batch.ID); err != nil {
		t.Fatal(err)
	}
	var settled ledger.Event
	if _, err := f.log.Replay(ledger.Watermark{}, func(record ledger.Record) error {
		if record.Event.CompleteResourceRequest && record.Event.ProviderResourceID == batch.ID {
			settled = record.Event
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if settled.TokenUsageSource != ledger.TokenUsageSourceMixedInput || !settled.CostEstimated || settled.TokenEstimated ||
		settled.EstimatedInputTokens <= 0 || settled.EstimatedOutputTokens != 12 ||
		settled.ProviderInputTokens-settled.EstimatedInputTokens != 4 ||
		settled.ProviderOutputTokens-settled.EstimatedOutputTokens != 2 {
		t.Fatalf("settled mixed Batch job = %+v", settled)
	}
}
