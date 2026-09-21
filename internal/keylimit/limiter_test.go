package keylimit

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestAllowRequiresBothKeyAndProjectCapacity(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 30, 0, time.UTC)
	limiter := New(0, 0)
	if allowed, _ := limiter.Allow(now, "write", "key_1", "prj_1", 2, 2); !allowed {
		t.Fatal("first permit was refused")
	}
	if allowed, _ := limiter.Allow(now, "write", "key_2", "prj_1", 2, 2); !allowed {
		t.Fatal("second project permit was refused")
	}
	allowed, wait := limiter.Allow(now, "write", "key_1", "prj_1", 2, 2)
	if allowed || wait != 30*time.Second {
		t.Fatalf("project limit allowed=%t wait=%s, want refused with the 30s left in the window", allowed, wait)
	}
	if allowed, _ := limiter.Allow(now, "write", "key_1", "prj_2", 2, 2); !allowed {
		t.Fatal("the project refusal consumed the key's second permit")
	}
	if allowed, _ := limiter.Allow(now, "read", "key_1", "prj_2", 1, 1); !allowed {
		t.Fatal("two classes shared one bucket")
	}
	if allowed, _ := limiter.Allow(now.Add(30*time.Second), "write", "key_1", "prj_1", 2, 2); !allowed {
		t.Fatal("the new minute did not restore the bucket")
	}
	if rejected := limiter.Rejected(); rejected != 1 {
		t.Fatalf("rejected = %d, want 1", rejected)
	}
}

// The retry hint is what the caller is told to wait, so it must never be zero:
// a caller obeying "retry after 0 seconds" retries into the same refusal.
func TestRetryHintIsTheRemainderOfTheWindowAndNeverZero(t *testing.T) {
	for second := range 60 {
		limiter := New(0, 0)
		now := time.Date(2026, 9, 4, 12, 0, second, 0, time.UTC)
		if allowed, _ := limiter.Allow(now, "read", "key", "prj", 1, 0); !allowed {
			t.Fatalf("second %d: the first permit of the window was refused", second)
		}
		allowed, wait := limiter.Allow(now, "read", "key", "prj", 1, 0)
		if allowed {
			t.Fatalf("second %d: a second request passed a limit of one", second)
		}
		if want := time.Duration(60-second) * time.Second; wait != want {
			t.Fatalf("second %d: wait = %s, want %s", second, wait, want)
		}
	}
}

// A limit of zero on one half must bound nothing there while the other half
// still applies — that is how a Key ceiling and a Project ceiling are set
// independently.
func TestNonPositiveLimitBoundsOnlyItsOwnHalf(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	limiter := New(0, 0)
	for attempt := range 100 {
		if allowed, _ := limiter.Allow(now, "read", fmt.Sprintf("key_%d", attempt), "prj", 0, 0); !allowed {
			t.Fatalf("request %d was refused by a limiter that bounds neither half", attempt)
		}
	}
	for attempt := range 3 {
		if allowed, _ := limiter.Allow(now, "read", "key_a", "prj", 3, 0); !allowed {
			t.Fatalf("request %d inside the key budget was refused", attempt)
		}
	}
	if allowed, _ := limiter.Allow(now, "read", "key_a", "prj", 3, 0); allowed {
		t.Fatal("the key ceiling did not apply while the project half was unbounded")
	}
}

// Key rotation must not be able to grow the tracking map without bound. Past
// the ceiling the newcomers share one bucket, which is shed rather than served.
func TestKeyRotationCannotFillTheTrackingTable(t *testing.T) {
	const maxTracked = 8
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	limiter := New(maxTracked, maxTracked)
	admitted := 0
	for attempt := range maxTracked * 8 {
		if allowed, _ := limiter.Allow(now, "read", fmt.Sprintf("key_%d", attempt), "prj", 1, 0); allowed {
			admitted++
		}
	}
	if limiter.Overflows() == 0 {
		t.Fatal("distinct keys never reached the tracking ceiling")
	}
	if admitted > maxTracked+1 {
		t.Fatalf("admitted = %d; past the ceiling the newcomers must share one budget", admitted)
	}
	limiter.mu.Lock()
	tracked := len(limiter.keys)
	limiter.mu.Unlock()
	// The shared overflow bucket is itself one entry, which is the point of
	// having it: everything past the ceiling lands there rather than growing
	// the map by one entry per newcomer.
	if tracked > maxTracked+1 {
		t.Fatalf("tracked keys = %d, want at most %d", tracked, maxTracked+1)
	}
}

// The check and the two increments are one decision. Without the single lock,
// concurrent callers read the same count and each admit themselves past it.
func TestConcurrentCallersCannotPassTheSameFinalSlot(t *testing.T) {
	const callers = 64
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	limiter := New(0, 0)
	var start sync.WaitGroup
	var done sync.WaitGroup
	var mu sync.Mutex
	admitted := 0
	start.Add(1)
	for range callers {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			if allowed, _ := limiter.Allow(now, "read", "key", "prj", 10, 10); allowed {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}()
	}
	start.Done()
	done.Wait()
	if admitted != 10 {
		t.Fatalf("admitted = %d, want exactly the 10 the budget holds", admitted)
	}
}
