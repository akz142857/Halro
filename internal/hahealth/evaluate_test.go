package hahealth

import (
	"testing"
	"time"
)

func flag(value bool) *bool      { return &value }
func index(value uint64) *uint64 { return &value }

func fixture(now time.Time) Input {
	member := func(name, role string) Member {
		return Member{
			Instance: name, ClusterID: "ha", NodeID: name, SampledAt: now, UpSampledAt: now, NewestSampledAt: now, Up: flag(true), Role: role,
			Incarnation: "inc_1", Term: index(7), PromisedTerm: index(7),
			Durable: index(12), Confirmed: index(12), Applied: index(12),
			StartupReady: flag(true), Unavailable: flag(false),
			Maintenance: flag(false), Incompatible: flag(false),
			IncompatibleReasons: map[string]*bool{"schema": flag(false), "key_challenge": flag(false), "spki": flag(false)},
		}
	}
	primary := member("halro-0", "primary")
	primary.Peers = map[string]*bool{"halro-1": flag(true), "halro-2": flag(true)}
	replica1 := member("halro-1", "replica")
	replica1.Peers = map[string]*bool{"halro-0": flag(true), "halro-2": flag(true)}
	replica2 := member("halro-2", "replica")
	replica2.Peers = map[string]*bool{"halro-0": flag(true), "halro-1": flag(true)}
	return Input{
		Now: now, MaxSampleAge: 30 * time.Second,
		Expected: []string{"halro-0", "halro-1", "halro-2"}, ClusterID: "ha",
		Members:              []Member{primary, replica1, replica2},
		ClientProbe:          Signal{Healthy, "client Service reached Primary"},
		ObservedConfirmation: flag(true),
	}
}

func TestEvaluateRequiresActualConfirmationEvidence(t *testing.T) {
	now := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
	in := fixture(now)
	in.ObservedConfirmation = nil
	result := Evaluate(in)
	if result.Confirmation.Level != Unknown || result.Overall.Level != Unknown {
		t.Fatalf("idle cluster became healthy without ACK evidence: %+v", result)
	}
	in.Members[0].Peers["halro-1"] = flag(false)
	in.Members[0].Peers["halro-2"] = flag(false)
	result = Evaluate(in)
	if result.Confirmation.Level != Degraded || result.Overall.Level != Degraded {
		t.Fatalf("disconnected peers remained healthy: %+v", result)
	}
}

func TestEvaluateKeepsObservedPrimaryBlockVisibleWithReplicaCoverageLoss(t *testing.T) {
	now := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
	in := fixture(now)
	in.Members[0].Unavailable = flag(true)
	in.Members[1].Up = flag(false)
	in.Members[2].Up = flag(false)
	result := Evaluate(in)
	if result.Confirmation.Level != Degraded || result.Safety.Level != Unknown ||
		result.Catchup.Level != Unknown || result.Overall.Level != Degraded {
		t.Fatalf("observed Primary block hidden by down Replicas: %+v", result)
	}

	for name, mutate := range map[string]func(*Input){
		"stale Primary":          func(in *Input) { in.Members[0].SampledAt = now.Add(-31 * time.Second) },
		"conflicted Primary":     func(in *Input) { in.Members[0].Conflicted = true },
		"wrong Primary identity": func(in *Input) { in.Members[0].NodeID = "other" },
		"second Primary":         func(in *Input) { in.Members[1].Up = flag(true); in.Members[1].Role = "primary" },
		"unknown observed role":  func(in *Input) { in.Members[1].Up = flag(true); in.Members[1].Role = "" },
		"unexpected member": func(in *Input) {
			in.Unexpected = []UnexpectedMember{{Instance: "other", Observed: true}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			in := in
			in.Members = append([]Member(nil), in.Members...)
			mutate(&in)
			if got := Evaluate(in).Confirmation.Level; got != Unknown {
				t.Fatalf("ambiguous Primary block level=%s, want unknown", got)
			}
		})
	}
}

func TestEvaluateRequiresCompleteMemberHASignalInventory(t *testing.T) {
	now := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
	in := fixture(now)
	in.Members[1].SampledAt = now.Add(-15 * time.Second)
	if result := Evaluate(in); result.Safety.Level != Unknown || result.Overall.Level == Healthy {
		t.Fatalf("previous-scrape metric was treated as part of the latest scrape: %+v", result)
	}
	in = fixture(now)
	in.Members[1].UpSampledAt = now.Add(-15 * time.Second)
	if result := Evaluate(in); result.Safety.Level != Unknown || result.Overall.Level == Healthy {
		t.Fatalf("previous-scrape up was matched to newer member metrics: %+v", result)
	}
	for name, mutate := range map[string]func(*Input){
		"primary maintenance missing": func(in *Input) { in.Members[0].Maintenance = nil },
		"replica startup missing":     func(in *Input) { in.Members[1].StartupReady = nil },
		"replica unavailable missing": func(in *Input) { in.Members[1].Unavailable = nil },
		"replica peer missing":        func(in *Input) { delete(in.Members[1].Peers, "halro-2") },
		"reason missing":              func(in *Input) { delete(in.Members[2].IncompatibleReasons, "spki") },
	} {
		t.Run(name, func(t *testing.T) {
			in := fixture(now)
			mutate(&in)
			result := Evaluate(in)
			if result.Safety.Level != Unknown || result.Overall.Level == Healthy {
				t.Fatalf("missing HA signal became safe: %+v", result)
			}
		})
	}
	in = fixture(now)
	in.Members[0].Maintenance = nil
	in.Members[2].PromisedTerm = index(8)
	if result := Evaluate(in); result.Safety.Level != Critical {
		t.Fatalf("known higher promise was hidden by another member's missing signal: %+v", result.Safety)
	}
	in = fixture(now)
	in.Members[1].SampledAt = now.Add(-31 * time.Second)
	in.Members[2].PromisedTerm = index(8)
	if result := Evaluate(in); result.Safety.Level != Critical {
		t.Fatalf("fresh higher promise was hidden by another member's stale scrape: %+v", result.Safety)
	}
	for name, mutate := range map[string]func(*Input){
		"replica maintenance": func(in *Input) { in.Members[1].Maintenance = flag(true) },
		"replica unready":     func(in *Input) { in.Members[1].StartupReady = flag(false) },
		"replica unavailable": func(in *Input) { in.Members[1].Unavailable = flag(true) },
	} {
		t.Run(name, func(t *testing.T) {
			in := fixture(now)
			mutate(&in)
			result := Evaluate(in)
			if result.Safety.Level != Degraded || result.Overall.Level == Healthy {
				t.Fatalf("member HA condition became safe: %+v", result)
			}
		})
	}
}

func TestEvaluateFailsClosedOnMissingAndConflictingMembers(t *testing.T) {
	now := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
	for name, mutate := range map[string]func(*Input){
		"missing": func(in *Input) { in.Members = in.Members[:2] },
		"stale":   func(in *Input) { in.Members[2].SampledAt = now.Add(-31 * time.Second) },
		"down":    func(in *Input) { in.Members[2].Up = flag(false) },
	} {
		t.Run(name, func(t *testing.T) {
			in := fixture(now)
			mutate(&in)
			result := Evaluate(in)
			if result.Overall.Level == Healthy {
				t.Fatalf("incomplete observation became healthy: %+v", result)
			}
		})
	}
	in := fixture(now)
	in.Members[1].Role = "primary"
	if result := Evaluate(in); result.Safety.Level != Critical {
		t.Fatalf("multiple Primaries not critical: %+v", result.Safety)
	}
	in = fixture(now)
	in.Members[1].Incarnation = "inc_2"
	if result := Evaluate(in); result.Safety.Level != Critical {
		t.Fatalf("incarnation conflict not critical: %+v", result.Safety)
	}
	in = fixture(now)
	in.Members[1].Applied = index(13)
	if result := Evaluate(in); result.Safety.Level != Critical {
		t.Fatalf("invalid index ordering not critical: %+v", result.Safety)
	}
	in = fixture(now)
	in.Members[1].Durable = nil
	in.Members[1].Applied = index(13)
	if result := Evaluate(in); result.Safety.Level != Critical {
		t.Fatalf("known confirmed/applied inversion hidden by missing durable index: %+v", result.Safety)
	}
	in = fixture(now)
	in.Members[1].Applied = nil
	in.Members[1].Confirmed = index(13)
	if result := Evaluate(in); result.Safety.Level != Critical {
		t.Fatalf("known durable/confirmed inversion hidden by missing applied index: %+v", result.Safety)
	}
	in = fixture(now)
	in.Members[1].PromisedTerm = index(6)
	if result := Evaluate(in); result.Safety.Level != Critical || result.Overall.Level != Critical {
		t.Fatalf("member promised term below its term not critical: %+v", result)
	}
	in = fixture(now)
	in.Members[1].Conflicted = true
	if result := Evaluate(in); result.Safety.Level != Critical || result.Overall.Level != Critical {
		t.Fatalf("conflicting member observations not critical: %+v", result)
	}
	in = fixture(now)
	in.Unexpected = []UnexpectedMember{{Instance: "rogue-0", Observed: true}}
	if result := Evaluate(in); result.Safety.Level != Critical || len(result.Unexpected) != 1 {
		t.Fatalf("unconfigured member not surfaced: %+v", result)
	}
	in.Unexpected[0].Observed = false
	if result := Evaluate(in); result.Safety.Level != Unknown || result.Overall.Level == Healthy {
		t.Fatalf("unverifiable extra series was treated as a current safety conflict or ignored: %+v", result)
	}
	in.Unexpected = []UnexpectedMember{{IdentityMissing: true, Observed: true}}
	if result := Evaluate(in); result.Safety.Level != Unknown || result.Overall.Level == Healthy {
		t.Fatalf("unidentified member series was treated as a verified extra member or ignored: %+v", result)
	}
	in = fixture(now)
	in.Members[1].NodeID = "halro-rogue"
	if result := Evaluate(in); result.Safety.Level != Critical || result.Confirmation.Level != Unknown || result.Catchup.Level != Unknown {
		t.Fatalf("scrape instance spoofed a node identity: %+v", result)
	}
	in = fixture(now)
	in.Members[1].ClusterID = "other-ha"
	if result := Evaluate(in); result.Safety.Level != Critical {
		t.Fatalf("scrape cluster label spoofed a member identity: %+v", result.Safety)
	}
	in = fixture(now)
	in.Members[1].NodeID = ""
	if result := Evaluate(in); result.Safety.Level != Unknown {
		t.Fatalf("missing member identity stayed green: %+v", result.Safety)
	}
}

func TestEvaluateReportsObservedCatchupWithoutPromisingPromotion(t *testing.T) {
	now := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
	in := fixture(now)
	result := Evaluate(in)
	if result.Overall.Level != Healthy {
		t.Fatalf("expected fully observed fixture healthy, got %+v", result)
	}
	in.Members[1].Applied = index(11)
	in.Members[2].Applied = index(11)
	result = Evaluate(in)
	if result.Catchup.Level != Degraded || result.Overall.Level != Degraded {
		t.Fatalf("lagging Replicas appeared caught up: %+v", result)
	}
	in = fixture(now)
	in.Members[1].Durable = index(13)
	in.Members[1].Confirmed = index(13)
	in.Members[1].Applied = index(13)
	result = Evaluate(in)
	if result.Safety.Level != Unknown || result.Catchup.Level != Healthy || result.Overall.Level != Unknown {
		t.Fatalf("Replica ahead of observed Primary was hidden by another caught-up Replica: %+v", result)
	}
}

func TestEvaluateCardsRejectAmbiguousOrPartialMemberEvidence(t *testing.T) {
	now := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
	for name, mutate := range map[string]func(*Input){
		"second Primary": func(in *Input) { in.Members[2].Role = "primary" },
		"missing role":   func(in *Input) { in.Members[2].Role = "" },
		"missing progress on another Replica": func(in *Input) {
			in.Members[2].Applied = nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			in := fixture(now)
			mutate(&in)
			result := Evaluate(in)
			if result.Catchup.Level != Unknown {
				t.Fatalf("partial member evidence left catchup card healthy: %+v", result)
			}
			if name != "missing progress on another Replica" && result.Confirmation.Level != Unknown {
				t.Fatalf("ambiguous Primary role left confirmation card healthy: %+v", result)
			}
			if result.Overall.Level == Healthy {
				t.Fatalf("partial member evidence left overall healthy: %+v", result)
			}
		})
	}
}

func TestSafetySeparatesMissingAndStaleRolesFromFreshNoPrimary(t *testing.T) {
	now := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
	in := fixture(now)
	in.Members[0].Role = "replica"
	in.Members[1].Role = ""
	if result := Evaluate(in); result.Safety.Level != Unknown || result.Overall.Level == Healthy {
		t.Fatalf("missing role was treated as a confirmed no-Primary state: %+v", result)
	}
	in = fixture(now)
	in.Members[0].Role = "replica"
	if result := Evaluate(in); result.Safety.Level != Critical {
		t.Fatalf("fresh complete no-Primary state was not critical: %+v", result.Safety)
	}
	in = fixture(now)
	in.Members[1].Role = "primary"
	in.Members[1].SampledAt = now.Add(-31 * time.Second)
	if result := Evaluate(in); result.Safety.Level != Unknown || result.Overall.Level == Healthy {
		t.Fatalf("stale Primary role caused a current conflict: %+v", result)
	}
	in = fixture(now)
	in.Members[1].PromisedTerm = index(6)
	in.Members[1].SampledAt = now.Add(-31 * time.Second)
	if result := Evaluate(in); result.Safety.Level != Unknown {
		t.Fatalf("stale promise caused a current safety conflict: %+v", result.Safety)
	}
	in = fixture(now)
	in.Members[1].Applied = index(13)
	in.Members[2].SampledAt = now.Add(-31 * time.Second)
	if result := Evaluate(in); result.Safety.Level != Critical {
		t.Fatalf("fresh index inversion was hidden by another stale member: %+v", result.Safety)
	}
}

func TestNonnegativeIndexRejectsLossySamples(t *testing.T) {
	for _, value := range []float64{-1, 1.5, 1 << 53} {
		if _, ok := NonnegativeIndex(value); ok {
			t.Fatalf("accepted invalid index %v", value)
		}
	}
	if value, ok := NonnegativeIndex(12); !ok || value != 12 {
		t.Fatalf("valid index rejected: %d %v", value, ok)
	}
}
