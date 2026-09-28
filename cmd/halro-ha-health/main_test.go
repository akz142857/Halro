package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/hahealth"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func jsonResponse(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestHealthKeepsConfirmationUnknownWithoutProgress(t *testing.T) {
	now := float64(time.Now().Unix())
	vector := make([]map[string]any, 0)
	add := func(instance, metric string, labels map[string]string, value string) {
		identity := map[string]string{"__name__": metric, "instance": instance}
		for key, item := range labels {
			identity[key] = item
		}
		vector = append(vector, map[string]any{"metric": identity, "value": []any{now, value}})
	}
	addMember := func(name, role string) {
		add(name, "up", nil, "1")
		add(name, "halro_cluster_member_info", map[string]string{"cluster_id": "ha", "node_id": name}, "1")
		add(name, "halro_cluster_role", map[string]string{"role": role}, "1")
		add(name, "halro_cluster_incarnation_info", map[string]string{"incarnation": "inc_1"}, "1")
		add(name, "halro_cluster_term", nil, "7")
		add(name, "halro_cluster_promised_term", nil, "7")
		for _, kind := range []string{"durable", "confirmed", "applied"} {
			add(name, "halro_replication_index", map[string]string{"kind": kind}, "12")
		}
		add(name, "halro_replication_startup_ready", nil, "1")
		add(name, "halro_replication_unavailable", nil, "0")
		add(name, "halro_cluster_maintenance", nil, "0")
		for _, reason := range []string{"schema", "spki", "key_challenge"} {
			add(name, "halro_replication_member_incompatible", map[string]string{"reason": reason}, "0")
		}
	}
	addMember("halro-0", "primary")
	addMember("halro-1", "replica")
	add("halro-0", "halro_replication_peer_connected", map[string]string{"peer": "halro-1"}, "1")
	add("halro-1", "halro_replication_peer_connected", map[string]string{"peer": "halro-0"}, "1")
	confirmedBarrier := false
	duplicateConfirmation := false
	confirmationCoverageFresh := true
	epochFresh := true
	usedEpochRecord := false
	usedRequiredWaitMetric := false
	alertFiring := false
	alertsUnavailable := false
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/api/v1/alerts" {
			if alertsUnavailable {
				response := jsonResponse(`{"status":"error"}`)
				response.StatusCode = http.StatusServiceUnavailable
				return response, nil
			}
			if alertFiring {
				return jsonResponse(`{"status":"success","data":{"alerts":[{"labels":{"alertname":"HalroReplicaApplyStalled","category":"ha","environment":"test","cluster":"ha","severity":"warning"},"state":"firing"}]}}`), nil
			}
			return jsonResponse(`{"status":"success","data":{"alerts":[]}}`), nil
		}
		expression := r.URL.Query().Get("query")
		var result any = vector
		resultType := "vector"
		if strings.HasSuffix(expression, "[45s]") {
			matrix := make([]map[string]any, 0, len(vector))
			for _, item := range vector {
				matrix = append(matrix, map[string]any{"metric": item["metric"], "values": []any{item["value"]}})
			}
			result, resultType = matrix, "matrix"
		}
		if strings.Contains(expression, "delta(halro_replication_confirmed_index") {
			// A non-required frame can move the index without a required write ACK.
			result = []any{map[string]any{"metric": map[string]string{"instance": "halro-0"}, "value": []any{now, "1"}}}
		}
		if strings.Contains(expression, "halro_replication_required_confirmation_wait_total") {
			if strings.HasSuffix(expression, "[45s]") {
				resultType = "matrix"
				counters := make([]map[string]any, 0, 2)
				for _, store := range []string{"ledger", "metadata"} {
					at := now
					if store == "metadata" && !confirmationCoverageFresh {
						at -= 40
					}
					counters = append(counters, map[string]any{"metric": map[string]string{"__name__": "halro_replication_required_confirmation_wait_total", "instance": "halro-0", "store": store, "outcome": "confirmed"},
						"values": []any{[]any{at, "1"}}})
				}
				result = counters
			} else {
				result = []any{}
				usedEpochRecord = strings.Contains(expression, "halro:ha_epoch_stable:bool")
				usedRequiredWaitMetric = strings.Contains(expression, `outcome="confirmed"`) &&
					strings.Contains(expression, "increase(") && strings.Contains(expression, "resets(")
				if confirmedBarrier {
					result = []any{map[string]any{"metric": map[string]string{"instance": "halro-0"}, "value": []any{now, "1"}}}
					if duplicateConfirmation {
						result = append(result.([]any), map[string]any{"metric": map[string]string{"instance": "halro-0"}, "value": []any{now, "1"}})
					}
				}
			}
		}
		if strings.HasSuffix(expression, "[45s]") && strings.Contains(expression, "halro:ha_epoch_stable:bool") {
			at := now
			if !epochFresh {
				at -= 40
			}
			result, resultType = []any{map[string]any{"metric": map[string]string{"__name__": "halro:ha_epoch_stable:bool", "instance": "halro-0"},
				"values": []any{[]any{at, "1"}}}}, "matrix"
		}
		body, _ := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": resultType, "result": result}})
		return jsonResponse(string(body)), nil
	})}
	probe := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(`{"role":"primary","cluster_id":"ha"}`), nil
	})}
	base, _ := url.Parse("http://127.0.0.1:9091")
	s := &server{client: client, probeHTTP: probe, base: base, environment: "test", cluster: "ha", members: []string{"halro-0", "halro-1"}, clientURL: "https://service.invalid/"}
	response := httptest.NewRecorder()
	s.health(response, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var result hahealth.Result
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Safety.Level != hahealth.Healthy || result.Catchup.Level != hahealth.Healthy ||
		result.Confirmation.Level != hahealth.Unknown || result.Overall.Level != hahealth.Unknown {
		t.Fatalf("idle cluster incorrectly assessed: %+v", result)
	}
	var withEvidence struct {
		Evidence   confirmationEvidence `json:"confirmation_evidence"`
		ObservedAt time.Time            `json:"observed_at"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &withEvidence); err != nil || withEvidence.Evidence.Status != "no_fresh_sample" || withEvidence.ObservedAt.IsZero() ||
		withEvidence.ObservedAt.Before(result.Members[0].SampledAt) {
		t.Fatalf("idle confirmation evidence=%+v err=%v", withEvidence, err)
	}
	// Index movement by itself must not turn the confirmation card green.
	confirmedBarrier = true
	response = httptest.NewRecorder()
	s.health(response, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !usedEpochRecord || !usedRequiredWaitMetric || result.Confirmation.Level != hahealth.Healthy || result.Overall.Level != hahealth.Healthy {
		t.Fatalf("confirmed barrier did not use required wait and shared epoch rule: %+v queryRecord=%v queryWait=%v", result, usedEpochRecord, usedRequiredWaitMetric)
	}
	testedSecondPrimary := false
	for _, item := range vector {
		metric := item["metric"].(map[string]string)
		if metric["__name__"] != "halro_cluster_role" || metric["instance"] != "halro-1" {
			continue
		}
		testedSecondPrimary = true
		metric["role"] = "primary"
		response = httptest.NewRecorder()
		s.health(response, httptest.NewRequest(http.MethodGet, "/api/health", nil))
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil ||
			result.Safety.Level != hahealth.Critical || result.Confirmation.Level != hahealth.Unknown || result.Overall.Level == hahealth.Healthy {
			t.Fatalf("second Primary inherited internal confirmation success: %+v err=%v", result, err)
		}
		metric["role"] = "replica"
		response = httptest.NewRecorder()
		s.health(response, httptest.NewRequest(http.MethodGet, "/api/health", nil))
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil ||
			result.Safety.Level != hahealth.Healthy || result.Confirmation.Level != hahealth.Healthy || result.Overall.Level != hahealth.Healthy {
			t.Fatalf("role recovery did not restore complete observation: %+v err=%v", result, err)
		}
		break
	}
	if !testedSecondPrimary {
		t.Fatal("second Primary metric scenario was not exercised")
	}
	baselineLength := len(vector)
	addMember("halro-2", "replica")
	add("halro-0", "halro_replication_peer_connected", map[string]string{"peer": "halro-2"}, "1")
	add("halro-1", "halro_replication_peer_connected", map[string]string{"peer": "halro-2"}, "1")
	add("halro-2", "halro_replication_peer_connected", map[string]string{"peer": "halro-0"}, "1")
	add("halro-2", "halro_replication_peer_connected", map[string]string{"peer": "halro-1"}, "1")
	s.members = append(s.members, "halro-2")
	response = httptest.NewRecorder()
	s.health(response, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	result = hahealth.Result{}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil ||
		result.Safety.Level != hahealth.Healthy || result.Catchup.Level != hahealth.Healthy ||
		result.Confirmation.Level != hahealth.Healthy || result.Overall.Level != hahealth.Healthy {
		t.Fatalf("complete three-member observation not healthy: %+v err=%v", result, err)
	}
	testedThreeMemberPrimaryConflict := false
	for _, item := range vector {
		metric := item["metric"].(map[string]string)
		if metric["__name__"] != "halro_cluster_role" || metric["instance"] != "halro-2" {
			continue
		}
		testedThreeMemberPrimaryConflict = true
		metric["role"] = "primary"
		response = httptest.NewRecorder()
		s.health(response, httptest.NewRequest(http.MethodGet, "/api/health", nil))
		result = hahealth.Result{}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil ||
			result.Safety.Level != hahealth.Critical || result.Catchup.Level != hahealth.Unknown ||
			result.Confirmation.Level != hahealth.Unknown || result.Overall.Level == hahealth.Healthy {
			t.Fatalf("three-member second Primary retained a green card: %+v err=%v", result, err)
		}
		metric["role"] = "replica"
		break
	}
	if !testedThreeMemberPrimaryConflict {
		t.Fatal("three-member Primary conflict was not exercised")
	}
	completeVector := append([]map[string]any(nil), vector...)
	for missingIndex, missing := range []string{"halro-1", "halro-2"} {
		vector = make([]map[string]any, 0, len(completeVector)-1)
		for _, item := range completeVector {
			metric := item["metric"].(map[string]string)
			if metric["__name__"] == "halro_replication_index" && metric["kind"] == "applied" && metric["instance"] == missing {
				continue
			}
			vector = append(vector, item)
		}
		response = httptest.NewRecorder()
		s.health(response, httptest.NewRequest(http.MethodGet, "/api/health", nil))
		result = hahealth.Result{}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil ||
			result.Safety.Level != hahealth.Unknown || result.Catchup.Level != hahealth.Unknown ||
			result.Confirmation.Level != hahealth.Healthy || result.Overall.Level != hahealth.Unknown ||
			len(result.Members) != 3 || result.Members[missingIndex+1].Applied != nil {
			t.Fatalf("missing %s progress inherited other Replica evidence: %+v err=%v", missing, result, err)
		}
	}
	vector = completeVector
	response = httptest.NewRecorder()
	s.health(response, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	result = hahealth.Result{}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.Overall.Level != hahealth.Healthy {
		t.Fatalf("three-member progress recovery not healthy: %+v err=%v", result, err)
	}
	vector = vector[:baselineLength]
	s.members = s.members[:2]
	for _, metricName := range []string{"up", "halro_cluster_role"} {
		t.Run("missing instance label on "+metricName, func(t *testing.T) {
			metric := map[string]string{"__name__": metricName}
			if metricName == "halro_cluster_role" {
				metric["role"] = "replica"
			}
			vector = append(vector, map[string]any{"metric": metric, "value": []any{now, "1"}})
			response := httptest.NewRecorder()
			s.health(response, httptest.NewRequest(http.MethodGet, "/api/health", nil))
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			var got hahealth.Result
			if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Safety.Level != hahealth.Unknown || got.Overall.Level != hahealth.Unknown ||
				len(got.Unexpected) != 1 || !got.Unexpected[0].IdentityMissing || got.Unexpected[0].Observed || got.Unexpected[0].SampledAt == nil {
				t.Fatalf("unattributable source was hidden: %+v", got)
			}
			vector = vector[:len(vector)-1]
			response = httptest.NewRecorder()
			s.health(response, httptest.NewRequest(http.MethodGet, "/api/health", nil))
			got = hahealth.Result{}
			if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil || got.Overall.Level != hahealth.Healthy || len(got.Unexpected) != 0 {
				t.Fatalf("health did not recover after removing unattributable source: %+v err=%v", got, err)
			}
		})
	}
	durableArchive, err := openDurableArchive(filepath.Join(t.TempDir(), "durable.json"), "test", "ha", []string{"halro-0", "halro-1"})
	if err != nil {
		t.Fatal(err)
	}
	s.durableArchive = durableArchive
	response = httptest.NewRecorder()
	s.health(response, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.Overall.Level != hahealth.Unknown {
		t.Fatalf("uncollected durable chain left overall green: %+v err=%v", result, err)
	}
	if err := durableArchive.Close(); err != nil {
		t.Fatal(err)
	}
	s.durableArchive = nil
	if err := json.Unmarshal(response.Body.Bytes(), &withEvidence); err != nil || withEvidence.Evidence.Status != "observed" ||
		withEvidence.Evidence.Instance != "halro-0" || withEvidence.Evidence.Increase5m == nil || *withEvidence.Evidence.Increase5m != 1 ||
		withEvidence.Evidence.SampledAt.IsZero() || withEvidence.Evidence.EpochSampledAt.IsZero() || withEvidence.Evidence.EvaluatedAt.IsZero() {
		t.Fatalf("confirmed evidence=%+v err=%v", withEvidence, err)
	}
	confirmationCoverageFresh = false
	response = httptest.NewRecorder()
	s.health(response, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.Confirmation.Level != hahealth.Unknown || result.Overall.Level != hahealth.Unknown {
		t.Fatalf("old confirmation counter left the confirmation card green: %+v err=%v", result, err)
	}
	confirmationCoverageFresh = true
	epochFresh = false
	response = httptest.NewRecorder()
	s.health(response, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.Confirmation.Level != hahealth.Unknown || result.Overall.Level != hahealth.Unknown {
		t.Fatalf("old stable-epoch rule sample left the confirmation card green: %+v err=%v", result, err)
	}
	epochFresh = true
	for _, item := range vector {
		metric := item["metric"].(map[string]string)
		if metric["__name__"] == "halro_cluster_term" && metric["instance"] == "halro-1" {
			item["value"] = []any{now - 15, "7"}
			response = httptest.NewRecorder()
			s.health(response, httptest.NewRequest(http.MethodGet, "/api/health", nil))
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.Safety.Level != hahealth.Unknown || result.Overall.Level != hahealth.Unknown {
				t.Fatalf("metric omitted by the latest successful scrape stayed green: %+v err=%v", result, err)
			}
			item["value"] = []any{now - 40, "7"}
			response = httptest.NewRecorder()
			s.health(response, httptest.NewRequest(http.MethodGet, "/api/health", nil))
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.Safety.Level != hahealth.Unknown || result.Overall.Level != hahealth.Unknown {
				t.Fatalf("old scrape sample received a fresh query timestamp: %+v err=%v", result, err)
			}
			item["value"] = []any{now, "7"}
			break
		}
	}
	healthTokenFile := filepath.Join(t.TempDir(), "health-status-token")
	if err := os.WriteFile(healthTokenFile, []byte("health-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	primaryReadyStatus := http.StatusServiceUnavailable
	s.statusCollectors = map[string]statusCollector{}
	for _, member := range []struct{ node, role string }{{"halro-0", "primary"}, {"halro-1", "replica"}} {
		member := member
		payload := fmt.Sprintf(`{"mode":"ha","cluster_id":"ha","incarnation":"inc_1","node_id":%q,"role":%q,"term":7,"promised_term":7,"durable_index":12,"confirmed_index":12,"applied_index":12}`, member.node, member.role)
		s.statusCollectors[member.node] = statusCollector{config: statusMemberConfig{NodeID: member.node, URL: "https://member.invalid/ha/status",
			LiveURL: "https://member.invalid/health/live", ReadyURL: "https://member.invalid/health/ready", TokenFile: healthTokenFile},
			client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if request.URL.Path == "/ha/status" {
					return jsonResponse(payload), nil
				}
				status := http.StatusOK
				if request.URL.Path == "/health/ready" && member.role == "primary" {
					status = primaryReadyStatus
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
			})}}
	}
	response = httptest.NewRecorder()
	s.health(response, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.Overall.Level != hahealth.Degraded {
		t.Fatalf("known Primary not-ready did not degrade overview: %+v err=%v", result, err)
	}
	primaryReadyStatus = http.StatusFound
	response = httptest.NewRecorder()
	s.health(response, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.Overall.Level != hahealth.Unknown {
		t.Fatalf("readiness redirect did not make overview unknown: %+v err=%v", result, err)
	}
	s.statusCollectors = nil
	duplicateConfirmation = true
	response = httptest.NewRecorder()
	s.health(response, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(response.Body.Bytes(), &withEvidence); err != nil {
		t.Fatal(err)
	}
	if result.Confirmation.Level != hahealth.Unknown || withEvidence.Evidence.Status != "ambiguous" {
		t.Fatalf("duplicate confirmation evidence trusted: result=%+v evidence=%+v", result, withEvidence)
	}
	duplicateConfirmation = false
	for _, archive := range []*eventArchive{
		{doc: archiveDocument{PolledAt: time.Now().UTC()}, lastErr: "journal_write_failed"},
		{doc: archiveDocument{PolledAt: time.Now().Add(-time.Minute).UTC()}},
		{doc: archiveDocument{PolledAt: time.Now().UTC(), CollectionErrors: []string{"halro-1"}}},
	} {
		s.archive = archive
		response = httptest.NewRecorder()
		s.health(response, httptest.NewRequest(http.MethodGet, "/api/health", nil))
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.Overall.Level != hahealth.Unknown {
			t.Fatalf("configured event archive failure left overview green: %+v archive=%+v", result, archive.view())
		}
	}
	s.archive = nil
	statusTokenFile := filepath.Join(t.TempDir(), "status-token")
	if err := os.WriteFile(statusTokenFile, []byte("status-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, role           string
		term, promised       uint64
		startup, unavailable bool
	}{
		{"role", "replica", 7, 7, true, false},
		{"term", "primary", 8, 8, true, false},
		{"promise", "primary", 7, 8, true, false},
		{"startup", "primary", 7, 7, false, false},
		{"unavailable", "primary", 7, 7, true, true},
	} {
		t.Run("machine metrics disagreement "+tc.name, func(t *testing.T) {
			payload := fmt.Sprintf(`{"mode":"ha","cluster_id":"ha","incarnation":"inc_1","node_id":"halro-0","role":%q,"term":%d,"promised_term":%d,"startup_ready":%t,"replication_unavailable":%t,"durable_index":12,"confirmed_index":12,"applied_index":12}`,
				tc.role, tc.term, tc.promised, tc.startup, tc.unavailable)
			s.statusCollectors = map[string]statusCollector{"halro-0": {config: statusMemberConfig{NodeID: "halro-0", URL: "https://member.invalid/ha/status", TokenFile: statusTokenFile},
				client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return jsonResponse(payload), nil })}}}
			response := httptest.NewRecorder()
			s.health(response, httptest.NewRequest(http.MethodGet, "/api/health", nil))
			var observed struct {
				hahealth.Result
				Statuses []memberStatus `json:"member_statuses"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &observed); err != nil || response.Code != http.StatusOK ||
				observed.Safety.Level != hahealth.Unknown || observed.Overall.Level != hahealth.Unknown ||
				len(observed.Statuses) != 1 || observed.Statuses[0].Error != "metrics_disagreement" {
				t.Fatalf("fresh machine/Metrics disagreement left a green safety conclusion: %+v status=%d err=%v", observed, response.Code, err)
			}
		})
	}
	s.statusCollectors = map[string]statusCollector{
		"halro-0": {config: statusMemberConfig{NodeID: "halro-0", TokenFile: filepath.Join(t.TempDir(), "missing-token")}},
	}
	response = httptest.NewRecorder()
	s.health(response, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	var withStatus struct {
		hahealth.Result
		Statuses []memberStatus `json:"member_statuses"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &withStatus); err != nil {
		t.Fatal(err)
	}
	if withStatus.Overall.Level != hahealth.Unknown || len(withStatus.Statuses) != 1 || withStatus.Statuses[0].Error != "credential_unavailable" {
		t.Fatalf("missing machine status left overview green: %+v", withStatus)
	}
	tokenFile := filepath.Join(t.TempDir(), "status-token")
	if err := os.WriteFile(tokenFile, []byte("test-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.statusCollectors = map[string]statusCollector{}
	for _, member := range []struct{ node, role, digest string }{
		{"halro-0", "primary", strings.Repeat("a", 64)}, {"halro-1", "replica", strings.Repeat("b", 64)},
	} {
		payload := fmt.Sprintf(`{"mode":"ha","cluster_id":"ha","incarnation":"inc_1","node_id":%q,"role":%q,"term":7,"promised_term":7,"durable_index":12,"confirmed_index":12,"applied_index":12,"prefix_digest":{"index":12,"last_frame_term":7,"ordering_head_sha256":%q}}`, member.node, member.role, member.digest)
		s.statusCollectors[member.node] = statusCollector{config: statusMemberConfig{NodeID: member.node, URL: "https://member.invalid/ha/status", TokenFile: tokenFile, RequirePrefixDigest: true},
			client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return jsonResponse(payload), nil })}}
	}
	response = httptest.NewRecorder()
	s.health(response, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Safety.Level != hahealth.Critical || result.Overall.Level != hahealth.Critical {
		t.Fatalf("authenticated prefix conflict left HA green: %+v", result)
	}
	s.statusCollectors = nil
	alertFiring = true
	response = httptest.NewRecorder()
	s.health(response, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Overall.Level != hahealth.Degraded {
		t.Fatalf("firing HA alert left overview green: %+v", result)
	}
	alertFiring = false
	alertsUnavailable = true
	response = httptest.NewRecorder()
	s.health(response, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Overall.Level != hahealth.Unknown {
		t.Fatalf("missing alert observations left overview green: %+v", result)
	}
	alertsUnavailable = false
	for _, item := range vector {
		metric := item["metric"].(map[string]string)
		if metric["__name__"] == "halro_cluster_member_info" && metric["instance"] == "halro-1" {
			metric["node_id"] = "another-node"
		}
	}
	response = httptest.NewRecorder()
	s.health(response, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Safety.Level != hahealth.Critical || result.Confirmation.Level != hahealth.Unknown || result.Overall.Level != hahealth.Critical {
		t.Fatalf("mislabeled Prometheus target left HA green: %+v", result)
	}
}

func TestProbeClientRetriesReplicaWithoutTreatingItAsOutage(t *testing.T) {
	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		role := "replica"
		if calls.Add(1) == 2 {
			role = "primary"
		}
		return jsonResponse(fmt.Sprintf(`{"role":%q,"cluster_id":"ha"}`, role)), nil
	})}
	s := &server{client: client, clientURL: "https://service.invalid/", cluster: "ha", members: []string{"one", "two"}}
	if result := s.probeClient(context.Background()); result.Level != hahealth.Healthy || calls.Load() != 2 {
		t.Fatalf("healthy retry failed: %+v calls=%d", result, calls.Load())
	}
}

func TestProbeClientRejectsWrongOrMissingClusterIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		level      hahealth.Level
	}{
		{"wrong primary", `{"role":"primary","cluster_id":"other"}`, hahealth.Critical},
		{"wrong replica", `{"role":"replica","cluster_id":"other"}`, hahealth.Critical},
		{"missing identity", `{"role":"primary"}`, hahealth.Unknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
				return jsonResponse(tc.body), nil
			})}
			s := &server{client: client, clientURL: "https://service.invalid/", cluster: "ha", members: []string{"one", "two"}}
			if result := s.probeClient(context.Background()); result.Level != tc.level {
				t.Fatalf("result=%+v", result)
			}
		})
	}
}

func TestProbeClientDoesNotHideIdentityGapBehindLaterPrimary(t *testing.T) {
	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			return jsonResponse(`{"role":"replica"}`), nil
		}
		return jsonResponse(`{"role":"primary","cluster_id":"ha"}`), nil
	})}
	s := &server{client: client, clientURL: "https://service.invalid/", cluster: "ha", members: []string{"one", "two"}}
	if result := s.probeClient(context.Background()); result.Level != hahealth.Unknown || calls.Load() != 2 {
		t.Fatalf("identity gap masked by later Primary: %+v calls=%d", result, calls.Load())
	}
}

func TestServerRejectsUncredentialedOrNonOriginPrometheus(t *testing.T) {
	for _, address := range []string{"http://remote.example:9090", "https://remote.example:9090",
		"https://remote.example:9090/api/v1/query", "https://user@remote.example:9090/", "https://remote.example:9090/?query=up"} {
		_, _, err := newServer(config{environment: "test", cluster: "ha", members: "one,two", cert: "not-read", key: "not-read", clientCA: "not-read", prometheus: address})
		if err == nil {
			t.Fatalf("uncredentialed or non-origin Prometheus accepted: %s", address)
		}
	}
}

func TestRunbookBaseRequiresSafeHTTPSOriginAndRendersOnlyWhenConfigured(t *testing.T) {
	for _, address := range []string{"http://docs.internal/", "https://docs.internal/private/", "https://user@docs.internal/",
		"https://docs.internal/?token=secret", "https://docs.internal/#fragment", "https://docs.internal/#",
		"https://docs.internal/%2F", "javascript:alert(1)"} {
		if validRunbookBaseURL(address) {
			t.Fatalf("unsafe runbook base accepted: %q", address)
		}
	}
	if !validRunbookBaseURL("https://docs.internal/") {
		t.Fatal("HTTPS documentation root rejected")
	}
	for _, tc := range []struct{ base, want string }{{"", `data-runbook-base=""`}, {"https://docs.internal/", `data-runbook-base="https://docs.internal/"`}} {
		s := &server{runbookBase: tc.base}
		response := httptest.NewRecorder()
		s.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), tc.want) || strings.Contains(response.Body.String(), "__HALRO_RUNBOOK_BASE__") {
			t.Fatalf("runbook base not rendered safely: status=%d body=%s", response.Code, response.Body.String())
		}
	}
}

func TestOperatorEndpointRequiresTrustedClientCertificate(t *testing.T) {
	now := time.Now()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "operator-test-ca"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	issue := func(serial int64, usage x509.ExtKeyUsage) ([]byte, []byte) {
		t.Helper()
		leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		template := &x509.Certificate{
			SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "operator-test"},
			NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
			KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage},
			DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		}
		der, err := x509.CreateCertificate(rand.Reader, template, ca, &leafKey.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		privateDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
		if err != nil {
			t.Fatal(err)
		}
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})
	}
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	serverPEM, serverKey := issue(2, x509.ExtKeyUsageServerAuth)
	clientPEM, clientKey := issue(3, x509.ExtKeyUsageClientAuth)
	clientCertificate, err := tls.X509KeyPair(clientPEM, clientKey)
	if err != nil {
		t.Fatal(err)
	}
	s, tlsConfig, err := newServer(config{
		environment: "test", cluster: "ha", members: "one,two", prometheus: "http://127.0.0.1:9091",
		cert: write("server.crt", serverPEM), key: write("server.key", serverKey),
		clientCA: write("client-ca.crt", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})),
	})
	if err != nil {
		t.Fatal(err)
	}
	endpoint := httptest.NewUnstartedServer(s.routes())
	endpoint.TLS = tlsConfig
	endpoint.StartTLS()
	defer endpoint.Close()
	if response, err := endpoint.Client().Get(endpoint.URL + "/health/live"); err == nil {
		response.Body.Close()
		t.Fatal("operator endpoint accepted a client without a certificate")
	}
	client := endpoint.Client()
	transport := client.Transport.(*http.Transport).Clone()
	transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	transport.TLSClientConfig.Certificates = []tls.Certificate{clientCertificate}
	client.Transport = transport
	response, err := client.Get(endpoint.URL + "/health/live")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("trusted operator certificate rejected: %d", response.StatusCode)
	}
}

func TestAlertsExposeOnlyCurrentClusterHA(t *testing.T) {
	body := `{"status":"success","data":{"alerts":[
		{"labels":{"alertname":"HalroNoPrimary","category":"ha","environment":"test","cluster":"ha"},"annotations":{"summary":"missing","runbook_url":"/runbook"},"state":"firing","activeAt":"2026-09-28T00:00:00Z","value":"1e+00"},
		{"labels":{"alertname":"HalroTargetDown","category":"availability","environment":"test","cluster":"ha"},"state":"firing"},
		{"labels":{"alertname":"OtherCluster","category":"ha","environment":"test","cluster":"other"},"state":"firing"},
		{"labels":{"alertname":"Platform","category":"platform","environment":"test","cluster":"ha"},"state":"firing"}
	]}}`
	client := &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) { return jsonResponse(body), nil })}
	base, _ := url.Parse("http://127.0.0.1:9091")
	s := &server{client: client, base: base, environment: "test", cluster: "ha"}
	response := httptest.NewRecorder()
	s.alerts(response, httptest.NewRequest(http.MethodGet, "/api/alerts", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d: %s", response.Code, response.Body.String())
	}
	var alerts []currentAlert
	if err := json.Unmarshal(response.Body.Bytes(), &alerts); err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 2 || alerts[0].Name != "HalroNoPrimary" || alerts[0].Runbook != "/runbook" || alerts[0].Value != "1e+00" || alerts[1].Name != "HalroTargetDown" {
		t.Fatalf("unexpected alerts: %+v", alerts)
	}
}

func TestAlertRuleEvidenceUsesOnlyLoadedHARules(t *testing.T) {
	var requested url.Values
	body := `{"status":"success","data":{"groups":[
		{"name":"other","rules":[{"name":"HalroNoPrimary","type":"alerting","query":"wrong"}]},
		{"name":"halro-alerts","rules":[{"name":"HalroNoPrimary","type":"alerting","query":"sum(halro_cluster_role{role=\"primary\"}) == 0","duration":120,"health":"ok","lastEvaluation":"2026-09-28T00:01:00Z"}]}
	]}}`
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/api/v1/rules" {
			t.Errorf("unexpected upstream path %q", request.URL.Path)
		}
		requested = request.URL.Query()
		return jsonResponse(body), nil
	})}
	base, _ := url.Parse("http://127.0.0.1:9091")
	s := &server{client: client, base: base}
	response := httptest.NewRecorder()
	s.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/alert-rule?name=HalroNoPrimary", nil))
	if response.Code != http.StatusOK || requested.Get("type") != "alert" || requested.Get("rule_group[]") != "halro-alerts" || requested.Get("rule_name[]") != "HalroNoPrimary" || requested.Get("exclude_alerts") != "true" {
		t.Fatalf("unexpected rule request: status=%d query=%v body=%s", response.Code, requested, response.Body.String())
	}
	var evidence alertRuleEvidence
	if err := json.Unmarshal(response.Body.Bytes(), &evidence); err != nil {
		t.Fatal(err)
	}
	if evidence.Status != "available" || evidence.ForSeconds != 120 || evidence.Health != "ok" || !strings.Contains(evidence.Query, "halro_cluster_role") {
		t.Fatalf("incorrect loaded rule evidence: %+v", evidence)
	}
	for _, name := range []string{"../../api/v1/targets", "OtherAlert", "HalroNoPrimary%0A"} {
		response = httptest.NewRecorder()
		s.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/alert-rule?name="+name, nil))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("unsafe name %q accepted: %d", name, response.Code)
		}
	}
	body = `{"status":"success","data":{"groups":[{"name":"halro-alerts","rules":[
		{"name":"HalroNoPrimary","type":"alerting","query":"one"},
		{"name":"HalroNoPrimary","type":"alerting","query":"two"}
	]}]}}`
	response = httptest.NewRecorder()
	s.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/alert-rule?name=HalroNoPrimary", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"status":"ambiguous"`) || strings.Contains(response.Body.String(), `"query"`) {
		t.Fatalf("ambiguous rule exposed as evidence: %d %s", response.Code, response.Body.String())
	}
}

func TestHistoryScopesAlertSamplesToCurrentCluster(t *testing.T) {
	var expression string
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		expression = request.URL.Query().Get("query")
		return jsonResponse(`{"status":"success","data":{"resultType":"matrix","result":[]}}`), nil
	})}
	base, _ := url.Parse("http://127.0.0.1:9091")
	s := &server{client: client, base: base, environment: "test", cluster: "ha"}
	response := httptest.NewRecorder()
	s.history(response, httptest.NewRequest(http.MethodGet, "/api/history?minutes=60", nil))
	if response.Code != http.StatusOK || !strings.Contains(expression, `ALERTS{environment="test",cluster="ha",category="ha",alertstate="firing"}`) ||
		!strings.Contains(expression, `alertname="HalroTargetDown"`) || !strings.Contains(expression, "halro_cluster_role") || !strings.Contains(expression, "halro_replication_state") ||
		!strings.Contains(expression, "halro_cluster_maintenance") || !strings.Contains(expression, "halro_replication_peer_connected") ||
		!strings.Contains(response.Body.String(), `"sampled_events"`) {
		t.Fatalf("alert history was not scoped: status=%d expression=%q", response.Code, expression)
	}
}

func TestEvidenceExportUsesOnlyFixedScopedQueries(t *testing.T) {
	queries := make(chan string, 3)
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		queries <- request.URL.Path + "?" + request.URL.Query().Get("query")
		if request.URL.Path == "/api/v1/alerts" {
			return jsonResponse(`{"status":"success","data":{"alerts":[]}}`), nil
		}
		return jsonResponse(`{"status":"success","data":{"resultType":"matrix","result":[]}}`), nil
	})}
	base, _ := url.Parse("http://127.0.0.1:9091")
	s := &server{client: client, base: base, environment: "test", cluster: "ha", members: []string{"one", "two"}}
	response := httptest.NewRecorder()
	s.evidence(response, httptest.NewRequest(http.MethodGet, "/api/evidence?minutes=60&query=evil", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("evidence download failed: %d %s", response.Code, response.Body.String())
	}
	var body struct {
		Environment string            `json:"environment"`
		Cluster     string            `json:"cluster"`
		Expected    []string          `json:"expected_members"`
		Window      int               `json:"window_minutes"`
		ClientFinal clientFinalResult `json:"client_final"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Environment != "test" || body.Cluster != "ha" || body.Window != 60 || len(body.Expected) != 2 || body.ClientFinal.Status != "not_configured" {
		t.Fatalf("wrong evidence identity: %+v error=%v", body, err)
	}
	close(queries)
	for query := range queries {
		if strings.Contains(query, "evil") || (strings.Contains(query, "/api/v1/query") && !strings.Contains(query, `cluster="ha"`)) {
			t.Fatalf("unscoped or caller-controlled evidence query: %q", query)
		}
	}
	response = httptest.NewRecorder()
	s.evidence(response, httptest.NewRequest(http.MethodGet, "/api/evidence?minutes=999", nil))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unsupported evidence window accepted: %d", response.Code)
	}
}

func TestConflictingMemberSeriesCannotOverwriteIdentity(t *testing.T) {
	var result prometheusResult
	result.Data.Result = append(result.Data.Result,
		struct {
			Metric map[string]string   `json:"metric"`
			Value  []json.RawMessage   `json:"value"`
			Values [][]json.RawMessage `json:"values"`
		}{Metric: map[string]string{"__name__": "halro_cluster_role", "instance": "halro-0", "role": "primary"}, Value: []json.RawMessage{json.RawMessage("1"), json.RawMessage(`"1"`)}},
		struct {
			Metric map[string]string   `json:"metric"`
			Value  []json.RawMessage   `json:"value"`
			Values [][]json.RawMessage `json:"values"`
		}{Metric: map[string]string{"__name__": "halro_cluster_role", "instance": "halro-0", "role": "replica"}, Value: []json.RawMessage{json.RawMessage("1"), json.RawMessage(`"1"`)}},
	)
	members := parseMembers(result, []string{"halro-0"})
	if len(members) != 1 || !members[0].Conflicted {
		t.Fatalf("conflicting role series hidden: %+v", members)
	}
	result.Data.Result[1].Metric["instance"] = "rogue-0"
	if got := unexpectedMembers(result, []string{"halro-0"}, time.Unix(1, 0), 30*time.Second); len(got) != 1 || got[0].Instance != "rogue-0" || !got[0].Observed {
		t.Fatalf("unconfigured member hidden: %v", got)
	}
}

func TestUnexpectedMemberEvidenceDistinguishesFreshFromUnverifiable(t *testing.T) {
	var result prometheusResult
	body := `{"status":"success","data":{"result":[
		{"metric":{"__name__":"up","instance":"rogue-fresh"},"value":[100,"1"]},
		{"metric":{"__name__":"up","instance":"rogue-down"},"value":[100,"0"]},
		{"metric":{"__name__":"halro_cluster_role","instance":"rogue-stale","role":"replica"},"value":[60,"1"]},
		{"metric":{"__name__":"halro_cluster_role","instance":"rogue-invalid","role":"replica"},"value":[100,"NaN"]},
		{"metric":{"__name__":"halro_cluster_role","instance":"rogue-metric","role":"replica"},"value":[100,"1"]},
		{"metric":{"__name__":"halro_cluster_role","role":"replica"},"value":[100,"1"]}
	]}}`
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatal(err)
	}
	got := unexpectedMembers(result, []string{"halro-0"}, time.Unix(100, 0), 30*time.Second)
	if len(got) != 6 {
		t.Fatalf("unexpected evidence lost: %+v", got)
	}
	byName := make(map[string]hahealth.UnexpectedMember, len(got))
	for _, member := range got {
		byName[member.Instance] = member
	}
	if !byName["rogue-fresh"].Observed || !byName["rogue-metric"].Observed ||
		byName["rogue-down"].Observed || byName["rogue-stale"].Observed || byName["rogue-invalid"].Observed ||
		!byName[""].IdentityMissing || byName[""].Observed || byName[""].SampledAt == nil ||
		!byName[""].SampledAt.Equal(time.Unix(100, 0)) ||
		byName["rogue-stale"].SampledAt == nil || !byName["rogue-stale"].SampledAt.Equal(time.Unix(60, 0)) || byName["rogue-invalid"].SampledAt != nil {
		t.Fatalf("unexpected evidence misclassified: %+v", byName)
	}
}

func TestHAMetricsQueryUsesOnlyExpectedHalroTargetUp(t *testing.T) {
	s := &server{environment: "test", cluster: "ha"}
	expression := s.haMetricsExpression()
	if !strings.Contains(expression, `__name__=~"up|halro_cluster_`) ||
		!strings.Contains(expression, `,environment="test",cluster="ha",job="halro",expected_target="true"}[45s]`) {
		t.Fatalf("HA membership query accepts unrelated up series: %s", expression)
	}
	if history := s.historyExpression(); !strings.Contains(history, `halro_replication_index{environment="test",cluster="ha",job="halro",expected_target="true"}`) {
		t.Fatalf("member history accepts unrelated scrape jobs: %s", history)
	}
}

func TestCurrentMetricsRequiresOrderedRawScrapeSamples(t *testing.T) {
	now := float64(time.Now().Unix())
	resultType := "matrix"
	values := []any{[]any{now - 40, "1"}, []any{now - 20, "0"}}
	forgedInstant := false
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/api/v1/query" || !strings.HasSuffix(request.URL.Query().Get("query"), "[45s]") {
			t.Fatalf("current metrics did not request raw range samples: %s", request.URL)
		}
		series := map[string]any{"metric": map[string]string{"__name__": "up", "instance": "halro-0"}, "values": values}
		if forgedInstant {
			series["value"] = []any{now, "1"}
		}
		body, _ := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": resultType,
			"result": []any{series}}})
		return jsonResponse(string(body)), nil
	})}
	base, _ := url.Parse("http://127.0.0.1:9091")
	s := &server{client: client, base: base, environment: "test", cluster: "ha"}
	observed, err := s.currentMetrics(context.Background(), s.haMetricsExpression())
	if err != nil || observed.Data.ResultType != "vector" || len(observed.Data.Result) != 1 {
		t.Fatalf("raw current samples unavailable: result=%+v err=%v", observed, err)
	}
	value, sampledAt, ok := sample(observed.Data.Result[0].Value)
	if !ok || value != 0 || !sampledAt.Equal(time.Unix(int64(now-20), 0)) {
		t.Fatalf("last scrape value/time lost: value=%v sampledAt=%v ok=%v", value, sampledAt, ok)
	}
	resultType = "vector"
	if _, err := s.currentMetrics(context.Background(), s.haMetricsExpression()); err == nil {
		t.Fatal("instant-vector query timestamp accepted as scrape time")
	}
	resultType = "matrix"
	values = []any{[]any{now - 20, "0"}, []any{now - 40, "1"}}
	if _, err := s.currentMetrics(context.Background(), s.haMetricsExpression()); err == nil {
		t.Fatal("out-of-order raw samples accepted")
	}
	values, forgedInstant = nil, true
	observed, err = s.currentMetrics(context.Background(), s.haMetricsExpression())
	if err != nil || len(observed.Data.Result) != 1 || len(observed.Data.Result[0].Value) != 0 {
		t.Fatalf("matrix without raw samples inherited an instant value: %+v err=%v", observed, err)
	}
}

func TestFreshConfirmationCountersRequiresUniquePrimaryStores(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	makeData := func(stores ...string) prometheusResult {
		items := make([]any, 0, len(stores))
		for _, store := range stores {
			items = append(items, map[string]any{"metric": map[string]string{
				"__name__": "halro_replication_required_confirmation_wait_total", "instance": "halro-0", "outcome": "confirmed", "store": store,
			}, "value": []any{now.Unix(), "1"}})
		}
		encoded, _ := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": items}})
		var data prometheusResult
		if err := json.Unmarshal(encoded, &data); err != nil {
			t.Fatal(err)
		}
		return data
	}
	if sampledAt, ok := freshConfirmationCounters(makeData("ledger", "metadata"), "halro-0", now); !ok || !sampledAt.Equal(now) {
		t.Fatal("complete Primary counter coverage rejected")
	}
	for _, stores := range [][]string{{"ledger"}, {"ledger", "ledger", "metadata"}, {"ledger", "metadata", "other"}} {
		if _, ok := freshConfirmationCounters(makeData(stores...), "halro-0", now); ok {
			t.Fatalf("incomplete or contradictory Primary counters accepted: %v", stores)
		}
	}
}

func TestFreshStableEpochRejectsAmbiguousOrFalseRecord(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	makeData := func(values ...string) prometheusResult {
		items := make([]any, 0, len(values))
		for _, value := range values {
			items = append(items, map[string]any{"metric": map[string]string{"__name__": "halro:ha_epoch_stable:bool", "instance": "halro-0"},
				"value": []any{now.Unix(), value}})
		}
		encoded, _ := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": items}})
		var data prometheusResult
		if err := json.Unmarshal(encoded, &data); err != nil {
			t.Fatal(err)
		}
		return data
	}
	if sampledAt, ok := freshStableEpoch(makeData("1"), "halro-0", now); !ok || !sampledAt.Equal(now) {
		t.Fatal("fresh stable epoch rejected")
	}
	for _, values := range [][]string{nil, {"0"}, {"1", "1"}, {"1", "0"}} {
		if _, ok := freshStableEpoch(makeData(values...), "halro-0", now); ok {
			t.Fatalf("false or ambiguous stable epoch accepted: %v", values)
		}
	}
}

func TestConfirmationEvidenceExpiresWhenEpochRuleSampleExpiresFirst(t *testing.T) {
	now := time.Date(2026, 9, 28, 6, 0, 0, 0, time.UTC)
	evidence := confirmationEvidence{SampledAt: now.Add(-5 * time.Second), EpochSampledAt: now.Add(-29 * time.Second)}
	if !freshConfirmationEvidence(evidence, now) {
		t.Fatal("fresh counter and epoch evidence rejected")
	}
	if freshConfirmationEvidence(evidence, now.Add(2*time.Second)) {
		t.Fatal("stale epoch rule sample kept confirmation evidence fresh")
	}
	evidence.EpochSampledAt = time.Time{}
	if freshConfirmationEvidence(evidence, now) {
		t.Fatal("missing epoch timestamp kept confirmation evidence fresh")
	}
}

func TestInvalidMemberSamplesAndUnconfiguredPeersCannotBecomeHealthy(t *testing.T) {
	for _, tc := range []struct {
		name, metric, value string
	}{
		{"invalid up gauge", `{"__name__":"up","instance":"halro-0"}`, `"2"`},
		{"invalid unavailable gauge", `{"__name__":"halro_replication_unavailable","instance":"halro-0"}`, `"2"`},
		{"invalid incompatible gauge", `{"__name__":"halro_replication_member_incompatible","instance":"halro-0","reason":"schema"}`, `"2"`},
		{"unknown incompatible reason", `{"__name__":"halro_replication_member_incompatible","instance":"halro-0","reason":"other"}`, `"0"`},
		{"lossy frame index", `{"__name__":"halro_replication_index","instance":"halro-0","kind":"durable"}`, `"9007199254740992"`},
		{"unconfigured peer", `{"__name__":"halro_replication_peer_connected","instance":"halro-0","peer":"rogue-0"}`, `"1"`},
		{"self peer", `{"__name__":"halro_replication_peer_connected","instance":"halro-0","peer":"halro-0"}`, `"1"`},
		{"nonfinite sample", `{"__name__":"halro_cluster_term","instance":"halro-0"}`, `"NaN"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var result prometheusResult
			body := `{"status":"success","data":{"result":[{"metric":` + tc.metric + `,"value":[1,` + tc.value + `]},` +
				`{"metric":{"__name__":"up","instance":"halro-0"},"value":[1,"1"]}]}}`
			if err := json.Unmarshal([]byte(body), &result); err != nil {
				t.Fatal(err)
			}
			members := parseMembers(result, []string{"halro-0", "halro-1"})
			if len(members) != 1 || !members[0].Conflicted {
				t.Fatalf("invalid sample did not mark member conflicted: %+v", members)
			}
			if got := hahealth.Evaluate(hahealth.Input{Now: time.Unix(1, 0), MaxSampleAge: 30 * time.Second,
				Expected: []string{"halro-0", "halro-1"}, ClusterID: "ha", Members: members}); got.Safety.Level != hahealth.Critical {
				t.Fatalf("invalid sample did not prevent a green safety conclusion: %+v", got)
			}
		})
	}
}

func TestDuplicateLogicalMemberSeriesCannotMaskTargetAmbiguity(t *testing.T) {
	for name, metric := range map[string]string{
		"duplicate scrape target": `{"__name__":"up","instance":"halro-0"}`,
		"duplicate term":          `{"__name__":"halro_cluster_term","instance":"halro-0"}`,
		"duplicate index kind":    `{"__name__":"halro_replication_index","instance":"halro-0","kind":"confirmed"}`,
	} {
		t.Run(name, func(t *testing.T) {
			var result prometheusResult
			first := strings.TrimSuffix(metric, "}") + `,"scrape_source":"a"}`
			second := strings.TrimSuffix(metric, "}") + `,"scrape_source":"b"}`
			body := `{"status":"success","data":{"result":[{"metric":` + first + `,"value":[1,"1"]},` +
				`{"metric":` + second + `,"value":[1,"1"]}]}}`
			if err := json.Unmarshal([]byte(body), &result); err != nil {
				t.Fatal(err)
			}
			members := parseMembers(result, []string{"halro-0", "halro-1"})
			if len(members) != 1 || !members[0].Conflicted {
				t.Fatalf("same-valued duplicate member series hid ambiguous scrape provenance: %+v", members)
			}
		})
	}
}
