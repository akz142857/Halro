package deadman

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTransitionUsesFailureAndRecoveryHysteresis(t *testing.T) {
	now := time.Now().UTC()
	state, changed := transition(TargetState{}, false, "request_failed", now, 2, 2)
	if changed || state.Phase != PhasePendingDown {
		t.Fatalf("first failure = %q, changed %v", state.Phase, changed)
	}
	state, changed = transition(state, false, "request_failed", now, 2, 2)
	if !changed || state.Phase != PhaseDown {
		t.Fatalf("second failure = %q, changed %v", state.Phase, changed)
	}
	state, changed = transition(state, true, "", now, 2, 2)
	if changed || state.Phase != PhasePendingUp {
		t.Fatalf("first recovery = %q, changed %v", state.Phase, changed)
	}
	state, changed = transition(state, true, "", now, 2, 2)
	if !changed || state.Phase != PhaseUp {
		t.Fatalf("second recovery = %q, changed %v", state.Phase, changed)
	}
}

func TestInitialHealthyObservationDoesNotEmitFalseRecovery(t *testing.T) {
	state, changed := transition(TargetState{}, true, "", time.Now().UTC(), 1, 1)
	if changed || state.Phase != PhaseUp || state.EverDown {
		t.Fatalf("initial observation = %#v, changed %v", state, changed)
	}
}

func TestFailedRecoveryDoesNotEmitDuplicateDown(t *testing.T) {
	now := time.Now().UTC()
	state := TargetState{Phase: PhaseDown, StablePhase: PhaseDown, EverDown: true}
	state, changed := transition(state, true, "", now, 2, 2)
	if changed || state.Phase != PhasePendingUp {
		t.Fatalf("recovery start = %#v, changed %v", state, changed)
	}
	state, changed = transition(state, false, "request_failed", now, 2, 2)
	if changed || state.Phase != PhasePendingDown {
		t.Fatalf("failed recovery = %#v, changed %v", state, changed)
	}
	state, changed = transition(state, false, "request_failed", now, 2, 2)
	if changed || state.Phase != PhaseDown || state.StablePhase != PhaseDown {
		t.Fatalf("continued outage emitted duplicate down: %#v, changed %v", state, changed)
	}
}

func TestHeartbeatOutboxIsCoalescedAndBounded(t *testing.T) {
	engine := &Engine{cfg: Config{ProbeID: "probe", Environment: "test", Region: "local", Cluster: "one", HeartbeatTTL: Duration(time.Minute), OutboxLimit: 16}, state: persistedState{Version: 1, Targets: make(map[string]TargetState)}}
	now := time.Now().UTC()
	for index := 0; index < 50; index++ {
		if err := engine.enqueue("heartbeat", TargetConfig{}, "", "", 0, now.Add(time.Duration(index)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if len(engine.state.Outbox) != 1 || engine.state.Outbox[0].Event.Sequence != 50 {
		t.Fatalf("outbox was not coalesced: %#v", engine.state.Outbox)
	}
	for index := 0; index < 15; index++ {
		if err := engine.enqueue("state_transition", TargetConfig{ID: "target", Kind: "heimdall"}, PhaseDown, "request_failed", 0, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := engine.enqueue("state_transition", TargetConfig{ID: "target", Kind: "heimdall"}, PhaseDown, "request_failed", 0, now); err == nil {
		t.Fatal("full outbox accepted another transition")
	}
}

func TestStateIsDurableAndRejectsCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "deadman.json")
	want := persistedState{Version: 1, Sequence: 7, Targets: map[string]TargetState{"prometheus": {Phase: PhaseDown, StablePhase: PhaseDown}}, Outbox: []queuedEvent{{Event: Event{EventID: "probe-7", Sequence: 7}}}}
	if err := saveState(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := loadState(path, 64)
	if err != nil || got.Sequence != 7 || got.Targets["prometheus"].Phase != PhaseDown || len(got.Outbox) != 1 {
		t.Fatalf("loaded %#v, %v", got, err)
	}
	if err := os.WriteFile(path, []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadState(path, 64); err == nil {
		t.Fatal("corrupt state was accepted")
	}
}

func TestStateLoadRejectsOversizeAndOutboxBeyondConfiguredLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, make([]byte, (1<<20)+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadState(path, 16); err == nil {
		t.Fatal("oversized state was accepted")
	}
	state := persistedState{Version: 1, Sequence: 17, Targets: map[string]TargetState{}}
	for sequence := uint64(1); sequence <= 17; sequence++ {
		state.Outbox = append(state.Outbox, queuedEvent{Event: Event{EventID: fmt.Sprintf("event-%d", sequence), Sequence: sequence}})
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadState(path, 16); err == nil {
		t.Fatal("outbox above configured limit was accepted")
	}
}

func TestStateLoadRejectsNonIncreasingOutboxSequence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	for _, sequences := range [][]uint64{{2, 1}, {1, 1}} {
		state := persistedState{Version: 1, Sequence: 2, Targets: map[string]TargetState{}}
		for _, sequence := range sequences {
			state.Outbox = append(state.Outbox, queuedEvent{Event: Event{
				EventID:  fmt.Sprintf("event-%d", sequence),
				Sequence: sequence,
			}})
		}
		data, err := json.Marshal(state)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadState(path, 16); err == nil {
			t.Fatalf("outbox sequence %v was accepted", sequences)
		}
	}
}

func TestSecureClientUsesBearerAndForbidsRedirect(t *testing.T) {
	pki := newTestPKI(t, false)
	tokenPath := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tokenPath, []byte("test-secret\n"), 0o600)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{pki.serverCertificate}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	defer server.Close()
	client, err := secureClient(TLSConfig{CAFile: pki.caFile}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := request(context.Background(), client, http.MethodGet, server.URL, tokenPath, nil); err == nil {
		t.Fatal("redirect was followed")
	}
}

func TestNewPrebuildsAndReusesTargetClients(t *testing.T) {
	pki := newTestPKI(t, false)
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("probe-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{pki.serverCertificate}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	defer server.Close()

	cfg := validConfig(t.TempDir(), server.URL, pki.caFile, tokenPath)
	engine, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer engine.closeIdleConnections()
	if len(engine.targetClients) != len(cfg.Targets) {
		t.Fatalf("prebuilt target clients = %d, want %d", len(engine.targetClients), len(cfg.Targets))
	}
	client := engine.targetClients[cfg.Targets[0].ID]
	if client == nil {
		t.Fatal("target client was not initialized")
	}
	if err := os.Remove(pki.caFile); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if _, reason := engine.check(context.Background(), cfg, cfg.Targets[0]); reason != "" {
			t.Fatalf("cached target client check %d failed: %s", attempt+1, reason)
		}
		if engine.targetClients[cfg.Targets[0].ID] != client {
			t.Fatal("target client was rebuilt between checks")
		}
	}
}

func TestRetryDelayNeverExceedsMaximum(t *testing.T) {
	engine := &Engine{cfg: Config{Notification: NotificationConfig{
		RetryMin: Duration(time.Second),
		RetryMax: Duration(10 * time.Second),
	}}}
	for attempt := 1; attempt <= 20; attempt++ {
		for sample := 0; sample < 100; sample++ {
			if delay := engine.retryDelay(attempt); delay > 10*time.Second {
				t.Fatalf("attempt %d retry delay %s exceeded maximum", attempt, delay)
			}
		}
	}
}

func TestEnginePersistsOutboxAndRetriesWithoutStoppingProbes(t *testing.T) {
	pki := newTestPKI(t, false)
	tokenPath := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tokenPath, []byte("notify-secret"), 0o600)
	var notifications atomic.Int32
	var receivedMu sync.Mutex
	var received []Event
	receiver := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer notify-secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var event Event
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			http.Error(w, "invalid event", http.StatusBadRequest)
			return
		}
		receivedMu.Lock()
		received = append(received, event)
		receivedMu.Unlock()
		if notifications.Add(1) == 1 {
			http.Error(w, "temporary", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	receiver.TLS = &tls.Config{Certificates: []tls.Certificate{pki.serverCertificate}, MinVersion: tls.VersionTLS12}
	receiver.StartTLS()
	defer receiver.Close()
	directory := t.TempDir()
	cfg := validConfig(directory, receiver.URL, pki.caFile, tokenPath)
	engine, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	var checks atomic.Int32
	engine.check = func(context.Context, Config, TargetConfig) (time.Duration, string) {
		if checks.Add(1) <= 3 {
			return time.Millisecond, "request_failed"
		}
		return time.Millisecond, ""
	}
	now := time.Now().UTC()
	engine.now = func() time.Time { return now }
	if err := engine.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if checks.Load() != 3 || len(engine.state.Outbox) != 4 {
		t.Fatalf("checks=%d outbox=%d", checks.Load(), len(engine.state.Outbox))
	}
	if progressed, err := engine.drainOne(context.Background()); err != nil || progressed {
		t.Fatalf("failed FIFO head = progressed %v, err %v", progressed, err)
	}
	if notifications.Load() != 1 || len(engine.state.Outbox) != 4 {
		t.Fatalf("delivery advanced past failed FIFO head: notifications=%d outbox=%d", notifications.Load(), len(engine.state.Outbox))
	}
	reloaded, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	reloaded.check = engine.check
	now = now.Add(20 * time.Second)
	reloaded.now = func() time.Time { return now }
	for len(reloaded.state.Outbox) > 0 {
		progressed, err := reloaded.drainOne(context.Background())
		if err != nil || !progressed {
			t.Fatalf("drain persisted FIFO = progressed %v, err %v", progressed, err)
		}
	}
	if err := reloaded.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if checks.Load() != 6 {
		t.Fatalf("notification failure stopped probing: checks=%d", checks.Load())
	}
	for len(reloaded.state.Outbox) > 0 {
		progressed, err := reloaded.drainOne(context.Background())
		if err != nil || !progressed {
			t.Fatalf("drain recovery FIFO = progressed %v, err %v", progressed, err)
		}
	}
	states := make(map[string]map[Phase]bool)
	heartbeats := 0
	receivedMu.Lock()
	for _, event := range received {
		if event.SchemaVersion != "heimdall.deadman.event/v1" || event.EventID == "" || event.Sequence == 0 || event.ProbeID != "probe-a" || event.Environment != "test" || event.Region != "local" || event.Cluster != "one" || event.ObservedAt.IsZero() || event.HeartbeatTTL != "30s" || event.Attempt < 1 {
			t.Fatalf("event does not satisfy the v1 payload contract: %#v", event)
		}
		if event.Kind == "heartbeat" {
			heartbeats++
			continue
		}
		if event.Kind != "state_transition" {
			t.Fatalf("unexpected event kind %q", event.Kind)
		}
		if states[event.TargetKind] == nil {
			states[event.TargetKind] = make(map[Phase]bool)
		}
		states[event.TargetKind][event.State] = true
	}
	receivedMu.Unlock()
	if heartbeats < 2 {
		t.Fatalf("heartbeat payloads=%d", heartbeats)
	}
	for _, kind := range []string{"heimdall", "prometheus", "alertmanager"} {
		if !states[kind][PhaseDown] || !states[kind][PhaseUp] {
			t.Fatalf("%s transitions = %#v", kind, states[kind])
		}
	}
	ids := make(map[string]bool)
	for _, item := range reloaded.state.Outbox {
		if ids[item.Event.EventID] {
			t.Fatalf("duplicate event ID %q", item.Event.EventID)
		}
		ids[item.Event.EventID] = true
	}
	if len(reloaded.state.Outbox) != 0 {
		t.Fatalf("outbox was not durably drained: %d", len(reloaded.state.Outbox))
	}
}

func TestMutualTLSIsPresented(t *testing.T) {
	pki := newTestPKI(t, true)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{pki.serverCertificate}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pki.pool, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	defer server.Close()
	client, err := secureClient(TLSConfig{CAFile: pki.caFile, ClientCertFile: pki.clientCertFile, ClientKeyFile: pki.clientKeyFile}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	response, err := request(context.Background(), client, http.MethodGet, server.URL, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
}

func TestPrometheusFreshnessRejectsStaleScalar(t *testing.T) {
	pki := newTestPKI(t, false)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"status":"success","data":{"resultType":"scalar","result":[1700000000,"121"]}}`)
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{pki.serverCertificate}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	defer server.Close()
	client, err := secureClient(TLSConfig{CAFile: pki.caFile}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	target := TargetConfig{Freshness: &FreshnessConfig{URL: server.URL, Mode: "prometheus_scalar_age", MaxAge: Duration(2 * time.Minute)}}
	if reason := checkFreshness(context.Background(), client, target); reason != "freshness_stale" {
		t.Fatalf("reason = %q", reason)
	}
}

func TestPrometheusFreshnessRejectsNonFiniteScalar(t *testing.T) {
	pki := newTestPKI(t, false)
	var value atomic.Value
	value.Store("NaN")
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"success","data":{"resultType":"scalar","result":[1700000000,%q]}}`, value.Load().(string))
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{pki.serverCertificate}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	defer server.Close()
	client, err := secureClient(TLSConfig{CAFile: pki.caFile}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	target := TargetConfig{Freshness: &FreshnessConfig{URL: server.URL, Mode: "prometheus_scalar_age", MaxAge: Duration(2 * time.Minute)}}
	for _, scalar := range []string{"NaN", "+Inf", "-Inf"} {
		value.Store(scalar)
		if reason := checkFreshness(context.Background(), client, target); reason != "freshness_stale" {
			t.Fatalf("scalar %q reason = %q", scalar, reason)
		}
	}
}

type selectiveAudit struct {
	failNotify bool
}

func (a *selectiveAudit) append(record auditRecord) error {
	if a.failNotify && record.Action == "deadman.notify" {
		return errors.New("audit storage unavailable")
	}
	return nil
}

func TestAcknowledgedEventIsRetainedWhenAuditFails(t *testing.T) {
	pki := newTestPKI(t, false)
	tokenPath := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tokenPath, []byte("notify-secret"), 0o600)
	receiver := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	receiver.TLS = &tls.Config{Certificates: []tls.Certificate{pki.serverCertificate}, MinVersion: tls.VersionTLS12}
	receiver.StartTLS()
	defer receiver.Close()
	cfg := validConfig(t.TempDir(), receiver.URL, pki.caFile, tokenPath)
	engine, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	engine.check = func(context.Context, Config, TargetConfig) (time.Duration, string) { return 0, "" }
	if err := engine.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	engine.audit = &selectiveAudit{failNotify: true}
	if progressed, err := engine.drainOne(context.Background()); err == nil || progressed {
		t.Fatalf("audit failure = progressed %v, err %v", progressed, err)
	}
	if len(engine.state.Outbox) != 1 {
		t.Fatalf("acknowledged event was lost after audit failure: outbox=%d", len(engine.state.Outbox))
	}
}

func TestSlowReceiverDoesNotBlockProbeTick(t *testing.T) {
	pki := newTestPKI(t, false)
	tokenPath := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tokenPath, []byte("notify-secret"), 0o600)
	entered, release := make(chan struct{}), make(chan struct{})
	receiver := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	receiver.TLS = &tls.Config{Certificates: []tls.Certificate{pki.serverCertificate}, MinVersion: tls.VersionTLS12}
	receiver.StartTLS()
	defer receiver.Close()
	cfg := validConfig(t.TempDir(), receiver.URL, pki.caFile, tokenPath)
	engine, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	engine.check = func(context.Context, Config, TargetConfig) (time.Duration, string) { return 0, "" }
	if err := engine.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { engine.drainOne(context.Background()); close(done) }()
	<-entered
	started := time.Now()
	if err := engine.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("slow receiver blocked probe tick for %s", elapsed)
	}
	close(release)
	<-done
}

func validConfig(directory, notificationURL, caFile, tokenFile string) Config {
	tlsConfig := TLSConfig{CAFile: caFile}
	return Config{
		ProbeID: "probe-a", Environment: "test", Region: "local", Cluster: "one",
		Interval: Duration(10 * time.Second), Timeout: Duration(time.Second), HeartbeatTTL: Duration(30 * time.Second), FailuresToDown: 1, SuccessesToUp: 1,
		OutboxLimit: 64, DeliveryBatch: 8,
		StateFile: filepath.Join(directory, "state.json"), AuditFile: filepath.Join(directory, "audit.jsonl"),
		Notification: NotificationConfig{URL: notificationURL, BearerTokenFile: tokenFile, TLS: tlsConfig, Timeout: Duration(time.Second), RetryMin: Duration(time.Second), RetryMax: Duration(10 * time.Second)},
		Targets:      []TargetConfig{{ID: "heimdall", Kind: "heimdall", URL: notificationURL, BearerTokenFile: tokenFile, TLS: tlsConfig}, {ID: "prometheus", Kind: "prometheus", URL: notificationURL, BearerTokenFile: tokenFile, TLS: tlsConfig, Freshness: &FreshnessConfig{URL: notificationURL, Mode: "prometheus_scalar_age", MaxAge: Duration(time.Minute)}}, {ID: "alertmanager", Kind: "alertmanager", URL: notificationURL, BearerTokenFile: tokenFile, TLS: tlsConfig}},
	}
}

type testPKI struct {
	caFile, clientCertFile, clientKeyFile string
	serverCertificate                     tls.Certificate
	pool                                  *x509.CertPool
}

func newTestPKI(t *testing.T, client bool) testPKI {
	t.Helper()
	directory := t.TempDir()
	caKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	ca, _ := x509.ParseCertificate(caDER)
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	caFile := filepath.Join(directory, "ca.pem")
	os.WriteFile(caFile, caPEM, 0o600)
	issue := func(name string, usages []x509.ExtKeyUsage, ips []net.IP) (string, string, tls.Certificate) {
		key, _ := rsa.GenerateKey(rand.Reader, 2048)
		template := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: usages, KeyUsage: x509.KeyUsageDigitalSignature, IPAddresses: ips}
		der, _ := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
		certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
		keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
		certPath, keyPath := filepath.Join(directory, name+".pem"), filepath.Join(directory, name+".key")
		os.WriteFile(certPath, certPEM, 0o600)
		os.WriteFile(keyPath, keyPEM, 0o600)
		pair, _ := tls.X509KeyPair(certPEM, keyPEM)
		return certPath, keyPath, pair
	}
	_, _, serverCertificate := issue("server", []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, []net.IP{net.ParseIP("127.0.0.1")})
	result := testPKI{caFile: caFile, serverCertificate: serverCertificate, pool: x509.NewCertPool()}
	result.pool.AppendCertsFromPEM(caPEM)
	if client {
		result.clientCertFile, result.clientKeyFile, _ = issue("client", []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, nil)
	}
	return result
}
