package app

import (
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
)

// The header timeout is now minutes because a reasoning model sends no header
// until it has finished; a connection test or a health probe must not inherit
// that, and an install that set the header timeout below a minute keeps it.
func TestProbesWaitAtMostAMinuteWhateverAModelMayTake(t *testing.T) {
	defaults := config.Default()
	if header := defaults.Gateway.AttemptResponseHeaderTimeout.Value(); header <= probeResponseTimeout {
		t.Fatalf("default header timeout %s no longer exceeds the probe cap, so this test checks nothing", header)
	}
	for _, tc := range []struct {
		header, want time.Duration
	}{
		{defaults.Gateway.AttemptResponseHeaderTimeout.Value(), time.Minute},
		{20 * time.Second, 20 * time.Second},
		{0, 15 * time.Second},
	} {
		cfg := config.Default()
		cfg.Gateway.AttemptResponseHeaderTimeout = config.Duration(tc.header)
		if got := probeTimeout(cfg); got != tc.want {
			t.Fatalf("header %s: probe timeout %s, want %s", tc.header, got, tc.want)
		}
	}
}
