package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestSelectedSeedDecoderEnforcesGraphBoundsBeforePeerDecode(t *testing.T) {
	// The third peer deliberately has no valid peer encoding. Count exhaustion
	// must win before parsing it, so an attacker cannot grow the peer graph.
	payload := []byte(`{"Peers":[{}, {}, "never decode this third peer"]}`)
	if _, err := decodeSelectedSeedMember(payload); !errors.Is(err, errSeedLimit) {
		t.Fatal("third peer parsed before fixed count guard", err)
	}
	for _, raw := range []string{
		`{"Peers":[],"Peers":[]}`,
		`{"Version":1,"Version":1}`,
		`{"Peers":[{"unknown":"field"}]}`,
	} {
		if _, err := decodeSelectedSeedMember([]byte(raw)); err == nil {
			t.Fatal("ambiguous member declaration accepted", raw)
		}
	}
	var declaration selectedSeedState
	for _, raw := range []string{
		`{"source_data_dir":"one","source_data_dir":"two"}`,
		`{"source_member":{},"unknown":"field"}`,
	} {
		if err := json.Unmarshal([]byte(raw), &declaration); err == nil {
			t.Fatal("ambiguous selected declaration accepted", raw)
		}
	}
	if err := declaration.UnmarshalJSON(bytes.Repeat([]byte{' '}, selectedSeedDeclarationBytes+1)); !errors.Is(err, errSeedLimit) {
		t.Fatal("selected declaration grew beyond fixed byte bound", err)
	}
	if _, err := decodeSelectedSeedMember([]byte(`{"Peers":[{"Address":"` + strings.Repeat("x", 4097) + `"}]}`)); !errors.Is(err, errSeedLimit) {
		t.Fatal("unbounded peer address accepted", err)
	}
}
