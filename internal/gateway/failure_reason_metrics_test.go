package gateway

import (
	"context"
	"testing"

	"github.com/akz142857/Halro/internal/provider"
)

func failureReasonCount(t *testing.T, snapshot ProviderFailureReasons, reason string, status int) uint64 {
	t.Helper()
	for _, item := range snapshot.Counts {
		if item.Reason == reason && item.Status == status {
			return item.Count
		}
	}
	return 0
}

// A 401 is the one refusal the classifier already reaches a canonical reason
// for without any vendor-specific knowledge, and a 402 is the shape the whole
// evidence exercise exists for: an upstream said "payment required" and nothing
// here knows what that means for this vendor. The metric has to distinguish
// them, because "we classified it" and "we saw it and made nothing of it" send
// an operator to different places.
func TestProviderFailureReasonsSeparateClassifiedFromUnclassified(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		status     int
		wantReason string
	}{
		{name: "credential refused", status: 401, wantReason: string(provider.FailureReasonInvalidCredential)},
		{name: "rate limited", status: 429, wantReason: string(provider.FailureReasonRateLimited)},
		{name: "payment required is not understood", status: 402, wantReason: unclassifiedFailureReason},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			f := newFixture(t, 10_000)
			defer f.close()
			f.adapter.err = &provider.Error{
				Class: provider.ErrorBadRequest, StatusCode: testCase.status, Message: "refused",
			}
			if _, err := f.service.Chat(context.Background(), f.plaintext, chatRequest()); err == nil {
				t.Fatal("the upstream refused but the request succeeded")
			}
			snapshot := f.service.ProviderFailureReasons()
			if got := failureReasonCount(t, snapshot, testCase.wantReason, testCase.status); got != 1 {
				t.Fatalf("reason=%q status=%d count=%d, snapshot=%+v",
					testCase.wantReason, testCase.status, got, snapshot.Counts)
			}
			if snapshot.Overflow != 0 {
				t.Fatalf("overflow=%d", snapshot.Overflow)
			}
		})
	}
}

// A caller hanging up teaches nothing about the upstream, and one frontend
// deploy cancels every request in flight at once. Counting those would put a
// spike of "unclassified" against upstreams that never faltered — the same
// reason reportBreaker refuses to judge a target on this signal.
func TestProviderFailureReasonsIgnoreACallerHangingUp(t *testing.T) {
	f := newFixture(t, 10_000)
	defer f.close()
	f.adapter.err = context.Canceled
	if _, err := f.service.Chat(context.Background(), f.plaintext, chatRequest()); err == nil {
		t.Fatal("the caller cancelled but the request succeeded")
	}
	if snapshot := f.service.ProviderFailureReasons(); len(snapshot.Counts) != 0 {
		t.Fatalf("a cancelled caller was counted as an upstream failure: %+v", snapshot.Counts)
	}
}

// A failure that never reached an upstream carries no status, and it must not
// borrow one. Status 0 is its own observation: the refusal happened on this side.
func TestProviderFailureReasonsRecordATransportRefusalWithoutAStatus(t *testing.T) {
	f := newFixture(t, 10_000)
	defer f.close()
	f.adapter.err = &provider.Error{Class: provider.ErrorConnect, Message: "dial refused"}
	if _, err := f.service.Chat(context.Background(), f.plaintext, chatRequest()); err == nil {
		t.Fatal("the transport refused but the request succeeded")
	}
	snapshot := f.service.ProviderFailureReasons()
	if got := failureReasonCount(t, snapshot, unclassifiedFailureReason, 0); got == 0 {
		t.Fatalf("a transport refusal was not counted: %+v", snapshot.Counts)
	}
}

// The cap is what keeps an upstream from turning this counter into unbounded
// series. Past it nothing new is tracked and the drop is reported, rather than
// the map growing on whatever statuses arrive.
func TestProviderFailureReasonsStopTrackingAtTheCapAndSaySo(t *testing.T) {
	var counters failureReasonCounters
	for status := 0; status < maxTrackedFailureReasons+25; status++ {
		counters.observe(FailureDescriptor{Phase: phaseProvider, ProviderStatus: status})
	}
	snapshot := counters.snapshot()
	if len(snapshot.Counts) != maxTrackedFailureReasons {
		t.Fatalf("tracked %d label sets, cap is %d", len(snapshot.Counts), maxTrackedFailureReasons)
	}
	if snapshot.Overflow != 25 {
		t.Fatalf("overflow=%d, want 25", snapshot.Overflow)
	}
	// A key already being tracked still counts after the cap is reached;
	// otherwise the busiest series would freeze the moment a burst of new ones
	// filled the map.
	counters.observe(FailureDescriptor{Phase: phaseProvider, ProviderStatus: 0})
	if got := failureReasonCount(t, counters.snapshot(), unclassifiedFailureReason, 0); got != 2 {
		t.Fatalf("a tracked label set stopped counting after the cap: %d", got)
	}
}
