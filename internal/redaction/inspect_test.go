package redaction

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// ProcessJSON walked values only. A secret in a member name, or a card number
// written as a number rather than a string, passed every check — which broke the
// property the native paths depend on: that everything accepted is inspected.
func TestInspectJSONCoversMemberNamesAndNumbers(t *testing.T) {
	engine := NewDefault()
	gatewayKey := "gw_" + strings.Repeat("A", 44)
	for _, testCase := range []struct {
		name    string
		payload string
		wantErr bool
	}{
		{"secret in a value", `{"note":"` + gatewayKey + `"}`, true},
		{"secret in a member name", `{"` + gatewayKey + `":1}`, true},
		{"secret in a nested member name", `{"outer":[{"` + gatewayKey + `":true}]}`, true},
		{"benign document", `{"user_id":"tenant-42","count":7}`, false},
		{"long number is not corrupted", `{"amount":123456789012345678}`, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := engine.InspectJSON("", "inbound", json.RawMessage(testCase.payload))
			if (err != nil) != testCase.wantErr {
				t.Fatalf("InspectJSON=%v, wantErr=%v", err, testCase.wantErr)
			}
		})
	}
}

func TestInspectJSONReportsRewriteSeparatelyFromRejection(t *testing.T) {
	engine := NewDefault()
	// The mandatory outbound baseline sanitises rather than refuses, so a secret
	// on the way out is a rewrite, not a rule rejection.
	err := engine.InspectJSON("", "outbound", json.RawMessage(`{"note":"gw_`+strings.Repeat("A", 44)+`"}`))
	if !errors.Is(err, ErrRewriteRequired) || !errors.Is(err, ErrPolicyRejected) {
		t.Fatalf("want a rewrite that also reads as a policy rejection, got %v", err)
	}
	if err := engine.InspectJSON("", "outbound", json.RawMessage(`{"note":`)); !errors.Is(err, ErrMalformedJSON) {
		t.Fatalf("want ErrMalformedJSON, got %v", err)
	}
}

// The inspector answers the question Stream cannot: how much of the original is
// confirmed unchanged. Confirmation lags the input by the withheld window and
// catches up when the channel closes.
func TestStreamInspectorConfirmsCleanTextAndCatchesUpOnClose(t *testing.T) {
	engine := NewDefault()
	inspector, err := engine.NewStreamInspector("")
	if err != nil {
		t.Fatal(err)
	}
	// Longer than the widest mandatory rule (~2 KiB), or nothing is released
	// until the channel closes — which is the withheld window working, not a bug.
	text := strings.Repeat("the quick brown fox ", 400)
	confirmed, err := inspector.Push("0:text_delta", false, text)
	if err != nil {
		t.Fatal(err)
	}
	if confirmed <= 0 || confirmed >= int64(len(text)) {
		t.Fatalf("confirmed=%d for %d bytes: want a lagging, non-zero prefix", confirmed, len(text))
	}
	final, err := inspector.Close("0:text_delta")
	if err != nil {
		t.Fatal(err)
	}
	if final != int64(len(text)) {
		t.Fatalf("close confirmed %d of %d bytes", final, len(text))
	}
}

func TestStreamInspectorRefusesTextTheBaselineWouldRewrite(t *testing.T) {
	engine := NewDefault()
	inspector, err := engine.NewStreamInspector("")
	if err != nil {
		t.Fatal(err)
	}
	secret := "gw_" + strings.Repeat("A", 44)
	// Split across two pushes: the whole point of the rolling window.
	if _, err := inspector.Push("0:text_delta", false, "prefix "+secret[:20]); err != nil {
		t.Fatalf("first half should still be inconclusive, got %v", err)
	}
	if _, err := inspector.Push("0:text_delta", false, secret[20:]+" suffix"); err != nil {
		t.Fatalf("second half is still inside the withheld window, got %v", err)
	}
	// Closing is what forces the withheld suffix through the rules; a stream that
	// ends without it leaves exactly this case uninspected.
	if _, err := inspector.Close("0:text_delta"); !errors.Is(err, ErrPolicyRejected) {
		t.Fatalf("want the joined secret refused on close, got %v", err)
	}
}
