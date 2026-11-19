package provider

import (
	"net/http"
	"testing"
	"time"
)

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		value string
		delay time.Duration
	}{
		{"7", 7 * time.Second},
		{"0", time.Second},
		{now.Add(3 * time.Second).Format(http.TimeFormat), 3 * time.Second},
		{now.Add(72 * time.Hour).Format(http.TimeFormat), MaxRetryAfter},
		{"9999999999999999999999999999999999", MaxRetryAfter},
		{"", 0},
		{"-1", 0},
		{"invalid", 0},
		{now.Add(-time.Second).Format(http.TimeFormat), 0},
	} {
		if got := parseRetryAfter(tc.value, now); got != tc.delay {
			t.Errorf("parseRetryAfter(%q) = %s, want %s", tc.value, got, tc.delay)
		}
	}
}

func TestRetryAfterReadsOnlyComeBackLaterStatuses(t *testing.T) {
	now := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	header := http.Header{"Retry-After": []string{"7"}}
	for status, want := range map[int]time.Duration{
		http.StatusTooManyRequests:     7 * time.Second,
		http.StatusServiceUnavailable:  7 * time.Second,
		529:                            7 * time.Second,
		http.StatusOK:                  0,
		http.StatusBadRequest:          0,
		http.StatusInternalServerError: 0,
	} {
		if got := RetryAfter(status, header, now); got != want {
			t.Errorf("RetryAfter(%d) = %v, want %v", status, got, want)
		}
	}
}
