package idempotency

import (
	"strings"
	"testing"
)

// ValidateKey guards a caller-supplied header that becomes a lookup key, so its
// job is to be narrow: visible ASCII, bounded length, nothing that could carry a
// separator or a control character into whatever consumes it.
func TestValidateKeyAcceptsOnlyBoundedVisibleASCII(t *testing.T) {
	for _, testCase := range []struct {
		name string
		key  string
		want bool
	}{
		{"typical", "retry-1", true},
		{"punctuation", "a.b_c:d/e", true},
		{"single character", "x", true},
		{"at the length limit", strings.Repeat("k", 128), true},

		{"empty", "", false},
		{"one past the limit", strings.Repeat("k", 129), false},
		{"inner space", "retry 1", false},
		{"leading space", " retry", false},
		{"tab", "retry\t1", false},
		{"newline", "retry\n1", false},
		{"null byte", "retry\x001", false},
		{"non-ASCII", "重试-1", false},
		{"delete character", "retry\x7f", false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := ValidateKey(testCase.key)
			if testCase.want && err != nil {
				t.Fatalf("ValidateKey(%q) rejected a valid key: %v", testCase.key, err)
			}
			if !testCase.want && err == nil {
				t.Fatalf("ValidateKey(%q) accepted it", testCase.key)
			}
		})
	}
}
