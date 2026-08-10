// Command prepare-model-catalog canonicalizes an unsigned snapshot for an
// external KMS/HSM or offline signer. It never accepts private key material.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/akz142857/Halro/internal/modelcatalog"
)

func main() {
	if len(os.Args) != 3 {
		fatalf("usage: go run ./tools/modelcatalog/prepare <unsigned-snapshot.json> <canonical-payload.json>")
	}
	input, err := os.ReadFile(os.Args[1])
	if err != nil {
		fatalf("read unsigned snapshot: %v", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.DisallowUnknownFields()
	var snapshot modelcatalog.Snapshot
	if err := decoder.Decode(&snapshot); err != nil {
		fatalf("decode unsigned snapshot: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		fatalf("unsigned snapshot has trailing JSON values")
	}
	snapshot, canonical, err := modelcatalog.PrepareSnapshot(snapshot)
	if err != nil {
		fatalf("prepare snapshot: %v", err)
	}
	if err := os.WriteFile(os.Args[2], canonical, 0o600); err != nil {
		fatalf("write canonical payload: %v", err)
	}
	fmt.Printf("catalog_revision=%s\n", snapshot.CatalogRevision)
}

func fatalf(format string, arguments ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", arguments...)
	os.Exit(1)
}
