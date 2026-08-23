package compatibility

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// docs/contracts/adding-a-platform.md is a map, not a memory aid: each step
// names the guard that fails when it is skipped. A map is only worth following
// while it matches the ground, and the way this one goes wrong is quiet — a
// guard renamed or deleted leaves the document citing a test that no longer
// runs, and the step it protects becomes silent again without anything saying
// so.
//
// This walks the repository rather than a list of its own, for the same reason
// the other completeness checks do.
func TestTheChecklistNamesGuardsThatExist(t *testing.T) {
	root := filepath.Join("..", "..")
	checklist, err := os.ReadFile(filepath.Join(root, "docs", "contracts", "adding-a-platform.md"))
	if err != nil {
		t.Fatal(err)
	}
	cited := regexp.MustCompile(`\bTest[A-Za-z0-9_]+`).FindAllString(string(checklist), -1)
	if len(cited) < 5 {
		t.Fatalf("the checklist cites %d guards; it is meant to name one per step", len(cited))
	}

	declared := map[string]struct{}{}
	err = filepath.Walk(filepath.Join(root, "internal"), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, "_test.go") {
			return err
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, name := range regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]+)\(`).FindAllStringSubmatch(string(source), -1) {
			declared[name[1]] = struct{}{}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range cited {
		if _, ok := declared[name]; !ok {
			t.Errorf("the checklist cites %s, which no test declares", name)
		}
	}
}
