package vault

import (
	"bytes"
	"testing"
)

func TestGovernanceKeyUsesIndependentStableDomain(t *testing.T) {
	master := bytes.Repeat([]byte{0x42}, MasterKeySize)
	ledgerKey, err := DeriveLedgerHMACKey(master)
	if err != nil {
		t.Fatal(err)
	}
	first, err := DeriveGovernanceHMACKey(ledgerKey)
	if err != nil {
		t.Fatal(err)
	}
	second, err := DeriveGovernanceHMACKey(ledgerKey)
	if err != nil {
		t.Fatal(err)
	}
	audit, err := DeriveAuditHMACKey(master)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) || bytes.Equal(first, ledgerKey) || bytes.Equal(first, audit) {
		t.Fatal("Governance key domain is not stable and independent")
	}
}
