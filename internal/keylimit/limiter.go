// Package keylimit bounds how many requests one authenticated Gateway Key, and
// one Project, may make per minute on the paths that reach no upstream.
//
// Those paths sit between the two bounds Halro already has and are held by
// neither. `sourcelimit` runs before authentication, counts addresses rather
// than principals, and an operator may legitimately set it to zero behind a
// proxy that does its own shedding. The Project limiter in `limiter` needs a
// request that is about to cost something: it charges RPM, TPM and concurrency
// for work that reaches a provider, and a Project that sets RPM to zero has
// chosen unlimited spending rather than unlimited free reads. A request Halro
// answers from its own state — a model list, a deferred poll, a governance read
// — costs no provider call and no money, so neither of those is the right
// budget to charge it against; but it is not free either, and an authenticated
// caller must not be able to ask for it without limit.
//
// This is that bound: a fixed per-minute ceiling on a Key and on its Project,
// with no configuration surface, so no deployment can be assembled in which an
// authenticated principal has none.
package keylimit

import (
	"sync"
	"sync/atomic"
	"time"
)

// DefaultMaxTrackedKeys and DefaultMaxTrackedProjects cap how many distinct
// principals one window remembers, for the same reason sourcelimit caps
// addresses: an unbounded map keyed by something the caller influences turns
// the limiter into the amplifier it exists to prevent.
const (
	DefaultMaxTrackedKeys     = 16_384
	DefaultMaxTrackedProjects = 4_096
)

type window struct {
	minute int64
	count  int
}

// Limiter owns one Key bucket set and one Project bucket set. Both checks and
// both increments happen under one lock, so a Project refusal does not consume
// a Key permit and two concurrent callers cannot pass the same final slot.
//
// The zero Limiter is usable, and tracks the default number of principals.
type Limiter struct {
	maxTrackedKeys     int
	maxTrackedProjects int

	mu       sync.Mutex
	keys     map[string]window
	projects map[string]window

	rejected  atomic.Uint64
	overflows atomic.Uint64
}

// New returns a limiter tracking at most the given number of distinct Keys and
// Projects per window. A non-positive value takes the default.
func New(maxTrackedKeys, maxTrackedProjects int) *Limiter {
	if maxTrackedKeys < 1 {
		maxTrackedKeys = DefaultMaxTrackedKeys
	}
	if maxTrackedProjects < 1 {
		maxTrackedProjects = DefaultMaxTrackedProjects
	}
	return &Limiter{maxTrackedKeys: maxTrackedKeys, maxTrackedProjects: maxTrackedProjects}
}

// Allow charges one request to keyID and to projectID within class and reports
// whether it may proceed. When it may not, the returned duration is the
// remainder of the current window, which is when both budgets are restored.
//
// class separates budgets that must not share one: a Project's governance
// writes and its governance reads are counted apart, and so are its gateway
// reads. A non-positive limit is unlimited for that half alone, which is what
// lets a caller bound Keys without bounding Projects; it is a caller's
// compile-time choice, never an operator's.
func (l *Limiter) Allow(
	now time.Time,
	class, keyID, projectID string,
	keyLimit, projectLimit int,
) (bool, time.Duration) {
	minute := now.UTC().Unix() / 60
	// Whole seconds, always at least one: Retry-After is a seconds header, and
	// a hint of zero invites the caller to retry into the same refusal.
	wait := time.Duration(60-now.UTC().Unix()%60) * time.Second
	keyName, projectName := class+"\x00"+keyID, class+"\x00"+projectID
	overflow := class + "\x00overflow"
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.keys == nil {
		if l.maxTrackedKeys < 1 {
			l.maxTrackedKeys = DefaultMaxTrackedKeys
		}
		if l.maxTrackedProjects < 1 {
			l.maxTrackedProjects = DefaultMaxTrackedProjects
		}
		l.keys = make(map[string]window)
		l.projects = make(map[string]window)
	}
	keyName = l.bucket(l.keys, keyName, overflow, minute, l.maxTrackedKeys)
	projectName = l.bucket(l.projects, projectName, overflow, minute, l.maxTrackedProjects)
	keyWindow := current(l.keys[keyName], minute)
	projectWindow := current(l.projects[projectName], minute)
	if keyLimit > 0 && keyWindow.count >= keyLimit || projectLimit > 0 && projectWindow.count >= projectLimit {
		l.rejected.Add(1)
		return false, wait
	}
	keyWindow.count++
	projectWindow.count++
	l.keys[keyName] = keyWindow
	l.projects[projectName] = projectWindow
	return true, 0
}

// Rejected is how many requests this limiter has turned away.
func (l *Limiter) Rejected() uint64 {
	if l == nil {
		return 0
	}
	return l.rejected.Load()
}

// Overflows is how many requests were charged to a shared overflow bucket
// because the window already tracked its ceiling of distinct principals. A
// non-zero value means legitimate callers may be sharing a budget.
func (l *Limiter) Overflows() uint64 {
	if l == nil {
		return 0
	}
	return l.overflows.Load()
}

// bucket names the window a principal is charged under. Past the ceiling, the
// entries left over from an earlier minute are dropped first — they are dead
// weight, not budget — and only a window still full of live principals pushes
// the newcomer onto the shared overflow bucket.
func (l *Limiter) bucket(windows map[string]window, wanted, overflow string, minute int64, limit int) string {
	if _, exists := windows[wanted]; exists || len(windows) < limit {
		return wanted
	}
	for name, held := range windows {
		if held.minute != minute {
			delete(windows, name)
		}
	}
	if len(windows) < limit {
		return wanted
	}
	l.overflows.Add(1)
	return overflow
}

func current(held window, minute int64) window {
	if held.minute != minute {
		return window{minute: minute}
	}
	return held
}
