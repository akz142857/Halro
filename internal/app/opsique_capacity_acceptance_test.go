package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/provider"
)

const opsiqueFixtureReply = "opsique-batch-b-fallback-fixture-reply"
const opsiqueFixtureSecret = "loopback-fixture-upstream-secret"

type opsiqueAttemptSample struct {
	started time.Time
	elapsed time.Duration
	reused  bool
}

type opsiqueAttemptTimings struct {
	mu       sync.Mutex
	requests map[string][]opsiqueAttemptSample
}

func (s *opsiqueAttemptTimings) take(requestID string) []opsiqueAttemptSample {
	s.mu.Lock()
	defer s.mu.Unlock()
	attempts := s.requests[requestID]
	delete(s.requests, requestID)
	return attempts
}

// Keep the real provider profile, model, credentials and operation path. Only
// the test transport redirects the request to an upstream on loopback.
func registerOpsiqueUpstream(t *testing.T, rt *Runtime, registry *provider.Registry, target provider.Target, upstream *httptest.Server, timings *opsiqueAttemptTimings) {
	t.Helper()
	instance, err := rt.store.GetProvider(context.Background(), target.ProviderID)
	if err != nil {
		t.Fatal(err)
	}
	bindings := instance.EffectiveProfileBindings()
	index := slices.IndexFunc(bindings, func(binding domain.ProviderProfileBinding) bool { return binding.ID == target.BindingID })
	if index == -1 {
		t.Fatalf("missing provider binding %s", target.BindingID)
	}
	endpoint, err := url.Parse(instance.BaseURL)
	if err != nil {
		t.Fatal(err)
	}
	loopback, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	transport := upstream.Client().Transport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = 60
	transport.ResponseHeaderTimeout = time.Duration(rt.config.Gateway.AttemptResponseHeaderTimeout)
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: catalogRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		start := time.Now()
		reused := false
		ctx := httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
			GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused },
		})
		local := req.Clone(ctx)
		local.URL.Scheme, local.URL.Host, local.Host = loopback.Scheme, loopback.Host, loopback.Host
		resp, err := transport.RoundTrip(local)
		if err == nil {
			// Read the full fixture response so the sample includes headers and
			// body. All fixture responses are short unary JSON.
			original := resp.Body
			var body []byte
			body, err = io.ReadAll(original)
			_ = original.Close()
			resp.Body = io.NopCloser(bytes.NewReader(body))
		}
		if err == nil {
			id := req.Header.Get("X-Request-ID")
			timings.mu.Lock()
			timings.requests[id] = append(timings.requests[id], opsiqueAttemptSample{started: start, elapsed: time.Since(start), reused: reused})
			timings.mu.Unlock()
		}
		if err != nil {
			return nil, err
		}
		return resp, nil
	})}
	adapter, err := newProviderBindingAdapterWithClient(instance, bindings[index], endpoint, []byte(opsiqueFixtureSecret), client)
	if err != nil {
		t.Fatal(err)
	}
	target.Adapter, err = provider.NewLegacyAdapterBridge(adapter, target.Adapter.(provider.ProfiledAdapter).Profile(), target.CapabilityEvidence)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(target); err != nil {
		t.Fatal(err)
	}
}

func createOpsiqueMantleRoute(t *testing.T, rt *Runtime) domain.Route {
	t.Helper()
	const endpoint = "https://bedrock-mantle.us-east-2.api.aws"
	cookie, csrf := loginAdminForTest(t, rt)
	create := func(relative string, input map[string]any, output any) {
		t.Helper()
		response := performAdminMutation(t, rt, cookie, csrf, http.MethodPost, "/admin/api/v1/"+relative, "", input)
		if response.Code != http.StatusCreated || json.Unmarshal(response.Body.Bytes(), output) != nil {
			t.Fatalf("create %s: status=%d body=%s", relative, response.Code, response.Body.String())
		}
	}
	var credential credentialView
	create("credentials", map[string]any{
		"name": "Fixture Mantle", "type": domain.ProviderBedrock, "base_url": endpoint,
		"access_surface": domain.SurfaceBedrockMantle, "scheme": domain.CredentialBedrockAPIKey, "secret": opsiqueFixtureSecret,
	}, &credential)
	var instance domain.ProviderInstance
	create("providers", map[string]any{
		"name": "Fixture Mantle", "type": domain.ProviderBedrock, "base_url": endpoint,
		"access_surface": domain.SurfaceBedrockMantle, "profile_id": domain.ProfileBedrockMantleOpenAIChat,
		"credential_id": credential.ID, "enabled": true,
	}, &instance)
	var deployment domain.Deployment
	create("deployments", map[string]any{
		"name": "Fixture Mantle Chat", "provider_id": instance.ID,
		"provider_model": mantleOpenAIRouteModel, "target_kind": "model_id", "region": "us-east-2",
		"profile_id": domain.ProfileBedrockMantleOpenAIChat, "mode": "operator_declared", "enabled": false,
		"capabilities": map[string]bool{"chat": true, "tools": true, "json_object": true, "structured_outputs": true},
	}, &deployment)
	createEffectiveMeteredPriceForTest(t, rt, deployment.ID)
	// Skip the real deployment probe for the loopback-only fixture.
	enableStoredDeploymentForTest(t, rt, deployment.ID)
	var route domain.Route
	create("routes", map[string]any{
		"public_model": "chat", "deployment_id": deployment.ID, "priority": 1,
		"strategy": "ordered", "enabled": true,
	}, &route)
	return route
}

func opsiqueCompletion(w http.ResponseWriter, model string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": "chatcmpl-batch-b", "object": "chat.completion", "created": 1, "model": model,
		"choices": []any{map[string]any{
			"index": 0, "finish_reason": "stop", "message": map[string]string{
				"role": "assistant", "content": fmt.Sprintf(`{"text":%q,"ui":null}`, opsiqueFixtureReply),
			},
		}},
		"usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
	})
}

func opsiqueRequest(t *testing.T, endpoint, key, id, body string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, endpoint+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", id)
	return req
}

func assertOpsiqueSettled(t *testing.T, rt *Runtime, wantRequests, wantAttempts int, check func(ledger.Event)) {
	t.Helper()
	var finalized, attempts int
	_, err := rt.ledger.Replay(ledger.Watermark{}, func(record ledger.Record) error {
		switch record.Event.Kind {
		case ledger.EventRequestFinalized:
			finalized++
		case ledger.EventAttemptSettled:
			attempts++
			check(record.Event)
		}
		return nil
	})
	if err != nil || finalized != wantRequests || attempts != wantAttempts {
		t.Fatalf("finalized=%d, settled attempts=%d, err=%v; want %d/%d", finalized, attempts, err, wantRequests, wantAttempts)
	}
	rows, err := rt.store.ListProviderResources(context.Background())
	if err != nil || len(rows) != wantRequests {
		t.Fatalf("inference claims=%d, err=%v; want %d", len(rows), err, wantRequests)
	}
	for _, row := range rows {
		if row.CreationStatus != "completed" {
			t.Fatalf("inference claim status=%s", row.CreationStatus)
		}
	}
	for id, active := range rt.gatewayService.ActiveProviderRequests() {
		if active != 0 {
			t.Errorf("provider %s retains %d active requests", id, active)
		}
	}
	if leases, reservations := len(rt.state.PendingLeases()), rt.state.PendingReservations(); leases != 0 || reservations != 0 {
		t.Fatalf("pending leases=%d reservations=%d", leases, reservations)
	}
}

func TestOpsiqueMantleFallback(t *testing.T) {
	readyFile := os.Getenv("HALRO_MANTLE_FIXTURE_READY_FILE")
	rt, bootstrap, _ := openBootstrappedRuntimeShaped(t, "gpt-test", func(cfg *config.Config) {
		cfg.Gateway.MaxTotalAttempts = 4
		cfg.Retry.MaxAttemptsPerTarget = 1
	})
	backup := createOpsiqueMantleRoute(t, rt)
	candidates := rt.providers.ResolveCandidatesFor("chat", provider.OperationChat)
	if len(candidates) != 2 || candidates[0].ID != bootstrap.RouteID || candidates[1].ID != backup.ID {
		t.Fatalf("ordered candidates are not OpenAI then Mantle: got %d", len(candidates))
	}
	var primaryCalls, mantleCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			t.Error("unexpected primary operation")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "300")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"code":"rate_limit_exceeded","type":"rate_limit_error","message":"fixture capacity exhausted"}}`)
	}))
	defer primary.Close()
	mantle := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mantleCalls.Add(1)
		var payload struct {
			Model          string            `json:"model"`
			Tools          []json.RawMessage `json:"tools"`
			ResponseFormat json.RawMessage   `json:"response_format"`
		}
		if r.Method != http.MethodPost || r.URL.Path != "/openai/v1/chat/completions" ||
			r.Header.Get("Authorization") != "Bearer "+opsiqueFixtureSecret ||
			json.NewDecoder(r.Body).Decode(&payload) != nil || payload.Model != mantleOpenAIRouteModel {
			t.Error("unexpected Mantle operation, authorization or model")
			http.Error(w, "invalid fixture request", http.StatusBadRequest)
			return
		}
		if readyFile == "" && (len(payload.Tools) != 1 || !bytes.Contains(payload.ResponseFormat, []byte(`"json_schema"`))) {
			t.Error("Mantle body did not carry the function tool and JSON Schema response format")
		}
		opsiqueCompletion(w, mantleOpenAIRouteModel)
	}))
	defer mantle.Close()
	timings := &opsiqueAttemptTimings{requests: make(map[string][]opsiqueAttemptSample)}
	registry := provider.NewRegistry()
	registerOpsiqueUpstream(t, rt, registry, candidates[0], primary, timings)
	registerOpsiqueUpstream(t, rt, registry, candidates[1], mantle, timings)
	rt.providers.Replace(registry)

	finished := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rt.gatewayHandler().ServeHTTP(w, r)
		select {
		case finished <- struct{}{}:
		default:
		}
	}))
	defer server.Close()
	var clientElapsed time.Duration
	var requestID string
	if readyFile != "" {
		data, err := json.Marshal(map[string]string{
			"base_url": server.URL + "/v1", "name": "chat", "api_key": bootstrap.GatewayKey,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(readyFile+".new", data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(readyFile+".new", readyFile); err != nil {
			t.Fatal(err)
		}
		t.Log("Halro ready for Platform Temporal workflow")
		select {
		case <-finished:
		case <-time.After(90 * time.Second):
			t.Fatal("Platform did not complete its gateway call")
		}
	} else {
		const body = `{"model":"chat","messages":[{"role":"user","content":"Find the order status and return JSON"}],"tools":[{"type":"function","function":{"name":"lookup_order","parameters":{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}}}],"response_format":{"type":"json_schema","json_schema":{"name":"reply","strict":true,"schema":{"type":"object","properties":{"text":{"type":"string"},"ui":{"type":"null"}},"required":["text","ui"],"additionalProperties":false}}}}`
		begin := time.Now()
		resp, err := server.Client().Do(opsiqueRequest(t, server.URL, bootstrap.GatewayKey, "fixture-failover", body))
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		clientElapsed = time.Since(begin)
		if err != nil || resp.StatusCode != http.StatusOK || !bytes.Contains(data, []byte(opsiqueFixtureReply)) {
			t.Fatalf("status=%d resp=%s err=%v", resp.StatusCode, data, err)
		}
		requestID = resp.Header.Get("X-Request-ID")
	}
	if primaryCalls.Load() != 1 || mantleCalls.Load() != 1 {
		t.Fatalf("primary=%d Mantle=%d; want 1 each", primaryCalls.Load(), mantleCalls.Load())
	}
	var settled []ledger.Event
	assertOpsiqueSettled(t, rt, 1, 2, func(event ledger.Event) {
		settled = append(settled, event)
	})
	first, second := settled[0], settled[1]
	if requestID == "" && readyFile != "" {
		requestID = first.RequestID
	}
	if requestID == "" || first.RequestID != requestID || second.RequestID != requestID ||
		first.RouteID != bootstrap.RouteID || first.CommittedMicrosUSD == nil || *first.CommittedMicrosUSD != 0 ||
		second.RouteID != backup.ID || second.ProviderID != candidates[1].ProviderID ||
		second.ProfileID != domain.ProfileBedrockMantleOpenAIChat || second.PriceSnapshot == nil ||
		second.CommittedMicrosUSD == nil || *second.CommittedMicrosUSD != 20 {
		t.Fatal("expected one request with a zero-cost primary rejection and a 20µUSD fixture Mantle settlement")
	}
	attempts := timings.take(requestID)
	if len(attempts) != 2 {
		t.Fatalf("request correlation reported %d upstream attempts", len(attempts))
	}
	t.Logf("request=%s primary=%v Mantle=%v between_attempts=%v primary_cost=0 Mantle_cost=20µUSD",
		requestID, attempts[0].elapsed, attempts[1].elapsed, attempts[1].started.Sub(attempts[0].started.Add(attempts[0].elapsed)))
	if clientElapsed > 0 {
		t.Logf("client end-to-end=%s", clientElapsed)
	}
}

func TestOpsiqueSixtyInflight(t *testing.T) {
	if os.Getenv("HALRO_RUN_OPSIQUE_CAPACITY") != "1" {
		t.Skip("set HALRO_RUN_OPSIQUE_CAPACITY=1 for the minute-scale load fixture")
	}
	const concurrency = 60
	const headerTimeout = 75 * time.Second
	rt, bootstrap, _ := openBootstrappedRuntimeShaped(t, "gpt-test", func(cfg *config.Config) {
		cfg.Gateway.AttemptResponseHeaderTimeout = config.Duration(headerTimeout)
	})
	project, err := rt.store.GetProject(context.Background(), bootstrap.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	project.RPM, project.TPM, project.MaxConcurrency = 300, 2_000_000, concurrency
	if _, err := rt.store.PutProject(context.Background(), project, project.Revision, nil); err != nil {
		t.Fatal(err)
	}
	if err := rt.auth.Refresh(context.Background(), rt.store); err != nil {
		t.Fatal(err)
	}
	target, ok := rt.providers.Resolve("chat")
	if !ok {
		t.Fatal("primary route unavailable")
	}
	t.Logf("fixture project rpm=%d tpm=%d concurrency=%d ingress source rpm=%d; provider concurrency=%d deployment concurrency=%d token guard=%q daily budget=%d; header timeout=%s durability=%s",
		project.RPM, project.TPM, project.MaxConcurrency, rt.config.Gateway.SourceRateLimit.SourceRequestsPerMinute(), target.MaxConcurrency, target.DeploymentConcurrency,
		project.TokenGuardPolicyID, project.DailyBudgetMicrosUSD, headerTimeout, rt.config.Usage.Durability)

	var atLimit atomic.Int32
	var active, peak atomic.Int32
	allArrived := make(chan struct{})
	firstRelease := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Messages []struct{ Content string } `json:"messages"`
		}
		if r.URL.Path != "/v1/chat/completions" || json.NewDecoder(r.Body).Decode(&payload) != nil || len(payload.Messages) == 0 {
			http.Error(w, "invalid fixture request", http.StatusBadRequest)
			return
		}
		n := active.Add(1)
		defer active.Add(-1)
		for previous := peak.Load(); n > previous && !peak.CompareAndSwap(previous, n); previous = peak.Load() {
		}
		delay := 80 * time.Millisecond
		phase := payload.Messages[len(payload.Messages)-1].Content
		if phase == "minute" || phase == "timeout-edge" {
			if atLimit.Add(1) == concurrency {
				close(allArrived)
			}
			select {
			case <-firstRelease:
			case <-r.Context().Done():
				return
			}
			if phase == "timeout-edge" {
				delay = 8 * time.Second
			} else {
				delay = 0
			}
		}
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
			opsiqueCompletion(w, "gpt-test")
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()
	timings := &opsiqueAttemptTimings{requests: make(map[string][]opsiqueAttemptSample)}
	registry := provider.NewRegistry()
	registerOpsiqueUpstream(t, rt, registry, target, upstream, timings)
	rt.providers.Replace(registry)
	server := httptest.NewServer(rt.gatewayHandler())
	defer server.Close()
	client := server.Client()
	client.Timeout = 90 * time.Second

	type result struct {
		phase  string
		begin  time.Time
		end    time.Time
		id     string
		status int
		body   string
		err    error
	}
	total, reused := 0, 0
	requestIDs := make(map[string]bool)
	type waveSamples struct {
		added, before, after []time.Duration
	}
	run := func(group string, size, waves int) {
		t.Helper()
		measurements := make(map[string]waveSamples)
		groupPeak := int32(0)
		for wave := range waves {
			peak.Store(0)
			responses := make(chan result, size)
			startAll := make(chan struct{})
			for i := range size {
				phase := group
				if group == "minute" && i >= size-4 {
					phase = "timeout-edge"
				}
				history := "Customer order, shipment state, prior tool results and next action."
				if i%10 == 0 {
					history = strings.Repeat(history+" ", 750)
				}
				body := fmt.Sprintf(`{"model":"chat","max_tokens":512,"messages":[{"role":"system","content":"Summarize the order and return JSON."},{"role":"user","content":%q},{"role":"assistant","content":"I found the order."},{"role":"user","content":%q}]}`, history, phase)
				req := opsiqueRequest(t, server.URL, bootstrap.GatewayKey, fmt.Sprintf("fixture-%s-%d-%d", group, wave, i), body)
				go func() {
					<-startAll
					sample := result{phase: phase, begin: time.Now()}
					resp, err := client.Do(req)
					sample.err = err
					if err == nil {
						data, err := io.ReadAll(resp.Body)
						_ = resp.Body.Close()
						sample.id, sample.status, sample.err, sample.body = resp.Header.Get("X-Request-ID"), resp.StatusCode, err, string(data)
					}
					sample.end = time.Now()
					responses <- sample
				}()
			}
			close(startAll)
			if group == "minute" {
				arrivalTimer := time.NewTimer(10 * time.Second)
				select {
				case <-allArrived:
					arrivalTimer.Stop()
					time.Sleep(65 * time.Second)
					t.Logf("after 65s: upstream in-flight=%d provider in-flight=%d", active.Load(), rt.gatewayService.ActiveProviderRequests()[target.ProviderID])
					if active.Load() != concurrency || rt.gatewayService.ActiveProviderRequests()[target.ProviderID] != concurrency {
						t.Error("60 requests did not remain in flight")
					}
				case <-arrivalTimer.C:
					t.Errorf("only %d of %d upstream requests reached the minute barrier", atLimit.Load(), concurrency)
				}
				close(firstRelease)
			}
			for range size {
				sample := <-responses
				if sample.err != nil || sample.status != http.StatusOK || sample.id == "" ||
					!strings.Contains(sample.body, opsiqueFixtureReply) {
					t.Fatalf("%s: status=%d body=%s err=%v", sample.phase, sample.status, sample.body, sample.err)
				}
				attempts := timings.take(sample.id)
				if len(attempts) != 1 || requestIDs[sample.id] {
					t.Fatalf("request %s has %d upstream attempts or duplicate correlation", sample.id, len(attempts))
				}
				requestIDs[sample.id] = true
				total++
				if attempts[0].reused {
					reused++
				}
				values := measurements[sample.phase]
				attempt := attempts[0]
				values.before = append(values.before, attempt.started.Sub(sample.begin))
				values.after = append(values.after, sample.end.Sub(attempt.started.Add(attempt.elapsed)))
				values.added = append(values.added, sample.end.Sub(sample.begin)-attempt.elapsed)
				measurements[sample.phase] = values
			}
			groupPeak = max(groupPeak, peak.Load())
		}
		for phase, values := range measurements {
			slices.Sort(values.added)
			slices.Sort(values.before)
			slices.Sort(values.after)
			percentile := func(samples []time.Duration, p int) time.Duration { return samples[(len(samples)*p+99)/100-1] }
			p95 := percentile(values.added, 95)
			t.Logf("%s: success=%d peak=%d added p50=%s p95=%s p99=%s; before-upstream p95=%s after-upstream p95=%s",
				phase, len(values.added), groupPeak, percentile(values.added, 50), p95, percentile(values.added, 99),
				percentile(values.before, 95), percentile(values.after, 95))
			if p95 >= 200*time.Millisecond {
				t.Errorf("%s p95 %s exceeds the 200ms gate", phase, p95)
			}
		}
		writePath := rt.writePathSummary()
		t.Logf("%s cumulative write metrics: WAL sync=%.3fms/batch records=%.1f/batch accounting lock wait=%.3fms/acquisition metadata batch=%.1f",
			group, 1000*writePath.WALSyncSeconds, writePath.WALBatchSize,
			1000*writePath.ProjectLockWaitSeconds, writePath.MetadataBatchSize)
	}
	run("session-turn", 20, 3)
	run("fast", 60, 3)
	run("minute", concurrency, 1)
	rejected := rt.gatewayService.RejectionMetrics()
	if rejected.RPM != 0 || rejected.TPM != 0 || rejected.ProjectConcurrency != 0 {
		t.Fatalf("gateway project rejections: %+v", rejected)
	}
	assertOpsiqueSettled(t, rt, total, total, func(event ledger.Event) {
		if !requestIDs[event.RequestID] || event.CommittedMicrosUSD == nil || event.RouteID != target.ID {
			t.Errorf("unexpected attempt settlement for %s", event.RequestID)
		}
	})
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	t.Logf("settled=%d project_429=0 pending=0 upstream_connections_reused=%d/%d heap_at_end=%d CPU_threads=%d WAL_bytes=%d",
		total, reused, total, memory.HeapAlloc, runtime.GOMAXPROCS(0), rt.ledger.ActiveBytes())
}
