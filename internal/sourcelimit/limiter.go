// Package sourcelimit bounds how many gateway requests one source address may
// start per minute, before the request body is read and before the per-project
// limiter can apply. The per-project limiter needs an authenticated key to know
// which budget to charge; everything ahead of that point — TLS, header parsing,
// the authentication attempt itself — is work an anonymous caller can ask for
// without any bound at all. This is that bound.
package sourcelimit

import (
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultMaxTrackedSources is the ceiling on distinct addresses held in one
// window when the caller does not choose its own.
const DefaultMaxTrackedSources = 16384

// ipv6AggregationBits is the prefix length IPv6 sources are counted under.
//
// A single IPv6 host is routinely handed a /64, so keying on the full address
// lets one machine present 2^64 distinct, genuinely routable sources — no
// spoofing needed. That both multiplies its own budget without limit and fills
// the tracking table, which pushes every source that appears afterwards into
// the shared overflow budget. /64 is the smallest unit an operator is expected
// to be able to allocate, so it is the unit worth counting.
const ipv6AggregationBits = 64

// bucketKey collapses a source address to the unit it is charged under: the
// exact address for IPv4, where one address really is one allocation, and the
// /64 prefix for IPv6.
func bucketKey(source netip.Addr) netip.Addr {
	source = source.Unmap()
	if !source.Is6() {
		return source
	}
	prefix, err := source.Prefix(ipv6AggregationBits)
	if err != nil {
		return source
	}
	return prefix.Addr()
}

// Limiter admits requests per source address in fixed one-minute windows.
//
// The counter map is bounded on purpose. An unbounded map keyed by source
// address turns the limiter itself into the amplifier it was added to prevent:
// a flood from spoofed or merely numerous addresses would be admitted and
// remembered. Past MaxTrackedSources distinct addresses in one window, further
// addresses share a single overflow budget rather than each receiving their
// own, so memory stays flat and the excess is shed instead of served.
type Limiter struct {
	limit      int
	maxTracked int

	mu       sync.Mutex
	minute   time.Time
	counts   map[netip.Addr]int
	overflow int

	rejected  atomic.Uint64
	overflows atomic.Uint64
}

// New returns a limiter admitting requestsPerMinute requests per source per
// minute. A non-positive requestsPerMinute disables it, in which case Allow is
// a cheap unconditional yes.
func New(requestsPerMinute, maxTrackedSources int) *Limiter {
	if maxTrackedSources < 1 {
		maxTrackedSources = DefaultMaxTrackedSources
	}
	return &Limiter{limit: requestsPerMinute, maxTracked: maxTrackedSources}
}

// Enabled reports whether the limiter admits anything at all. A disabled
// limiter is still safe to call.
func (l *Limiter) Enabled() bool { return l != nil && l.limit > 0 }

// Allow charges one request to source and reports whether it may proceed. When
// it may not, the returned duration is how long the caller should wait — the
// remainder of the current window, which is when the source's budget is
// restored.
//
// The zero Addr is a legitimate key: a caller whose address cannot be resolved
// is charged to it collectively rather than skipping the limiter, because
// "unparseable" must not be the cheapest way past a bound.
func (l *Limiter) Allow(source netip.Addr, now time.Time) (bool, time.Duration) {
	if !l.Enabled() {
		return true, 0
	}
	minute := now.UTC().Truncate(time.Minute)
	l.mu.Lock()
	defer l.mu.Unlock()
	if !minute.Equal(l.minute) {
		// A fresh map rather than clearing the old one: clearing keeps the
		// backing array a flood grew, which is the memory this bound exists to
		// give back.
		l.minute = minute
		l.counts = make(map[netip.Addr]int)
		l.overflow = 0
	}
	wait := minute.Add(time.Minute).Sub(now)
	if wait < 0 {
		wait = 0
	}
	source = bucketKey(source)
	count, tracked := l.counts[source]
	if !tracked && len(l.counts) >= l.maxTracked {
		l.overflow++
		l.overflows.Add(1)
		if l.overflow > l.limit {
			l.rejected.Add(1)
			return false, wait
		}
		return true, 0
	}
	if count >= l.limit {
		l.rejected.Add(1)
		return false, wait
	}
	l.counts[source] = count + 1
	return true, 0
}

// Rejected is how many requests this limiter has turned away.
func (l *Limiter) Rejected() uint64 {
	if l == nil {
		return 0
	}
	return l.rejected.Load()
}

// Overflows is how many requests were charged to the shared overflow budget
// because the window already tracked MaxTrackedSources distinct addresses. A
// non-zero value means distinct sources per minute have outgrown the tracking
// ceiling and legitimate callers may be sharing a budget.
func (l *Limiter) Overflows() uint64 {
	if l == nil {
		return 0
	}
	return l.overflows.Load()
}
