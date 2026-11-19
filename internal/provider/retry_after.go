package provider

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

const MaxRetryAfter = 24 * time.Hour

// RetryAfter is an upstream response's wait hint, and the only place any adapter
// reads one, so a hint means the same thing whichever wire carried it.
//
// Only a status that says "come back later" carries one: 429, 503, and
// Anthropic's 529 overload. A Retry-After on any other answer is not a wait the
// upstream asked for, and honouring it would stop retries on a 500 that one
// adapter sent the header with and another did not. The value is seconds or an
// HTTP date, at least a second, at most a day; zero means no usable hint.
func RetryAfter(status int, header http.Header, now time.Time) time.Duration {
	switch status {
	case http.StatusTooManyRequests, http.StatusServiceUnavailable, statusOverloaded:
		return parseRetryAfter(header.Get("Retry-After"), now)
	default:
		return 0
	}
}

// statusOverloaded is Anthropic's "temporarily overloaded", which has no
// constant in net/http.
const statusOverloaded = 529

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if strings.Trim(value, "0123456789") == "" {
		seconds, err := strconv.ParseUint(value, 10, 64)
		if err != nil || seconds >= uint64(MaxRetryAfter/time.Second) {
			return MaxRetryAfter
		}
		return max(time.Second, time.Duration(seconds)*time.Second)
	}
	if deadline, err := http.ParseTime(value); err == nil && deadline.After(now) {
		return min(max(time.Second, deadline.Sub(now)), MaxRetryAfter)
	}
	return 0
}
