package gateway

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Heimdall/internal/auth"
	"github.com/akz142857/Heimdall/internal/budget"
	"github.com/akz142857/Heimdall/internal/domain"
	"github.com/akz142857/Heimdall/internal/ledger"
	"github.com/akz142857/Heimdall/internal/openaiapi"
	"github.com/akz142857/Heimdall/internal/provider"
	"github.com/akz142857/Heimdall/internal/redaction"
	"github.com/akz142857/Heimdall/internal/semantic"
)

var errPhase2ResourceNotFound = errors.New("phase2 resource not found")

type phase2MemoryStore struct {
	mu                      sync.Mutex
	resources               map[string]domain.ProviderResource
	failCleanupPendingWrite bool
}

func newPhase2MemoryStore(resources ...domain.ProviderResource) *phase2MemoryStore {
	store := &phase2MemoryStore{resources: make(map[string]domain.ProviderResource)}
	for _, resource := range resources {
		store.resources[resource.ID] = resource
	}
	return store
}

func (s *phase2MemoryStore) PutProviderResource(_ context.Context, resource domain.ProviderResource, expected uint64) (domain.ProviderResource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, exists := s.resources[resource.ID]
	if s.failCleanupPendingWrite && resource.CleanupStatus == "pending" {
		s.failCleanupPendingWrite = false
		return domain.ProviderResource{}, errors.New("injected cleanup state write failure")
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

func (s *phase2MemoryStore) ProviderResource(_ context.Context, projectID, id string) (domain.ProviderResource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	resource, ok := s.resources[id]
	if !ok || resource.ProjectID != projectID {
		return domain.ProviderResource{}, errPhase2ResourceNotFound
	}
	return resource, nil
}

func (s *phase2MemoryStore) DeleteProviderResource(_ context.Context, projectID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	resource, ok := s.resources[id]
	if !ok || resource.ProjectID != projectID {
		return errPhase2ResourceNotFound
	}
	delete(s.resources, id)
	return nil
}

func (s *phase2MemoryStore) ProviderResourceByIdempotency(_ context.Context, projectID string, kind domain.ProviderResourceKind, hash [32]byte) (domain.ProviderResource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, resource := range s.resources {
		if resource.ProjectID == projectID && resource.Kind == kind && resource.IdempotencyKeyHash == hash {
			return resource, nil
		}
	}
	return domain.ProviderResource{}, errPhase2ResourceNotFound
}

type phase2Adapter struct {
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

func (a *phase2Adapter) Type() string { return a.providerType }
func (a *phase2Adapter) Close()       {}
func (a *phase2Adapter) Chat(context.Context, provider.ChatCall) (openaiapi.ChatCompletionResponse, error) {
	return openaiapi.ChatCompletionResponse{}, errors.New("unused")
}
func (a *phase2Adapter) ChatStream(context.Context, provider.ChatCall, func(semantic.Event) error) (*openaiapi.Usage, error) {
	return nil, errors.New("unused")
}
func (a *phase2Adapter) Embed(context.Context, provider.EmbeddingCall) (openaiapi.EmbeddingResponse, error) {
	return openaiapi.EmbeddingResponse{}, errors.New("unused")
}
func (a *phase2Adapter) Moderate(context.Context, provider.ModerationCall) (provider.ModerationResult, error) {
	return provider.ModerationResult{}, nil
}
func (a *phase2Adapter) GenerateImage(context.Context, provider.ImageCall) (provider.ImageResult, error) {
	return a.image, nil
}
func (a *phase2Adapter) Transcribe(context.Context, provider.TranscriptionCall) (provider.TranscriptionResult, error) {
	return a.transcript, nil
}
func (a *phase2Adapter) Synthesize(context.Context, provider.SpeechCall) (provider.SpeechResult, error) {
	return provider.SpeechResult{}, nil
}
func (a *phase2Adapter) CreateFile(_ context.Context, call provider.FileCreateCall) (provider.FileObject, error) {
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
func (a *phase2Adapter) GetFile(context.Context, string, string) (provider.FileObject, error) {
	a.getFileCalls++
	return a.file, a.getFileErr
}
func (a *phase2Adapter) DownloadFile(context.Context, string, string) (provider.FileContent, error) {
	return provider.FileContent{}, nil
}
func (a *phase2Adapter) DeleteFile(_ context.Context, _, id string) (provider.FileDeleteResult, error) {
	a.deleteCalls++
	if a.deleteErr != nil {
		return provider.FileDeleteResult{}, a.deleteErr
	}
	return provider.FileDeleteResult{ID: id, Object: "file", Deleted: true}, nil
}
func (a *phase2Adapter) CreateBatch(context.Context, provider.BatchCreateCall) (provider.BatchObject, error) {
	return provider.BatchObject{}, nil
}
func (a *phase2Adapter) GetBatch(context.Context, string, string) (provider.BatchObject, error) {
	return a.batch, nil
}
func (a *phase2Adapter) CancelBatch(context.Context, string, string) (provider.BatchObject, error) {
	return provider.BatchObject{}, nil
}

type phase2ServiceFixture struct {
	service   *Service
	plaintext string
	project   domain.Project
	state     *ledger.State
	store     *phase2MemoryStore
	close     func()
}

func newPhase2ServiceFixture(t *testing.T, profileID domain.ProviderProfileID, adapter provider.Adapter, target provider.Target, policies []domain.RedactionPolicy) phase2ServiceFixture {
	t.Helper()
	project := domain.Project{ID: "phase2-project", Name: "Phase 2", Enabled: true, AllowedRoutes: []string{target.PublicModel}, DailyBudgetMicrosUSD: 1_000_000, MaxInputTokens: 100_000, MaxOutputTokens: 100_000}
	if len(policies) > 0 {
		project.RedactionPolicyID = policies[0].ID
	}
	plaintext, key, err := auth.GenerateGatewayKey(project.ID, "phase2", nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := auth.NewSnapshot()
	if err := snapshot.Refresh(context.Background(), source{keys: []domain.GatewayKey{key}, projects: []domain.Project{project}}); err != nil {
		t.Fatal(err)
	}
	log, err := ledger.Open(filepath.Join(t.TempDir(), "phase2-usage.wal"), ledger.NewStatus())
	if err != nil {
		t.Fatal(err)
	}
	state := ledger.NewState()
	accounting, err := budget.New(log, state, time.UTC)
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
	store := newPhase2MemoryStore()
	objectDir := filepath.Join(t.TempDir(), "objects")
	service, err := NewServiceWithOptions(snapshot, registry, accounting, ServiceOptions{Resources: store, ResourceObjectDir: objectDir, Redactor: redactor})
	if err != nil {
		t.Fatal(err)
	}
	return phase2ServiceFixture{service: service, plaintext: plaintext, project: project, state: state, store: store, close: func() { _ = log.Close() }}
}

func phase2TargetFor(model string, adapter provider.Adapter) provider.Target {
	return provider.Target{ID: "phase2-target", DeploymentID: "phase2-deployment", ProviderID: "phase2-provider", PublicModel: model, ProviderModel: "provider-model", Region: "us-east-1", Adapter: adapter, FixedRequestMicrosUSD: 250}
}

func TestPhase2OwnerRegionMismatchFailsClosed(t *testing.T) {
	adapter := &phase2Adapter{providerType: string(domain.ProviderOpenAI)}
	f := newPhase2ServiceFixture(t, domain.ProfileOpenAIPhase2, adapter, phase2TargetFor("media", adapter), nil)
	defer f.close()
	resource := domain.ProviderResource{ProviderID: "phase2-provider", DeploymentID: "phase2-deployment", ProfileID: domain.ProfileOpenAIPhase2, PublicModel: "media", Region: "us-east-1"}
	if _, err := f.service.ownedTarget(resource); err != nil {
		t.Fatalf("matching owner did not resolve: %v", err)
	}
	resource.Region = "eu-west-1"
	_, err := f.service.ownedTarget(resource)
	if err == nil {
		t.Fatal("resource bound to a different region resolved to the current deployment")
	}
}

func TestPhase2AsyncCancelRequiresRecordedOwner(t *testing.T) {
	adapter := &phase2Adapter{providerType: string(domain.ProviderBedrock)}
	target := phase2TargetFor("video", adapter)
	f := newPhase2ServiceFixture(t, domain.ProfileBedrockAsyncNovaReel, adapter, target, nil)
	defer f.close()
	resource := domain.ProviderResource{ID: "async-owned", Kind: domain.ResourceAsyncInvoke, ProjectID: f.project.ID, ProviderID: "missing-provider", DeploymentID: "missing-deployment", PublicModel: "video", ProfileID: domain.ProfileBedrockAsyncNovaReel, Region: "us-east-1", CreationStatus: "completed", Status: "in_progress", CreatedAt: time.Now().Add(-time.Hour), UpdatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour), Revision: 1}
	f.store.resources[resource.ID] = resource
	_, err := f.service.CancelAsyncInvoke(context.Background(), f.plaintext, resource.ID)
	assertGatewayCode(t, err, "resource_owner_unavailable")
}

func TestPhase2TranscriptionAppliesOutboundRedaction(t *testing.T) {
	policy := domain.RedactionPolicy{ID: "phase2-redaction", Name: "outbound", Enabled: true, Mode: "strict", Rules: []domain.RedactionRule{{ID: "email-out", Name: "email", Kind: "builtin", Builtin: "email", Scopes: []string{"outbound"}, Action: "reject", Enabled: true}}}
	adapter := &phase2Adapter{providerType: string(domain.ProviderOpenAI), transcript: provider.TranscriptionResult{ContentType: "application/json", Data: []byte(`{"text":"private@example.com"}`)}}
	f := newPhase2ServiceFixture(t, domain.ProfileOpenAIPhase2, adapter, phase2TargetFor("audio", adapter), []domain.RedactionPolicy{policy})
	defer f.close()
	audio := make([]byte, 512)
	copy(audio, []byte("ID3\x04\x00\x00\x00\x00\x00\x15"))
	_, err := f.service.Transcription(context.Background(), f.plaintext, "audio", provider.TranscriptionCall{Filename: "audio.mp3", ContentType: "audio/mpeg", Data: audio})
	assertGatewayCode(t, err, "sensitive_data_detected")
}

func TestPhase2UnknownFileCreationBlocksRetry(t *testing.T) {
	adapter := &phase2Adapter{providerType: string(domain.ProviderOpenAI), fileErr: &provider.Error{Class: provider.ErrorTimeout, Ambiguous: true, Message: "timeout"}}
	f := newPhase2ServiceFixture(t, domain.ProfileOpenAIPhase2, adapter, phase2TargetFor("files", adapter), nil)
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

func TestPhase2FileRedactionCannotBeBypassedWithOctetStream(t *testing.T) {
	adapter := &phase2Adapter{providerType: string(domain.ProviderOpenAI)}
	f := newPhase2ServiceFixture(t, domain.ProfileOpenAIPhase2, adapter, phase2TargetFor("files", adapter), nil)
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

func TestPhase2ResourceMetadataAppliesOutboundRedaction(t *testing.T) {
	policy := domain.RedactionPolicy{ID: "resource-outbound", Name: "outbound", Enabled: true, Mode: "strict", Rules: []domain.RedactionRule{{ID: "email-out", Name: "email", Kind: "builtin", Builtin: "email", Scopes: []string{"outbound"}, Action: "reject", Enabled: true}}}
	adapter := &phase2Adapter{
		providerType: string(domain.ProviderOpenAI),
		file:         provider.FileObject{ID: "upstream-file", Object: "file", Filename: "private@example.com", Purpose: "batch", Status: "uploaded"},
		batch:        provider.BatchObject{ID: "upstream-batch", Object: "batch", Status: "in_progress", Metadata: map[string]string{"owner": "private@example.com"}},
	}
	f := newPhase2ServiceFixture(t, domain.ProfileOpenAIPhase2, adapter, phase2TargetFor("resources", adapter), []domain.RedactionPolicy{policy})
	defer f.close()
	now := time.Now()
	file := domain.ProviderResource{ID: "file-owned", Kind: domain.ResourceFile, ProjectID: f.project.ID, ProviderID: "phase2-provider", DeploymentID: "phase2-deployment", PublicModel: "resources", ProfileID: domain.ProfileOpenAIPhase2, Region: "us-east-1", UpstreamID: "upstream-file", CreationStatus: "completed", Status: "uploaded", CreatedAt: now.Add(-time.Hour), UpdatedAt: now, ExpiresAt: now.Add(time.Hour), Revision: 1}
	batch := domain.ProviderResource{ID: "batch-owned", Kind: domain.ResourceBatch, ProjectID: f.project.ID, ProviderID: "phase2-provider", DeploymentID: "phase2-deployment", PublicModel: "resources", ProfileID: domain.ProfileOpenAIPhase2, Region: "us-east-1", UpstreamID: "upstream-batch", CreationStatus: "completed", Status: "in_progress", CreatedAt: now.Add(-time.Hour), UpdatedAt: now, ExpiresAt: now.Add(time.Hour), Revision: 1}
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

func TestPhase2FileFingerprintConflictAndPrivateObjectLifecycle(t *testing.T) {
	adapter := &phase2Adapter{providerType: string(domain.ProviderOpenAI)}
	f := newPhase2ServiceFixture(t, domain.ProfileOpenAIPhase2, adapter, phase2TargetFor("files", adapter), nil)
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

func TestPhase2FixedRequestPriceIsAccounted(t *testing.T) {
	adapter := &phase2Adapter{providerType: string(domain.ProviderOpenAI), image: provider.ImageResult{Data: []provider.ImageData{{URL: "https://example.invalid/image"}}}}
	target := phase2TargetFor("image", adapter)
	target.InputMicrosPerMillion = 0
	f := newPhase2ServiceFixture(t, domain.ProfileOpenAIPhase2, adapter, target, nil)
	defer f.close()
	if _, err := f.service.Images(context.Background(), f.plaintext, openaiapi.ImageGenerationRequest{Model: "image", Prompt: "owl", N: 1}); err != nil {
		t.Fatal(err)
	}
	balance := f.state.Balance(f.project.ID, time.Now().In(time.UTC).Format("2006-01-02"))
	if balance.CommittedMicrosUSD != target.FixedRequestMicrosUSD || balance.ReservedMicrosUSD != 0 {
		t.Fatalf("balance=%#v, want committed=%d", balance, target.FixedRequestMicrosUSD)
	}
}

func TestPhase2FileCleanupFailureKeepsRetryState(t *testing.T) {
	adapter := &phase2Adapter{providerType: string(domain.ProviderOpenAI)}
	f := newPhase2ServiceFixture(t, domain.ProfileOpenAIPhase2, adapter, phase2TargetFor("files", adapter), nil)
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

func TestPhase2FileDeleteRecoversAfterCleanupStateWriteFailure(t *testing.T) {
	adapter := &phase2Adapter{providerType: string(domain.ProviderOpenAI)}
	f := newPhase2ServiceFixture(t, domain.ProfileOpenAIPhase2, adapter, phase2TargetFor("files", adapter), nil)
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
	if _, err := f.store.ProviderResource(context.Background(), f.project.ID, created.ID); !errors.Is(err, errPhase2ResourceNotFound) {
		t.Fatalf("resource metadata remained after recovery: %v", err)
	}
}

func TestPhase2ExpiredFileCleansPinnedUpstreamBeforeOwnerMapping(t *testing.T) {
	adapter := &phase2Adapter{providerType: string(domain.ProviderOpenAI)}
	f := newPhase2ServiceFixture(t, domain.ProfileOpenAIPhase2, adapter, phase2TargetFor("files", adapter), nil)
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
	if _, err := f.store.ProviderResource(context.Background(), f.project.ID, created.ID); !errors.Is(err, errPhase2ResourceNotFound) {
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
