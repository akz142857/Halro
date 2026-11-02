package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/openaiapi"
)

// Full native runtime acceptance: temporary encrypted store -> managed CONNECT
// egress -> production SafeTransport/net/http/TLS -> Ark -> canonical mapping,
// redaction, gateway HTTP/SSE and ledger. No curl or response-buffering adapter.
// The Darwin-only network helper pins the test's sockets to the existing NIC;
// it never edits the machine's DNS, routes, VPN or production configuration.
func TestRealVolcengineRuntime(t *testing.T) {
	if os.Getenv("HALRO_REAL_PROVIDER_SMOKE") != "1" || os.Getenv("HALRO_SMOKE_PROFILE") != "volcengine_runtime" {
		t.Skip("explicit billable native Ark runtime acceptance")
	}
	key, model, ifaceName := os.Getenv("HALRO_SMOKE_API_KEY"), os.Getenv("HALRO_SMOKE_MODEL"), os.Getenv("HALRO_SMOKE_NETWORK_INTERFACE")
	if key == "" || model == "" || ifaceName == "" {
		t.Fatal("explicit key, summary-capable model and network interface are required")
	}
	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		t.Fatal("network interface unavailable")
	}
	dialer := &net.Dialer{Timeout: 8 * time.Second, Control: func(network, address string, raw syscall.RawConn) error {
		var socketErr error
		if err := raw.Control(func(fd uintptr) {
			socketErr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_BOUND_IF, iface.Index)
		}); err != nil {
			return err
		}
		return socketErr
	}}
	dns := os.Getenv("HALRO_SMOKE_DNS_SERVER")
	if dns == "" {
		dns = "223.5.5.5:53"
	}
	originalResolver := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		return dialer.DialContext(ctx, "udp4", dns)
	}}
	defer func() { net.DefaultResolver = originalResolver }()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", "ark.cn-beijing.volces.com")
	if err != nil || len(addresses) == 0 {
		t.Fatal("native public DNS resolution failed")
	}
	allowed := map[string]bool{}
	for _, ip := range addresses {
		if !ip.IsGlobalUnicast() || ip.IsPrivate() || strings.HasPrefix(ip.String(), "198.18.") {
			t.Fatal("public DNS returned an unsafe or Fake-IP address")
		}
		allowed[ip.String()] = true
	}
	var allowedMu sync.Mutex
	allowTarget := func(ctx context.Context, host string) bool {
		// Public DNS may rotate its A records between the initial lookup and
		// SafeTransport's own lookup. Extend only from fresh DNS answers for
		// the same Ark hostname; never broaden this into a general TCP proxy.
		for attempt := 0; attempt < 4; attempt++ {
			allowedMu.Lock()
			found := allowed[host]
			allowedMu.Unlock()
			if found {
				return true
			}
			if attempt == 3 {
				break
			}
			fresh, err := net.DefaultResolver.LookupNetIP(ctx, "ip", "ark.cn-beijing.volces.com")
			if err != nil {
				return false
			}
			allowedMu.Lock()
			for _, ip := range fresh {
				if ip.IsGlobalUnicast() && !ip.IsPrivate() && !strings.HasPrefix(ip.String(), "198.18.") && !strings.HasPrefix(ip.String(), "198.19.") {
					allowed[ip.String()] = true
				}
			}
			allowedMu.Unlock()
		}
		return false
	}
	var connectionsMu sync.Mutex
	connections := map[net.Conn]bool{}
	var handlers sync.WaitGroup
	proxyFailures := make(chan string, 16)
	noteProxyFailure := func(reason string) {
		select {
		case proxyFailures <- reason:
		default:
		}
	}
	proxy := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		host, port, err := net.SplitHostPort(request.Host)
		if err != nil || request.Method != "CONNECT" || port != "443" || !allowTarget(request.Context(), host) {
			noteProxyFailure("refused target " + request.Host)
			http.Error(writer, "refused", 403)
			return
		}
		upstream, err := dialer.DialContext(request.Context(), "tcp4", request.Host)
		if err != nil {
			noteProxyFailure("connect " + request.Host + ": " + err.Error())
			http.Error(writer, "connect failed", 502)
			return
		}
		downstream, buffer, err := writer.(http.Hijacker).Hijack()
		if err != nil {
			upstream.Close()
			return
		}
		handlers.Add(1)
		defer handlers.Done()
		connectionsMu.Lock()
		connections[upstream] = true
		connections[downstream] = true
		connectionsMu.Unlock()
		defer func() {
			upstream.Close()
			downstream.Close()
			connectionsMu.Lock()
			delete(connections, upstream)
			delete(connections, downstream)
			connectionsMu.Unlock()
		}()
		if _, err := buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			return
		}
		if buffer.Flush() != nil {
			return
		}
		done := make(chan struct{})
		go func() { io.Copy(upstream, buffer); upstream.Close(); close(done) }()
		io.Copy(downstream, upstream)
		downstream.Close()
		<-done
	}))
	defer func() {
		proxy.Close()
		connectionsMu.Lock()
		for conn := range connections {
			conn.Close()
		}
		connectionsMu.Unlock()
		handlers.Wait()
	}()
	cfg := testConfig(t)
	cfg.Gateway.MaxTotalAttempts = 1
	if err := Initialize(cfg); err != nil {
		t.Fatal("initialize runtime")
	}
	if err := BootstrapAdmin(ctx, cfg, "admin", []byte(stepUpTestPassword)); err != nil {
		t.Fatal("bootstrap admin")
	}
	bootstrap, err := Bootstrap(ctx, cfg, BootstrapOptions{ProviderName: "Ark live acceptance", ProviderType: domain.ProviderVolcengine, ProviderBaseURL: domain.ResolveBaseURL(domain.ProfileVolcengineChat, ""), ProviderModel: model, PublicModel: "ark-live", ProjectName: "Ark acceptance", BillingMode: domain.BillingModeFree}, []byte(key))
	if err != nil {
		t.Fatal("bootstrap Ark runtime")
	}
	runtime, err := Open(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal("open runtime")
	}
	defer func() {
		if runtime != nil {
			runtime.Close()
		}
	}()
	cookie, csrf := loginAdminForTest(t, runtime)
	created := performAdminMutation(t, runtime, cookie, csrf, "POST", "/admin/api/v1/provider-egress-proxies", "", map[string]any{"name": "Ark acceptance physical egress", "kind": "http_connect", "endpoint": proxy.URL, "allow_loopback_endpoint": true, "current_password": stepUpTestPassword})
	if created.Code != 201 {
		t.Fatalf("managed egress creation: HTTP %d", created.Code)
	}
	var proxyDefinition domain.ProviderEgressProxy
	if json.Unmarshal(created.Body.Bytes(), &proxyDefinition) != nil {
		t.Fatal("decode managed egress")
	}
	instance, err := runtime.store.GetProvider(ctx, bootstrap.ProviderID)
	if err != nil {
		t.Fatal("load provider")
	}
	// Bootstrap intentionally enables its first deployment. The Admin API
	// correctly refuses changing egress underneath it. Seed the initial test
	// association in the fixture store, then reopen the complete runtime from
	// persisted configuration; do not weaken the production mutation guard.
	instance.EgressProxyID = proxyDefinition.ID
	if _, err := runtime.store.PutProvider(ctx, instance, instance.Revision, nil); err != nil {
		t.Fatal("seed initial egress association")
	}
	if err := runtime.Close(); err != nil {
		t.Fatal("close setup runtime")
	}
	runtime, err = Open(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal("reopen native runtime with persisted egress")
	}
	cookie, csrf = loginAdminForTest(t, runtime)

	listing := performAdminMutation(t, runtime, cookie, csrf, "POST", "/admin/api/v1/providers/"+instance.ID+"/invocation-targets", "", nil)
	if listing.Code != 200 {
		t.Fatalf("native catalogue: HTTP %d", listing.Code)
	}
	var catalog invocationTargetCatalogResponse
	if json.Unmarshal(listing.Body.Bytes(), &catalog) != nil {
		t.Fatal("decode native catalogue")
	}
	found := false
	for _, item := range catalog.Items {
		if item.TargetID == model {
			found = true
			if len(item.Variants) == 0 || !item.Variants[0].Capabilities.Reasoning || !item.Variants[0].Capabilities.Tools || !item.Variants[0].Capabilities.StructuredOutputs {
				t.Fatal("real provider metadata did not resolve reasoning/tools/schema")
			}
		}
	}
	if !found {
		select {
		case reason := <-proxyFailures:
			t.Log(reason)
		default:
		}
		t.Fatalf("selected model absent from native catalogue: entries=%d degraded=%+v", len(catalog.Items), catalog.DegradedBindings)
	}
	t.Logf("native catalogue: entries=%d model capability metadata resolved", len(catalog.Items))
	gateway := httptest.NewServer(runtime.gatewayRouter())
	defer gateway.Close()
	client := &http.Client{Timeout: 90 * time.Second}
	limit := int64(1024)
	ask := func(prompt, effort string) openaiapi.ChatCompletionRequest {
		return openaiapi.ChatCompletionRequest{Model: "ark-live", Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent(prompt)}}, ReasoningEffort: effort, MaxCompletionTokens: &limit}
	}
	invoke := func(request openaiapi.ChatCompletionRequest) *http.Response {
		body, _ := json.Marshal(request)
		req, err := http.NewRequestWithContext(ctx, "POST", gateway.URL+"/v1/chat/completions", bytes.NewReader(body))
		if err != nil {
			t.Fatal("build gateway request")
		}
		req.Header.Set("Authorization", "Bearer "+bootstrap.GatewayKey)
		req.Header.Set("Content-Type", "application/json")
		response, err := client.Do(req)
		if err != nil {
			t.Fatal("native gateway transport failed")
		}
		if response.StatusCode != 200 {
			defer response.Body.Close()
			var refusal struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			json.NewDecoder(response.Body).Decode(&refusal)
			// Ledger diagnostics retain only the already-sanitized upstream code,
			// never provider response text, account IDs, keys or request IDs.
			_, _ = runtime.ledger.Replay(ledger.Watermark{}, func(record ledger.Record) error {
				if record.Event.ProviderCode != "" {
					t.Logf("native refusal upstream_code=%s", record.Event.ProviderCode)
				}
				return nil
			})
			t.Fatalf("gateway HTTP %d code=%s stream=%v", response.StatusCode, refusal.Error.Code, request.Stream)
		}
		return response
	}
	var totalInput, totalOutput int64
	chat := func(request openaiapi.ChatCompletionRequest) openaiapi.ChatCompletionResponse {
		response := invoke(request)
		defer response.Body.Close()
		var result openaiapi.ChatCompletionResponse
		if json.NewDecoder(response.Body).Decode(&result) != nil || len(result.Choices) != 1 || result.Choices[0].Message == nil || result.Usage == nil {
			t.Fatal("native gateway response incomplete")
		}
		if result.Model != "ark-live" {
			t.Fatal("public model alias not preserved")
		}
		totalInput += result.Usage.PromptTokens
		totalOutput += result.Usage.CompletionTokens
		return result
	}
	plain := chat(ask("Reply with OK only.", "none"))
	if plain.Usage.ReasoningTokens() != 0 {
		t.Fatal("thinking off was ignored")
	}
	request := ask("Use the echo tool with value ping. You must use the tool, not answer directly. After its result, reply with that result only.", "low")
	request.Tools = []openaiapi.Tool{{Type: "function", Function: openaiapi.ToolFunction{Name: "echo", Parameters: json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":false}`)}}}
	// Measured Seed 2.1 Turbo returns a forced named tool call without thinking
	// state. Let it plan the tool call so this phase actually exercises encrypted
	// reasoning replay; forced tool selection is covered by the basic smoke.
	request.ToolChoice = json.RawMessage(`"auto"`)
	tool := chat(request)
	message := tool.Choices[0].Message
	if message.EncryptedContent == "" || message.ReasoningContent == "" || tool.Usage.ReasoningTokens() <= 0 || len(message.ToolCalls) != 1 {
		t.Fatal("native thinking/tool response lost encrypted state or usage")
	}
	opaqueLength := len(message.EncryptedContent)
	request.Messages = append(request.Messages, *message, openaiapi.Message{Role: "tool", ToolCallID: message.ToolCalls[0].ID, Content: openaiapi.TextContent("pong")})
	request.ToolChoice = json.RawMessage(`"none"`)
	replay := chat(request)
	var text string
	if json.Unmarshal(replay.Choices[0].Message.Content, &text) != nil || strings.TrimSpace(text) != "pong" {
		t.Fatal("native encrypted tool replay failed")
	}
	t.Logf("native encrypted tool replay: bytes=%d reasoning_tokens=%d", opaqueLength, tool.Usage.ReasoningTokens()+replay.Usage.ReasoningTokens())
	stream := ask("What is 17 times 23? Reply with the number only.", "low")
	stream.Stream = true
	stream.StreamOptions = &openaiapi.StreamOptions{IncludeUsage: true}
	started := time.Now()
	response := invoke(stream)
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	chunks, encryptedBlocks, reasoningBlocks := 0, 0, 0
	done := false
	var streamUsage *openaiapi.Usage
	var answer strings.Builder
	var firstDelta time.Duration
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		raw := strings.TrimPrefix(line, "data: ")
		if raw == "[DONE]" {
			done = true
			break
		}
		var chunk openaiapi.ChatCompletionResponse
		if json.Unmarshal([]byte(raw), &chunk) != nil {
			t.Fatal("invalid SSE event")
		}
		chunks++
		if firstDelta == 0 {
			firstDelta = time.Since(started)
		}
		if chunk.Usage != nil {
			streamUsage = chunk.Usage
		}
		for _, choice := range chunk.Choices {
			if choice.Delta == nil {
				continue
			}
			delta := choice.Delta
			if delta.EncryptedContent != "" {
				encryptedBlocks++
			}
			if delta.ReasoningContent != "" {
				reasoningBlocks++
			}
			if value, ok := openaiapi.DecodeTextContent(delta.Content); ok {
				answer.WriteString(value)
			}
		}
	}
	response.Body.Close()
	// This is a streaming protocol acceptance, not a free-text formatting
	// benchmark. Seed 2.1 Lite may explain the calculation before its final
	// answer; preserve that output and check the standalone final result.
	answerWords := strings.Fields(answer.String())
	correctAnswer := len(answerWords) > 0 && answerWords[len(answerWords)-1] == "391"
	if scanner.Err() != nil || !done || encryptedBlocks == 0 || reasoningBlocks == 0 || !correctAnswer || streamUsage == nil || streamUsage.ReasoningTokens() <= 0 {
		t.Fatalf("native incremental encrypted SSE acceptance failed: scanner=%v done=%v chunks=%d encrypted=%d reasoning=%d answer=%q usage=%+v", scanner.Err(), done, chunks, encryptedBlocks, reasoningBlocks, answer.String(), streamUsage)
	}
	totalInput += streamUsage.PromptTokens
	totalOutput += streamUsage.CompletionTokens
	t.Logf("native SSE: chunks=%d encrypted_blocks=%d reasoning_blocks=%d first_delta_ms=%d total_ms=%d", chunks, encryptedBlocks, reasoningBlocks, firstDelta.Milliseconds(), time.Since(started).Milliseconds())
	jsonRequest := ask("Return JSON with exactly one property ok equal to true.", "none")
	jsonRequest.ResponseFormat = json.RawMessage(`{"type":"json_schema","json_schema":{"name":"smoke","strict":true,"schema":{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}}}`)
	structured := chat(jsonRequest)
	var value map[string]any
	if json.Unmarshal(structured.Choices[0].Message.Content, &text) != nil || json.Unmarshal([]byte(text), &value) != nil || value["ok"] != true {
		t.Fatal("native schema output failed")
	}
	settled := 0
	var ledgerInput, ledgerOutput int64
	_, err = runtime.ledger.Replay(ledger.Watermark{}, func(record ledger.Record) error {
		if record.Event.Kind == ledger.EventAttemptSettled {
			settled++
			ledgerInput += record.Event.ProviderInputTokens
			ledgerOutput += record.Event.ProviderOutputTokens
		}
		return nil
	})
	if err != nil {
		t.Fatal("replay native ledger")
	}
	if settled != 5 || ledgerInput != totalInput || ledgerOutput != totalOutput {
		t.Fatalf("native ledger mismatch: settled=%d ledger=%d/%d reported=%d/%d", settled, ledgerInput, ledgerOutput, totalInput, totalOutput)
	}
	t.Logf("native ledger: successful_settlements=%d input=%d output=%d total=%d", settled, totalInput, totalOutput, totalInput+totalOutput)
}
