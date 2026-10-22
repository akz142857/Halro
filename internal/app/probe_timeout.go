package app

import (
	"time"

	"github.com/akz142857/Halro/internal/config"
)

// probeResponseTimeout caps how long a control-plane call waits for an
// upstream to answer: an Admin connection test, the deployment health probe,
// an invocation-target lookup, capability detection, the model catalog fetch.
//
// They all waited gateway.attempt_response_header_timeout, which is the time a
// model may take to its first byte. A reasoning model withholds its headers
// until it has finished reasoning, so that limit has to be minutes; a probe
// asking whether an upstream answers at all does not, and an operator pressing
// Test should not wait minutes to learn a host is dead.
const probeResponseTimeout = time.Minute

// probeTimeout is the configured header timeout, never more than
// probeResponseTimeout, so an install that set it lower keeps its own value.
func probeTimeout(cfg config.Config) time.Duration {
	timeout := cfg.Gateway.AttemptResponseHeaderTimeout.Value()
	if timeout <= 0 {
		return 15 * time.Second
	}
	return min(timeout, probeResponseTimeout)
}
