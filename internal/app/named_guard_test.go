package app

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// namedTestPattern finds the test names comments cite. It requires the Test
// prefix plus enough of a name to be a real function rather than a type called
// TestAdapter or a field called TestRevision.
var namedTestPattern = regexp.MustCompile(`\bTest[A-Z][A-Za-z0-9_]{12,}\b`)

// A comment that says "X is what stops this" is load-bearing: the next person
// reads it instead of checking, which is what it is for. When X does not exist,
// the comment is worse than nothing — it spends the reader's suspicion on a
// guard that will never fire.
//
// This round found one: four comments and three documents named
// TestNoEndpointIsServedByATargetThatReasonsUnasked as the reason an unsafe
// profile could not be offered again, and no such function existed. It also
// found two comments naming tests that had been renamed underneath them, which
// are harmless and were still wrong. The cost of never having this class again
// is one walk of the tree.
func TestEveryTestNamedInACommentExists(t *testing.T) {
	root := repositoryRoot(t)
	defined := definedTestNames(t, root)
	if len(defined) == 0 {
		t.Fatal("no test functions were found, so this guard asserts nothing")
	}
	cited := map[string][]string{}
	walkGoFiles(t, root, func(path string, source []byte) {
		if strings.HasSuffix(path, "_test.go") {
			return
		}
		for _, line := range strings.Split(string(source), "\n") {
			trimmed := strings.TrimSpace(line)
			if !strings.HasPrefix(trimmed, "//") {
				continue
			}
			for _, name := range namedTestPattern.FindAllString(trimmed, -1) {
				cited[name] = append(cited[name], path)
			}
		}
	})
	for name, places := range cited {
		if _, exists := defined[name]; !exists {
			t.Errorf("%s is named as a guard in %s and does not exist", name, strings.Join(places, ", "))
		}
	}
}

func definedTestNames(t *testing.T, root string) map[string]struct{} {
	t.Helper()
	defined := map[string]struct{}{}
	pattern := regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]+)\(`)
	walkGoFiles(t, root, func(path string, source []byte) {
		if !strings.HasSuffix(path, "_test.go") {
			return
		}
		for _, match := range pattern.FindAllStringSubmatch(string(source), -1) {
			defined[match[1]] = struct{}{}
		}
	})
	return defined
}

func walkGoFiles(t *testing.T, root string, visit func(path string, source []byte)) {
	t.Helper()
	for _, tree := range []string{"internal", "cmd", "tests"} {
		base := filepath.Join(root, tree)
		if _, err := os.Stat(base); err != nil {
			continue
		}
		if err := filepath.WalkDir(base, func(path string, entry os.DirEntry, err error) error {
			if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") {
				return err
			}
			source, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			relative, relErr := filepath.Rel(root, path)
			if relErr != nil {
				relative = path
			}
			visit(relative, source)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil {
			return directory
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			t.Fatal("no go.mod above the working directory")
		}
		directory = parent
	}
}
