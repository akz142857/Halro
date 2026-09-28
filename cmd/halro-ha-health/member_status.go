package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

type statusMemberConfig struct {
	NodeID                         string `json:"node_id"`
	URL                            string `json:"url"`
	LiveURL                        string `json:"live_url"`
	ReadyURL                       string `json:"ready_url"`
	TokenFile                      string `json:"token_file"`
	CAFile                         string `json:"ca_file"`
	CertFile                       string `json:"cert_file"`
	KeyFile                        string `json:"key_file"`
	RequirePrefixDigest            bool   `json:"require_prefix_digest"`
	RequireLiveTransitions         bool   `json:"require_live_transitions"`
	RequireAvailabilityTransitions bool   `json:"require_availability_transitions"`
	RequireReplicaStageTransitions bool   `json:"require_replica_stage_transitions"`
}

type prefixDigest struct {
	Index              uint64 `json:"index"`
	LastFrameTerm      uint64 `json:"last_frame_term"`
	OrderingHeadSHA256 string `json:"ordering_head_sha256"`
}

type liveTransition struct {
	Sequence         uint64    `json:"sequence"`
	At               time.Time `json:"at"`
	Kind             string    `json:"kind"`
	FromRole         string    `json:"from_role"`
	ToRole           string    `json:"to_role"`
	FromTerm         uint64    `json:"from_term"`
	ToTerm           uint64    `json:"to_term"`
	FromPromisedTerm uint64    `json:"from_promised_term"`
	ToPromisedTerm   uint64    `json:"to_promised_term"`
}

type liveTransitionHistory struct {
	PublisherStartedAt time.Time        `json:"publisher_started_at"`
	Dropped            uint64           `json:"dropped"`
	Events             []liveTransition `json:"events"`
}

type availabilityTransition struct {
	Sequence uint64    `json:"sequence"`
	At       time.Time `json:"at"`
	From     string    `json:"from"`
	To       string    `json:"to"`
	Reason   string    `json:"reason"`
}

type availabilityTransitionHistory struct {
	CoordinatorStartedAt time.Time                `json:"coordinator_started_at"`
	Current              string                   `json:"current"`
	Dropped              uint64                   `json:"dropped"`
	Events               []availabilityTransition `json:"events"`
}

type replicaStageTransition struct {
	Sequence    uint64    `json:"sequence"`
	At          time.Time `json:"at"`
	Stage       string    `json:"stage"`
	From        string    `json:"from"`
	To          string    `json:"to"`
	Reason      string    `json:"reason"`
	TargetIndex uint64    `json:"target_index"`
}

type replicaStageHistory struct {
	ReceiverStartedAt time.Time                `json:"receiver_started_at"`
	ReceiveState      string                   `json:"receive_state"`
	ApplyState        string                   `json:"apply_state"`
	Dropped           uint64                   `json:"dropped"`
	Events            []replicaStageTransition `json:"events"`
}

type statusCollector struct {
	config        statusMemberConfig
	client        *http.Client
	expectedPeers map[string]bool
}

type memberStatus struct {
	NodeID                  string                         `json:"node_id"`
	Source                  string                         `json:"source"`
	SampledAt               time.Time                      `json:"sampled_at,omitempty"`
	Error                   string                         `json:"error,omitempty"`
	HealthProbeError        string                         `json:"health_probe_error,omitempty"`
	HealthLive              *bool                          `json:"health_live,omitempty"`
	HealthReady             *bool                          `json:"health_ready,omitempty"`
	ClusterID               string                         `json:"cluster_id,omitempty"`
	Incarnation             string                         `json:"incarnation,omitempty"`
	Role                    string                         `json:"role,omitempty"`
	Term                    uint64                         `json:"term,omitempty"`
	PromisedTerm            uint64                         `json:"promised_term,omitempty"`
	DurableIndex            uint64                         `json:"durable_index"`
	ConfirmedIndex          uint64                         `json:"confirmed_index"`
	AppliedIndex            uint64                         `json:"applied_index"`
	StartupReady            bool                           `json:"startup_ready"`
	PrefixDigest            *prefixDigest                  `json:"prefix_digest,omitempty"`
	LiveTransitions         *liveTransitionHistory         `json:"live_transitions,omitempty"`
	AvailabilityTransitions *availabilityTransitionHistory `json:"availability_transitions,omitempty"`
	ReplicaStageTransitions *replicaStageHistory           `json:"replica_stage_transitions,omitempty"`
	ReplicationUnavailable  *bool                          `json:"replication_unavailable,omitempty"`
	Projection              struct {
		Index            uint64 `json:"index"`
		MetadataEpoch    uint64 `json:"metadata_epoch"`
		MetadataSequence uint64 `json:"metadata_sequence"`
	} `json:"projection"`
	Peers []struct {
		NodeID    string `json:"node_id"`
		Connected bool   `json:"connected"`
	} `json:"peers"`
}

func loadStatusCollectors(path string, members []string) (map[string]statusCollector, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read member status manifest: %w", err)
	}
	var manifest struct {
		Members []statusMemberConfig `json:"members"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("decode member status manifest: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("member status manifest has trailing data")
	}
	if len(manifest.Members) != len(members) {
		return nil, errors.New("member status manifest must cover the exact expected inventory")
	}
	expected := make(map[string]bool, len(members))
	for _, member := range members {
		expected[member] = true
	}
	collectors := make(map[string]statusCollector, len(members))
	for _, entry := range manifest.Members {
		if !expected[entry.NodeID] || entry.NodeID == "" {
			return nil, errors.New("member status manifest contains an unexpected or duplicate node")
		}
		if _, exists := collectors[entry.NodeID]; exists {
			return nil, errors.New("member status manifest contains a duplicate node")
		}
		u, err := url.Parse(entry.URL)
		if err != nil || u == nil || u.Scheme != "https" || u.Hostname() == "" || u.EscapedPath() != "/ha/status" || u.RawQuery != "" || u.Fragment != "" || u.User != nil || u.Opaque != "" {
			return nil, errors.New("member status URL must be a fixed HTTPS /ha/status URL")
		}
		if (entry.LiveURL == "") != (entry.ReadyURL == "") {
			return nil, errors.New("member live and ready URLs must be configured together")
		}
		if entry.LiveURL != "" && (!validMemberHealthURL(entry.LiveURL, u.Hostname(), "/health/live") ||
			!validMemberHealthURL(entry.ReadyURL, u.Hostname(), "/health/ready")) {
			return nil, errors.New("member health URLs must use fixed HTTPS paths on the member status hostname")
		}
		if entry.TokenFile == "" || entry.CAFile == "" || entry.CertFile == "" || entry.KeyFile == "" {
			return nil, errors.New("member status token, CA and client certificate files are required")
		}
		caPEM, err := os.ReadFile(entry.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read member status CA: %w", err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(caPEM) {
			return nil, errors.New("member status CA has no valid certificate")
		}
		if _, err := tls.LoadX509KeyPair(entry.CertFile, entry.KeyFile); err != nil {
			return nil, fmt.Errorf("load member status client identity: %w", err)
		}
		client := &http.Client{Timeout: 4 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Transport: &http.Transport{DisableKeepAlives: true, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots,
				GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
					cert, err := tls.LoadX509KeyPair(entry.CertFile, entry.KeyFile)
					return &cert, err
				},
			}}}
		peers := make(map[string]bool, len(members))
		for _, member := range members {
			if member != entry.NodeID {
				peers[member] = true
			}
		}
		collectors[entry.NodeID] = statusCollector{config: entry, client: client, expectedPeers: peers}
	}
	return collectors, nil
}

func validMemberHealthURL(raw, hostname, path string) bool {
	u, err := url.Parse(raw)
	return err == nil && u != nil && u.Scheme == "https" && strings.EqualFold(u.Hostname(), hostname) &&
		u.EscapedPath() == path && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && u.User == nil && u.Opaque == ""
}

func (c statusCollector) fetch(ctx context.Context, cluster string) (result memberStatus) {
	result = memberStatus{NodeID: c.config.NodeID, Source: "member /ha/status", SampledAt: time.Now().UTC()}
	if c.config.LiveURL != "" {
		liveCh, readyCh := make(chan healthProbeResult, 1), make(chan healthProbeResult, 1)
		go func() { liveCh <- c.probeHealth(ctx, c.config.LiveURL, "live") }()
		go func() { readyCh <- c.probeHealth(ctx, c.config.ReadyURL, "ready") }()
		defer func() {
			live, ready := <-liveCh, <-readyCh
			if live.err == "" {
				result.HealthLive = &live.healthy
			}
			if ready.err == "" {
				result.HealthReady = &ready.healthy
			}
			probeErrors := make([]string, 0, 2)
			for _, outcome := range []healthProbeResult{live, ready} {
				if outcome.err != "" {
					probeErrors = append(probeErrors, outcome.err)
				}
			}
			result.HealthProbeError = strings.Join(probeErrors, ",")
		}()
	}
	token, err := loadMachineStatusToken(c.config.TokenFile)
	if err != nil {
		result.Error = "credential_unavailable"
		return result
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.config.URL, nil)
	if err != nil {
		result.Error = "configuration"
		return result
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := c.client.Do(request)
	if err != nil {
		result.Error = "transport"
		return result
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		result.Error = "authentication"
		return result
	}
	if response.StatusCode != http.StatusOK {
		result.Error = "http_status"
		return result
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	if err != nil || len(body) > 64<<10 {
		result.Error = "response_size"
		return result
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	var payload struct {
		Mode string `json:"mode"`
		memberStatus
	}
	if err := decoder.Decode(&payload); err != nil || payload.Mode != "ha" || payload.NodeID != c.config.NodeID || payload.ClusterID != cluster || payload.Incarnation == "" ||
		(payload.Role != "primary" && payload.Role != "replica" && payload.Role != "awaiting_decision") || payload.PromisedTerm < payload.Term ||
		payload.DurableIndex < payload.ConfirmedIndex || payload.ConfirmedIndex < payload.AppliedIndex {
		result.Error = "identity_or_schema"
		return result
	}
	if c.expectedPeers != nil {
		seen := make(map[string]bool, len(payload.Peers))
		if len(payload.Peers) != len(c.expectedPeers) {
			result.Error = "peer_inventory"
			return result
		}
		for _, peer := range payload.Peers {
			if peer.NodeID == c.config.NodeID || !c.expectedPeers[peer.NodeID] || seen[peer.NodeID] {
				result.Error = "peer_inventory"
				return result
			}
			seen[peer.NodeID] = true
		}
	}
	// These fields belong to the independent collector, not the member's
	// /ha/status response. A member must not forge direct gateway probes or
	// override the collector's own source, time, and error classification.
	payload.memberStatus.Source = ""
	payload.memberStatus.SampledAt = time.Time{}
	payload.memberStatus.Error = ""
	payload.memberStatus.HealthProbeError = ""
	payload.memberStatus.HealthLive = nil
	payload.memberStatus.HealthReady = nil
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		result.Error = "identity_or_schema"
		return result
	}
	if payload.PrefixDigest != nil {
		decoded, err := hex.DecodeString(payload.PrefixDigest.OrderingHeadSHA256)
		if err != nil || len(decoded) != 32 || payload.PrefixDigest.Index != payload.DurableIndex ||
			payload.PrefixDigest.Index == 0 || payload.PrefixDigest.LastFrameTerm == 0 || payload.PrefixDigest.LastFrameTerm > payload.PromisedTerm {
			result.Error = "prefix_schema"
			return result
		}
		payload.PrefixDigest.OrderingHeadSHA256 = hex.EncodeToString(decoded)
	} else if c.config.RequirePrefixDigest && payload.DurableIndex > 0 {
		result.Error = "prefix_missing"
		return result
	}
	if payload.LiveTransitions == nil {
		if c.config.RequireLiveTransitions {
			result.Error = "transitions_missing"
			return result
		}
	} else if !validLiveTransitions(payload.memberStatus, result.SampledAt) {
		result.Error = "transitions_schema"
		return result
	}
	if payload.AvailabilityTransitions == nil {
		if c.config.RequireAvailabilityTransitions && payload.Role == "primary" {
			result.Error = "availability_missing"
			return result
		}
	} else if !validAvailabilityTransitions(payload.memberStatus, result.SampledAt) {
		result.Error = "availability_schema"
		return result
	}
	if payload.ReplicaStageTransitions == nil {
		if c.config.RequireReplicaStageTransitions && payload.Role == "replica" {
			result.Error = "replica_stages_missing"
			return result
		}
	} else if !validReplicaStageTransitions(payload.memberStatus, result.SampledAt) {
		result.Error = "replica_stages_schema"
		return result
	}
	payload.memberStatus.Source = result.Source
	payload.memberStatus.SampledAt = result.SampledAt
	return payload.memberStatus
}

func loadMachineStatusToken(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm()&0o077 != 0 || info.Size() > 512 {
		return "", errors.New("HA status credential unavailable")
	}
	secret, err := os.ReadFile(path)
	if err != nil {
		return "", errors.New("HA status credential unavailable")
	}
	token := strings.TrimSuffix(string(secret), "\n")
	if token == "" || strings.TrimSpace(token) != token || len(token) > 256 || strings.IndexFunc(token, func(r rune) bool {
		return !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_')
	}) >= 0 {
		return "", errors.New("HA status credential unavailable")
	}
	return token, nil
}

type healthProbeResult struct {
	healthy bool
	err     string
}

func (c statusCollector) probeHealth(ctx context.Context, target, kind string) healthProbeResult {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return healthProbeResult{err: kind + "_configuration"}
	}
	response, err := c.client.Do(request)
	if err != nil {
		return healthProbeResult{err: kind + "_transport"}
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
		return healthProbeResult{healthy: true}
	case http.StatusServiceUnavailable:
		return healthProbeResult{healthy: false}
	default:
		return healthProbeResult{err: kind + "_http_status"}
	}
}

func validReplicaStageTransitions(status memberStatus, sampledAt time.Time) bool {
	history := status.ReplicaStageTransitions
	if history == nil || status.Role == "primary" || history.ReceiverStartedAt.IsZero() || history.ReceiverStartedAt.After(sampledAt.Add(30*time.Second)) ||
		len(history.Events) > 128 || history.Dropped > ^uint64(0)-uint64(len(history.Events)) ||
		!validReplicaStageState(history.ReceiveState) || !validReplicaStageState(history.ApplyState) {
		return false
	}
	last := map[string]string{}
	for index, event := range history.Events {
		if event.Sequence != history.Dropped+uint64(index)+1 || event.At.Before(history.ReceiverStartedAt) ||
			event.At.After(sampledAt.Add(30*time.Second)) || (event.Stage != "receive" && event.Stage != "apply") ||
			!validReplicaStageState(event.From) || !validReplicaStageState(event.To) || event.From == event.To ||
			!validReplicaStageReason(event) {
			return false
		}
		if previous, exists := last[event.Stage]; exists && previous != event.From {
			return false
		}
		if _, exists := last[event.Stage]; !exists && history.Dropped == 0 && event.From != "ready" {
			return false
		}
		if index > 0 && event.At.Before(history.Events[index-1].At) {
			return false
		}
		last[event.Stage] = event.To
	}
	if len(history.Events) == 0 {
		return history.Dropped == 0 && history.ReceiveState == "ready" && history.ApplyState == "ready"
	}
	for stage, current := range map[string]string{"receive": history.ReceiveState, "apply": history.ApplyState} {
		if last[stage] != "" && last[stage] != current || last[stage] == "" && history.Dropped == 0 && current != "ready" {
			return false
		}
	}
	return true
}

func validReplicaStageState(value string) bool { return value == "ready" || value == "blocked" }

func validReplicaStageReason(event replicaStageTransition) bool {
	if event.To == "ready" {
		return event.Stage == "apply" && event.Reason == "apply_recovered"
	}
	if event.Stage == "receive" {
		switch event.Reason {
		case "promise_persist_failed", "term_adoption_persist_failed", "confirmation_persist_failed", "sink_persist_failed", "ordering_persist_failed", "durable_state_persist_failed", "apply_state_persist_failed":
			return true
		}
	} else {
		switch event.Reason {
		case "apply_invariant_failed", "ordering_read_failed", "schema_boundary_failed", "ordering_kind_invalid", "projection_failed", "apply_state_advance_failed", "apply_state_persist_failed":
			return true
		}
	}
	return false
}

func validAvailabilityTransitions(status memberStatus, sampledAt time.Time) bool {
	history := status.AvailabilityTransitions
	if history == nil || status.ReplicationUnavailable == nil || history.CoordinatorStartedAt.IsZero() ||
		history.CoordinatorStartedAt.After(sampledAt.Add(30*time.Second)) || len(history.Events) > 128 ||
		history.Dropped > ^uint64(0)-uint64(len(history.Events)) ||
		(history.Current != "replicating" && history.Current != "unavailable") ||
		(*status.ReplicationUnavailable != (history.Current == "unavailable")) {
		return false
	}
	for index, event := range history.Events {
		if event.Sequence != history.Dropped+uint64(index)+1 || event.At.Before(history.CoordinatorStartedAt) ||
			event.At.After(sampledAt.Add(30*time.Second)) || event.From == event.To ||
			(event.From != "replicating" && event.From != "unavailable") ||
			(event.To != "replicating" && event.To != "unavailable") ||
			!validAvailabilityReason(event.Reason) {
			return false
		}
		if index > 0 {
			previous := history.Events[index-1]
			if event.At.Before(previous.At) || event.From != previous.To {
				return false
			}
		}
	}
	if len(history.Events) == 0 {
		return history.Dropped == 0
	}
	return history.Events[len(history.Events)-1].To == history.Current
}

func validAvailabilityReason(reason string) bool {
	switch reason {
	case "notice_queue_failed", "state_persist_failed", "projection_failed", "confirmation_persist_failed",
		"manual_unavailable", "planned_stepdown", "frame_queue_failed", "poisoned", "recovered":
		return true
	}
	return false
}

func validLiveTransitions(status memberStatus, sampledAt time.Time) bool {
	history := status.LiveTransitions
	if history == nil || history.PublisherStartedAt.IsZero() || history.PublisherStartedAt.After(sampledAt.Add(30*time.Second)) ||
		len(history.Events) > 128 || history.Dropped > ^uint64(0)-uint64(len(history.Events)) {
		return false
	}
	for index, event := range history.Events {
		if event.Sequence != history.Dropped+uint64(index)+1 || event.At.Before(history.PublisherStartedAt) ||
			event.At.After(sampledAt.Add(30*time.Second)) || event.FromTerm > event.ToTerm ||
			event.FromPromisedTerm > event.ToPromisedTerm || event.ToPromisedTerm < event.ToTerm ||
			(event.Kind != "promise" && event.Kind != "promote" && event.Kind != "adopt_higher_term") ||
			(event.FromRole != "primary" && event.FromRole != "replica") ||
			(event.ToRole != "primary" && event.ToRole != "replica") {
			return false
		}
		if index > 0 {
			previous := history.Events[index-1]
			if event.At.Before(previous.At) || event.FromRole != previous.ToRole ||
				event.FromTerm != previous.ToTerm || event.FromPromisedTerm != previous.ToPromisedTerm {
				return false
			}
		}
	}
	if len(history.Events) == 0 {
		return history.Dropped == 0
	}
	last := history.Events[len(history.Events)-1]
	return last.ToRole == status.Role && last.ToTerm == status.Term && last.ToPromisedTerm == status.PromisedTerm
}

func (s *server) collectStatuses(ctx context.Context) []memberStatus {
	if len(s.statusCollectors) == 0 {
		return nil
	}
	results := make(chan memberStatus, len(s.statusCollectors))
	for _, collector := range s.statusCollectors {
		collector := collector
		go func() { results <- collector.fetch(ctx, s.cluster) }()
	}
	statuses := make([]memberStatus, 0, len(s.statusCollectors))
	for range s.statusCollectors {
		statuses = append(statuses, <-results)
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].NodeID < statuses[j].NodeID })
	return statuses
}

// markPrefixConflicts compares only authenticated heads at the same exact
// global index in one incarnation. Different indexes are not comparable.
func markPrefixConflicts(statuses []memberStatus) bool {
	type position struct {
		cluster, incarnation string
		index                uint64
	}
	byPosition := make(map[position][]int)
	for index := range statuses {
		status := &statuses[index]
		if status.Error != "" || status.PrefixDigest == nil {
			continue
		}
		key := position{status.ClusterID, status.Incarnation, status.PrefixDigest.Index}
		byPosition[key] = append(byPosition[key], index)
	}
	conflict := false
	for _, indexes := range byPosition {
		if len(indexes) < 2 {
			continue
		}
		first := statuses[indexes[0]].PrefixDigest
		for _, index := range indexes[1:] {
			current := statuses[index].PrefixDigest
			if first.OrderingHeadSHA256 != current.OrderingHeadSHA256 || first.LastFrameTerm != current.LastFrameTerm {
				for _, affected := range indexes {
					statuses[affected].Error = "prefix_conflict"
				}
				conflict = true
				break
			}
		}
	}
	return conflict
}
