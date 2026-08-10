// Command assemble-model-catalog wraps a canonical payload and an externally
// produced Ed25519 signature. It accepts signatures, never private keys.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/akz142857/Halro/internal/modelcatalog"
)

func main() {
	if len(os.Args) != 5 {
		fatalf("usage: go run ./tools/modelcatalog/assemble <canonical-payload.json> <key-id> <base64-signature-file> <signed-catalog.json>")
	}
	payloadBytes, err := os.ReadFile(os.Args[1])
	if err != nil {
		fatalf("read canonical payload: %v", err)
	}
	keyID := strings.TrimSpace(os.Args[2])
	if keyID == "" {
		fatalf("key ID is empty")
	}
	signatureFile, err := os.ReadFile(os.Args[3])
	if err != nil {
		fatalf("read signature: %v", err)
	}
	signatureValue := strings.TrimSpace(string(signatureFile))
	roots, err := modelcatalog.ParseTrustRoots(os.Getenv("MODEL_CATALOG_TRUST_ROOTS"))
	if err != nil {
		fatalf("parse MODEL_CATALOG_TRUST_ROOTS: %v", err)
	}
	envelope, err := modelcatalog.AssembleSignedSnapshot(payloadBytes, keyID, signatureValue, roots)
	if err != nil {
		fatalf("verify detached signature: %v", err)
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		fatalf("encode signed catalog: %v", err)
	}
	if err := os.WriteFile(os.Args[4], append(encoded, '\n'), 0o600); err != nil {
		fatalf("write signed catalog: %v", err)
	}
}

func fatalf(format string, arguments ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", arguments...)
	os.Exit(1)
}
