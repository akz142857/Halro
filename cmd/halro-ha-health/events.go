package main

import (
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"time"
)

const maxSampledEvents = 200

// sampledEvent is evidence of a changed value at two adjacent Prometheus
// samples. Its times are observation bounds, not the transition's exact time.
type sampledEvent struct {
	Instance     string    `json:"instance"`
	Peer         string    `json:"peer,omitempty"`
	Kind         string    `json:"kind"`
	From         string    `json:"from,omitempty"`
	To           string    `json:"to,omitempty"`
	PreviousSeen time.Time `json:"previous_seen,omitempty"`
	FirstSeen    time.Time `json:"first_seen"`
	Source       string    `json:"source"`
}

type eventSample struct {
	role, term, incarnation, phase, maintenance                                         string
	roleConflict, termConflict, incarnationConflict, phaseConflict, maintenanceConflict bool
	roleValues, incarnationValues, phaseValues                                          map[string]string
	peers                                                                               map[string]string
	peerConflicts                                                                       map[string]bool
}

func sampledEvents(history prometheusResult, expected []string) []sampledEvent {
	allowed := make(map[string]bool, len(expected))
	for _, member := range expected {
		allowed[member] = true
	}
	byMember := make(map[string]map[int64]*eventSample, len(expected))
	for _, series := range history.Data.Result {
		instance := series.Metric["instance"]
		if !allowed[instance] {
			continue
		}
		name := series.Metric["__name__"]
		if name != "halro_cluster_role" && name != "halro_cluster_term" && name != "halro_cluster_incarnation_info" &&
			name != "halro_replication_state" && name != "halro_cluster_maintenance" && name != "halro_replication_peer_connected" {
			continue
		}
		for _, pair := range series.Values {
			if len(pair) != 2 {
				continue
			}
			var timestamp float64
			var value string
			if json.Unmarshal(pair[0], &timestamp) != nil || json.Unmarshal(pair[1], &value) != nil ||
				math.IsNaN(timestamp) || math.IsInf(timestamp, 0) || timestamp <= 0 {
				continue
			}
			at := int64(timestamp)
			if byMember[instance] == nil {
				byMember[instance] = make(map[int64]*eventSample)
			}
			point := byMember[instance][at]
			if point == nil {
				point = &eventSample{}
				byMember[instance][at] = point
			}
			switch name {
			case "halro_cluster_role":
				role := series.Metric["role"]
				if role != "primary" && role != "replica" && role != "awaiting_decision" {
					point.roleConflict = true
				} else if eventGaugeActive(&point.roleValues, role, value, &point.roleConflict) {
					assignEventValue(&point.role, role, &point.roleConflict)
				}
			case "halro_cluster_term":
				if parsed, err := strconv.ParseUint(value, 10, 64); err == nil {
					assignEventValue(&point.term, strconv.FormatUint(parsed, 10), &point.termConflict)
				} else {
					point.termConflict = true
				}
			case "halro_cluster_incarnation_info":
				incarnation := series.Metric["incarnation"]
				if incarnation == "" {
					point.incarnationConflict = true
				} else if eventGaugeActive(&point.incarnationValues, incarnation, value, &point.incarnationConflict) {
					assignEventValue(&point.incarnation, incarnation, &point.incarnationConflict)
				}
			case "halro_replication_state":
				phase := series.Metric["state"]
				if phase != "replicating" && phase != "unavailable" {
					point.phaseConflict = true
				} else if eventGaugeActive(&point.phaseValues, phase, value, &point.phaseConflict) {
					assignEventValue(&point.phase, phase, &point.phaseConflict)
				}
			case "halro_cluster_maintenance":
				if value == "0" || value == "1" {
					maintenance := "normal"
					if value == "1" {
						maintenance = "maintenance"
					}
					assignEventValue(&point.maintenance, maintenance, &point.maintenanceConflict)
				} else {
					point.maintenanceConflict = true
				}
			case "halro_replication_peer_connected":
				peer := series.Metric["peer"]
				if !allowed[peer] || peer == instance {
					continue
				}
				if point.peers == nil {
					point.peers = map[string]string{}
					point.peerConflicts = map[string]bool{}
				}
				if value != "0" && value != "1" {
					point.peerConflicts[peer] = true
					continue
				}
				connected := "disconnected"
				if value == "1" {
					connected = "connected"
				}
				if existing := point.peers[peer]; existing != "" && existing != connected {
					point.peerConflicts[peer] = true
				}
				point.peers[peer] = connected
			}
		}
	}
	events := make([]sampledEvent, 0)
	for _, instance := range expected {
		points := byMember[instance]
		times := make([]int64, 0, len(points))
		for at := range points {
			times = append(times, at)
		}
		sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
		var previous *eventSample
		var previousAt int64
		for _, at := range times {
			current := points[at]
			if previous != nil && at-previousAt <= 45 && at > previousAt {
				for _, change := range []struct{ kind, from, to string }{
					{"role", previous.role, current.role},
					{"term", previous.term, current.term},
					{"incarnation", previous.incarnation, current.incarnation},
					{"replication_phase", previous.phase, current.phase},
					{"maintenance", previous.maintenance, current.maintenance},
				} {
					if change.from == "" || change.to == "" || change.from == change.to ||
						change.kind == "role" && (previous.roleConflict || current.roleConflict) ||
						change.kind == "term" && (previous.termConflict || current.termConflict) ||
						change.kind == "incarnation" && (previous.incarnationConflict || current.incarnationConflict) ||
						change.kind == "replication_phase" && (previous.phaseConflict || current.phaseConflict) ||
						change.kind == "maintenance" && (previous.maintenanceConflict || current.maintenanceConflict) {
						continue
					}
					events = append(events, sampledEvent{
						Instance: instance, Kind: change.kind, From: change.from, To: change.to,
						PreviousSeen: time.Unix(previousAt, 0).UTC(), FirstSeen: time.Unix(at, 0).UTC(), Source: "Prometheus range samples",
					})
				}
				for peer, from := range previous.peers {
					to := current.peers[peer]
					if to == "" || to == from || previous.peerConflicts[peer] || current.peerConflicts[peer] {
						continue
					}
					events = append(events, sampledEvent{Instance: instance, Peer: peer, Kind: "peer_session", From: from, To: to,
						PreviousSeen: time.Unix(previousAt, 0).UTC(), FirstSeen: time.Unix(at, 0).UTC(), Source: "Prometheus range samples"})
				}
			}
			previous, previousAt = current, at
		}
	}
	sort.Slice(events, func(i, j int) bool {
		if !events[i].FirstSeen.Equal(events[j].FirstSeen) {
			return events[i].FirstSeen.After(events[j].FirstSeen)
		}
		if events[i].Instance != events[j].Instance {
			return events[i].Instance < events[j].Instance
		}
		if events[i].Kind != events[j].Kind {
			return events[i].Kind < events[j].Kind
		}
		return events[i].Peer < events[j].Peer
	})
	if len(events) > maxSampledEvents {
		events = events[:maxSampledEvents]
	}
	return events
}

func assignEventValue(destination *string, value string, conflicted *bool) {
	if value == "" {
		return
	}
	if *destination != "" && *destination != value {
		*conflicted = true
	}
	*destination = value
}

func eventGaugeActive(values *map[string]string, label, value string, conflicted *bool) bool {
	if value != "0" && value != "1" {
		*conflicted = true
		return false
	}
	if *values == nil {
		*values = make(map[string]string)
	}
	if previous, exists := (*values)[label]; exists && previous != value {
		*conflicted = true
	}
	(*values)[label] = value
	return value == "1"
}
