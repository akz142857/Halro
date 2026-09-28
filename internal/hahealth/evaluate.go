// Package hahealth evaluates observations of an HA cluster without modifying it.
package hahealth

import (
	"math"
	"sort"
	"time"
)

type Level string

const (
	Healthy  Level = "healthy"
	Degraded Level = "degraded"
	Critical Level = "critical"
	Unknown  Level = "unknown"
)

type Signal struct {
	Level  Level  `json:"level"`
	Reason string `json:"reason"`
}

// Member is one member's independently scraped, member-local observation.
// A nil value means unobserved, and must never be treated as zero.
type Member struct {
	Instance            string           `json:"instance"`
	ClusterID           string           `json:"cluster_id,omitempty"`
	NodeID              string           `json:"node_id,omitempty"`
	SampledAt           time.Time        `json:"sampled_at"`
	UpSampledAt         time.Time        `json:"up_sampled_at,omitempty"`
	NewestSampledAt     time.Time        `json:"newest_sampled_at,omitempty"`
	Up                  *bool            `json:"up,omitempty"`
	Role                string           `json:"role,omitempty"`
	Incarnation         string           `json:"incarnation,omitempty"`
	Term                *uint64          `json:"term,omitempty"`
	PromisedTerm        *uint64          `json:"promised_term,omitempty"`
	Durable             *uint64          `json:"durable,omitempty"`
	Confirmed           *uint64          `json:"confirmed,omitempty"`
	Applied             *uint64          `json:"applied,omitempty"`
	StartupReady        *bool            `json:"startup_ready,omitempty"`
	Unavailable         *bool            `json:"replication_unavailable,omitempty"`
	Maintenance         *bool            `json:"maintenance,omitempty"`
	Incompatible        *bool            `json:"incompatible,omitempty"`
	IncompatibleReasons map[string]*bool `json:"incompatible_reasons,omitempty"`
	Peers               map[string]*bool `json:"peers,omitempty"`
	Conflicted          bool             `json:"conflicted,omitempty"`
}

type Input struct {
	Now          time.Time
	MaxSampleAge time.Duration
	Expected     []string
	ClusterID    string
	Members      []Member
	Unexpected   []UnexpectedMember
	ClientProbe  Signal
	// ObservedConfirmation is true only when a Primary returned success from a
	// required confirmation barrier recently. A nil value is unknown, including
	// an idle cluster. This does not assert a final client response.
	ObservedConfirmation *bool
}

// UnexpectedMember is a series outside the deployment inventory or without an
// instance label. Observed means a fresh successful scrape or member metric
// proves an identified extra member exists now. An unidentified, older,
// failed, or malformed series still blocks a green conclusion.
type UnexpectedMember struct {
	Instance        string     `json:"instance"`
	IdentityMissing bool       `json:"identity_missing,omitempty"`
	Observed        bool       `json:"observed"`
	SampledAt       *time.Time `json:"sampled_at,omitempty"`
}

type Result struct {
	Overall      Signal             `json:"overall"`
	Client       Signal             `json:"client"`
	Confirmation Signal             `json:"confirmation"`
	Safety       Signal             `json:"safety"`
	Catchup      Signal             `json:"catchup"`
	Members      []Member           `json:"members"`
	Unexpected   []UnexpectedMember `json:"unexpected_members,omitempty"`
}

func Evaluate(in Input) Result {
	result := Result{Client: in.ClientProbe}
	if result.Client.Level == "" {
		result.Client = Signal{Unknown, "client probe not observed"}
	}
	byInstance := make(map[string]Member, len(in.Members))
	for _, member := range in.Members {
		byInstance[member.Instance] = member
	}
	result.Members = make([]Member, 0, len(in.Expected))
	result.Unexpected = append([]UnexpectedMember(nil), in.Unexpected...)
	sort.Slice(result.Unexpected, func(i, j int) bool { return result.Unexpected[i].Instance < result.Unexpected[j].Instance })
	complete := len(in.Expected) > 0 && in.ClusterID != ""
	for _, instance := range in.Expected {
		member, ok := byInstance[instance]
		if !ok {
			member = Member{Instance: instance}
			complete = false
		} else if member.ClusterID != in.ClusterID || member.NodeID != instance ||
			!freshObservation(member, in.Now, in.MaxSampleAge) {
			complete = false
		}
		result.Members = append(result.Members, member)
	}
	sort.Slice(result.Members, func(i, j int) bool { return result.Members[i].Instance < result.Members[j].Instance })
	result.Safety = safety(result.Members, complete, result.Unexpected, in.ClusterID, in.Now, in.MaxSampleAge)
	result.Confirmation = confirmation(result.Members, complete, in.ObservedConfirmation,
		result.Unexpected, in.ClusterID, in.Now, in.MaxSampleAge)
	result.Catchup = catchup(result.Members, complete)
	result.Overall = Combine(result.Client, result.Confirmation, result.Safety, result.Catchup)
	return result
}

func freshObservation(member Member, now time.Time, maxAge time.Duration) bool {
	age := now.Sub(member.SampledAt)
	// The oldest observed member signal must belong to the same scrape as up.
	// Otherwise a metric omitted by the latest successful scrape can still look
	// fresh for most of MaxSampleAge while up has already advanced.
	oldestSkew := member.UpSampledAt.Sub(member.SampledAt)
	newestSkew := member.NewestSampledAt.Sub(member.UpSampledAt)
	return member.Up != nil && *member.Up && !member.SampledAt.IsZero() && !member.UpSampledAt.IsZero() &&
		!member.NewestSampledAt.IsZero() && age >= 0 && age <= maxAge &&
		oldestSkew >= -time.Second && oldestSkew <= time.Second &&
		newestSkew >= -time.Second && newestSkew <= time.Second
}

func safety(members []Member, complete bool, unexpected []UnexpectedMember, expectedCluster string, now time.Time, maxAge time.Duration) Signal {
	primaryCount := 0
	incarnation := ""
	for _, m := range members {
		if !freshObservation(m, now, maxAge) {
			continue
		}
		if m.Conflicted {
			return Signal{Critical, "conflicting observations for one member identity"}
		}
		if (m.NodeID != "" && m.NodeID != m.Instance) || (m.ClusterID != "" && expectedCluster != "" && m.ClusterID != expectedCluster) {
			return Signal{Critical, "member-reported identity differs from scrape inventory"}
		}
		if m.Role == "primary" {
			primaryCount++
		}
		if m.Incarnation != "" {
			if incarnation != "" && incarnation != m.Incarnation {
				return Signal{Critical, "members report different incarnations"}
			}
			incarnation = m.Incarnation
		}
		if m.Term != nil && m.PromisedTerm != nil && *m.PromisedTerm < *m.Term {
			return Signal{Critical, "a member promised a term below its current term"}
		}
		if (m.Durable != nil && m.Confirmed != nil && *m.Durable < *m.Confirmed) ||
			(m.Confirmed != nil && m.Applied != nil && *m.Confirmed < *m.Applied) {
			return Signal{Critical, "a member reports invalid index ordering"}
		}
	}
	if primaryCount > 1 {
		return Signal{Critical, "multiple members report Primary"}
	}
	for _, member := range unexpected {
		if member.Observed && !member.IdentityMissing {
			return Signal{Critical, "unconfigured HA member observed"}
		}
	}
	if primaryCount == 1 {
		var primaryTerm *uint64
		for _, member := range members {
			if member.Role == "primary" && freshObservation(member, now, maxAge) {
				primaryTerm = member.Term
			}
		}
		if primaryTerm != nil && *primaryTerm > 0 {
			for _, member := range members {
				if freshObservation(member, now, maxAge) && member.PromisedTerm != nil && *member.PromisedTerm > *primaryTerm {
					return Signal{Critical, "a member promised a term above the observed Primary"}
				}
			}
		}
	}
	if len(unexpected) > 0 {
		return Signal{Unknown, "unconfigured HA member evidence is stale or incomplete"}
	}
	if !complete || incarnation == "" {
		return Signal{Unknown, "member coverage or identity is incomplete"}
	}
	for _, m := range members {
		if m.Role == "" {
			return Signal{Unknown, "member role observation is incomplete"}
		}
	}
	if primaryCount == 0 {
		return Signal{Critical, "no member reports Primary"}
	}
	var primary *Member
	for i := range members {
		if members[i].Role == "primary" {
			primary = &members[i]
		}
	}
	if primary == nil || primary.Term == nil || *primary.Term == 0 {
		return Signal{Unknown, "Primary term is missing"}
	}
	for _, m := range members {
		if !freshObservation(m, now, maxAge) {
			continue
		}
		if m.Incompatible != nil && *m.Incompatible {
			return Signal{Degraded, "member observed an incompatible peer"}
		}
		if m.Maintenance != nil && *m.Maintenance || m.StartupReady != nil && !*m.StartupReady || m.Unavailable != nil && *m.Unavailable {
			return Signal{Degraded, "member reports maintenance, startup, or replication unavailability"}
		}
	}
	for _, m := range members {
		if m.ClusterID == "" || m.NodeID == "" || m.Role == "" || m.Term == nil || m.PromisedTerm == nil || m.Durable == nil || m.Confirmed == nil || m.Applied == nil || m.Incompatible == nil {
			return Signal{Unknown, "member safety evidence is incomplete"}
		}
		if m.Role != "primary" && m.Role != "replica" {
			return Signal{Unknown, "member is awaiting a role decision"}
		}
		if !memberSignalsComplete(m, members) {
			return Signal{Unknown, "member HA signal inventory is incomplete"}
		}
	}
	for _, member := range members {
		if member.Role == "replica" && member.Incarnation == primary.Incarnation && *member.Term == *primary.Term &&
			member.SampledAt.Sub(primary.SampledAt) <= 30*time.Second && primary.SampledAt.Sub(member.SampledAt) <= 30*time.Second &&
			*member.Applied > *primary.Confirmed {
			return Signal{Unknown, "Replica applied index exceeds the observed Primary confirmed index"}
		}
	}
	return Signal{Healthy, "one observed Primary and consistent member identities"}
}

func memberSignalsComplete(member Member, members []Member) bool {
	if member.Maintenance == nil || member.StartupReady == nil || member.Unavailable == nil ||
		len(member.IncompatibleReasons) != 3 || len(member.Peers) != len(members)-1 {
		return false
	}
	for _, reason := range []string{"schema", "key_challenge", "spki"} {
		if member.IncompatibleReasons[reason] == nil {
			return false
		}
	}
	for _, other := range members {
		if other.Instance != member.Instance && member.Peers[other.Instance] == nil {
			return false
		}
	}
	return true
}

func confirmation(members []Member, complete bool, observed *bool, unexpected []UnexpectedMember,
	clusterID string, now time.Time, maxSampleAge time.Duration) Signal {
	if !complete {
		if observedPrimaryReplicationBlock(members, unexpected, clusterID, now, maxSampleAge) {
			return Signal{Degraded, "Primary reports an internal replication block"}
		}
		return Signal{Unknown, "Primary confirmation evidence is incomplete"}
	}
	var primary *Member
	for i := range members {
		switch members[i].Role {
		case "primary":
			if primary != nil {
				return Signal{Unknown, "Primary confirmation evidence is ambiguous"}
			}
			primary = &members[i]
		case "replica":
		default:
			return Signal{Unknown, "member role observation is incomplete"}
		}
	}
	if primary == nil || primary.StartupReady == nil || primary.Unavailable == nil {
		return Signal{Unknown, "Primary confirmation evidence is incomplete"}
	}
	if !*primary.StartupReady {
		return Signal{Critical, "Primary has not completed startup adjudication"}
	}
	if *primary.Unavailable {
		return Signal{Degraded, "Primary reports an internal replication block"}
	}
	if len(primary.Peers) == 0 {
		return Signal{Unknown, "peer sessions were not observed"}
	}
	connected := false
	for _, peer := range primary.Peers {
		if peer == nil {
			return Signal{Unknown, "peer session observation is missing"}
		}
		connected = connected || *peer
	}
	if !connected {
		return Signal{Degraded, "Primary has no authenticated Replica session"}
	}
	if observed == nil || !*observed {
		return Signal{Unknown, "no recent required confirmation success observed"}
	}
	return Signal{Healthy, "Primary recently completed a required confirmation barrier"}
}

// A failed member scrape prevents a positive confirmation claim, but cannot
// erase a current, unambiguous block reported by the observed Primary itself.
func observedPrimaryReplicationBlock(members []Member, unexpected []UnexpectedMember,
	clusterID string, now time.Time, maxSampleAge time.Duration) bool {
	if clusterID == "" || len(unexpected) != 0 {
		return false
	}
	primaryCount := 0
	blocked := false
	for _, member := range members {
		if !freshObservation(member, now, maxSampleAge) {
			continue
		}
		if member.Conflicted || member.ClusterID != clusterID || member.NodeID != member.Instance {
			return false
		}
		switch member.Role {
		case "primary":
			primaryCount++
			blocked = member.StartupReady != nil && *member.StartupReady &&
				member.Unavailable != nil && *member.Unavailable
		case "replica":
		default:
			return false
		}
	}
	return primaryCount == 1 && blocked
}

func catchup(members []Member, complete bool) Signal {
	if !complete {
		return Signal{Unknown, "Replica coverage is incomplete"}
	}
	var primary *Member
	replicas := 0
	for i := range members {
		m := &members[i]
		switch m.Role {
		case "primary":
			if primary != nil {
				return Signal{Unknown, "Primary progress evidence is ambiguous"}
			}
			primary = &members[i]
		case "replica":
			replicas++
			if m.Applied == nil || m.Durable == nil || m.Confirmed == nil || m.Term == nil ||
				m.Maintenance == nil || m.Incompatible == nil || m.StartupReady == nil {
				return Signal{Unknown, "Replica progress evidence is incomplete"}
			}
		default:
			return Signal{Unknown, "member role observation is incomplete"}
		}
	}
	if primary == nil || primary.Confirmed == nil || primary.Term == nil {
		return Signal{Unknown, "Primary confirmed index is missing"}
	}
	if replicas == 0 {
		return Signal{Unknown, "no Replica role observed"}
	}
	for _, m := range members {
		if m.Role != "replica" {
			continue
		}
		if m.Incarnation == primary.Incarnation && *m.Term == *primary.Term && *m.StartupReady && !*m.Maintenance && !*m.Incompatible &&
			*m.Durable == *m.Confirmed && *m.Confirmed == *m.Applied && *m.Applied == *primary.Confirmed {
			return Signal{Healthy, "at least one Replica has an observed matching index"}
		}
	}
	return Signal{Degraded, "no Replica has an observed matching index"}
}

// Combine returns the highest-severity observation without hiding a known
// hazard behind an unavailable lower-priority source.
func Combine(signals ...Signal) Signal {
	order := []Level{Critical, Degraded, Unknown, Healthy}
	for _, level := range order {
		for _, signal := range signals {
			if signal.Level == level {
				return Signal{level, signal.Reason}
			}
		}
	}
	return Signal{Unknown, "no health evidence"}
}

// NonnegativeIndex converts Prometheus's floating-point sample representation
// only when it represents an exact nonnegative integer.
func NonnegativeIndex(value float64) (uint64, bool) {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value >= 1<<53 || math.Trunc(value) != value {
		return 0, false
	}
	return uint64(value), true
}
