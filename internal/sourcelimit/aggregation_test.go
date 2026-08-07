package sourcelimit

import (
	"fmt"
	"net/netip"
	"testing"
	"time"
)

// TestIPv6AddressesShareOneBudgetPerSixtyFour is the difference between a
// bound and a formality. A host with a /64 — the ordinary allocation, not a
// large one — can source addresses from 2^64 of them without spoofing
// anything, so charging each address separately hands that one machine an
// unbounded multiple of the configured budget.
func TestIPv6AddressesShareOneBudgetPerSixtyFour(t *testing.T) {
	limiter := New(3, DefaultMaxTrackedSources)
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	for attempt := range 3 {
		source := netip.MustParseAddr(fmt.Sprintf("2001:db8::%x", attempt))
		if allowed, _ := limiter.Allow(source, now); !allowed {
			t.Fatalf("request %d within the budget was rejected", attempt)
		}
	}
	// A fourth address inside the same /64 is the same allocation and must
	// find the budget already spent.
	if allowed, _ := limiter.Allow(netip.MustParseAddr("2001:db8::ffff"), now); allowed {
		t.Fatal("a fourth address in the same /64 must not receive a fresh budget")
	}
	// A different /64 is a different allocation and keeps its own budget.
	if allowed, _ := limiter.Allow(netip.MustParseAddr("2001:db8:0:1::1"), now); !allowed {
		t.Fatal("a separate /64 must keep its own budget")
	}
}

// TestIPv6RotationCannotFillTheTrackingTable covers the second half of the
// same weakness: filling the table pushes every source that appears afterwards
// into the shared overflow budget, so address rotation was a way to degrade
// service for everyone else rather than only for the attacker.
func TestIPv6RotationCannotFillTheTrackingTable(t *testing.T) {
	const maxTracked = 8
	limiter := New(1, maxTracked)
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	for attempt := range maxTracked * 4 {
		limiter.Allow(netip.MustParseAddr(fmt.Sprintf("2001:db8::%x", attempt)), now)
	}
	if overflows := limiter.Overflows(); overflows != 0 {
		t.Fatalf("one /64 filled the tracking table: overflows=%d", overflows)
	}
	// IPv4 keeps address-level accounting, where one address is one
	// allocation, so the ceiling still applies there.
	for attempt := range maxTracked * 2 {
		limiter.Allow(netip.MustParseAddr(fmt.Sprintf("198.51.100.%d", attempt)), now)
	}
	if overflows := limiter.Overflows(); overflows == 0 {
		t.Fatal("distinct IPv4 addresses must still be able to reach the tracking ceiling")
	}
}

// TestIPv4MappedAddressesShareTheirIPv4Budget stops the mapped form from being
// a second identity for the same address.
func TestIPv4MappedAddressesShareTheirIPv4Budget(t *testing.T) {
	limiter := New(1, DefaultMaxTrackedSources)
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	if allowed, _ := limiter.Allow(netip.MustParseAddr("198.51.100.7"), now); !allowed {
		t.Fatal("first request was rejected")
	}
	if allowed, _ := limiter.Allow(netip.MustParseAddr("::ffff:198.51.100.7"), now); allowed {
		t.Fatal("the IPv4-mapped form must share the budget of the address it maps")
	}
}
