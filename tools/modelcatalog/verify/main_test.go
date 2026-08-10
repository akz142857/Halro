package main

import (
	"math"
	"testing"
)

func TestParseArguments(t *testing.T) {
	tests := []struct {
		name, catalog, current string
		arguments              []string
		wantError              bool
	}{
		{name: "candidate", arguments: []string{"candidate.json"}, catalog: "candidate.json"},
		{name: "monotonic", arguments: []string{"--newer-than", "current.json", "candidate.json"}, catalog: "candidate.json", current: "current.json"},
		{name: "missing", wantError: true},
		{name: "unknown flag", arguments: []string{"--other", "current.json", "candidate.json"}, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			catalog, current, err := parseArguments(test.arguments)
			if (err != nil) != test.wantError || catalog != test.catalog || current != test.current {
				t.Fatalf("parseArguments() catalog=%q current=%q err=%v", catalog, current, err)
			}
		})
	}
}

func TestSequenceFromSignedEnvelopePreservesUint64(t *testing.T) {
	payload := []byte(`{"payload":{"sequence":18446744073709551615},"signatures":[]}`)
	sequence, err := sequenceFromSignedEnvelope(payload)
	if err != nil || sequence != math.MaxUint64 {
		t.Fatalf("sequence=%d err=%v", sequence, err)
	}
}

func TestSequenceFromSignedEnvelopeRejectsUnsafeShapes(t *testing.T) {
	tests := [][]byte{
		[]byte(`{"payload":{"sequence":0},"signatures":[]}`),
		[]byte(`{"payload":{"sequence":1},"signatures":[],"unexpected":true}`),
		[]byte(`{"payload":{"sequence":1},"signatures":[]} {}`),
	}
	for _, payload := range tests {
		if _, err := sequenceFromSignedEnvelope(payload); err == nil {
			t.Fatalf("payload %s unexpectedly passed", payload)
		}
	}
}

func TestRequireNewerSequence(t *testing.T) {
	tests := []struct {
		name               string
		candidate, current uint64
		wantError          bool
	}{
		{name: "higher", candidate: 10, current: 9},
		{name: "equal", candidate: 9, current: 9, wantError: true},
		{name: "rollback", candidate: 8, current: 9, wantError: true},
		{name: "uint64 ceiling", candidate: math.MaxUint64, current: math.MaxUint64 - 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := requireNewerSequence(test.candidate, test.current); (err != nil) != test.wantError {
				t.Fatalf("requireNewerSequence(%d, %d) err=%v", test.candidate, test.current, err)
			}
		})
	}
}
