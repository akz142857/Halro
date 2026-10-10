//go:build linux

package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/usage"
)

// This local opt-in experiment runs default Runtime workers and real HTTP
// handlers with a deterministic, free synthetic provider. It is not production
// acceptance. The existing fixed capacity profile and real OS sampler apply;
// submitting 64 calls does not promise admission beyond that profile's cap.
type memoryLoadCall struct {
	calls   atomic.Int64
	retry   atomic.Bool
	cancel  context.CancelFunc
	gate    <-chan struct{}
	entered chan struct{}
}
type memoryLoadCallKey struct{}
type memoryLoadAdapter struct {
	*contentCanaryAdapter // unused methods retain the existing test adapter API
	calls                 atomic.Int64
	active                atomic.Int64
	peak                  atomic.Int64
}

func (a *memoryLoadAdapter) Chat(ctx context.Context, call provider.ChatCall) (openaiapi.ChatCompletionResponse, error) {
	state, ok := ctx.Value(memoryLoadCallKey{}).(*memoryLoadCall)
	if !ok {
		return openaiapi.ChatCompletionResponse{}, errors.New("synthetic provider lost request scope")
	}
	a.calls.Add(1)
	state.calls.Add(1)
	if state.entered != nil {
		close(state.entered)
		state.entered = nil
	}
	active := a.active.Add(1)
	defer a.active.Add(-1)
	for previous := a.peak.Load(); active > previous && !a.peak.CompareAndSwap(previous, active); previous = a.peak.Load() {
	}
	if state.gate != nil {
		select {
		case <-state.gate:
		case <-ctx.Done():
			return openaiapi.ChatCompletionResponse{}, ctx.Err()
		}
	}
	if state.retry.CompareAndSwap(true, false) {
		return openaiapi.ChatCompletionResponse{}, &provider.Error{Class: provider.ErrorProvider5xx, StatusCode: 503, Retryable: true, Message: "local deterministic retry"}
	}
	if state.cancel != nil {
		state.cancel()
	}
	// Do not keep bodies or one entry per historical request in the provider.
	return openaiapi.ChatCompletionResponse{ID: "synthetic-memory-load", Object: "chat.completion", Model: call.ProviderModel,
		Choices: []openaiapi.Choice{{Index: 0, Message: &openaiapi.Message{Role: "assistant", Content: openaiapi.TextContent("local synthetic answer")}}},
		Usage:   &openaiapi.Usage{PromptTokens: 7, CompletionTokens: 5, TotalTokens: 12}}, nil
}

// Probe answers the periodic active deployment probe. The bridge always
// presents a Prober, and a failed probe suspends the deployment, so a local
// synthetic upstream that cannot answer would be taken out of routing every
// probe interval.
func (a *memoryLoadAdapter) Probe(context.Context, string) error { return nil }

type memoryLoadResponse struct {
	ID         string
	Status     int
	Calls      int64
	ErrorCode  string
	Message    string
	RetryAfter string
	Body       string
}
type memoryLoadManifest struct {
	Config      config.Config
	Credentials BootstrapResult
	Usage       usage.DiskUsageStats
	Balance     ledger.Balance
	BalanceKey  ledger.BalanceKey
	Project     domain.Project
	Suspension  domain.RouteSuspension
	Head        ledger.ReplayAnchor
}

func memoryLoadFDs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}
func memoryLoadPoint(t *testing.T) runtimeCapacityPoint {
	t.Helper()
	p := runtimeCapacityMemory(t)
	if p.CgroupMax != strconv.FormatInt(2<<30, 10) {
		t.Fatal("load requires an actual isolated 2 GiB cgroup", p.CgroupMax)
	}
	if p.CgroupPeak > 1536<<20 || p.CgroupEvents["oom"] != 0 || p.CgroupEvents["oom_kill"] != 0 || p.CgroupEvents["max"] != 0 {
		t.Fatal("fixed physical memory acceptance failed", p)
	}
	return p
}

func installMemoryLoadProvider(t *testing.T, r *Runtime, credentials BootstrapResult) *memoryLoadAdapter {
	t.Helper()
	a := &memoryLoadAdapter{contentCanaryAdapter: &contentCanaryAdapter{}}
	profile, _ := provider.BuiltinProfile(domain.ProfileOpenAIChatEmbeddings)
	bridge, err := provider.NewLegacyAdapterBridge(a, profile, domain.EvidenceForCapabilities(domain.ProviderCapabilities{Chat: true}, domain.EvidenceDeclared))
	if err != nil {
		t.Fatal(err)
	}
	registry := provider.NewRegistry()
	if err := registry.Register(provider.Target{ID: credentials.RouteID, DeploymentID: credentials.DeploymentID, ProviderID: credentials.ProviderID, PublicModel: "chat", ProviderModel: "local-synthetic", Adapter: bridge, ProfileID: profile.ID, AccessSurface: profile.AccessSurface, Capabilities: provider.Capabilities{Chat: true}}); err != nil {
		t.Fatal(err)
	}
	for _, retired := range r.providers.Replace(registry) {
		retired.Close()
	}
	return a
}

func serveMemoryLoadCall(r *Runtime, credentials BootstrapResult, state *memoryLoadCall, body, authorization string) memoryLoadResponse {
	ctx := context.WithValue(context.Background(), memoryLoadCallKey{}, state)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if state.cancel != nil {
		state.cancel = cancel
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)).WithContext(ctx)
	req.Header.Set("Authorization", authorization)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.gatewayRouter().ServeHTTP(w, req)
	result := memoryLoadResponse{ID: w.Header().Get("X-Request-ID"), Status: w.Code, Calls: state.calls.Load()}
	if w.Code >= 400 {
		var failure struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(w.Body.Bytes(), &failure) == nil {
			result.ErrorCode, result.Message = failure.Error.Code, failure.Error.Message
		}
		result.RetryAfter = w.Header().Get("Retry-After")
		result.Body = w.Body.String()
	}
	return result
}

func assertMemoryLoadTerminal(t *testing.T, r *Runtime, response memoryLoadResponse, wantCalls int64) {
	t.Helper()
	if response.Calls != wantCalls {
		t.Fatal("provider attempts differ", response, wantCalls)
	}
	request, found, err := r.selectedState.generation.Owners.Ledger.ReadRequest(context.Background(), response.ID)
	if err != nil || !found || request.Finalized == nil {
		t.Fatal("accepted HTTP returned without actual terminal fact", response, found, err)
	}
	if got := r.resourcePressure.budget.Snapshot(); got.CompletionReserved != (resourcebudget.Cost{}) || got.Classes[resourcebudget.Active] != (resourcebudget.Cost{}) || got.Classes[resourcebudget.Completion] != (resourcebudget.Cost{}) {
		t.Fatal("actual HTTP jobs retained terminal/active fees", got)
	}
}

func memoryLoadBalance(t *testing.T, r *Runtime, credentials BootstrapResult) (ledger.BalanceKey, ledger.Balance) {
	t.Helper()
	var key ledger.BalanceKey
	var expected ledger.Balance
	_, err := r.ledger.ReplayAuthenticated(context.Background(), ledger.ReplayAnchor{}, func(record ledger.Record) error {
		e := record.Event
		if e.ProjectID != credentials.ProjectID {
			return nil
		}
		if key.ProjectID == "" {
			key = ledger.BalanceKey{ProjectID: e.ProjectID, PeriodID: e.PeriodID, TimezoneVersion: e.PeriodTimezoneVersion}
		}
		if e.Kind == ledger.EventAttemptSettled {
			if e.CommittedMicrosUSD != nil {
				expected.CommittedMicrosUSD += *e.CommittedMicrosUSD
			}
			expected.InputTokens += e.ProviderInputTokens
			expected.OutputTokens += e.ProviderOutputTokens
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	actual, err := r.accountingState.reader.ReadBalance(context.Background(), key)
	if err != nil || actual.ReservedMicrosUSD != 0 || actual.CommittedMicrosUSD != expected.CommittedMicrosUSD || actual.InputTokens != expected.InputTokens || actual.OutputTokens != expected.OutputTokens {
		t.Fatal("exact selected balance differs from authenticated native settled facts", actual, expected, err)
	}
	return key, actual
}

func TestRuntimeMemoryLoadLinux(t *testing.T) {
	if os.Getenv("HALRO_RUNTIME_MEMORY_LOAD") != "1" {
		t.Skip("opt-in actual 2 GiB Linux selected Runtime load experiment")
	}
	root := os.Getenv("HALRO_EXT4_ROOT")
	if root != "/halro-fixture/data" {
		t.Fatal("load requires the isolated ext4 runner", root)
	}
	before := memoryLoadPoint(t)
	fixture := filepath.Join(root, "memory-load")
	if _, err := os.Lstat(fixture); !os.IsNotExist(err) {
		t.Fatal("load requires a new isolated fixture", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	legacy, credentials, _ := openBootstrappedRuntimeShaped(t, "local-synthetic", func(cfg *config.Config) {
		cfg.Storage.DataDir = filepath.Join(fixture, "data")
		cfg.Storage.MasterKey.File = filepath.Join(fixture, "master.key")
		cfg.Retry.MaxAttemptsPerTarget = 2
	})
	cfg, instance := legacy.config, legacy.instanceID
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	limits, pressure := selectedCapacityBudget(), selectedCapacityPressure()
	cfg.ResourceBudget, cfg.ResourcePressure = &limits, &pressure
	cfg = activateClosedRuntimeMemoryStateFixtureWithPressure(t, ctx, cfg, instance, false, &pressure)
	fdBefore := memoryLoadFDs(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r, err := Open(ctx, cfg, logger)
	if err != nil {
		t.Fatal("default selected Runtime with actual sampler", err)
	}
	defer r.Close()
	if r.selectedState == nil || r.accountingState.projection != nil {
		t.Fatal("load did not open actual selected seven roots")
	}
	if err := r.selectedState.generation.WaitStartupAudit(ctx); err != nil {
		t.Fatal(err)
	}
	a := installMemoryLoadProvider(t, r, credentials)
	var heapPeak atomic.Uint64
	stop, joined := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(joined)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				var mem runtime.MemStats
				runtime.ReadMemStats(&mem)
				for old := heapPeak.Load(); mem.HeapInuse > old && !heapPeak.CompareAndSwap(old, mem.HeapInuse); old = heapPeak.Load() {
				}
			}
		}
	}()
	var once sync.Once
	stopSampler := func() { once.Do(func() { close(stop); <-joined }) }
	defer stopSampler()
	body := `{"model":"chat","messages":[{"role":"user","content":"local load"}],"max_tokens":10}`
	auth := "Bearer " + credentials.GatewayKey
	submitted, admittedTotal, successfulHTTP, rejected, preAdmissionRejected, providerCalls := 0, 0, 0, 0, 0, int64(0)
	statusCounts := map[int]int{}
	// Create real OS pressure after one accepted provider owns its terminal
	// workspace. This fixed 1 GiB fixture buffer is visible to the actual sampler;
	// neither the sampler nor Manager pressure state is replaced or overridden.
	entered, release := make(chan struct{}), make(chan struct{})
	accepted := make(chan memoryLoadResponse, 1)
	beforePressure, _, _ := r.ledger.ChainHead()
	go func() {
		accepted <- serveMemoryLoadCall(r, credentials, &memoryLoadCall{gate: release, entered: entered}, body, auth)
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	pressureBuffer := make([]byte, 1<<30)
	for offset := 0; offset < len(pressureBuffer); offset += os.Getpagesize() {
		pressureBuffer[offset] = 1
	}
	waitRuntimePressure(t, r.resourcePressure, resourcebudget.Pressured)
	refused := serveMemoryLoadCall(r, credentials, &memoryLoadCall{}, body, auth)
	if refused.Status != 503 || refused.Calls != 0 {
		t.Fatal("real OS pressure admitted a new upstream call", refused)
	}
	close(release)
	finished := <-accepted
	if finished.Status != http.StatusOK {
		t.Fatal("already accepted terminal failed under actual OS pressure", finished)
	}
	afterPressure, _, _ := r.ledger.ChainHead()
	if afterPressure.Sequence != beforePressure.Sequence+5 {
		t.Fatal("accepted pressure lifecycle did not finish five events", beforePressure, afterPressure)
	}
	pressurePoint := memoryLoadPoint(t)
	runtime.KeepAlive(pressureBuffer)
	pressureBuffer = nil
	debug.FreeOSMemory()
	waitRuntimePressure(t, r.resourcePressure, resourcebudget.Healthy)
	assertMemoryLoadTerminal(t, r, finished, 1)
	submitted += 2
	admittedTotal++
	successfulHTTP++
	rejected++
	statusCounts[finished.Status]++
	statusCounts[refused.Status]++
	providerCalls++
	project := domain.Project{ID: "prj_memory_load_churn", Name: "bounded actual metadata churn", Enabled: true, AllowedModels: []string{strings.Repeat("m", 512<<10)}}
	project, err = r.store.PutProject(ctx, project, 0, nil)
	if err != nil {
		t.Fatal("actual authoritative wide Metadata fact", err)
	}
	// A fixed node-local row exercises COW obsolete-page reclamation without
	// growing the authoritative journal merely to fill a derived page pack.
	// This stays within the same profile, preserves native facts and uses the
	// real classified DAO; no raw Store/root write or fabricated seal is used.
	suspension := domain.RouteSuspension{ScopeKind: "credential", ScopeKey: "memory-load-local-fixture", Reason: string(provider.FailureReasonInvalidCredential), ProviderCode: strings.Repeat("code", 128<<10), ObservedAt: time.Now().UTC(), Indefinite: true}
	for cycle := 0; cycle < 2; cycle++ {
		beforeHTTP, _, _ := r.ledger.ChainHead()
		start, gate := make(chan struct{}), make(chan struct{})
		results := make(chan memoryLoadResponse, 64)
		for i := 0; i < 64; i++ {
			go func() {
				<-start
				results <- serveMemoryLoadCall(r, credentials, &memoryLoadCall{gate: gate}, body, auth)
			}()
		}
		close(start)
		// Hold the actual provider's accepted terminal ticket while other HTTP
		// handlers exercise the unchanged finite cap. No cap is enlarged.
		time.AfterFunc(250*time.Millisecond, func() { close(gate) })
		admitted := 0
		for i := 0; i < 64; i++ {
			response := <-results
			submitted++
			statusCounts[response.Status]++
			providerCalls += response.Calls
			if response.Status == http.StatusOK {
				admitted++
				admittedTotal++
				successfulHTTP++
				if response.Calls != 1 {
					t.Fatal("ordinary synthetic HTTP retried", response)
				}
				request, found, err := r.selectedState.generation.Owners.Ledger.ReadRequest(ctx, response.ID)
				if err != nil || !found || request.Finalized == nil {
					t.Fatal("accepted concurrent call lost terminal", response, err)
				}
			} else {
				rejected++
				if response.Calls != 0 {
					t.Fatal("refused HTTP reached upstream", response)
				}
				if response.Status != 503 && response.Status != 429 {
					t.Fatal("unexpected bounded HTTP refusal", response)
				}
			}
		}
		if admitted == 0 {
			t.Fatal("64 submitted HTTP calls made no actual business progress")
		}
		afterHTTP, _, _ := r.ledger.ChainHead()
		if afterHTTP.Sequence != beforeHTTP.Sequence+uint64(admitted*5) {
			t.Fatal("actual ordinary HTTP did not commit five-event business lifecycle", beforeHTTP, afterHTTP, admitted)
		}
		for _, scenario := range []string{"retry", "cancel", "large"} {
			state := &memoryLoadCall{}
			input := body
			wantCalls := int64(1)
			if scenario == "retry" {
				state.retry.Store(true)
				wantCalls = 2
			}
			if scenario == "cancel" {
				state.cancel = func() {}
			}
			if scenario == "large" {
				input = `{"model":"chat","messages":[{"role":"user","content":"` + strings.Repeat("L", 64<<10) + `"}],"max_tokens":10}`
			}
			beforeSpecial, _, _ := r.ledger.ChainHead()
			response := serveMemoryLoadCall(r, credentials, state, input, auth)
			if scenario != "cancel" && response.Status != http.StatusOK {
				t.Fatal("finite special HTTP failed", scenario, response)
			}
			assertMemoryLoadTerminal(t, r, response, wantCalls)
			afterSpecial, _, _ := r.ledger.ChainHead()
			if afterSpecial.Sequence != beforeSpecial.Sequence+uint64(2+3*wantCalls) {
				t.Fatal("special HTTP actual lifecycle differs", scenario, beforeSpecial, afterSpecial, wantCalls)
			}
			providerCalls += response.Calls
			submitted++
			admittedTotal++
			statusCounts[response.Status]++
			if response.Status == http.StatusOK {
				successfulHTTP++
			}
		}
		for _, failure := range []struct {
			body, auth string
			status     int
			code       string
		}{{body, "Bearer invalid", 401, ""}, {`{"model":`, auth, 400, ""}, {`{"model":"chat","messages":[{"role":"user","content":"` + strings.Repeat("X", int(cfg.Server.MaxRequestBytes)+1) + `"}]}`, auth, 400, "request_too_large"}} {
			response := serveMemoryLoadCall(r, credentials, &memoryLoadCall{}, failure.body, failure.auth)
			if response.Status != failure.status || response.Calls != 0 || (failure.code != "" && response.ErrorCode != failure.code) {
				t.Fatal("pre-admission failure reached synthetic upstream", response, failure.status)
			}
			submitted++
			preAdmissionRejected++
			statusCounts[response.Status]++
		}
		if err := r.usageCollector.CatchUp(ctx); err != nil {
			t.Fatal(err)
		}
		stats, err := r.usage.disk.ReadStats(ctx)
		if err != nil || stats.Metrics.ActiveRequests != 0 {
			t.Fatal("accepted terminal jobs did not close", stats, err)
		}
		if a.active.Load() != 0 {
			t.Fatal("synthetic provider jobs remain active")
		}
		pack := "pages"
		if cycle%2 != 0 {
			pack = "pages.next"
		}
		old := filepath.Join(memoryStateDirectory(cfg), "metadata-root", pack)
		for updates := 0; ; updates++ {
			info, err := os.Stat(old)
			if err != nil {
				t.Fatal(err)
			}
			if info.Size() >= limits.MetadataStoreDiskBytes/2 {
				break
			}
			if updates >= 128 {
				t.Fatal("fixed metadata churn failed to reach actual repack threshold")
			}
			suspension.CredentialRevision++
			err = r.store.PutRouteSuspension(ctx, suspension)
			if err != nil {
				t.Fatal("actual metadata churn", err)
			}
		}
		if _, err := r.ledger.Roll(); err != nil {
			t.Fatal("actual native Roll", err)
		}
		if err := r.CheckpointMemoryState(ctx); err != nil {
			t.Fatal("actual Roll/checkpoint/repack", err)
		}
		if _, err := os.Stat(old); !os.IsNotExist(err) {
			t.Fatal("actual selected repack did not retire old pack", err)
		}
		memoryLoadPoint(t)
	}
	if err := r.usageCollector.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	stats, err := r.usage.disk.ReadStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	key, balance := memoryLoadBalance(t, r, credentials)
	head, err := r.usage.disk.ReadReplayAnchor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	actualHead, hash, ok := r.ledger.ChainHead()
	if !ok || actualHead != head.Head || hash != head.Hash {
		t.Fatal("Usage/native cut mismatch", head, actualHead)
	}
	if stats.Totals.InputTokens != balance.InputTokens || stats.Totals.OutputTokens != balance.OutputTokens || stats.Totals.CostMicrosUSD != balance.CommittedMicrosUSD || stats.Metrics.ActiveRequests != 0 {
		t.Fatal("Usage/native exact balances differ", stats, balance)
	}
	if a.calls.Load() != providerCalls {
		t.Fatal("provider call inventory differs", a.calls.Load(), providerCalls)
	}
	if got := r.resourcePressure.budget.Snapshot(); got.CompletionReserved != (resourcebudget.Cost{}) || got.Classes[resourcebudget.Active] != (resourcebudget.Cost{}) || got.Classes[resourcebudget.Completion] != (resourcebudget.Cost{}) {
		t.Fatal("admitted work fees did not exit", got)
	}
	r.liveCheckpoint.mu.Lock()
	activeJobs := r.liveCheckpoint.active
	r.liveCheckpoint.mu.Unlock()
	if activeJobs != 0 {
		t.Fatal("HTTP tickets remain live", activeJobs)
	}
	manifest := memoryLoadManifest{Config: cfg, Credentials: credentials, Usage: stats, BalanceKey: key, Balance: balance, Project: project, Suspension: suspension, Head: head}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(fixture, "load-manifest.json")
	if err := os.WriteFile(manifestPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	live := memoryLoadPoint(t)
	if err := r.Close(); err != nil {
		t.Fatal("join actual Runtime owners", err)
	}
	if after := memoryLoadFDs(t); after > fdBefore {
		t.Fatal("actual Runtime FD leak", fdBefore, after)
	}
	stopSampler()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRuntimeMemoryLoadFreshProcessHelper$", "-test.count=1", "-test.v")
	child.Env = append(os.Environ(), "HALRO_RUNTIME_MEMORY_LOAD_MANIFEST="+manifestPath)
	output, err := child.CombinedOutput()
	if err != nil {
		t.Fatalf("fresh process selected reopen failed: %v %s", err, output)
	}
	after := memoryLoadPoint(t)
	report := map[string]any{"scope": "local Linux functional load; not production", "concurrency": 64, "cycles": 2, "submitted": submitted, "admitted_terminal": admittedTotal, "http_success": successfulHTTP, "capacity_refused": rejected, "pre_admission_refused": preAdmissionRejected, "status_counts": statusCounts, "provider_calls": providerCalls, "provider_peak": a.peak.Load(), "before": before, "actual_pressure": pressurePoint, "fixture_pressure_buffer_bytes": 1 << 30, "live_after_gc": live, "after_close_and_fresh_process": after, "sampled_heap_inuse_peak_bytes": heapPeak.Load(), "usage": stats, "balance": balance, "native_head": head, "metadata_revision": project.Revision, "ready": true, "fd_before": fdBefore, "fd_after": memoryLoadFDs(t), "fresh_process": string(output)}
	raw, err = json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "memory-load-report.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Log("RUNTIME_MEMORY_LOAD " + string(raw))
}

func TestRuntimeMemoryLoadFreshProcessHelper(t *testing.T) {
	path := os.Getenv("HALRO_RUNTIME_MEMORY_LOAD_MANIFEST")
	if path == "" {
		t.Skip("isolated fresh process load proof helper")
	}
	if path != "/halro-fixture/data/memory-load/load-manifest.json" {
		t.Fatal("foreign load manifest")
	}
	memoryLoadPoint(t)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var manifest memoryLoadManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	fdBefore := memoryLoadFDs(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	r, err := Open(ctx, manifest.Config, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.selectedState == nil || r.accountingState.projection != nil {
		t.Fatal("fresh process selected a legacy owner")
	}
	if err := r.selectedState.generation.WaitStartupAudit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.usageCollector.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	stats, err := r.usage.disk.ReadStats(ctx)
	if err != nil || !reflect.DeepEqual(stats.Totals, manifest.Usage.Totals) || stats.Metrics != manifest.Usage.Metrics || stats.Metrics.ActiveRequests != 0 {
		t.Fatal("fresh process exact Usage differs", stats, err)
	}
	anchor, err := r.usage.disk.ReadReplayAnchor(ctx)
	if err != nil || anchor != manifest.Head {
		t.Fatal("fresh process cut differs", anchor, manifest.Head, err)
	}
	balance, err := r.accountingState.reader.ReadBalance(ctx, manifest.BalanceKey)
	if err != nil || !reflect.DeepEqual(balance, manifest.Balance) {
		t.Fatal("fresh process exact balance differs", balance, err)
	}
	project, err := r.store.GetProject(ctx, manifest.Project.ID)
	if err != nil || !reflect.DeepEqual(project, manifest.Project) {
		t.Fatal("fresh process Metadata differs", err)
	}
	suspensions, err := r.store.ListRouteSuspensions(ctx)
	if err != nil || len(suspensions) != 1 || !reflect.DeepEqual(suspensions[0], manifest.Suspension) {
		t.Fatal("fresh process node-local wide Metadata differs", len(suspensions), err)
	}
	response := httptest.NewRecorder()
	r.gatewayRouter().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if response.Code != http.StatusOK {
		t.Fatal("fresh process readiness refused", response.Code, response.Body.String())
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if after := memoryLoadFDs(t); after > fdBefore {
		t.Fatal("fresh process actual FD leak", fdBefore, after)
	}
	t.Logf("FRESH_SELECTED_MEMORY_LOAD %+v", memoryLoadPoint(t))
}
