package bolt

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The design says there is no bbolt write that bypasses the journal (§6.1.4).
// Nothing made that true except that somebody converted 96 call sites once.
//
// `journal_entry.go` already says it in a comment — "*bbolt.Tx anywhere in this
// package would be a way to write without recording" — and a comment is what
// this package had. Adding `s.db.Update(func(tx *bbolt.Tx) error { … })` to any
// other file compiles, passes every test, and writes state that no journal
// frame describes. On a Standalone instance that state is simply unrecorded; on
// a Replica it is state that never arrives, and the first symptom is a
// promotion that is missing it.
//
// So the recorder is the only way in, and this is what holds the door. The
// entry layer and the journal's own attach path are named exemptions with
// their reasons, because both legitimately hold a raw transaction: one wraps
// it, the other applies the journal *to* bbolt and must not re-record what it
// is replaying.
//
// This catches drift, not evasion. A handler defined in an exempt file and
// called from elsewhere would pass, and that is fine — the failure this is for
// is somebody reaching for the obvious shape without knowing it was closed.
var rawTransactionExemptions = map[string]string{
	"journal_entry.go": "the entry layer itself: it takes the raw transaction and wraps it in the " +
		"recorder that every other file receives",
	"journal_attach.go": "applies the journal to bbolt on replay, and writes the journal's own " +
		"epoch and sequence bookkeeping — both must not be recorded as ordinary writes",
	"journal_tx.go": "the recorder type itself; the raw transaction is the field it wraps",
}

func TestTheRecorderIsTheOnlyWayToWriteMetadata(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fileSet := token.NewFileSet()
	var offenders []string
	var exemptionsUsed []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fileSet, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		var holdsRaw bool
		ast.Inspect(parsed, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "Tx" {
				return true
			}
			if pkg, ok := selector.X.(*ast.Ident); ok && pkg.Name == "bbolt" {
				holdsRaw = true
			}
			return true
		})
		if !holdsRaw {
			continue
		}
		if _, exempt := rawTransactionExemptions[name]; exempt {
			exemptionsUsed = append(exemptionsUsed, name)
			continue
		}
		offenders = append(offenders, name)
	}

	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("these files take a raw *bbolt.Tx, which writes without recording a journal "+
			"frame: %v\nuse the recorder (update/updateAll/batch/view in journal_entry.go). "+
			"If a file genuinely has to hold the raw transaction, add it to "+
			"rawTransactionExemptions with the reason", offenders)
	}

	// A stale exemption is not dangerous, but it is a claim about the package
	// that stopped being true, and this list is read as documentation.
	for name := range rawTransactionExemptions {
		var used bool
		for _, seen := range exemptionsUsed {
			if seen == name {
				used = true
			}
		}
		if !used {
			t.Errorf("%s is exempted from the recorder rule but no longer holds a raw "+
				"*bbolt.Tx; drop the exemption", name)
		}
	}
}
