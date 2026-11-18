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
		if got := ParseRetryAfter(tc.value, now); got != tc.delay {
			t.Errorf("ParseRetryAfter(%q) = %s, want %s", tc.value, got, tc.delay)
		}
	}
}
