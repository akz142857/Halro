package app

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/bearercred"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/replication"
)

func TestHAMemberStatusRequiresIndependentMachineCredential(t *testing.T) {
	now := time.Now()
	path := filepath.Join(t.TempDir(), "ha-status.json")
	rotation, err := bearercred.Rotate(path, time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(rotation.Token)
	authorizer, err := bearercred.NewAuthorizer(path)
	if err != nil {
		t.Fatal(err)
	}
	state := replication.MemberState{Version: replication.StateVersion, ClusterID: "ha", Incarnation: "inc_01", NodeID: "halro-0", Role: replication.RolePrimary, Term: 1, PromisedTerm: 1,
		Peers: []replication.StatePeer{{Name: "halro-1", Address: "private.invalid:9910", SPKISHA256: "sha256:" + strings.Repeat("a", 64)}}}
	publisher, err := replication.NewStatePublisher(filepath.Join(t.TempDir(), "state.json"), make([]byte, 32), state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(publisher.Close)
	runtime := &Runtime{config: config.Config{Metrics: config.Metrics{RequireAuth: true, HAStatus: config.HAStatusAccess{Enabled: true, CredentialFile: path}}},
		replication: &replicationRuntime{role: replication.RolePrimary, publisher: publisher}, haStatus: haStatusRuntime{authorizer: authorizer, requests: make(chan struct{}, 2)}}
	runtime.metricsTokenHash = sha256.Sum256([]byte("metrics-only"))
	request := func(token string, certified bool) *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/ha/status", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if certified {
			cert := &x509.Certificate{RawSubjectPublicKeyInfo: []byte("collector")}
			req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}
		}
		return req
	}
	for _, tc := range []struct {
		name, token string
		cert        bool
		want        int
	}{
		{"missing certificate", string(rotation.Token), false, http.StatusUnauthorized},
		{"missing token", "", true, http.StatusUnauthorized},
		{"metrics token", "metrics-only", true, http.StatusUnauthorized},
		{"HA token", string(rotation.Token), true, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			runtime.metricsRouter().ServeHTTP(response, request(tc.token, tc.cert))
			if response.Code != tc.want {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if tc.want == http.StatusOK && (strings.Contains(response.Body.String(), "private.invalid") || strings.Contains(response.Body.String(), "sha256:")) {
				t.Fatalf("HA status leaked private peer details: %s", response.Body.String())
			}
		})
	}
	// The version-2 source has only process-local transitions and cannot offer
	// a durable page even when the same machine credential is valid.
	legacyRequest := request(string(rotation.Token), true)
	legacyRequest.URL.Path = "/ha/transitions"
	legacyTransitions := httptest.NewRecorder()
	runtime.metricsRouter().ServeHTTP(legacyTransitions, legacyRequest)
	if legacyTransitions.Code != http.StatusServiceUnavailable {
		t.Fatalf("legacy member advertised durable transitions: %d %s", legacyTransitions.Code, legacyTransitions.Body.String())
	}
	if _, err := publisher.Promise(2); err != nil {
		t.Fatal(err)
	}
	transitionResponse := httptest.NewRecorder()
	runtime.metricsRouter().ServeHTTP(transitionResponse, request(string(rotation.Token), true))
	if transitionResponse.Code != http.StatusOK || !strings.Contains(transitionResponse.Body.String(), `"kind":"promise"`) ||
		!strings.Contains(transitionResponse.Body.String(), `"from_role":"primary","to_role":"replica"`) ||
		!strings.Contains(transitionResponse.Body.String(), `"publisher_started_at"`) {
		t.Fatalf("machine status lost a durable role transition: %d %s", transitionResponse.Code, transitionResponse.Body.String())
	}
	coordinator, journal := newStepdownTestCoordinator(t)
	defer journal.Close()
	coordinator.MarkUnavailable(errors.New("private peer address: secret.internal"))
	runtime.replication.coordinator = coordinator
	availabilityResponse := httptest.NewRecorder()
	runtime.metricsRouter().ServeHTTP(availabilityResponse, request(string(rotation.Token), true))
	if availabilityResponse.Code != http.StatusOK || !strings.Contains(availabilityResponse.Body.String(), `"reason":"manual_unavailable"`) ||
		!strings.Contains(availabilityResponse.Body.String(), `"replication_unavailable":true`) ||
		strings.Contains(availabilityResponse.Body.String(), "secret.internal") {
		t.Fatalf("machine status availability evidence=%d %s", availabilityResponse.Code, availabilityResponse.Body.String())
	}
	replicaJournal, err := replication.OpenOrderingJournal(filepath.Join(t.TempDir(), "replica-ordering.journal"), make([]byte, 32),
		replication.OrderingHeader{ClusterID: "ha", Incarnation: "inc_01"}, 0, [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	defer replicaJournal.Close()
	receiver, err := replication.NewReplicaReceiver("ha", "inc_01", "halro-0", "halro-1", 1, 2, 0, 0, replicaJournal, statusTestFrameSink{})
	if err != nil {
		t.Fatal(err)
	}
	runtime.replication.receiver = receiver
	replicaResponse := httptest.NewRecorder()
	runtime.metricsRouter().ServeHTTP(replicaResponse, request(string(rotation.Token), true))
	if replicaResponse.Code != http.StatusOK || !strings.Contains(replicaResponse.Body.String(), `"replica_stage_transitions"`) ||
		!strings.Contains(replicaResponse.Body.String(), `"receive_state":"ready"`) {
		t.Fatalf("machine status Replica stage evidence=%d %s", replicaResponse.Code, replicaResponse.Body.String())
	}
	runtime.replication.receiver = nil
	metrics := httptest.NewRecorder()
	metricsRequest := request(string(rotation.Token), true)
	metricsRequest.URL.Path = "/metrics"
	runtime.metricsRouter().ServeHTTP(metrics, metricsRequest)
	if metrics.Code == http.StatusOK {
		t.Fatal("HA credential opened Metrics")
	}
	mutation := request(string(rotation.Token), true)
	mutation.Method = http.MethodPost
	response := httptest.NewRecorder()
	runtime.metricsRouter().ServeHTTP(response, mutation)
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("machine status accepted mutation method: %d", response.Code)
	}
	if err := bearercred.Revoke(path, rotation.Version, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	runtime.metricsRouter().ServeHTTP(response, request(string(rotation.Token), true))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("revoked credential accepted: %d", response.Code)
	}
	if runtime.haStatus.authFailed.Load() != 4 {
		t.Fatalf("auth failure counter=%d", runtime.haStatus.authFailed.Load())
	}
	runtime.config.Metrics.HAStatus.Enabled = false
	response = httptest.NewRecorder()
	runtime.metricsRouter().ServeHTTP(response, request(string(rotation.Token), true))
	if response.Code != http.StatusNotFound {
		t.Fatalf("disabled route status=%d", response.Code)
	}
}

func TestHAMemberDurableTransitionsRequireMachineIdentityAndCursor(t *testing.T) {
	root := t.TempDir()
	credentialPath := filepath.Join(root, "ha-token.json")
	rotation, err := bearercred.Rotate(credentialPath, time.Minute, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer clear(rotation.Token)
	authorizer, err := bearercred.NewAuthorizer(credentialPath)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(root, "cluster", "state.json")
	key := make([]byte, 32)
	state := replication.MemberState{Version: replication.StateVersion, ClusterID: "ha", Incarnation: "inc_01", NodeID: "halro-0", Role: replication.RoleReplica, Term: 1, PromisedTerm: 1,
		Peers: []replication.StatePeer{{Name: "halro-1", Address: "private.invalid:9910", SPKISHA256: "sha256:" + strings.Repeat("a", 64)}}}
	if err := replication.WriteState(statePath, state, key); err != nil {
		t.Fatal(err)
	}
	state, err = replication.MigrateStateTransitionJournal(statePath, key)
	if err != nil {
		t.Fatal(err)
	}
	publisher, err := replication.OpenStatePublisher(statePath, key, state)
	if err != nil {
		t.Fatal(err)
	}
	defer publisher.Close()
	if _, err := publisher.Promise(2); err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{config: config.Config{Metrics: config.Metrics{RequireAuth: true, HAStatus: config.HAStatusAccess{Enabled: true, CredentialFile: credentialPath}}},
		replication: &replicationRuntime{role: replication.RoleReplica, publisher: publisher}, haStatus: haStatusRuntime{authorizer: authorizer, requests: make(chan struct{}, 2)}}
	request := func(address string, cert bool) *http.Request {
		req := httptest.NewRequest(http.MethodGet, address, nil)
		req.Header.Set("Authorization", "Bearer "+string(rotation.Token))
		if cert {
			identity := &x509.Certificate{RawSubjectPublicKeyInfo: []byte("collector")}
			req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{identity}, VerifiedChains: [][]*x509.Certificate{{identity}}}
		}
		return req
	}
	response := httptest.NewRecorder()
	runtime.replicaMetricsRouter().ServeHTTP(response, request("/ha/transitions", false))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("uncertified transition read=%d", response.Code)
	}
	response = httptest.NewRecorder()
	runtime.replicaMetricsRouter().ServeHTTP(response, request("/ha/transitions", true))
	if response.Code != http.StatusOK {
		t.Fatalf("transition page=%d %s", response.Code, response.Body.String())
	}
	var page replication.DurableTransitionPage
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.BaselineKind != "legacy_baseline" || page.CommittedSequence != 1 || len(page.Events) != 1 || page.Events[0].Kind != "promise" ||
		page.NextCursor.Sequence != 1 || page.HasMore {
		t.Fatalf("unexpected durable page: %+v", page)
	}
	snapshot := publisher.Snapshot()
	if strings.Contains(response.Body.String(), hex.EncodeToString(snapshot.Transition.Digest[:])) {
		t.Fatal("machine page exposed the raw journal MAC")
	}
	address := "/ha/transitions?after=1&digest=" + page.NextCursor.Digest + "&journal_id=" + page.NextCursor.JournalID
	response = httptest.NewRecorder()
	runtime.replicaMetricsRouter().ServeHTTP(response, request(address, true))
	if response.Code != http.StatusOK {
		t.Fatalf("cursor continuation=%d %s", response.Code, response.Body.String())
	}
	response = httptest.NewRecorder()
	runtime.replicaMetricsRouter().ServeHTTP(response, request("/ha/transitions?after=1&digest=wrong&journal_id="+page.NextCursor.JournalID, true))
	if response.Code != http.StatusConflict {
		t.Fatalf("wrong cursor=%d %s", response.Code, response.Body.String())
	}
	response = httptest.NewRecorder()
	runtime.replicaMetricsRouter().ServeHTTP(response, request("/ha/transitions?after=1&digest="+page.NextCursor.Digest, true))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("missing journal identity=%d %s", response.Code, response.Body.String())
	}
	response = httptest.NewRecorder()
	malformed := request("/ha/transitions", true)
	malformed.URL.RawQuery = "after=%zz"
	runtime.replicaMetricsRouter().ServeHTTP(response, malformed)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("malformed cursor query=%d %s", response.Code, response.Body.String())
	}
	runtime.replication.role = replication.RolePrimary
	response = httptest.NewRecorder()
	runtime.metricsRouter().ServeHTTP(response, request("/ha/transitions", true))
	if response.Code != http.StatusOK {
		t.Fatalf("primary metrics listener lost transition route: %d %s", response.Code, response.Body.String())
	}
	journalPath := replication.TransitionJournalPath(statePath)
	data, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	data[20] ^= 1
	if err := os.WriteFile(journalPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	runtime.metricsRouter().ServeHTTP(response, request("/ha/transitions", true))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("changed journal served as a valid or empty page: %d %s", response.Code, response.Body.String())
	}
}

type statusTestFrameSink struct{}

func (statusTestFrameSink) Persist(replication.Frame) error { return nil }

func TestHAMemberPrefixDigestUsesExactDurableAuthenticatedRecord(t *testing.T) {
	root := t.TempDir()
	credentialPath := filepath.Join(root, "status-credential.json")
	rotation, err := bearercred.Rotate(credentialPath, time.Minute, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer clear(rotation.Token)
	authorizer, err := bearercred.NewAuthorizer(credentialPath)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := replication.OpenOrderingJournal(filepath.Join(root, "ordering.journal"), []byte("0123456789abcdef0123456789abcdef"),
		replication.OrderingHeader{ClusterID: "ha", Incarnation: "inc_01"}, 0, [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	record, err := journal.Append(replication.OrderingRecord{Kind: replication.KindLeadershipEstablished, Store: replication.StoreNone, Index: 1, Term: 1,
		FrameDigest: sha256.Sum256([]byte("leadership anchor"))})
	if err != nil {
		t.Fatal(err)
	}
	state := replication.MemberState{Version: replication.StateVersion, ClusterID: "ha", Incarnation: "inc_01", NodeID: "halro-0", Role: replication.RolePrimary, Term: 1, PromisedTerm: 1,
		Peers: []replication.StatePeer{{Name: "halro-1", Address: "private.invalid:9910", SPKISHA256: "sha256:" + strings.Repeat("a", 64)}}}
	publisher, err := replication.NewStatePublisher(filepath.Join(root, "state.json"), make([]byte, 32), state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(publisher.Close)
	if err := publisher.PublishPrimary(replication.PrimaryProgress{DurableIndex: 1, ConfirmedIndex: 1, OrderingHeadMAC: record.MAC, Projection: replication.ProjectionState{Index: 1}}); err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{config: config.Config{Metrics: config.Metrics{HAStatus: config.HAStatusAccess{Enabled: true, IncludePrefixDigest: true}}},
		replication: &replicationRuntime{role: replication.RolePrimary, publisher: publisher, journal: journal}, haStatus: haStatusRuntime{authorizer: authorizer, requests: make(chan struct{}, 2)}}
	request := httptest.NewRequest(http.MethodGet, "/ha/status", nil)
	request.Header.Set("Authorization", "Bearer "+string(rotation.Token))
	cert := &x509.Certificate{RawSubjectPublicKeyInfo: []byte("collector")}
	request.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}
	response := httptest.NewRecorder()
	runtime.metricsRouter().ServeHTTP(response, request)
	want := sha256.Sum256(record.MAC[:])
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), hex.EncodeToString(want[:])) ||
		!strings.Contains(response.Body.String(), `"last_frame_term":1`) || strings.Contains(response.Body.String(), hex.EncodeToString(record.MAC[:])) {
		t.Fatalf("prefix digest response=%d %s", response.Code, response.Body.String())
	}
	runtime.config.Metrics.HAStatus.IncludePrefixDigest = false
	response = httptest.NewRecorder()
	runtime.metricsRouter().ServeHTTP(response, request)
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "prefix_digest") {
		t.Fatalf("prefix digest leaked while field scope disabled: %d %s", response.Code, response.Body.String())
	}
	runtime.config.Metrics.HAStatus.IncludePrefixDigest = true
	state.DurableIndex, state.ConfirmedIndex, state.AppliedIndex = 1, 1, 1
	state.Projection.Index = 1
	state.OrderingHeadMAC = sha256.Sum256([]byte("wrong authenticated head"))
	wrong, err := replication.NewStatePublisher(filepath.Join(root, "wrong-state.json"), make([]byte, 32), state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(wrong.Close)
	runtime.replication.publisher = wrong
	response = httptest.NewRecorder()
	runtime.metricsRouter().ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "ordering_head_sha256") {
		t.Fatalf("mismatched state/journal prefix disclosed: %d %s", response.Code, response.Body.String())
	}
}
