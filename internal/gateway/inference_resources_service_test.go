package gateway

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/auth"
	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/redaction"
	"github.com/akz142857/Halro/internal/semantic"
)

var errInferenceResourcesResourceNotFound = errors.New("inferenceResources resource not found")

type inferenceResourcesMemoryStore struct {
	mu                      sync.Mutex
	resources               map[string]domain.ProviderResource
	failCleanupPendingWrite bool
	failInFlightWrite       bool
	failOutcomeWrite        bool
}

func newInferenceResourcesMemoryStore(resources ...domain.ProviderResource) *inferenceResourcesMemoryStore {
	store := &inferenceResourcesMemoryStore{resources: make(map[string]domain.ProviderResource)}
	for _, resource := range resources {
		store.resources[resource.ID] = resource
	}
	return store
}

func (s *inferenceResourcesMemoryStore) PutProviderResource(_ context.Context, resource domain.ProviderResource, expected uint64) (domain.ProviderResource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, exists := s.resources[resource.ID]
	if s.failCleanupPendingWrite && resource.CleanupStatus == "pending" {
		s.failCleanupPendingWrite = false
		return domain.ProviderResource{}, errors.New("injected cleanup state write failure")
	}
	// Stops a request between reserving the key and calling the provider, which
	// is the window a crash used to make unrecoverable.
	if s.failInFlightWrite && resource.CreationStatus == creationInFlight {
		s.failInFlightWrite = false
		return domain.ProviderResource{}, errors.New("injected in-flight state write failure")
	}
	// Leaves the record exactly as a crash during the provider call would: the
	// outcome was never recorded, so the stored state is still in_flight.
	if s.failOutcomeWrite && resource.CreationStatus == creationUnknown {
		s.failOutcomeWrite = false
		return domain.ProviderResource{}, errors.New("injected outcome write failure")
	}
	if exists && current.Revision != expected {
		return domain.ProviderResource{}, errors.New("revision conflict")
	}
	if !exists && expected != 0 {
		return domain.ProviderResource{}, errors.New("revision conflict")
	}
	if !exists {
		for _, existing := range s.resources {
			if resource.IdempotencyKeyHash != ([32]byte{}) && existing.ProjectID == resource.ProjectID && existing.Kind == resource.Kind && existing.IdempotencyKeyHash == resource.IdempotencyKeyHash {
				return domain.ProviderResource{}, errors.New("duplicate idempotency key")
			}
		}
	}
	resource.Revision = expected + 1
	s.resources[resource.ID] = resource
	return resource, nil
}

func (s *inferenceResourcesMemoryStore) ProviderResource(_ context.Context, projectID, id string) (domain.ProviderResource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	resource, ok := s.resources[id]
	if !ok || resource.ProjectID != projectID {
		return domain.ProviderResource{}, errInferenceResourcesResourceNotFound
	}
	return resource, nil
}

func (s *inferenceResourcesMemoryStore) DeleteProviderResource(_ context.Context, projectID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	resource, ok := s.resources[id]
	if !ok || resource.ProjectID != projectID {
		return errInferenceResourcesResourceNotFound
	}
	delete(s.resources, id)
	return nil
}

func (s *inferenceResourcesMemoryStore) ProviderResourceByIdempotency(_ context.Context, projectID string, kind domain.ProviderResourceKind, hash [32]byte) (domain.ProviderResource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, resource := range s.resources {
		if resource.ProjectID == projectID && resource.Kind == kind && resource.IdempotencyKeyHash == hash {
			return resource, nil
		}
	}
	return domain.ProviderResource{}, errInferenceResourcesResourceNotFound
}

type inferenceResourcesAdapter struct {
	providerType string
	fileCalls    int
	getFileCalls int
	deleteCalls  int
	fileErr      error
	getFileErr   error
	deleteErr    error
	file         provider.FileObject
	batch        provider.BatchObject
	transcript   provider.TranscriptionResult
	image        provider.ImageResult
}

func (a *inferenceResourcesAdapter) Type() string { return a.providerType }
func (a *inferenceResourcesAdapter) Close()       {}
func (a *inferenceResourcesAdapter) Chat(context.Context, provider.ChatCall) (openaiapi.ChatCompletionResponse, error) {
	return openaiapi.ChatCompletionResponse{}, errors.New("unused")
}
func (a *inferenceResourcesAdapter) ChatStream(context.Context, provider.ChatCall, func(semantic.Event) error) (*openaiapi.Usage, error) {
	return nil, errors.New("unused")
}
func (a *inferenceResourcesAdapter) Embed(context.Context, provider.EmbeddingCall) (openaiapi.EmbeddingResponse, error) {
	return openaiapi.EmbeddingResponse{}, errors.New("unused")
}
func (a *inferenceResourcesAdapter) Moderate(context.Context, provider.ModerationCall) (provider.ModerationResult, error) {
	return provider.ModerationResult{}, nil
}
func (a *inferenceResourcesAdapter) GenerateImage(context.Context, provider.ImageCall) (provider.ImageResult, error) {
	return a.image, nil
}
func (a *inferenceResourcesAdapter) Transcribe(context.Context, provider.TranscriptionCall) (provider.TranscriptionResult, error) {
	return a.transcript, nil
}
func (a *inferenceResourcesAdapter) Synthesize(context.Context, provider.SpeechCall) (provider.SpeechResult, error) {
	return provider.SpeechResult{}, nil
}
func (a *inferenceResourcesAdapter) CreateFile(_ context.Context, call provider.FileCreateCall) (provider.FileObject, error) {
	a.fileCalls++
	if a.fileErr != nil {
		return provider.FileObject{}, a.fileErr
	}
	result := a.file
	if result.ID == "" {
		result = provider.FileObject{ID: "upstream-file", Object: "file", Bytes: int64(len(call.Data)), Filename: call.Filename, Purpose: call.Purpose, Status: "uploaded"}
	}
	return result, nil
}
func (a *inferenceResourcesAdapter) GetFile(context.Context, string, string) (provider.FileObject, error) {
	a.getFileCalls++
	return a.file, a.getFileErr
}
func (a *inferenceResourcesAdapter) DownloadFile(context.Context, string, string) (provider.FileContent, error) {
	return provider.FileContent{}, nil
}
func (a *inferenceResourcesAdapter) DeleteFile(_ context.Context, _, id string) (provider.FileDeleteResult, error) {
	a.deleteCalls++
	if a.deleteErr != nil {
		return provider.FileDeleteResult{}, a.deleteErr
	}
	return provider.FileDeleteResult{ID: id, Object: "file", Deleted: true}, nil
}
func (a *inferenceResourcesAdapter) CreateBatch(context.Context, provider.BatchCreateCall) (provider.BatchObject, error) {
	return provider.BatchObject{}, nil
}
func (a *inferenceResourcesAdapter) GetBatch(context.Context, string, string) (provider.BatchObject, error) {
	return a.batch, nil
}
func (a *inferenceResourcesAdapter) CancelBatch(context.Context, string, string) (provider.BatchObject, error) {
	return provider.BatchObject{}, nil
}

type inferenceResourcesServiceFixture struct {
	service   *Service
	plaintext string
	project   domain.Project
	state     *ledger.State
	store     *inferenceResourcesMemoryStore
	objectDir string
	close     func()
}

func newInferenceResourcesServiceFixture(t *testing.T, profileID domain.ProviderProfileID, adapter provider.Adapter, target provider.Target, policies []domain.RedactionPolicy) inferenceResourcesServiceFixture {
	t.Helper()
	project := domain.Project{ID: "inferenceResources-project", Name: "Phase 2", Enabled: true, AllowedRoutes: []string{target.PublicModel}, DailyBudgetMicrosUSD: 1_000_000, MaxInputTokens: 100_000, MaxOutputTokens: 100_000}
	if len(policies) > 0 {
		project.RedactionPolicyID = policies[0].ID
	}
	plaintext, key, err := auth.GenerateGatewayKey(project.ID, "inferenceResources", nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := auth.NewSnapshot()
	if err := snapshot.Refresh(context.Background(), source{keys: []domain.GatewayKey{key}, projects: []domain.Project{project}}); err != nil {
		t.Fatal(err)
	}
	log, err := ledger.OpenWithOptions(filepath.Join(t.TempDir(), "inferenceResources-usage.wal"), ledger.NewStatus(), ledger.Options{ChainKey: testChainKey})
	if err != nil {
		t.Fatal(err)
	}
	state := ledger.NewState()
	accounting, err := budget.New(log, state, mustResolver(t, "UTC"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, ok := provider.BuiltinProfile(profileID)
	if !ok {
		t.Fatalf("unknown profile %s", profileID)
	}
	capabilities := domain.DefaultProviderCapabilitiesForProfile(manifest.ProviderType, profileID)
	bridge, err := provider.NewLegacyAdapterBridge(adapter, manifest, domain.EvidenceForCapabilities(capabilities, domain.EvidenceDeclared))
	if err != nil {
		t.Fatal(err)
	}
	target.Adapter = bridge
	target.ProfileID = manifest.ID
	target.AccessSurface = manifest.AccessSurface
	target.Capabilities = provider.Capabilities{
		Chat: capabilities.Chat, Streaming: capabilities.Streaming, Embeddings: capabilities.Embeddings,
		Moderations: capabilities.Moderations, Images: capabilities.Images, Transcriptions: capabilities.Transcriptions,
		Speech: capabilities.Speech, Files: capabilities.Files, Batches: capabilities.Batches,
		Rerank: capabilities.Rerank, AsyncGenerate: capabilities.AsyncGenerate,
	}
	registry := provider.NewRegistry()
	if err := registry.Register(target); err != nil {
		t.Fatal(err)
	}
	redactor, err := redaction.New(policies)
	if err != nil {
		t.Fatal(err)
	}
	store := newInferenceResourcesMemoryStore()
	objectDir := filepath.Join(t.TempDir(), "objects")
	service, err := NewServiceWithOptions(snapshot, registry, accounting, ServiceOptions{Resources: store, ResourceObjectDir: objectDir, Redactor: redactor})
	if err != nil {
		t.Fatal(err)
	}
	return inferenceResourcesServiceFixture{service: service, plaintext: plaintext, project: project, state: state, store: store, objectDir: objectDir, close: func() { _ = log.Close() }}
}

func inferenceResourcesTargetFor(model string, adapter provider.Adapter) provider.Target {
	return provider.Target{ID: "inferenceResources-target", DeploymentID: "inferenceResources-deployment", ProviderID: "inferenceResources-provider", PublicModel: model, ProviderModel: "provider-model", Region: "us-east-1", Adapter: adapter, FixedRequestMicrosUSD: 250}
}

func TestInferenceResourcesOwnerRegionMismatchFailsClosed(t *testing.T) {
	adapter := &inferenceResourcesAdapter{providerType: string(domain.ProviderOpenAI)}
	f := newInferenceResourcesServiceFixture(t, domain.ProfileOpenAIMediaResources, adapter, inferenceResourcesTargetFor("media", adapter), nil)
	defer f.close()
	resource := domain.ProviderResource{ProviderID: "inferenceResources-provider", DeploymentID: "inferenceResources-deployment", ProfileID: domain.ProfileOpenAIMediaResources, PublicModel: "media", Region: "us-east-1"}
	if _, err := f.service.ownedTarget(resource); err != nil {
		t.Fatalf("matching owner did not resolve: %v", err)
	}
	resource.Region = "eu-west-1"
	_, err := f.service.ownedTarget(resource)
	if err == nil {
		t.Fatal("resource bound to a different region resolved to the current deployment")
	}
}

func TestInferenceResourcesAsyncCancelRequiresRecordedOwner(t *testing.T) {
	adapter := &inferenceResourcesAdapter{providerType: string(domain.ProviderBedrock)}
	target := inferenceResourcesTargetFor("video", adapter)
	f := newInferenceResourcesServiceFixture(t, domain.ProfileBedrockAsyncNovaReel, adapter, target, nil)
	defer f.close()
	resource := domain.ProviderResource{ID: "async-owned", Kind: domain.ResourceAsyncInvoke, ProjectID: f.project.ID, ProviderID: "missing-provider", DeploymentID: "missing-deployment", PublicModel: "video", ProfileID: domain.ProfileBedrockAsyncNovaReel, Region: "us-east-1", CreationStatus: "completed", Status: "in_progress", CreatedAt: time.Now().Add(-time.Hour), UpdatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour), Revision: 1}
	f.store.resources[resource.ID] = resource
	_, err := f.service.CancelAsyncInvoke(context.Background(), f.plaintext, resource.ID)
	assertGatewayCode(t, err, "resource_owner_unavailable")
}

func TestInferenceResourcesTranscriptionAppliesOutboundRedaction(t *testing.T) {
	policy := domain.RedactionPolicy{ID: "inferenceResources-redaction", Name: "outbound", Enabled: true, Mode: "strict", Rules: []domain.RedactionRule{{ID: "email-out", Name: "email", Kind: "builtin", Builtin: "email", Scopes: []string{"outbound"}, Action: "reject", Enabled: true}}}
	adapter := &inferenceResourcesAdapter{providerType: string(domain.ProviderOpenAI), transcript: provider.TranscriptionResult{ContentType: "application/json", Data: []byte(`{"text":"private@example.com"}`)}}
	f := newInferenceResourcesServiceFixture(t, domain.ProfileOpenAIMediaResources, adapter, inferenceResourcesTargetFor("audio", adapter), []domain.RedactionPolicy{policy})
	defer f.close()
	audio := make([]byte, 512)
	copy(audio, []byte("ID3\x04\x00\x00\x00\x00\x00\x15"))
	_, err := f.service.Transcription(context.Background(), f.plaintext, "audio", provider.TranscriptionCall{Filename: "audio.mp3", ContentType: "audio/mpeg", Data: audio})
	assertGatewayCode(t, err, "sensitive_data_detected")
}

func TestInferenceResourcesUnknownFileCreationBlocksRetry(t *testing.T) {
	adapter := &inferenceResourcesAdapter{providerType: string(domain.ProviderOpenAI), fileErr: &provider.Error{Class: provider.ErrorTimeout, Ambiguous: true, Message: "timeout"}}
	f := newInferenceResourcesServiceFixture(t, domain.ProfileOpenAIMediaResources, adapter, inferenceResourcesTargetFor("files", adapter), nil)
	defer f.close()
	call := provider.FileCreateCall{Filename: "batch.jsonl", ContentType: "application/json", Purpose: "batch", Data: []byte(`{"x":1}`)}
	if _, err := f.service.CreateFile(context.Background(), f.plaintext, "files", "same-key", call); err == nil {
		t.Fatal("ambiguous provider result unexpectedly succeeded")
	}
	if _, err := f.service.CreateFile(context.Background(), f.plaintext, "files", "same-key", call); err == nil {
		t.Fatal("unknown creation was blindly retried")
	} else {
		assertGatewayCode(t, err, "idempotency_in_progress")
	}
	if adapter.fileCalls != 1 {
		t.Fatalf("provider calls=%d, want 1", adapter.fileCalls)
	}
}

func TestInferenceResourcesFileRedactionCannotBeBypassedWithOctetStream(t *testing.T) {
	adapter := &inferenceResourcesAdapter{providerType: string(domain.ProviderOpenAI)}
	f := newInferenceResourcesServiceFixture(t, domain.ProfileOpenAIMediaResources, adapter, inferenceResourcesTargetFor("files", adapter), nil)
	defer f.close()
	_, err := f.service.CreateFile(context.Background(), f.plaintext, "files", "disguised-text", provider.FileCreateCall{
		Filename: "batch.bin", ContentType: "application/octet-stream", Purpose: "batch",
		Data: []byte(`{"secret":"sk-abcdefghijklmnop"}`),
	})
	assertGatewayCode(t, err, "sensitive_data_detected")
	if adapter.fileCalls != 0 {
		t.Fatalf("disguised secret reached provider: calls=%d", adapter.fileCalls)
	}
}

func TestInferenceResourcesResourceMetadataAppliesOutboundRedaction(t *testing.T) {
	policy := domain.RedactionPolicy{ID: "resource-outbound", Name: "outbound", Enabled: true, Mode: "strict", Rules: []domain.RedactionRule{{ID: "email-out", Name: "email", Kind: "builtin", Builtin: "email", Scopes: []string{"outbound"}, Action: "reject", Enabled: true}}}
	adapter := &inferenceResourcesAdapter{
		providerType: string(domain.ProviderOpenAI),
		file:         provider.FileObject{ID: "upstream-file", Object: "file", Filename: "private@example.com", Purpose: "batch", Status: "uploaded"},
		batch:        provider.BatchObject{ID: "upstream-batch", Object: "batch", Status: "in_progress", Metadata: map[string]string{"owner": "private@example.com"}},
	}
	f := newInferenceResourcesServiceFixture(t, domain.ProfileOpenAIMediaResources, adapter, inferenceResourcesTargetFor("resources", adapter), []domain.RedactionPolicy{policy})
	defer f.close()
	now := time.Now()
	file := domain.ProviderResource{ID: "file-owned", Kind: domain.ResourceFile, ProjectID: f.project.ID, ProviderID: "inferenceResources-provider", DeploymentID: "inferenceResources-deployment", PublicModel: "resources", ProfileID: domain.ProfileOpenAIMediaResources, Region: "us-east-1", UpstreamID: "upstream-file", CreationStatus: "completed", Status: "uploaded", CreatedAt: now.Add(-time.Hour), UpdatedAt: now, ExpiresAt: now.Add(time.Hour), Revision: 1}
	batch := domain.ProviderResource{ID: "batch-owned", Kind: domain.ResourceBatch, ProjectID: f.project.ID, ProviderID: "inferenceResources-provider", DeploymentID: "inferenceResources-deployment", PublicModel: "resources", ProfileID: domain.ProfileOpenAIMediaResources, Region: "us-east-1", UpstreamID: "upstream-batch", CreationStatus: "completed", Status: "in_progress", CreatedAt: now.Add(-time.Hour), UpdatedAt: now, ExpiresAt: now.Add(time.Hour), Revision: 1}
	f.store.resources[file.ID] = file
	f.store.resources[batch.ID] = batch
	if _, err := f.service.GetFile(context.Background(), f.plaintext, file.ID); err == nil {
		t.Fatal("sensitive file metadata was released")
	} else {
		assertGatewayCode(t, err, "sensitive_data_detected")
	}
	if _, err := f.service.GetBatch(context.Background(), f.plaintext, batch.ID); err == nil {
		t.Fatal("sensitive batch metadata was released")
	} else {
		assertGatewayCode(t, err, "sensitive_data_detected")
	}
}

func TestInferenceResourcesFileFingerprintConflictAndPrivateObjectLifecycle(t *testing.T) {
	adapter := &inferenceResourcesAdapter{providerType: string(domain.ProviderOpenAI)}
	f := newInferenceResourcesServiceFixture(t, domain.ProfileOpenAIMediaResources, adapter, inferenceResourcesTargetFor("files", adapter), nil)
	defer f.close()
	first, err := f.service.CreateFile(context.Background(), f.plaintext, "files", "stable-key", provider.FileCreateCall{Filename: "batch.jsonl", ContentType: "application/json", Purpose: "batch", Data: []byte(`{"x":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	resource, err := f.store.ProviderResource(context.Background(), f.project.ID, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.service.resourceObjectDir, resource.ObjectPath)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("object mode=%o, want 600", info.Mode().Perm())
	}
	_, err = f.service.CreateFile(context.Background(), f.plaintext, "files", "stable-key", provider.FileCreateCall{Filename: "batch.jsonl", ContentType: "application/json", Purpose: "batch", Data: []byte(`{"x":2}`)})
	assertGatewayCode(t, err, "idempotency_conflict")
	if adapter.fileCalls != 1 {
		t.Fatalf("provider calls=%d, want 1", adapter.fileCalls)
	}
	if _, err := f.service.DeleteFile(context.Background(), f.plaintext, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private object remained after delete: %v", err)
	}
}

func TestInferenceResourcesFixedRequestPriceIsAccounted(t *testing.T) {
	adapter := &inferenceResourcesAdapter{providerType: string(domain.ProviderOpenAI), image: provider.ImageResult{Data: []provider.ImageData{{URL: "https://example.invalid/image"}}}}
	target := inferenceResourcesTargetFor("image", adapter)
	target.InputMicrosPerMillion = 0
	f := newInferenceResourcesServiceFixture(t, domain.ProfileOpenAIMediaResources, adapter, target, nil)
	defer f.close()
	if _, err := f.service.Images(context.Background(), f.plaintext, openaiapi.ImageGenerationRequest{Model: "image", Prompt: "owl", N: 1}); err != nil {
		t.Fatal(err)
	}
	balance := f.state.Balance(f.project.ID, time.Now().In(time.UTC).Format("2006-01-02"), testTimezoneVersion)
	if balance.CommittedMicrosUSD != target.FixedRequestMicrosUSD || balance.ReservedMicrosUSD != 0 {
		t.Fatalf("balance=%#v, want committed=%d", balance, target.FixedRequestMicrosUSD)
	}
}

func TestInferenceResourcesFileCleanupFailureKeepsRetryState(t *testing.T) {
	adapter := &inferenceResourcesAdapter{providerType: string(domain.ProviderOpenAI)}
	f := newInferenceResourcesServiceFixture(t, domain.ProfileOpenAIMediaResources, adapter, inferenceResourcesTargetFor("files", adapter), nil)
	defer f.close()
	created, err := f.service.CreateFile(context.Background(), f.plaintext, "files", "cleanup-key", provider.FileCreateCall{Filename: "batch.jsonl", ContentType: "application/json", Purpose: "batch", Data: []byte(`{"x":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	resource, err := f.store.ProviderResource(context.Background(), f.project.ID, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.service.resourceObjectDir, resource.ObjectPath)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(path, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.DeleteFile(context.Background(), f.plaintext, created.ID); err == nil {
		t.Fatal("non-empty object path unexpectedly cleaned")
	}
	resource, err = f.store.ProviderResource(context.Background(), f.project.ID, created.ID)
	if err != nil || resource.CleanupStatus != "pending" {
		t.Fatalf("cleanup retry state=%#v err=%v", resource, err)
	}
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.DeleteFile(context.Background(), f.plaintext, created.ID); err != nil {
		t.Fatal(err)
	}
	if adapter.deleteCalls != 1 {
		t.Fatalf("upstream delete calls=%d, want 1", adapter.deleteCalls)
	}
}

func TestInferenceResourcesFileDeleteRecoversAfterCleanupStateWriteFailure(t *testing.T) {
	adapter := &inferenceResourcesAdapter{providerType: string(domain.ProviderOpenAI)}
	f := newInferenceResourcesServiceFixture(t, domain.ProfileOpenAIMediaResources, adapter, inferenceResourcesTargetFor("files", adapter), nil)
	defer f.close()
	created, err := f.service.CreateFile(context.Background(), f.plaintext, "files", "delete-recovery", provider.FileCreateCall{Filename: "batch.jsonl", ContentType: "application/json", Purpose: "batch", Data: []byte(`{"x":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	f.store.failCleanupPendingWrite = true
	if _, err := f.service.DeleteFile(context.Background(), f.plaintext, created.ID); err == nil {
		t.Fatal("injected cleanup state write failure was ignored")
	}
	resource, err := f.store.ProviderResource(context.Background(), f.project.ID, created.ID)
	if err != nil || resource.CleanupStatus != "deleting" {
		t.Fatalf("delete recovery state=%#v err=%v", resource, err)
	}
	adapter.getFileErr = &provider.Error{Class: provider.ErrorBadRequest, StatusCode: 404, Message: "not found"}
	if _, err := f.service.DeleteFile(context.Background(), f.plaintext, created.ID); err != nil {
		t.Fatal(err)
	}
	if adapter.deleteCalls != 1 || adapter.getFileCalls != 1 {
		t.Fatalf("delete calls=%d lookup calls=%d, want 1/1", adapter.deleteCalls, adapter.getFileCalls)
	}
	if _, err := f.store.ProviderResource(context.Background(), f.project.ID, created.ID); !errors.Is(err, errInferenceResourcesResourceNotFound) {
		t.Fatalf("resource metadata remained after recovery: %v", err)
	}
}

func TestInferenceResourcesExpiredFileCleansPinnedUpstreamBeforeOwnerMapping(t *testing.T) {
	adapter := &inferenceResourcesAdapter{providerType: string(domain.ProviderOpenAI)}
	f := newInferenceResourcesServiceFixture(t, domain.ProfileOpenAIMediaResources, adapter, inferenceResourcesTargetFor("files", adapter), nil)
	defer f.close()
	created, err := f.service.CreateFile(context.Background(), f.plaintext, "files", "ttl-cleanup", provider.FileCreateCall{Filename: "batch.jsonl", ContentType: "application/json", Purpose: "batch", Data: []byte(`{"x":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	resource, err := f.store.ProviderResource(context.Background(), f.project.ID, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.service.resourceObjectDir, resource.ObjectPath)
	resource.CreatedAt = time.Now().Add(-2 * time.Hour)
	resource.ExpiresAt = time.Now().Add(-time.Hour)
	resource, err = f.store.PutProviderResource(context.Background(), resource, resource.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.CleanupExpiredProviderResource(context.Background(), resource); err != nil {
		t.Fatal(err)
	}
	if adapter.deleteCalls != 1 {
		t.Fatalf("upstream delete calls=%d, want 1", adapter.deleteCalls)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired local object remained: %v", err)
	}
	if _, err := f.store.ProviderResource(context.Background(), f.project.ID, created.ID); !errors.Is(err, errInferenceResourcesResourceNotFound) {
		t.Fatalf("expired owner mapping remained after complete cleanup: %v", err)
	}
}

func assertGatewayCode(t *testing.T, err error, code string) {
	t.Helper()
	var gatewayErr *Error
	if !errors.As(err, &gatewayErr) {
		t.Fatalf("error=%v is not a gateway error; want code %q", err, code)
	}
	if gatewayErr.Code != code {
		t.Fatalf("error=%v code=%q, want %q", err, gatewayErr.Code, code)
	}
}

// A reservation is written before the provider is called, so a process that dies
// in between leaves one behind. It used to be indistinguishable from a call that
// might already have reached the provider, so the idempotency key was refused
// for the next seven to thirty days — the caller could neither complete nor
// retry that request. A reservation owned by an instance that is gone never
// reached the provider, and can be taken over.
func TestInferenceResourcesReservationLeftByACrashIsReclaimed(t *testing.T) {
	adapter := &inferenceResourcesAdapter{providerType: string(domain.ProviderOpenAI)}
	f := newInferenceResourcesServiceFixture(t, domain.ProfileOpenAIMediaResources, adapter, inferenceResourcesTargetFor("files", adapter), nil)
	defer f.close()
	call := provider.FileCreateCall{Filename: "batch.jsonl", ContentType: "application/json", Purpose: "batch", Data: []byte(`{"x":1}`)}

	f.store.failInFlightWrite = true
	if _, err := f.service.CreateFile(context.Background(), f.plaintext, "files", "crash-key", call); err == nil {
		t.Fatal("the interrupted reservation reported success")
	}
	if adapter.fileCalls != 0 {
		t.Fatalf("the provider was called despite the interruption: calls=%d", adapter.fileCalls)
	}

	// The same process still owns that reservation, so a retry here is a
	// concurrent duplicate and must be refused.
	if _, err := f.service.CreateFile(context.Background(), f.plaintext, "files", "crash-key", call); err == nil {
		t.Fatal("a live reservation was taken over by its own process")
	} else {
		assertGatewayCode(t, err, "idempotency_in_progress")
	}

	// Restart: same data directory, new process. The reservation is now owned by
	// nobody and provably never reached the provider.
	f.service.instanceID = "inst_after_restart"
	created, err := f.service.CreateFile(context.Background(), f.plaintext, "files", "crash-key", call)
	if err != nil {
		t.Fatalf("the abandoned reservation was not reclaimed: %v", err)
	}
	if adapter.fileCalls != 1 {
		t.Fatalf("provider calls=%d, want 1", adapter.fileCalls)
	}

	// And it is a normal completed resource afterwards: the same key returns it
	// rather than creating a second one upstream.
	repeated, err := f.service.CreateFile(context.Background(), f.plaintext, "files", "crash-key", call)
	if err != nil {
		t.Fatal(err)
	}
	if repeated.ID != created.ID || adapter.fileCalls != 1 {
		t.Fatalf("repeat created a second upstream resource: id=%s want=%s calls=%d", repeated.ID, created.ID, adapter.fileCalls)
	}
}

// The other half of the same split: once the provider has been called, a crash
// leaves an outcome nobody can determine, and that stays refused.
func TestInferenceResourcesInFlightCrashIsNotReclaimed(t *testing.T) {
	adapter := &inferenceResourcesAdapter{providerType: string(domain.ProviderOpenAI), fileErr: &provider.Error{Class: provider.ErrorTimeout, Ambiguous: true, Message: "timeout"}}
	f := newInferenceResourcesServiceFixture(t, domain.ProfileOpenAIMediaResources, adapter, inferenceResourcesTargetFor("files", adapter), nil)
	defer f.close()
	call := provider.FileCreateCall{Filename: "batch.jsonl", ContentType: "application/json", Purpose: "batch", Data: []byte(`{"x":1}`)}

	f.store.failOutcomeWrite = true
	if _, err := f.service.CreateFile(context.Background(), f.plaintext, "files", "ambiguous-key", call); err == nil {
		t.Fatal("an ambiguous provider result reported success")
	}
	f.service.instanceID = "inst_after_restart"
	if _, err := f.service.CreateFile(context.Background(), f.plaintext, "files", "ambiguous-key", call); err == nil {
		t.Fatal("a restart retried a call that may already have reached the provider")
	} else {
		assertGatewayCode(t, err, "idempotency_in_progress")
	}
	if adapter.fileCalls != 1 {
		t.Fatalf("provider calls=%d, want 1", adapter.fileCalls)
	}
}

// A batch names three files, and only one of them was ever translated. The batch
// itself got a Halro identifier while input_file_id, output_file_id and
// error_file_id went back to the caller exactly as the upstream wrote them —
// leaking an upstream identifier through a surface whose manifest promises
// project-scoped opaque ones, and handing the caller identifiers that answer 404
// against Halro's own files endpoint. The documented way to collect a batch's
// results did not work.
func TestBatchNamesItsFilesWithHalroIdentifiers(t *testing.T) {
	adapter := &inferenceResourcesAdapter{providerType: string(domain.ProviderOpenAI)}
	f := newInferenceResourcesServiceFixture(t, domain.ProfileOpenAIMediaResources, adapter, inferenceResourcesTargetFor("resources", adapter), nil)
	defer f.close()
	now := time.Now()
	batch := domain.ProviderResource{
		ID: "batch-owned", Kind: domain.ResourceBatch, ProjectID: f.project.ID,
		ProviderID: "inferenceResources-provider", DeploymentID: "inferenceResources-deployment",
		PublicModel: "resources", ProfileID: domain.ProfileOpenAIMediaResources, Region: "us-east-1",
		UpstreamID: "upstream-batch", InputFileID: "file_halro_input",
		CreationStatus: "completed", Status: "in_progress",
		CreatedAt: now.Add(-time.Hour), UpdatedAt: now, ExpiresAt: now.Add(time.Hour), Revision: 1,
	}
	f.store.resources[batch.ID] = batch
	adapter.batch = provider.BatchObject{
		ID: "upstream-batch", Object: "batch", Status: "completed",
		InputFileID: "file-upstream-input", OutputFileID: "file-upstream-output", ErrorFileID: "file-upstream-errors",
	}

	result, err := f.service.GetBatch(context.Background(), f.plaintext, batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.ID != batch.ID {
		t.Fatalf("batch id=%q", result.ID)
	}
	if result.InputFileID != "file_halro_input" {
		t.Fatalf("input_file_id=%q, want the identifier the caller supplied", result.InputFileID)
	}
	for name, value := range map[string]string{"output_file_id": result.OutputFileID, "error_file_id": result.ErrorFileID} {
		if value == "" {
			t.Fatalf("%s was dropped", name)
		}
		if strings.HasPrefix(value, "file-upstream") {
			t.Fatalf("%s=%q is the upstream's own identifier", name, value)
		}
		// The translated identifier has to resolve in this project, which is the
		// whole point: an identifier the caller cannot use is no better than the
		// upstream's.
		resolved, ok := f.store.resources[value]
		if !ok || resolved.ProjectID != f.project.ID || resolved.Kind != domain.ResourceFile {
			t.Fatalf("%s=%q does not resolve to a file in this project: %#v", name, value, resolved)
		}
	}

	// A batch is polled. The second look must reuse the identifiers minted by the
	// first, or every poll leaves another record behind for one upstream file.
	before := len(f.store.resources)
	second, err := f.service.GetBatch(context.Background(), f.plaintext, batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if second.OutputFileID != result.OutputFileID || second.ErrorFileID != result.ErrorFileID {
		t.Fatalf("polling minted new identifiers: %q/%q then %q/%q",
			result.OutputFileID, result.ErrorFileID, second.OutputFileID, second.ErrorFileID)
	}
	if len(f.store.resources) != before {
		t.Fatalf("polling created %d extra resource records", len(f.store.resources)-before)
	}
}

// A batch record written before the file identifiers were recorded carries none
// of them. It answers with the fields absent rather than with the upstream's
// values: not knowing is a worse answer than knowing and a better one than
// being wrong.
func TestBatchWithoutRecordedFilesDoesNotFallBackToUpstreamIdentifiers(t *testing.T) {
	adapter := &inferenceResourcesAdapter{providerType: string(domain.ProviderOpenAI)}
	f := newInferenceResourcesServiceFixture(t, domain.ProfileOpenAIMediaResources, adapter, inferenceResourcesTargetFor("resources", adapter), nil)
	defer f.close()
	now := time.Now()
	batch := domain.ProviderResource{
		ID: "batch-legacy", Kind: domain.ResourceBatch, ProjectID: f.project.ID,
		ProviderID: "inferenceResources-provider", DeploymentID: "inferenceResources-deployment",
		PublicModel: "resources", ProfileID: domain.ProfileOpenAIMediaResources, Region: "us-east-1",
		UpstreamID: "upstream-batch", CreationStatus: "completed", Status: "in_progress",
		CreatedAt: now.Add(-time.Hour), UpdatedAt: now, ExpiresAt: now.Add(time.Hour), Revision: 1,
	}
	f.store.resources[batch.ID] = batch
	adapter.batch = provider.BatchObject{ID: "upstream-batch", Object: "batch", Status: "in_progress", InputFileID: "file-upstream-input"}

	result, err := f.service.GetBatch(context.Background(), f.plaintext, batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.InputFileID != "" {
		t.Fatalf("input_file_id=%q, want it absent rather than the upstream's", result.InputFileID)
	}
}

// A file Halro holds and the upstream never received has to be answerable
// without asking anyone. Every lifecycle path used to assume the opposite —
// metadata was fetched from the upstream, expiry deleted there first — which
// made the resource unusable rather than merely unusual. ADR 0021 settles that
// an empty UpstreamID is an ordinary state.
//
// The assertion that matters is the call count: not that the answers are right,
// but that no upstream was contacted to produce them.
func TestLocalOnlyFileIsServedAndReapedWithoutTouchingTheUpstream(t *testing.T) {
	adapter := &inferenceResourcesAdapter{providerType: string(domain.ProviderOpenAI)}
	f := newInferenceResourcesServiceFixture(t, domain.ProfileOpenAIMediaResources, adapter, inferenceResourcesTargetFor("resources", adapter), nil)
	defer f.close()
	now := time.Now()

	objectPath, err := f.service.writeResourceObject("file-local", []byte("{\"custom_id\":\"a\"}\n"))
	if err != nil {
		t.Fatal(err)
	}
	local := domain.ProviderResource{
		ID: "file-local", Kind: domain.ResourceFile, ProjectID: f.project.ID,
		ProviderID: "inferenceResources-provider", DeploymentID: "inferenceResources-deployment",
		PublicModel: "resources", ProfileID: domain.ProfileOpenAIMediaResources, Region: "us-east-1",
		UpstreamID: "", ObjectPath: objectPath, ObjectContentType: "application/jsonl",
		ObjectFilename: "batch-input.jsonl", ObjectPurpose: "batch",
		CreationStatus: "completed", Status: "uploaded",
		CreatedAt: now.Add(-time.Hour), UpdatedAt: now, ExpiresAt: now.Add(-time.Minute), Revision: 1,
	}
	f.store.resources[local.ID] = local

	object, err := f.service.GetFile(context.Background(), f.plaintext, local.ID)
	if err != nil {
		t.Fatalf("metadata for a local-only file: %v", err)
	}
	if object.ID != local.ID || object.Filename != "batch-input.jsonl" || object.Purpose != "batch" {
		t.Fatalf("metadata came back wrong: %#v", object)
	}
	if object.Bytes == 0 {
		t.Fatal("metadata reported no size for an object that exists")
	}
	if adapter.getFileCalls != 0 {
		t.Fatalf("the upstream was asked about a file it never received (%d calls)", adapter.getFileCalls)
	}

	content, err := f.service.DownloadFile(context.Background(), f.plaintext, local.ID)
	if err != nil {
		t.Fatalf("content for a local-only file: %v", err)
	}
	if len(content.Data) == 0 {
		t.Fatal("content was empty")
	}

	if err := f.service.CleanupExpiredProviderResource(context.Background(), f.store.resources[local.ID]); err != nil {
		t.Fatalf("expiry cleanup: %v", err)
	}
	if adapter.deleteCalls != 0 {
		t.Fatalf("expiry deleted upstream for a file the upstream never had (%d calls)", adapter.deleteCalls)
	}
	if _, present := f.store.resources[local.ID]; present {
		t.Fatal("the record survived its own cleanup")
	}
	if _, statErr := os.Stat(filepath.Join(f.objectDir, objectPath)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("the local object survived cleanup: %v", statErr)
	}
}
