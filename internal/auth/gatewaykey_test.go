package auth

import (
	"strings"
	"testing"
)

func TestGatewayKeyGeneration(t *testing.T) {
	plaintext, key, err := GenerateGatewayKey("prj_1", "production", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plaintext, GatewayKeyPrefix) {
		t.Fatalf("unexpected prefix: %q", plaintext)
	}
	if err := ValidateGatewayKeyFormat(plaintext); err != nil {
		t.Fatal(err)
	}
	if !ConstantTimeHashMatch(key.KeyHash, HashGatewayKey(plaintext)) {
		t.Fatal("stored hash does not match plaintext")
	}
	if strings.Contains(string(key.KeyHash[:]), plaintext) {
		t.Fatal("hash contains plaintext")
	}
}

func TestGatewayKeyFormatRejectsMalformedValues(t *testing.T) {
	for _, value := range []string{"", "sk-secret", "gw_bad!", "gw_c2hvcnQ"} {
		if err := ValidateGatewayKeyFormat(value); err == nil {
			t.Fatalf("expected %q to fail", value)
		}
	}
}
