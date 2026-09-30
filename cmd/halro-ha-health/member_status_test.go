package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMemberStatusCollectorScopesAndRevalidatesEveryRead(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "status-token")
	if err := os.WriteFile(tokenFile, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	provided := ""
	statusCode := http.StatusOK
	body := `{"mode":"ha","cluster_id":"ha","incarnation":"inc_1","node_id":"one","role":"primary","term":1,"promised_term":1,"durable_index":3,"confirmed_index":2,"applied_index":2,"startup_ready":true,"source":"forged","sampled_at":"2099-01-01T00:00:00Z","error":"forged","health_probe_error":"forged","health_live":true,"health_ready":true}`
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.String() != "https://member.invalid/ha/status" {
			t.Fatalf("collector escaped fixed route: %s %s", request.Method, request.URL)
		}
		provided = request.Header.Get("Authorization")
		return &http.Response{StatusCode: statusCode, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	collector := statusCollector{config: statusMemberConfig{NodeID: "one", URL: "https://member.invalid/ha/status", TokenFile: tokenFile}, client: client}
	result := collector.fetch(context.Background(), "ha")
	if result.Error != "" || result.NodeID != "one" || result.Projection.Index != 0 || provided != "Bearer first" ||
		result.Source != "member /ha/status" || result.SampledAt.After(time.Now().Add(time.Minute)) ||
		result.HealthLive != nil || result.HealthReady != nil || result.HealthProbeError != "" {
		t.Fatalf("valid status=%+v authorization=%q", result, provided)
	}
	if err := os.WriteFile(tokenFile, []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result = collector.fetch(context.Background(), "ha")
	if result.Error != "" || provided != "Bearer second" {
		t.Fatalf("rotated token not reread: %+v authorization=%q", result, provided)
	}
	body = strings.Replace(body, `"promised_term":1`, `"promised_term":0`, 1)
	if result := collector.fetch(context.Background(), "ha"); result.Error != "identity_or_schema" {
		t.Fatalf("promised term below current term accepted: %+v", result)
	}
	body = strings.Replace(body, `"promised_term":0`, `"promised_term":1`, 1)
	body = strings.Replace(body, `"node_id":"one"`, `"node_id":"other"`, 1)
	if result := collector.fetch(context.Background(), "ha"); result.Error != "identity_or_schema" {
		t.Fatalf("wrong member identity accepted: %+v", result)
	}
	statusCode = http.StatusUnauthorized
	if result := collector.fetch(context.Background(), "ha"); result.Error != "authentication" {
		t.Fatalf("credential rejection category=%+v", result)
	}
	if err := os.Chmod(tokenFile, 0o644); err != nil {
		t.Fatal(err)
	}
	if result := collector.fetch(context.Background(), "ha"); result.Error != "credential_unavailable" {
		t.Fatalf("insecure secret file accepted: %+v", result)
	}
}

func TestMemberStatusRejectsPeersOutsideExactInventory(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "status-token")
	if err := os.WriteFile(tokenFile, []byte("collector-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := `{"mode":"ha","cluster_id":"ha","incarnation":"inc_1","node_id":"one","role":"primary","term":2,"promised_term":2,"durable_index":3,"confirmed_index":2,"applied_index":2,"peers":`
	body := base + `[{"node_id":"two","connected":true},{"node_id":"three","connected":false}]}`
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return jsonResponse(body), nil })}
	collector := statusCollector{config: statusMemberConfig{NodeID: "one", URL: "https://member.invalid/ha/status", TokenFile: tokenFile},
		client: client, expectedPeers: map[string]bool{"two": true, "three": true}}
	if status := collector.fetch(context.Background(), "ha"); status.Error != "" || len(status.Peers) != 2 {
		t.Fatalf("valid fixed peer inventory rejected: %+v", status)
	}
	for _, peers := range []string{
		`[{"node_id":"two","connected":true}]`,
		`[{"node_id":"two","connected":true},{"node_id":"stranger","connected":false}]`,
		`[{"node_id":"two","connected":true},{"node_id":"two","connected":false}]`,
		`[{"node_id":"one","connected":true},{"node_id":"three","connected":false}]`,
	} {
		body = base + peers + "}"
		if status := collector.fetch(context.Background(), "ha"); status.Error != "peer_inventory" {
			t.Fatalf("invalid peer inventory %s accepted: %+v", peers, status)
		}
	}
}

func TestMemberHealthProbesUseFixedDirectRoutesAndKeepReadinessSeparate(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "status-token")
	if err := os.WriteFile(tokenFile, []byte("machine-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	readyCode, machineCode := http.StatusServiceUnavailable, http.StatusOK
	readyTransportFailed := false
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != "member.invalid" {
			t.Errorf("probe escaped member hostname: %s", request.URL)
		}
		body, status := ``, http.StatusOK
		switch request.URL.Path {
		case "/ha/status":
			if request.Header.Get("Authorization") != "Bearer machine-token" {
				t.Error("status token missing from machine route")
			}
			body = `{"mode":"ha","cluster_id":"ha","incarnation":"inc_1","node_id":"one","role":"primary","term":1,"promised_term":1,"durable_index":3,"confirmed_index":2,"applied_index":2}`
			status = machineCode
		case "/health/live":
			if request.Header.Get("Authorization") != "" {
				t.Error("machine token sent to liveness route")
			}
		case "/health/ready":
			if request.Header.Get("Authorization") != "" {
				t.Error("machine token sent to readiness route")
			}
			if readyTransportFailed {
				return nil, errors.New("simulated transport failure")
			}
			status = readyCode
		default:
			t.Errorf("unexpected probe route: %s", request.URL)
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	collector := statusCollector{config: statusMemberConfig{NodeID: "one", URL: "https://member.invalid/ha/status",
		LiveURL: "https://member.invalid/health/live", ReadyURL: "https://member.invalid/health/ready", TokenFile: tokenFile}, client: client}
	status := collector.fetch(context.Background(), "ha")
	if status.Error != "" || status.HealthProbeError != "" || status.HealthLive == nil || !*status.HealthLive ||
		status.HealthReady == nil || *status.HealthReady {
		t.Fatalf("valid live and explicit not-ready were conflated: %+v", status)
	}
	readyCode = http.StatusFound
	status = collector.fetch(context.Background(), "ha")
	if status.Error != "" || status.HealthProbeError != "ready_http_status" || status.HealthReady != nil || status.HealthLive == nil || !*status.HealthLive {
		t.Fatalf("redirect was not treated as an unknown readiness probe: %+v", status)
	}
	machineCode, readyCode = http.StatusUnauthorized, http.StatusOK
	status = collector.fetch(context.Background(), "ha")
	if status.Error != "authentication" || status.HealthLive == nil || !*status.HealthLive || status.HealthReady == nil || !*status.HealthReady {
		t.Fatalf("independent live/ready evidence was discarded after machine auth failure: %+v", status)
	}
	machineCode, readyTransportFailed = http.StatusOK, true
	status = collector.fetch(context.Background(), "ha")
	if status.Error != "" || status.HealthProbeError != "ready_transport" || status.HealthReady != nil || status.HealthLive == nil || !*status.HealthLive {
		t.Fatalf("readiness transport error was not kept unknown: %+v", status)
	}
}

func TestMemberHealthURLsStayOnFixedMemberRoutes(t *testing.T) {
	for _, candidate := range []string{"http://one.invalid/health/ready", "https://other.invalid/health/ready",
		"https://one.invalid/health/ready?debug=1", "https://one.invalid/admin/api/v1/cluster/status"} {
		if validMemberHealthURL(candidate, "one.invalid", "/health/ready") {
			t.Fatalf("unsafe member health URL accepted: %s", candidate)
		}
	}
	if !validMemberHealthURL("https://one.invalid:9443/health/ready", "one.invalid", "/health/ready") {
		t.Fatal("member-local HTTPS readiness URL rejected")
	}
}

func TestMemberStatusLiveTransitionsRequireCoherentProcessEvidence(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "status-token")
	if err := os.WriteFile(tokenFile, []byte("collector-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := `{"mode":"ha","cluster_id":"ha","incarnation":"inc_1","node_id":"one","role":"replica","term":2,"promised_term":2,"durable_index":3,"confirmed_index":2,"applied_index":2}`
	body := base
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return jsonResponse(body), nil })}
	collector := statusCollector{config: statusMemberConfig{NodeID: "one", URL: "https://member.invalid/ha/status", TokenFile: tokenFile, RequireLiveTransitions: true}, client: client}
	if status := collector.fetch(context.Background(), "ha"); status.Error != "transitions_missing" {
		t.Fatalf("missing required live transition evidence accepted: %+v", status)
	}
	started := time.Now().UTC().Add(-2 * time.Second).Format(time.RFC3339Nano)
	at := time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano)
	transition := fmt.Sprintf(`,"live_transitions":{"publisher_started_at":%q,"dropped":0,"events":[{"sequence":1,"at":%q,"kind":"adopt_higher_term","from_role":"primary","to_role":"replica","from_term":1,"to_term":2,"from_promised_term":1,"to_promised_term":2}]}`, started, at)
	body = strings.TrimSuffix(base, "}") + transition + "}"
	valid := collector.fetch(context.Background(), "ha")
	if valid.Error != "" || valid.LiveTransitions == nil || len(valid.LiveTransitions.Events) != 1 {
		t.Fatalf("valid transition history rejected: %+v", valid)
	}
	body = strings.Replace(body, `"sequence":1`, `"sequence":2`, 1)
	if status := collector.fetch(context.Background(), "ha"); status.Error != "transitions_schema" {
		t.Fatalf("noncontiguous event sequence accepted: %+v", status)
	}
	body = strings.Replace(body, `"sequence":2`, `"sequence":1`, 1)
	body = strings.Replace(body, `"to_term":2`, `"to_term":3`, 1)
	if status := collector.fetch(context.Background(), "ha"); status.Error != "transitions_schema" {
		t.Fatalf("event detached from current state accepted: %+v", status)
	}
}

func TestLiveTransitionHistoryRejectsBrokenChain(t *testing.T) {
	now := time.Now().UTC()
	status := memberStatus{Role: "primary", Term: 3, PromisedTerm: 3,
		LiveTransitions: &liveTransitionHistory{PublisherStartedAt: now.Add(-time.Minute), Events: []liveTransition{
			{Sequence: 1, At: now.Add(-2 * time.Second), Kind: "promise", FromRole: "replica", ToRole: "replica", FromTerm: 1, ToTerm: 1, FromPromisedTerm: 1, ToPromisedTerm: 3},
			{Sequence: 2, At: now.Add(-time.Second), Kind: "promote", FromRole: "replica", ToRole: "primary", FromTerm: 1, ToTerm: 3, FromPromisedTerm: 3, ToPromisedTerm: 3},
		}},
	}
	if !validLiveTransitions(status, now) {
		t.Fatal("valid publisher event chain rejected")
	}
	status.LiveTransitions.Events[1].FromTerm = 2
	if validLiveTransitions(status, now) {
		t.Fatal("disconnected publisher event chain accepted")
	}
}

func TestAvailabilityHistoryRequiresPrimarySourceAndCoherentChain(t *testing.T) {
	now := time.Now().UTC()
	unavailable := false
	status := memberStatus{Role: "primary", ReplicationUnavailable: &unavailable,
		AvailabilityTransitions: &availabilityTransitionHistory{CoordinatorStartedAt: now.Add(-time.Minute), Current: "replicating", Events: []availabilityTransition{
			{Sequence: 1, At: now.Add(-2 * time.Second), From: "replicating", To: "unavailable", Reason: "frame_queue_failed"},
			{Sequence: 2, At: now.Add(-time.Second), From: "unavailable", To: "replicating", Reason: "recovered"},
		}},
	}
	if !validAvailabilityTransitions(status, now) {
		t.Fatal("valid availability boundary events rejected")
	}
	status.AvailabilityTransitions.Events[1].From = "replicating"
	if validAvailabilityTransitions(status, now) {
		t.Fatal("broken availability event chain accepted")
	}
	status.AvailabilityTransitions.Events[1].From = "unavailable"
	status.AvailabilityTransitions.Events[1].Reason = "secret.internal"
	if validAvailabilityTransitions(status, now) {
		t.Fatal("arbitrary availability reason accepted")
	}
	status.AvailabilityTransitions.Events[1].Reason = "recovered"
	unavailable = true
	if validAvailabilityTransitions(status, now) {
		t.Fatal("availability history disagreed with current status")
	}
}

func TestMemberStatusRequiresPrimaryAvailabilityHistoryWhenConfigured(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "status-token")
	if err := os.WriteFile(tokenFile, []byte("collector-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	body := `{"mode":"ha","cluster_id":"ha","incarnation":"inc_1","node_id":"one","role":"primary","term":2,"promised_term":2,"durable_index":0,"confirmed_index":0,"applied_index":0}`
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return jsonResponse(body), nil })}
	collector := statusCollector{config: statusMemberConfig{NodeID: "one", URL: "https://member.invalid/ha/status", TokenFile: tokenFile, RequireAvailabilityTransitions: true}, client: client}
	if status := collector.fetch(context.Background(), "ha"); status.Error != "availability_missing" {
		t.Fatalf("missing Primary availability events accepted: %+v", status)
	}
	body = strings.Replace(body, `"role":"primary"`, `"role":"replica"`, 1)
	if status := collector.fetch(context.Background(), "ha"); status.Error != "" {
		t.Fatalf("Replica incorrectly required Primary coordinator evidence: %+v", status)
	}
}

func TestMemberStatusManifestRejectsPartialInventoryAndArbitraryRoutes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.json")
	write := func(data string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"members":[{"node_id":"one","url":"https://one.invalid/ha/status"}]}`)
	if _, err := loadStatusCollectors(path, []string{"one", "two"}); err == nil {
		t.Fatal("partial status inventory accepted")
	}
	for _, address := range []string{"http://one.invalid/ha/status", "https://one.invalid/admin/api/v1/cluster/status", "https://one.invalid/ha/status?query=all"} {
		write(`{"members":[{"node_id":"one","url":"` + address + `"}]}`)
		if _, err := loadStatusCollectors(path, []string{"one"}); err == nil {
			t.Fatalf("arbitrary status route accepted: %s", address)
		}
	}
	write(`{"members":[{"node_id":"one","url":"https://one.invalid/ha/status","live_url":"https://one.invalid/health/live"}]}`)
	if _, err := loadStatusCollectors(path, []string{"one"}); err == nil || !strings.Contains(err.Error(), "configured together") {
		t.Fatalf("partial health probe pair accepted: %v", err)
	}
	write(`{"members":[{"node_id":"one","url":"https://one.invalid/ha/status","live_url":"https://one.invalid/health/live","ready_url":"https://other.invalid/health/ready"}]}`)
	if _, err := loadStatusCollectors(path, []string{"one"}); err == nil || !strings.Contains(err.Error(), "member status hostname") {
		t.Fatalf("cross-host readiness probe accepted: %v", err)
	}
}

func TestMemberStatusPrefixRequiresValidEvidenceAndFlagsSamePositionConflict(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "status-token")
	if err := os.WriteFile(tokenFile, []byte("collector-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	body := `{"mode":"ha","cluster_id":"ha","incarnation":"inc_1","node_id":"one","role":"replica","term":2,"promised_term":2,"durable_index":3,"confirmed_index":2,"applied_index":2}`
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return jsonResponse(body), nil })}
	collector := statusCollector{config: statusMemberConfig{NodeID: "one", URL: "https://member.invalid/ha/status", TokenFile: tokenFile, RequirePrefixDigest: true}, client: client}
	if status := collector.fetch(context.Background(), "ha"); status.Error != "prefix_missing" {
		t.Fatalf("missing required prefix accepted: %+v", status)
	}
	body = strings.TrimSuffix(body, "}") + `,"prefix_digest":{"index":3,"last_frame_term":2,"ordering_head_sha256":"` + strings.Repeat("a", 64) + `"}}`
	first := collector.fetch(context.Background(), "ha")
	if first.Error != "" || first.PrefixDigest == nil || first.PrefixDigest.Index != 3 {
		t.Fatalf("valid prefix rejected: %+v", first)
	}
	second := first
	second.NodeID = "two"
	second.PrefixDigest = &prefixDigest{Index: 3, LastFrameTerm: 2, OrderingHeadSHA256: strings.Repeat("b", 64)}
	statuses := []memberStatus{first, second}
	if !markPrefixConflicts(statuses) || statuses[0].Error != "prefix_conflict" || statuses[1].Error != "prefix_conflict" {
		t.Fatalf("same-position divergence hidden: %+v", statuses)
	}
	third := first
	third.NodeID = "three"
	statuses = []memberStatus{first, second, third}
	if !markPrefixConflicts(statuses) || statuses[0].Error != "prefix_conflict" || statuses[1].Error != "prefix_conflict" || statuses[2].Error != "prefix_conflict" {
		t.Fatalf("three-member divergence did not mark all affected members: %+v", statuses)
	}
	second.PrefixDigest.Index = 4
	second.DurableIndex = 4
	statuses = []memberStatus{first, second}
	if markPrefixConflicts(statuses) {
		t.Fatalf("different indexes compared as equal prefixes: %+v", statuses)
	}
}

func TestReplicaStageHistoryRejectsBrokenSequenceReasonAndCurrentState(t *testing.T) {
	now := time.Now().UTC()
	status := memberStatus{Role: "replica", ReplicaStageTransitions: &replicaStageHistory{ReceiverStartedAt: now.Add(-time.Minute),
		ReceiveState: "ready", ApplyState: "ready", Events: []replicaStageTransition{
			{Sequence: 1, At: now.Add(-time.Second), Stage: "apply", From: "ready", To: "blocked", Reason: "projection_failed", TargetIndex: 7},
			{Sequence: 2, At: now, Stage: "apply", From: "blocked", To: "ready", Reason: "apply_recovered", TargetIndex: 7},
		}}}
	if !validReplicaStageTransitions(status, now) {
		t.Fatal("valid Replica failure/recovery history rejected")
	}
	status.ReplicaStageTransitions.Events[1].Sequence = 3
	if validReplicaStageTransitions(status, now) {
		t.Fatal("sequence gap accepted")
	}
	status.ReplicaStageTransitions.Events[1].Sequence = 2
	status.ReplicaStageTransitions.Events[0].Reason = "secret.internal"
	if validReplicaStageTransitions(status, now) {
		t.Fatal("arbitrary error text accepted")
	}
	status.ReplicaStageTransitions.Events[0].Reason = "projection_failed"
	status.ReplicaStageTransitions.ApplyState = "blocked"
	if validReplicaStageTransitions(status, now) {
		t.Fatal("current state disagrees with latest event")
	}
}
