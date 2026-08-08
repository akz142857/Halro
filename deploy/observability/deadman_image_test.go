package observability_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

const (
	modulePath        = "github.com/akz142857/Halro/"
	deadmanCommand    = "./cmd/halro-deadman"
	deadmanDockerfile = "external-probe/Dockerfile"
)

var copyDirective = regexp.MustCompile(`(?m)^COPY\s+((?:cmd|internal)/\S+)\s`)

// The dead-man image is built from a hand-listed set of source directories
// rather than the whole tree, because it exists to keep watching when the
// gateway cannot: it must not silently acquire the gateway's dependencies, its
// attack surface, or its build failures.
//
// The cost of that choice is that adding an import to the dead-man breaks the
// image, and nothing in a normal build says so — `go build ./...` is happy,
// `go test ./...` is happy, and the failure only appears when CI builds the
// image. It stayed broken on main for at least three commits that way: the
// binary imports internal/safelog, the Dockerfile never copied it.
func TestDeadmanImageCopiesEveryPackageItNeeds(t *testing.T) {
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dockerfile, err := os.ReadFile(filepath.Join(repoRoot, "deploy/observability", deadmanDockerfile))
	if err != nil {
		t.Fatal(err)
	}
	copied := map[string]bool{}
	for _, match := range copyDirective.FindAllStringSubmatch(string(dockerfile), -1) {
		copied[strings.TrimSuffix(match[1], "/")] = true
	}
	if len(copied) == 0 {
		t.Fatal("no source COPY directives found; the Dockerfile or this pattern changed")
	}

	// The build's own answer, not a second list that could drift from it.
	command := exec.Command("go", "list", "-deps", deadmanCommand)
	command.Dir = repoRoot
	output, err := command.Output()
	if err != nil {
		t.Fatalf("go list -deps %s: %v", deadmanCommand, err)
	}

	var missing []string
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if !strings.HasPrefix(line, modulePath) {
			continue
		}
		directory := strings.TrimPrefix(line, modulePath)
		if !copied[directory] {
			missing = append(missing, directory)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("the dead-man imports %s, which %s does not COPY; the image cannot build",
			strings.Join(missing, ", "), deadmanDockerfile)
	}
}

// The other half of the same promise: a directory copied but no longer needed
// widens the image for nothing and hides the fact that the dependency went
// away.
func TestDeadmanImageCopiesNothingItDoesNotNeed(t *testing.T) {
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dockerfile, err := os.ReadFile(filepath.Join(repoRoot, "deploy/observability", deadmanDockerfile))
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("go", "list", "-deps", deadmanCommand)
	command.Dir = repoRoot
	output, err := command.Output()
	if err != nil {
		t.Fatalf("go list -deps %s: %v", deadmanCommand, err)
	}
	needed := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if strings.HasPrefix(line, modulePath) {
			needed[strings.TrimPrefix(line, modulePath)] = true
		}
	}

	var unused []string
	for _, match := range copyDirective.FindAllStringSubmatch(string(dockerfile), -1) {
		directory := strings.TrimSuffix(match[1], "/")
		if !needed[directory] {
			unused = append(unused, directory)
		}
	}
	if len(unused) > 0 {
		sort.Strings(unused)
		t.Fatalf("%s copies %s, which the dead-man no longer imports",
			deadmanDockerfile, strings.Join(unused, ", "))
	}
}
