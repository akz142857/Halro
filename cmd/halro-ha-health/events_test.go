package main

import (
	"encoding/json"
	"testing"
)

func TestSampledEventsBoundTransitionsByAdjacentSamplesAndInventory(t *testing.T) {
	var history prometheusResult
	data := `{"status":"success","data":{"resultType":"matrix","result":[
		{"metric":{"__name__":"halro_cluster_role","instance":"one","role":"primary"},"values":[[100,"1"],[130,"0"],[200,"0"]]},
		{"metric":{"__name__":"halro_cluster_role","instance":"one","role":"replica"},"values":[[100,"0"],[130,"1"],[200,"1"]]},
		{"metric":{"__name__":"halro_cluster_term","instance":"one"},"values":[[100,"1"],[130,"2"],[200,"3"]]},
		{"metric":{"__name__":"halro_replication_state","instance":"one","state":"replicating"},"values":[[100,"1"],[130,"0"],[200,"0"]]},
		{"metric":{"__name__":"halro_replication_state","instance":"one","state":"unavailable"},"values":[[100,"0"],[130,"1"],[200,"1"]]},
		{"metric":{"__name__":"halro_cluster_maintenance","instance":"one"},"values":[[100,"0"],[130,"1"],[200,"1"]]},
		{"metric":{"__name__":"halro_replication_peer_connected","instance":"one","peer":"two"},"values":[[100,"1"],[130,"0"],[200,"0"]]},
		{"metric":{"__name__":"halro_replication_peer_connected","instance":"one","peer":"unconfigured"},"values":[[100,"1"],[130,"0"]]},
		{"metric":{"__name__":"halro_cluster_incarnation_info","instance":"one","incarnation":"inc_1"},"values":[[100,"1"],[130,"1"],[200,"0"]]},
		{"metric":{"__name__":"halro_cluster_incarnation_info","instance":"one","incarnation":"inc_2"},"values":[[100,"0"],[130,"0"],[200,"1"]]},
		{"metric":{"__name__":"halro_cluster_role","instance":"unconfigured","role":"primary"},"values":[[100,"1"],[130,"0"]]}
	]}}`
	if err := json.Unmarshal([]byte(data), &history); err != nil {
		t.Fatal(err)
	}
	events := sampledEvents(history, []string{"one", "two"})
	if len(events) != 5 {
		t.Fatalf("events=%+v, want adjacent role/term/phase/maintenance/peer changes", events)
	}
	kinds := map[string]bool{}
	for _, event := range events {
		if event.Instance != "one" || event.PreviousSeen.Unix() != 100 || event.FirstSeen.Unix() != 130 || event.Source != "Prometheus range samples" {
			t.Fatalf("event provenance or bounds incorrect: %+v", event)
		}
		kinds[event.Kind] = true
	}
	if !kinds["role"] || !kinds["term"] || !kinds["replication_phase"] || !kinds["maintenance"] || !kinds["peer_session"] || kinds["incarnation"] {
		t.Fatalf("unexpected transition types: %+v", events)
	}
	for _, event := range events {
		if event.Kind == "peer_session" && (event.Peer != "two" || event.From != "connected" || event.To != "disconnected") {
			t.Fatalf("peer marker identity or direction incorrect: %+v", event)
		}
	}
}

func TestSampledEventsRejectConflictingIdentityAndPeerSamples(t *testing.T) {
	var history prometheusResult
	data := `{"status":"success","data":{"resultType":"matrix","result":[
		{"metric":{"__name__":"halro_cluster_incarnation_info","instance":"one","incarnation":"inc_1"},"values":[[100,"1"],[130,"1"]]},
		{"metric":{"__name__":"halro_cluster_incarnation_info","instance":"one","incarnation":"inc_2"},"values":[[100,"0"],[130,"1"]]},
		{"metric":{"__name__":"halro_replication_peer_connected","instance":"one","peer":"two","job":"a"},"values":[[100,"1"],[130,"0"]]},
		{"metric":{"__name__":"halro_replication_peer_connected","instance":"one","peer":"two","job":"b"},"values":[[100,"1"],[130,"1"]]}
	]}}`
	if err := json.Unmarshal([]byte(data), &history); err != nil {
		t.Fatal(err)
	}
	if events := sampledEvents(history, []string{"one", "two"}); len(events) != 0 {
		t.Fatalf("conflicting samples yielded change events: %+v", events)
	}
}

func TestSampledEventsRejectConflictingTermSamplesRegardlessOfSeriesOrder(t *testing.T) {
	for _, conflicting := range []string{
		`{"metric":{"__name__":"halro_cluster_term","instance":"one","job":"a"},"values":[[130,"2"]]},
		 {"metric":{"__name__":"halro_cluster_term","instance":"one","job":"b"},"values":[[130,"3"]]}`,
		`{"metric":{"__name__":"halro_cluster_term","instance":"one","job":"b"},"values":[[130,"3"]]},
		 {"metric":{"__name__":"halro_cluster_term","instance":"one","job":"a"},"values":[[130,"2"]]}`,
		`{"metric":{"__name__":"halro_cluster_term","instance":"one","job":"a"},"values":[[130,"2"]]},
		 {"metric":{"__name__":"halro_cluster_term","instance":"one","job":"b"},"values":[[130,"2.5"]]}`,
	} {
		data := `{"status":"success","data":{"resultType":"matrix","result":[
			{"metric":{"__name__":"halro_cluster_term","instance":"one"},"values":[[100,"1"],[160,"4"],[190,"5"]]},` + conflicting + `]}}`
		var history prometheusResult
		if err := json.Unmarshal([]byte(data), &history); err != nil {
			t.Fatal(err)
		}
		events := sampledEvents(history, []string{"one"})
		if len(events) != 1 || events[0].Kind != "term" || events[0].From != "4" || events[0].To != "5" ||
			events[0].PreviousSeen.Unix() != 160 || events[0].FirstSeen.Unix() != 190 {
			t.Fatalf("conflicted term sample generated a false transition: %+v", events)
		}
	}
}

func TestSampledEventsRejectDuplicateRoleGaugeDisagreement(t *testing.T) {
	var history prometheusResult
	data := `{"status":"success","data":{"resultType":"matrix","result":[
		{"metric":{"__name__":"halro_cluster_role","instance":"one","role":"primary"},"values":[[100,"1"],[130,"0"]]},
		{"metric":{"__name__":"halro_cluster_role","instance":"one","role":"replica","job":"a"},"values":[[100,"0"],[130,"1"]]},
		{"metric":{"__name__":"halro_cluster_role","instance":"one","role":"replica","job":"b"},"values":[[130,"0"]]}
	]}}`
	if err := json.Unmarshal([]byte(data), &history); err != nil {
		t.Fatal(err)
	}
	if events := sampledEvents(history, []string{"one"}); len(events) != 0 {
		t.Fatalf("contradictory role gauges generated a role transition: %+v", events)
	}
}
