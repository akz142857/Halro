package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Proving that every administrative mutation leaves an audit record.
//
// The 2026-09-18 run found no such proof (260918-PV-F-19). The RBAC sweep next
// door works dynamically, because a rejection needs no valid body — any request
// to the route is refused before anything is parsed. Auditing cannot be swept
// that way: to observe that a mutation is recorded the mutation has to succeed,
// which means 63 routes with valid payloads, valid revisions and existing
// resources. That is not a sweep, it is a second test suite.
//
// So this reads the source instead. It finds every handler registered behind
// requireAdminMutation and walks the package's own call graph from it, looking
// for a path to one of the audit entry points. A handler with no such path
// cannot record anything, whatever it does at runtime.
//
// What it proves and what it does not: it proves no mutation route was added
// without audit machinery in reach, which is the regression the finding is
// about — a new write endpoint merged with nobody noticing. It does not prove
// the record is correct, that the call is on every branch, or that it commits
// in the same transaction as the change. Those are the per-handler tests that
// already exist; this is the exhaustive half they cannot be.

// auditEntryPoints are the functions that put an administrative mutation into
// the trail. Reaching any of them is what this sweep counts as audited.
var auditEntryPoints = map[string]struct{}{
	// The admin mutation intent, committed in the same bbolt transaction as
	// the change it describes.
	"newAdminAuditIntent":             {},
	"newAdminAuditIntentWithMetadata": {},
	"completeAdminMutation":           {},
	"auditAdminMutation":              {},
	// Direct appends, for actions with no store transaction to commit
	// alongside — a webhook test, a governance export.
	"appendAdminAudit":             {},
	"appendAdminAuditWithMetadata": {},
	// Pricing keeps its own intent bucket, separate from admin_audit_intents,
	// because a price change commits with the price rather than with the
	// administrator's session. It is no less audited for that.
	"newPricingAuditIntent": {},
	// Subject-specific writers.
	"auditTimezoneChange":            {},
	"auditCapabilitySnapshot":        {},
	"auditDeveloperExecution":        {},
	"auditModelCatalogConfiguration": {},
}

// auditSweepExemptions are mutation routes that deliberately write no
// administrative audit record, each with the reason it is not one.
//
// Kept as an explicit list rather than a rule: every entry here is a claim that
// a write to the control plane does not need to be in the trail, and that claim
// should be readable and argued with rather than inferred from a pattern.
var auditSweepExemptions = map[string]string{
	// Compute and return; they write nothing and change nothing. They sit
	// behind the mutation guard because they carry a CSRF token, not because
	// they mutate.
	"preflightAdminDeploymentCapabilities": "computes a capability preflight and returns it; no durable change",
	"previewAdminDeploymentPrice":          "computes a price preview and returns it; no durable change",

	// These two reach the network on the operator's behalf and still record
	// nothing. They change no control-plane state, which is why the sweep can
	// be made to pass by naming them — but it is an inconsistency, not a
	// conclusion: createAdminModelCapabilityDetection dials Providers with the
	// same credential and *is* audited. Recorded as 260918-PV-F-22 for the
	// four-party review to settle, because adding an event here changes the
	// audit contract (docs/contracts/audit-integrity.md) and is not a decision
	// a test should make on its own.
	"refreshAdminInvocationTargets": "dials Providers with the operator's credential and writes nothing durable; see 260918-PV-F-22",
	"refreshAdminModelCatalog":      "triggers a signed catalog refresh over the network and writes nothing durable; see 260918-PV-F-22",
}

// TestEveryAdminMutationRouteCanReachTheAuditTrail is the sweep.
func TestEveryAdminMutationRouteCanReachTheAuditTrail(t *testing.T) {
	fileSet := token.NewFileSet()
	files := parseAppPackage(t, fileSet)
	handlers := mutationHandlers(t, files)
	if len(handlers) < 40 {
		t.Fatalf("found %d mutation handlers; the registration pattern this sweep reads has changed", len(handlers))
	}
	calls := callGraph(files)

	var unaudited []string
	for _, handler := range handlers {
		if _, exempt := auditSweepExemptions[handler]; exempt {
			continue
		}
		if !reachesAudit(handler, calls, map[string]bool{}) {
			unaudited = append(unaudited, handler)
		}
	}
	sort.Strings(unaudited)
	if len(unaudited) > 0 {
		t.Fatalf("these admin mutation handlers cannot reach the audit trail: %v\n"+
			"either record the mutation, or add it to auditSweepExemptions with the reason it is not a control-plane change",
			unaudited)
	}
}

// TestTheAuditSweepExemptionsAllNameRealHandlers is the other direction. An
// exemption matching nothing exempts nothing today and silently exempts
// whatever takes that name tomorrow.
func TestTheAuditSweepExemptionsAllNameRealHandlers(t *testing.T) {
	fileSet := token.NewFileSet()
	files := parseAppPackage(t, fileSet)
	handlers := map[string]struct{}{}
	for _, handler := range mutationHandlers(t, files) {
		handlers[handler] = struct{}{}
	}
	for name, reason := range auditSweepExemptions {
		if _, registered := handlers[name]; !registered {
			t.Errorf("auditSweepExemptions names %q (%q), which is not a registered mutation handler", name, reason)
		}
		if strings.TrimSpace(reason) == "" {
			t.Errorf("exemption %q carries no reason", name)
		}
	}
}

// TestTheAuditEntryPointsAllExist keeps the sweep from passing because it is
// looking for functions nobody has any more.
func TestTheAuditEntryPointsAllExist(t *testing.T) {
	fileSet := token.NewFileSet()
	files := parseAppPackage(t, fileSet)
	defined := map[string]struct{}{}
	for _, file := range files {
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if ok {
				defined[function.Name.Name] = struct{}{}
			}
		}
	}
	for name := range auditEntryPoints {
		if _, exists := defined[name]; !exists {
			t.Errorf("audit entry point %q no longer exists; the sweep is looking for nothing", name)
		}
	}
}

func parseAppPackage(t *testing.T, fileSet *token.FileSet) []*ast.File {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fileSet, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files = append(files, parsed)
	}
	if len(files) == 0 {
		t.Fatal("parsed no source files")
	}
	return files
}

// mutationRegistration matches the router lines this sweep reads. Anchored on
// the guard, so a route registered without it is simply not a mutation route —
// and the RBAC sweep next door is what catches that mistake.
var mutationRegistration = regexp.MustCompile(
	`router\.With\(r\.requireAdminMutation\)\.\w+\("[^"]+",\s*r\.(\w+)\)`)

func mutationHandlers(t *testing.T, files []*ast.File) []string {
	t.Helper()
	source, err := os.ReadFile("runtime.go")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]struct{}{}
	var handlers []string
	for _, match := range mutationRegistration.FindAllStringSubmatch(string(source), -1) {
		if _, already := seen[match[1]]; already {
			continue
		}
		seen[match[1]] = struct{}{}
		handlers = append(handlers, match[1])
	}
	sort.Strings(handlers)
	return handlers
}

// callGraph maps each function in the package to the names it calls. Method
// receivers are ignored: within one package a name is enough to follow, and
// resolving types here would buy precision this sweep does not need.
func callGraph(files []*ast.File) map[string][]string {
	graph := make(map[string][]string, 512)
	for _, file := range files {
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			var called []string
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch target := call.Fun.(type) {
				case *ast.Ident:
					called = append(called, target.Name)
				case *ast.SelectorExpr:
					called = append(called, target.Sel.Name)
				}
				return true
			})
			graph[function.Name.Name] = append(graph[function.Name.Name], called...)
		}
	}
	return graph
}

// reachesAudit walks the call graph from one handler. Depth is bounded by the
// visited set rather than a counter, so a cycle terminates and a long chain
// still resolves.
func reachesAudit(name string, graph map[string][]string, visited map[string]bool) bool {
	if visited[name] {
		return false
	}
	visited[name] = true
	for _, called := range graph[name] {
		if _, isAudit := auditEntryPoints[called]; isAudit {
			return true
		}
		if reachesAudit(called, graph, visited) {
			return true
		}
	}
	return false
}
