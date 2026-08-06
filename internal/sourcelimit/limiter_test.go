package sourcelimit

import (
	"fmt"
	"net/netip"
	"sync"
	"testing"
	"time"
)

func addr(t *testing.T, text string) netip.Addr {
	t.Helper()
	parsed, err := netip.ParseAddr(text)
	if err != nil {
		t.Fatalf("parse %q: %v", text, err)
	}
	return parsed
}

func TestAllowChargesEachSourceItsOwnBudget(t *testing.T) {
	limiter := New(3, 0)
	now := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)
	first, second := addr(t, "203.0.113.7"), addr(t, "203.0.113.8")
	for attempt := 1; attempt <= 3; attempt++ {
		if allowed, _ := limiter.Allow(first, now); !allowed {
			t.Fatalf("request %d from the first source was rejected inside its budget", attempt)
		}
	}
	allowed, wait := limiter.Allow(first, now)
	if allowed {
		t.Fatal("the fourth request from the first source was admitted past a limit of three")
	}
	if wait != time.Minute {
		t.Fatalf("retry hint = %s, want the remainder of the window (1m)", wait)
	}
	// A second source must not inherit the first one's exhaustion.
	if allowed, _ := limiter.Allow(second, now); !allowed {
		t.Fatal("a different source was rejected because another source had exhausted its own budget")
	}
	if limiter.Rejected() != 1 {
		t.Fatalf("rejected = %d, want 1", limiter.Rejected())
	}
}

func TestWindowRolloverRestoresBudgetAndReleasesTheMap(t *testing.T) {
	limiter := New(1, 0)
	source := addr(t, "203.0.113.9")
	start := time.Date(2026, 8, 7, 10, 0, 30, 0, time.UTC)
	if allowed, _ := limiter.Allow(source, start); !allowed {
		t.Fatal("the first request was rejected")
	}
	allowed, wait := limiter.Allow(source, start)
	if allowed {
		t.Fatal("the second request in the same window was admitted")
	}
	if wait != 30*time.Second {
		t.Fatalf("retry hint = %s, want the 30s remaining in the window", wait)
	}
	next := start.Add(time.Minute)
	if allowed, _ := limiter.Allow(source, next); !allowed {
		t.Fatal("the budget was not restored when the window rolled over")
	}
	limiter.mu.Lock()
	entries := len(limiter.counts)
	limiter.mu.Unlock()
	if entries != 1 {
		t.Fatalf("the rolled-over window holds %d entries, want only the one source seen since the roll", entries)
	}
}

func TestTrackingCeilingSharesOneBudgetInsteadOfGrowing(t *testing.T) {
	const tracked = 4
	limiter := New(2, tracked)
	now := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)
	for index := 0; index < tracked; index++ {
		source := addr(t, fmt.Sprintf("198.51.100.%d", index+1))
		if allowed, _ := limiter.Allow(source, now); !allowed {
			t.Fatalf("source %d was rejected while the map still had room", index)
		}
	}
	// Every address past the ceiling shares one budget, so the flood is shed
	// rather than each spoofed address buying itself a fresh allowance.
	admitted := 0
	for index := 0; index < 50; index++ {
		source := addr(t, fmt.Sprintf("198.51.100.%d", index+100))
		if allowed, _ := limiter.Allow(source, now); allowed {
			admitted++
		}
	}
	if admitted != 2 {
		t.Fatalf("%d of 50 overflow requests were admitted, want exactly the shared budget of 2", admitted)
	}
	limiter.mu.Lock()
	entries := len(limiter.counts)
	limiter.mu.Unlock()
	if entries != tracked {
		t.Fatalf("the map grew to %d entries past a ceiling of %d", entries, tracked)
	}
	if limiter.Overflows() != 50 {
		t.Fatalf("overflows = %d, want all 50 charged to the shared budget", limiter.Overflows())
	}
}

func TestUnresolvableSourceIsChargedNotExempted(t *testing.T) {
	limiter := New(1, 0)
	now := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)
	var unresolved netip.Addr
	if allowed, _ := limiter.Allow(unresolved, now); !allowed {
		t.Fatal("the first unresolvable-source request was rejected")
	}
	if allowed, _ := limiter.Allow(unresolved, now); allowed {
		t.Fatal("an unresolvable source was admitted without limit — malformed input must not be the cheapest way past the bound")
	}
}

func TestDisabledLimiterAdmitsEverything(t *testing.T) {
	limiter := New(0, 0)
	now := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)
	source := addr(t, "203.0.113.10")
	for attempt := 0; attempt < 1000; attempt++ {
		if allowed, wait := limiter.Allow(source, now); !allowed || wait != 0 {
			t.Fatalf("a disabled limiter rejected request %d", attempt)
		}
	}
	if limiter.Enabled() {
		t.Fatal("a limiter built with a non-positive limit reports itself enabled")
	}
	if limiter.Rejected() != 0 {
		t.Fatalf("rejected = %d, want 0", limiter.Rejected())
	}
}

func TestConcurrentSourcesAdmitExactlyTheBudget(t *testing.T) {
	const limit = 20
	limiter := New(limit, 0)
	now := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)
	source := addr(t, "203.0.113.11")
	var wait sync.WaitGroup
	var mu sync.Mutex
	admitted := 0
	for worker := 0; worker < 8; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for attempt := 0; attempt < 25; attempt++ {
				if allowed, _ := limiter.Allow(source, now); allowed {
					mu.Lock()
					admitted++
					mu.Unlock()
				}
			}
		}()
	}
	wait.Wait()
	if admitted != limit {
		t.Fatalf("admitted %d of 200 concurrent requests, want exactly the budget of %d", admitted, limit)
	}
}
