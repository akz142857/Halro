// Command verify-model-catalog validates a signed model catalog exactly as a
// production Halro reader would. It accepts public trust roots only; signing
// and private-key handling stay outside this repository and ordinary CI.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/akz142857/Halro/internal/modelcatalog"
)

func main() {
	catalogPath, currentPath, err := parseArguments(os.Args[1:])
	if err != nil {
		fatalf("%v", err)
	}
	roots, err := modelcatalog.ParseTrustRoots(os.Getenv("MODEL_CATALOG_TRUST_ROOTS"))
	if err != nil {
		fatalf("parse MODEL_CATALOG_TRUST_ROOTS: %v", err)
	}
	payload, err := readCatalogFile(catalogPath)
	if err != nil {
		fatalf("read catalog: %v", err)
	}
	snapshot, catalog, err := modelcatalog.DecodeAndVerifySignedSnapshot(payload, modelcatalog.VerifyOptions{
		Now: time.Now().UTC(), TrustRoots: roots, MaxEntries: modelcatalog.DefaultMaxEntries,
	})
	if err != nil {
		fatalf("verify signed catalog: %v", err)
	}
	if currentPath != "" {
		currentPayload, readErr := readCatalogFile(currentPath)
		if readErr != nil {
			fatalf("read current catalog: %v", readErr)
		}
		currentSequence, sequenceErr := sequenceFromSignedEnvelope(currentPayload)
		if sequenceErr != nil {
			fatalf("read current catalog sequence: %v", sequenceErr)
		}
		if sequenceErr := requireNewerSequence(snapshot.Sequence, currentSequence); sequenceErr != nil {
			fatalf("%v", sequenceErr)
		}
	}
	result := map[string]any{
		"catalog_revision": snapshot.CatalogRevision,
		"sequence":         snapshot.Sequence,
		"expires_at":       snapshot.ExpiresAt,
		"entries":          catalog.Len(),
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fatalf("write result: %v", err)
	}
}

func parseArguments(arguments []string) (catalogPath, currentPath string, err error) {
	switch {
	case len(arguments) == 1:
		return arguments[0], "", nil
	case len(arguments) == 3 && arguments[0] == "--newer-than":
		return arguments[2], arguments[1], nil
	default:
		return "", "", fmt.Errorf("usage: go run ./tools/modelcatalog/verify [--newer-than current-catalog.json] <signed-catalog.json>")
	}
}

func readCatalogFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() <= 0 || info.Size() > modelcatalog.DefaultMaxDecodedBytes {
		return nil, fmt.Errorf("catalog file size %d is outside the verification limit", info.Size())
	}
	return os.ReadFile(path)
}

func sequenceFromSignedEnvelope(payload []byte) (uint64, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var signed modelcatalog.SignedSnapshot
	if err := decoder.Decode(&signed); err != nil {
		return 0, fmt.Errorf("decode signed catalog: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return 0, fmt.Errorf("signed catalog has trailing JSON values")
	}
	if signed.Payload.Sequence == 0 {
		return 0, fmt.Errorf("catalog sequence must be positive")
	}
	return signed.Payload.Sequence, nil
}

func requireNewerSequence(candidate, current uint64) error {
	if candidate <= current {
		return fmt.Errorf("catalog sequence %d must be greater than current sequence %d", candidate, current)
	}
	return nil
}

func fatalf(format string, arguments ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", arguments...)
	os.Exit(1)
}
