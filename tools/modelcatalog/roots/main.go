// Command roots-model-catalog validates the public trust-root representation
// supplied to release builds. It has no signing or private-key interface.
package main

import (
	"fmt"
	"os"

	"github.com/akz142857/Halro/internal/modelcatalog"
)

func main() {
	roots, err := modelcatalog.ParseTrustRoots(os.Getenv("MODEL_CATALOG_TRUST_ROOTS"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "validate MODEL_CATALOG_TRUST_ROOTS: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("validated_model_catalog_trust_roots=%d\n", len(roots))
}
