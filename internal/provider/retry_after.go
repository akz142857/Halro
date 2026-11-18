package provider

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

const MaxRetryAfter = 24 * time.Hour

// ParseRetryAfter returns zero when the upstream gave no usable hint.
func ParseRetryAfter(value string, now time.Time) time.Duration {
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
