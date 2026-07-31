package vault

import (
	"bytes"
	"testing"
)

func TestMetricsTokenIsDeterministicAndPurposeSeparated(t *testing.T) {
	master := bytes.Repeat([]byte{7}, MasterKeySize)
	first, err := DeriveMetricsBearerToken(master)
	if err != nil {
		t.Fatal(err)
	}
	second, err := DeriveMetricsBearerToken(master)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) || !bytes.HasPrefix(first, []byte("hmt_")) {
		t.Fatalf("unexpected metrics token")
	}
	auditKey, err := DeriveAuditHMACKey(master)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(first, auditKey) || bytes.Equal(first, auditKey) {
		t.Fatal("metrics and audit derivation domains must be separate")
	}
}
