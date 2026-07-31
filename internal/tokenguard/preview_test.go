package tokenguard

import (
	"testing"
	"time"

	"github.com/akz142857/Heimdall/internal/domain"
)

func TestPreviewUsesExistingWindowWithoutMutatingManager(t *testing.T) {
	result, err := Preview(domain.TokenGuardPolicy{
		ID: "guard", Name: "Guard", Enabled: true, Action: "alert",
		TokensPerMinute: 100, MinimumSamples: 2,
	}, Input{EstimatedTokens: 20}, PreviewWindow{Requests: 4, Tokens: 90})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Violated || result.Reason != "tokens_per_minute" || result.Action != "alert" {
		t.Fatalf("unexpected result: %#v", result)
	}
	if _, err := Preview(domain.TokenGuardPolicy{
		ID: "bad", Name: "Bad", Action: "temporary_block", BlockTTL: time.Minute,
	}, Input{}, PreviewWindow{}); err == nil {
		t.Fatal("invalid policy was accepted")
	}
}
