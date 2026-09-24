package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	referenceconfigs "github.com/akz142857/Halro/configs"
)

// A bound an operator cannot guess has to be written where they set the value.
//
// The gate next door proves every key is *mentioned*. That is not the same as
// documented: `metrics.max_concurrent_scrapes` said "maximum scrape requests
// handled concurrently" and never said the ceiling is 32, and
// `audit.anchor.credential_file` never said it must differ from
// `metrics.credential_file`. The rule lived only in the validation message,
// which an operator reaches by failing to start — after editing the file,
// restarting, and reading a log. Twenty keys were in that state.
//
// So this reads the constraints out of Validate() itself and holds the
// reference file to them. A new bound added in config.go with no matching
// sentence in config.example.yaml fails here, which is the only place the two
// can be kept in step: nothing else connects a validation message to the
// paragraph an operator reads before typing a number.
//
// What it checks is deliberately narrow — the parts of a message that cannot be
// inferred from the key's name: the numbers, and the other configuration keys
// it names. Prose is not compared; wording stays the author's business.

// constraintMarkers are the shapes of validation message this gate reads. A
// message outside the list is not a constraint an operator has to be told in
// advance — "is required in file mode" describes the mode they already chose.
var constraintMarkers = []string{
	"must be between",
	"must be at least",
	"must be at most",
	"must not exceed",
	"must differ from",
	"requires ",
}

var (
	// A dotted configuration path, as the validation messages spell them.
	constraintKeyPath = regexp.MustCompile(`\b[a-z][a-z0-9_]*(?:\.[a-z][a-z0-9_]+)+\b`)
	constraintNumber  = regexp.MustCompile(`\b\d+\b`)
	// A yaml key, live or commented out. The leading comment marker is stripped
	// first, so the key_slots block — which can only be documented commented,
	// because validation refuses to see those fields beside `file` — is in
	// scope rather than silently exempt.
	referenceKeyLine  = regexp.MustCompile(`^(\s*)(?:-\s+)?([a-z0-9_]+):`)
	referenceComment  = regexp.MustCompile(`^(\s*)#\s?`)
	referenceDescLine = regexp.MustCompile(`^\s*#\s*@description\.[a-zA-Z-]+\s*(.*)$`)
)

// referenceEntry is one documented key: the full dotted path, and every
// description line attached to it in either language.
type referenceEntry struct {
	path        string
	description string
}

// readConfigurationReference walks configs/config.example.yaml and returns the
// documented keys by full path, tracking nesting by indentation the way the
// file is actually read.
func readConfigurationReference() map[string]referenceEntry {
	lines := strings.Split(string(referenceconfigs.ExampleYAML), "\n")
	entries := map[string]referenceEntry{}
	type frame struct {
		indent int
		name   string
	}
	var stack []frame
	var pending []string
	for _, raw := range lines {
		line := referenceComment.ReplaceAllString(raw, "$1")
		if match := referenceDescLine.FindStringSubmatch(raw); match != nil {
			pending = append(pending, match[1])
			continue
		}
		match := referenceKeyLine.FindStringSubmatch(line)
		if match == nil {
			if strings.TrimSpace(raw) == "" {
				pending = nil
			}
			continue
		}
		indent, name := len(match[1]), match[2]
		for len(stack) > 0 && stack[len(stack)-1].indent >= indent {
			stack = stack[:len(stack)-1]
		}
		names := make([]string, 0, len(stack)+1)
		for _, item := range stack {
			names = append(names, item.name)
		}
		path := strings.Join(append(names, name), ".")
		stack = append(stack, frame{indent: indent, name: name})
		entries[path] = referenceEntry{path: path, description: strings.Join(pending, " ")}
		pending = nil
	}
	return entries
}

// validationConstraints reads every constraint message Validate() can raise,
// keyed by the configuration path the message opens with.
func validationConstraints(t *testing.T) map[string][]string {
	t.Helper()
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, "../config/config.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	constraints := map[string][]string{}
	ast.Inspect(parsed, func(node ast.Node) bool {
		literal, ok := node.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		message, err := strconv.Unquote(literal.Value)
		if err != nil {
			return true
		}
		subject := constraintKeyPath.FindString(message)
		if subject == "" || !strings.HasPrefix(message, subject) {
			return true
		}
		for _, marker := range constraintMarkers {
			if strings.Contains(message, marker) {
				constraints[subject] = append(constraints[subject], message)
				break
			}
		}
		return true
	})
	return constraints
}

func TestEveryValidationBoundIsWrittenWhereTheValueIsSet(t *testing.T) {
	reference := readConfigurationReference()
	constraints := validationConstraints(t)
	if len(constraints) < 10 {
		t.Fatalf("read %d constraints out of config.go; the reader stopped matching", len(constraints))
	}

	var undocumented []string
	for subject, messages := range constraints {
		leaf := subject[strings.LastIndex(subject, ".")+1:]
		entry, ok := reference[subject]
		if !ok {
			// The message names a section rather than a leaf an operator sets
			// (`metrics.tls files cannot be set ...`). Nothing to annotate.
			continue
		}
		for _, message := range messages {
			for _, want := range requiredTerms(message, subject, leaf, reference) {
				if !strings.Contains(entry.description, want) {
					undocumented = append(undocumented,
						subject+": description never mentions "+strconv.Quote(want)+
							"\n      validation says: "+message)
				}
			}
		}
	}
	if len(undocumented) > 0 {
		sort.Strings(undocumented)
		t.Fatalf("configs/config.example.yaml leaves %d validation bound(s) undocumented:\n  %s\n"+
			"add the bound to that key's @description lines in both languages, so it is read before the value is typed, not after the start fails",
			len(undocumented), strings.Join(undocumented, "\n  "))
	}
}

// requiredTerms returns the parts of a validation message that a reader cannot
// infer from the key's own name: the numbers it names, and the other
// configuration keys it depends on.
func requiredTerms(message, subject, leaf string, reference map[string]referenceEntry) []string {
	var terms []string
	for _, number := range constraintNumber.FindAllString(message, -1) {
		// A lower bound of one is what a count means; only the bounds that
		// carry information are required.
		if value, err := strconv.Atoi(number); err == nil && value >= 2 {
			terms = append(terms, number)
		}
	}
	for _, path := range constraintKeyPath.FindAllString(message, -1) {
		if path == subject {
			continue
		}
		if _, ok := reference[path]; !ok {
			continue
		}
		// Naming the sibling's own leaf is enough; a description written inside
		// that section reads better without repeating the whole path.
		other := path[strings.LastIndex(path, ".")+1:]
		if other == leaf {
			continue
		}
		terms = append(terms, other)
	}
	return terms
}
