package app

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"strings"
	"testing"
)

// The 260807 review declined to split internal/app into an adminapi package:
// the crossing surface was large, the payoff was zero functional and zero
// security, and the risk landed squarely on the Admin mutation paths. That
// judgement rested on a premise the same review then disproved — that the
// package was steady. Half of everything added to internal/ that round landed
// here, because this is where a new subsystem goes when nobody decides
// otherwise.
//
// So the decision keeps a gate rather than a note. Runtime is the honest
// measure of the package's breadth: a field is a subsystem this type now owns,
// and a mutex is a piece of state it now serialises. Both may fall freely.
// Raising either means editing this file, which is the moment to ask whether
// the thing being added belongs here — the question the split would have
// forced continuously, at the cost of one line instead of a refactor.
//
// internal/app is otherwise the only package in the repository under no
// executable architectural constraint, and the one that needs one most.
const (
	// 67: capabilityMetrics. Raised deliberately. It is per-instance mutable
	// state written by the admin handlers and the registry loader and read by
	// the metrics renderer — the same shape as usage, ledger and alerts. The
	// alternative shape available here is a process global, as the KMS metrics
	// use, and that one leaks across Runtime lifecycles and across tests.
	// 68: capabilityDetections. The persistent control-plane job manager owns
	// one bounded semaphore set and cancellation registry; grouping it keeps the
	// Runtime surface to one subsystem rather than four coordination fields.
	// 67: capabilityResolution now groups invocation-target cache coordination
	// and the signed catalog manager behind one subsystem boundary.
	// 68: activation. Raised deliberately. Whether the live snapshots still
	// reflect the store is read on the data-plane hot path and written by every
	// admin mutation, so it belongs to the Runtime rather than to any one
	// handler; it is one grouped tracker, not a field per snapshot.
	// 69: reload. Raised deliberately, and by one rather than five: the
	// certificate holders, the configuration path a reloadable scalar is read
	// from, the live log handle, and the per-item outcome record are one
	// subsystem behind reloadRuntime. They belong to the Runtime because the
	// listeners read the material on every handshake and the metrics renderer
	// reads the record.
	// 70: adminElevation. Raised deliberately, and by one rather than two: the
	// lock and the grants it guards are one field, and they belong to the
	// Runtime because a grant is scoped to this process by design — an
	// elevation that outlived a restart would be one the operator never gave
	// the process now holding it.
	// 71: routeWithheld. Raised deliberately, and by one rather than two: the
	// lock and the map travel together. It belongs to the Runtime because it
	// describes the live registry rather than the store — it is written where
	// the registry is activated and read by the Admin routes endpoints, and the
	// alternative, recomputing it per request, would answer from a load the
	// process is not running.
	runtimeFieldBudget = 71
	runtimeMutexBudget = 10
)

func TestRuntimeStaysWithinItsDeclaredBreadth(t *testing.T) {
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, "runtime.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	fields, mutexes := 0, 0
	ast.Inspect(file, func(node ast.Node) bool {
		spec, ok := node.(*ast.TypeSpec)
		if !ok || spec.Name.Name != "Runtime" {
			return true
		}
		structType, ok := spec.Type.(*ast.StructType)
		if !ok {
			return false
		}
		for _, field := range structType.Fields.List {
			names := len(field.Names)
			if names == 0 {
				names = 1 // embedded
			}
			fields += names
			if typeName := renderType(fileSet, field.Type); strings.Contains(typeName, "sync.Mutex") || strings.Contains(typeName, "sync.RWMutex") {
				mutexes += names
			}
		}
		return false
	})
	if fields == 0 {
		t.Fatal("Runtime was not found in runtime.go; this gate is not measuring anything")
	}
	if fields > runtimeFieldBudget {
		t.Fatalf("Runtime has %d fields, budget %d — raise the budget deliberately, or put the new subsystem somewhere else",
			fields, runtimeFieldBudget)
	}
	if mutexes > runtimeMutexBudget {
		t.Fatalf("Runtime holds %d mutexes, budget %d — each one is more state this single type serialises",
			mutexes, runtimeMutexBudget)
	}
	// A budget left far above reality stops being a gate, so it is lowered as
	// the package shrinks rather than only when it grows.
	if fields < runtimeFieldBudget || mutexes < runtimeMutexBudget {
		t.Fatalf("Runtime is down to %d fields and %d mutexes; lower the budgets to %d and %d to keep the gate tight",
			fields, mutexes, fields, mutexes)
	}
}

func renderType(fileSet *token.FileSet, expr ast.Expr) string {
	var builder strings.Builder
	if err := printer.Fprint(&builder, fileSet, expr); err != nil {
		return ""
	}
	return builder.String()
}
