package app

import (
	"bufio"
	"bytes"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/gateway"
)

func renderFailureReasons(t *testing.T, reasons gateway.ProviderFailureReasons) string {
	t.Helper()
	var buffer bytes.Buffer
	writer := bufio.NewWriter(&buffer)
	writeProviderFailureReasons(writer, reasons)
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}
	return buffer.String()
}

// A reason that has never fired still gets a series.
//
// An absent series and a series at zero read very differently: the first makes a
// graph and an alert expression silently evaluate to nothing, and the reason
// nobody has seen yet is exactly the one somebody wants to watch for.
func TestFailureReasonMetricPublishesReasonsThatHaveNotHappened(t *testing.T) {
	body := renderFailureReasons(t, gateway.ProviderFailureReasons{})
	for _, reason := range gateway.KnownFailureReasons() {
		want := `halro_provider_failure_reason_total{reason="` + reason + `",provider_status=""} 0`
		if !strings.Contains(body, want) {
			t.Fatalf("reason %q is absent from an empty snapshot:\n%s", reason, body)
		}
	}
}

// The pair is the point. {reason="unclassified", provider_status="402"} is an
// upstream refusal Halro took no meaning from, and it is the observation that
// turns a vendor classification table from recollection into a record — so it
// has to be legible on the endpoint, not only inside the process.
func TestFailureReasonMetricSeparatesTheStatusFromTheReason(t *testing.T) {
	body := renderFailureReasons(t, gateway.ProviderFailureReasons{
		Counts: []gateway.ProviderFailureReasonCount{
			{Reason: "unclassified", Status: 402, Count: 7},
			{Reason: "rate_limited", Status: 429, Count: 3},
		},
		Overflow: 2,
	})
	for _, want := range []string{
		`halro_provider_failure_reason_total{reason="unclassified",provider_status="402"} 7`,
		`halro_provider_failure_reason_total{reason="rate_limited",provider_status="429"} 3`,
		`halro_provider_failure_reason_dropped_total 2`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q:\n%s", want, body)
		}
	}
	// An observed reason is published once, from its observation, rather than
	// twice with a zero beside it.
	if strings.Contains(body, `reason="rate_limited",provider_status=""`) {
		t.Fatalf("an observed reason was also published as an empty-status zero:\n%s", body)
	}
}
