package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/provider/openai"
)

const rateLimitFixtureReply = "opsique-halro-rate-limit-fixture-reply"

func newRateLimitRuntime(t *testing.T, hints ...string) (*Runtime, BootstrapResult, []*atomic.Int32) {
	t.Helper()
	runtime, bootstrap, _ := openBootstrappedRuntimeShaped(t, "gpt-test", func(cfg *config.Config) {
		// Still switch promptly when per-target retries are configured.
		cfg.Retry.MaxAttemptsPerTarget = 2
	})
	target, ok := runtime.providers.Resolve("chat")
	if !ok {
		t.Fatal("bootstrap did not register a chat target")
	}
	registry := provider.NewRegistry()
	counts := make([]*atomic.Int32, 0, len(hints))
	for index, hint := range hints {
		candidate := target
		candidate.Priority = index
		if index > 0 {
			candidate.DeploymentID = addSecondRoute(t, runtime, bootstrap, "chat")
			createEffectiveMeteredPriceForTest(t, runtime, candidate.DeploymentID)
			candidate.ID = "rte-second"
			candidate.CredentialID = "fixture-second-credential"
		}
		count := &atomic.Int32{}
		counts = append(counts, count)
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if count.Add(1) == 1 {
				if hint != "" {
					w.Header().Set("Retry-After", hint)
				}
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = fmt.Fprint(w, `{"error":{"code":"rate_limit_exceeded","type":"rate_limit_error","message":"fixture capacity exhausted"}}`)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "chatcmpl-fixture", "object": "chat.completion", "created": 1,
				"model": "gpt-test", "choices": []any{map[string]any{
					"index": 0, "finish_reason": "stop", "message": map[string]string{
						"role": "assistant", "content": fmt.Sprintf(`{"text":%q,"ui":null}`, rateLimitFixtureReply),
					},
				}},
				"usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
			})
		}))
		t.Cleanup(upstream.Close)
		endpoint, err := url.Parse(upstream.URL)
		if err != nil {
			t.Fatal(err)
		}
		adapter, err := openai.NewWithOptions(openai.Options{
			Endpoint: endpoint, APIKey: []byte("fixture-upstream-key"), Client: upstream.Client(),
			ProviderType: string(domain.ProviderOpenAI), ProfileID: candidate.ProfileID,
			Capabilities: candidate.Capabilities,
		})
		if err != nil {
			t.Fatal(err)
		}
		candidate.Adapter, err = provider.NewLegacyAdapterBridge(adapter, target.Adapter.(provider.ProfiledAdapter).Profile(), candidate.CapabilityEvidence)
		if err != nil {
			t.Fatal(err)
		}
		if err := registry.Register(candidate); err != nil {
			t.Fatal(err)
		}
	}
	runtime.providers.Replace(registry)
	return runtime, bootstrap, counts
}

func TestChatRateLimitRetryHints(t *testing.T) {
	for _, tc := range []struct {
		name  string
		hints []string
		want  string
	}{
		{"single", []string{"7"}, "7"},
		{"7 then 3", []string{"7", "3"}, "3"},
		{"3 then 7", []string{"3", "7"}, "3"},
		{"one hint", []string{"7", ""}, "7"},
		{"no hint", []string{"", ""}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime, bootstrap, calls := newRateLimitRuntime(t, tc.hints...)
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"chat","messages":[{"role":"user","content":"fixture"}]}`))
			request.Header.Set("Authorization", "Bearer "+bootstrap.GatewayKey)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Idempotency-Key", "limited-key")
			response := httptest.NewRecorder()
			runtime.gatewayHandler().ServeHTTP(response, request)
			if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") != tc.want ||
				!strings.Contains(response.Body.String(), `"type":"rate_limit_error"`) ||
				!strings.Contains(response.Body.String(), `"code":"provider_rate_limit"`) {
				t.Fatalf("status=%d retry-after=%q body=%s", response.Code, response.Header().Get("Retry-After"), response.Body)
			}
			for index, count := range calls {
				if got := count.Load(); got != 1 {
					t.Fatalf("target %d attempts=%d, want 1", index, got)
				}
			}
		})
	}
}

// This opt-in fixture lets Platform's actual Temporal worker exercise a running
// Halro on loopback. The ready file holds a temporary, local Gateway key.
func TestServePlatformRateLimitRecoveryFixture(t *testing.T) {
	readyFile := os.Getenv("HALRO_RATE_LIMIT_FIXTURE_READY_FILE")
	if readyFile == "" {
		t.Skip("no Platform fixture ready file requested")
	}
	runtime, bootstrap, calls := newRateLimitRuntime(t, "7", "3")
	done := make(chan struct{}, 1)
	handler := runtime.gatewayHandler()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
		if calls[1].Load() == 2 {
			select {
			case done <- struct{}{}:
			default:
			}
		}
	}))
	t.Cleanup(server.Close)
	ready, err := json.Marshal(map[string]string{
		"base_url": server.URL + "/v1", "name": "chat", "api_key": bootstrap.GatewayKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(readyFile, ready, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(90 * time.Second):
		t.Fatal("Platform did not complete the recovery request")
	}
	if calls[0].Load() != 1 || calls[1].Load() != 2 {
		t.Fatalf("upstream calls primary=%d secondary=%d, want 1/2", calls[0].Load(), calls[1].Load())
	}
	rows, err := runtime.store.ListProviderResources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].CreationStatus != "completed" {
		t.Fatalf("expected one completed inference claim, got %d", len(rows))
	}
	var settled, refused, paid int
	_, err = runtime.ledger.Replay(ledger.Watermark{}, func(record ledger.Record) error {
		event := record.Event
		if event.Kind == ledger.EventAttemptSettled {
			settled++
			if event.CommittedMicrosUSD != nil && *event.CommittedMicrosUSD == 0 {
				refused++
			}
			if event.CommittedMicrosUSD != nil && *event.CommittedMicrosUSD > 0 && event.RouteID == "rte-second" {
				paid++
			}
		}
		return nil
	})
	if err != nil || settled != 3 || refused != 2 || paid != 1 {
		t.Fatalf("ledger settled=%d refused=%d paid=%d err=%v", settled, refused, paid, err)
	}
}
