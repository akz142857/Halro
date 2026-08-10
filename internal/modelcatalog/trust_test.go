package modelcatalog

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"testing"
	"time"
)

func TestParseTrustRootsIsStrictAndPublicOnly(t *testing.T) {
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	encoded := "release-2026|" + base64.StdEncoding.EncodeToString(publicKey) + "|" + now.Add(-time.Hour).Format(time.RFC3339) + "|" + now.Add(time.Hour).Format(time.RFC3339)
	roots, err := ParseTrustRoots(encoded)
	if err != nil || len(roots) != 1 || roots[0].KeyID != "release-2026" {
		t.Fatalf("parsed roots=%+v err=%v", roots, err)
	}
	for _, invalid := range []string{"", encoded + ";broken", encoded + ";" + encoded, "private|not-a-key|bad|bad"} {
		if _, err := ParseTrustRoots(invalid); err == nil {
			t.Fatalf("invalid trust root accepted: %q", invalid)
		}
	}
}

func TestProductionTrustRootsFailsClosedOnMixedInvalidInput(t *testing.T) {
	original := ReleaseTrustRoots
	t.Cleanup(func() { ReleaseTrustRoots = original })
	ReleaseTrustRoots = "broken"
	if roots := ProductionTrustRoots(); len(roots) != 0 {
		t.Fatalf("malformed release roots must fail closed: %+v", roots)
	}
}
