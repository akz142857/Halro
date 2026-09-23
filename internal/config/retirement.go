package config

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// A Retirement is a configuration key that no longer exists, recorded here so
// that meeting one names its replacement.
//
// Without the table an operator holding a released config meets the YAML
// decoder instead: `field elevation_window not found in type
// config.ModelCapabilityDetection` names a Go type and not the key that took
// over. That is what v0.3.0's config still gets today. Every removal adds a row
// rather than a hand-written branch, so the next one cannot repeat it.
//
// The table is not a compatibility layer. Nothing in the runtime reads a
// retired key: Decode refuses the file, and `halro config migrate` edits the
// file on disk so the retired key stops existing. The moment something read one
// of these values this would be exactly the compatibility layer pre-1.0.0
// exists to avoid.
type Retirement struct {
	// Path is the dotted YAML path the key had, as an operator's file still
	// spells it.
	Path string
	// ReplacedBy is the path that took the value over. Empty when the key was
	// removed and nothing replaced it.
	ReplacedBy string
	// Judgement, when set, is why a human has to decide rather than a migrator.
	// It is what refuses the mechanical move: a key whose name survived but
	// whose meaning did not, a security default that became stricter, or one
	// value splitting into two. An empty Judgement means moving the value is a
	// rename and nothing else.
	Judgement string
	// Why is the sentence an operator reads. It says what changed, not what to
	// type.
	Why string
}

// retirements is ordered outermost path first, so deleting a section is what
// removes the keys inside it.
var retirements = []Retirement{
	{
		Path:       "admin.model_capability_detection.elevation_window",
		ReplacedBy: "admin.reauth_elevation_window",
		Judgement: "the replacement is wider than what you set: the old window covered " +
			"capability detection alone, the new one covers every step-up endpoint. " +
			"Carrying the value across would apply a window chosen for one action to " +
			"all of them, so choose it again rather than inheriting it",
		Why: "re-authentication stopped being a model-capability-detection setting and " +
			"became one policy for every step-up endpoint",
	},
	{
		Path:       "circuit_breaker",
		ReplacedBy: "routing",
		Why: "the breaker counted only upstreams that stopped answering, and counted an " +
			"upstream that answered and refused — a 429, a 401, a spent quota — as a " +
			"success that cleared the failure streak; routing covers both",
	},
	{
		Path:       "circuit_breaker.consecutive_failures",
		ReplacedBy: "routing.availability_failures",
		Why:        "the same count, under the policy that now owns upstreams which stop answering",
	},
	{
		Path:       "circuit_breaker.open_duration",
		ReplacedBy: "routing.suspend_for",
		Why: "the same first suspension, which now doubles towards routing.max_suspend_for " +
			"instead of staying fixed",
	},
	{
		Path:       "circuit_breaker.half_open_max_requests",
		ReplacedBy: "routing.probe_requests",
		Why:        "the same probe count, renamed to say what the requests are for",
	},
}

// Retirements returns the table. It is a copy: a caller reporting on it must not
// be able to edit what Decode refuses by.
func Retirements() []Retirement {
	out := make([]Retirement, len(retirements))
	copy(out, retirements)
	return out
}

// RetiredKeyError is what an operator holding a retired key gets instead of a
// decoder error naming a Go type.
type RetiredKeyError struct {
	Found []Retirement
	// Migratable reports whether `halro config migrate` can do the whole edit.
	Migratable bool
}

// Error is one line, because an error travels through log handlers and generic
// printers that would flatten anything else. A caller that can lay the findings
// out — the CLI's failure reporter — reads Found instead.
func (e *RetiredKeyError) Error() string {
	named := make([]string, 0, len(e.Found))
	for _, found := range e.Found {
		if found.ReplacedBy == "" {
			named = append(named, found.Path+" was removed")
			continue
		}
		named = append(named, found.Path+" is now "+found.ReplacedBy)
	}
	if len(named) == 1 {
		return "configuration holds a retired key: " + named[0]
	}
	return fmt.Sprintf("configuration holds %d retired keys: %s", len(named), strings.Join(named, "; "))
}

// Remedy is the sentence an operator acts on, kept beside the finding rather
// than repeated per key.
func (e *RetiredKeyError) Remedy() string {
	if e.Migratable {
		return "Run `halro config migrate --config <path>` to see the edit, then again " +
			"with --write to apply it. It carries your values across; a key you leave " +
			"unset takes its built-in default."
	}
	return "`halro config migrate` will not do this one for you — the reason is above. " +
		"It can still move every other retired key."
}

// findRetired returns the retired keys present in document order of the table.
func findRetired(root *yaml.Node) []Retirement {
	var found []Retirement
	for _, retirement := range retirements {
		if key, _ := lookupPath(root, retirement.Path); key != nil {
			found = append(found, retirement)
		}
	}
	return found
}

// lookupPath resolves a dotted path to its key and value nodes, or nil when the
// path is absent.
func lookupPath(root *yaml.Node, path string) (key, value *yaml.Node) {
	node := root
	if node != nil && node.Kind == yaml.DocumentNode {
		if len(node.Content) == 0 {
			return nil, nil
		}
		node = node.Content[0]
	}
	for _, segment := range strings.Split(path, ".") {
		if node == nil || node.Kind != yaml.MappingNode {
			return nil, nil
		}
		var next *yaml.Node
		for index := 0; index+1 < len(node.Content); index += 2 {
			if node.Content[index].Value == segment {
				key, next = node.Content[index], node.Content[index+1]
				break
			}
		}
		if next == nil {
			return nil, nil
		}
		node = next
	}
	return key, node
}

// MigrationAction is one edit `halro config migrate` made or would make.
type MigrationAction struct {
	Description string
	Removed     []string
	Added       []string
}

// MigrationResult is what Migrate did to a configuration file.
type MigrationResult struct {
	// Output is the migrated file. It is empty when Refusals is not.
	Output  []byte
	Actions []MigrationAction
	// Refusals names every retired key a human has to decide, with the reason.
	// A refusal is total: the file is left alone rather than half-migrated, so
	// that what `config check` then reports is the whole remaining edit.
	Refusals []string
}

// Migrate applies the mechanical half of the retirement table to a
// configuration file: it deletes a retired key and writes its value under the
// path that replaced it, preserving the value the operator chose. It never
// changes a value, and never fills in a key that is merely absent — an omitted
// key already takes its built-in default at load.
//
// It refuses as a whole if any retired key present needs judgement, and it
// refuses to hand back output that the loader would reject, so a file this
// wrote always passes `halro config check`.
func Migrate(source []byte) (MigrationResult, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(source, &root); err != nil {
		return MigrationResult{}, fmt.Errorf("parse config: %w", err)
	}
	found := findRetired(&root)
	declared, versionLine, err := declaredVersion(&root)
	if err != nil {
		return MigrationResult{}, err
	}
	if len(found) == 0 && declared == SchemaVersion {
		return MigrationResult{Output: source}, nil
	}

	var result MigrationResult
	if declared > SchemaVersion {
		result.Refusals = append(result.Refusals, fmt.Sprintf(
			"version is %d and this Halro only knows %d: the file was written by a newer Halro, "+
				"and nothing here can migrate a shape it has never seen backwards",
			declared, SchemaVersion))
		return result, nil
	}
	for _, retirement := range found {
		if retirement.Judgement != "" {
			result.Refusals = append(result.Refusals, fmt.Sprintf(
				"%s is now %s, but %s", retirement.Path, retirement.ReplacedBy, retirement.Judgement))
		}
	}
	result.Refusals = append(result.Refusals, conflictingMoves(&root, found)...)
	if len(result.Refusals) > 0 {
		return result, nil
	}

	lines := splitLines(source)
	edits, err := planEdits(&root, lines, found)
	if err != nil {
		return MigrationResult{}, err
	}
	if declared != SchemaVersion {
		edits = append(edits, edit{
			start:       versionLine,
			end:         versionLine,
			replacement: []string{fmt.Sprintf("version: %d", SchemaVersion)},
			description: fmt.Sprintf("advance the schema version from %d to %d", declared, SchemaVersion),
		})
	}
	// Descending, so an earlier edit's line numbers stay valid.
	sort.Slice(edits, func(a, b int) bool { return edits[a].start > edits[b].start })
	for _, edit := range edits {
		result.Actions = append(result.Actions, MigrationAction{
			Description: edit.description,
			Removed:     append([]string(nil), lines[edit.start-1:edit.end]...),
			Added:       append([]string(nil), edit.replacement...),
		})
		rest := append(append([]string(nil), edit.replacement...), lines[edit.end:]...)
		lines = append(lines[:edit.start-1], rest...)
	}

	output := []byte(strings.Join(lines, "\n"))
	if len(source) > 0 && source[len(source)-1] == '\n' {
		output = append(output, '\n')
	}
	if err := loadsAfterMigration(output); err != nil {
		return MigrationResult{}, fmt.Errorf("the migrated configuration would not load, so none of it was written: %w", err)
	}
	result.Output = output
	return result, nil
}

// declaredVersion reads the file's own `version`, which says which shape of
// configuration it is. A file that declares none is not a shape any release
// shipped, and inventing one would be guessing at what the rest of the file
// means — so it is refused rather than repaired.
func declaredVersion(root *yaml.Node) (version, line int, err error) {
	key, value := lookupPath(root, "version")
	if key == nil {
		return 0, 0, errors.New("the configuration declares no `version`, so there is no shape to migrate from; add `version: 1` if this file came from a release, or start from the file `halro start` writes")
	}
	if _, scanErr := fmt.Sscanf(value.Value, "%d", &version); scanErr != nil {
		return 0, 0, fmt.Errorf("the configuration's `version` is %q, which is not a schema version", value.Value)
	}
	return version, key.Line, nil
}

// loadsAfterMigration is the guarantee that a file this wrote passes
// `halro config check`. The two host-facing gates are relaxed: whether a
// listener may bind and whether a public gateway is allowed are facts about the
// deployment, unchanged by moving a key, and refusing on them would blame the
// migration for something it did not do. Everything else — a moved value now
// out of range against a key it never had to agree with before — is exactly
// what this must catch before writing.
func loadsAfterMigration(output []byte) error {
	cfg, err := Decode(bytes.NewReader(output))
	if err != nil {
		return err
	}
	if err := cfg.Normalize(); err != nil {
		return err
	}
	return cfg.Validate(LoadOptions{AllowInsecurePublicGateway: true, SkipListenerValidation: true})
}

// edit is one contiguous line range replaced by zero or more lines. Deletion
// and the block that replaces it are the same operation so that the new section
// lands where the old one was, keeping the annotated file's grouping and the
// diff local.
type edit struct {
	start, end  int // inclusive, 1-indexed; end < start means insert before start
	replacement []string
	description string
}

// movedValue is one retired key's value on its way to the path that replaced it.
type movedValue struct {
	to    string
	value string
}

// conflictingMoves names every retired key whose destination the operator has
// already set by hand. Writing the value across would produce a duplicate key,
// and picking a winner would be choosing between two values a human wrote.
func conflictingMoves(root *yaml.Node, found []Retirement) []string {
	var conflicts []string
	for _, retirement := range found {
		if retirement.ReplacedBy == "" {
			continue
		}
		oldKey, oldValue := lookupPath(root, retirement.Path)
		if oldKey == nil || oldValue.Kind != yaml.ScalarNode {
			continue
		}
		if newKey, _ := lookupPath(root, retirement.ReplacedBy); newKey != nil {
			conflicts = append(conflicts, fmt.Sprintf(
				"%s is now %s, and both are set. Delete whichever is not the value you want; "+
					"nothing here can choose between two values a person wrote",
				retirement.Path, retirement.ReplacedBy))
		}
	}
	return conflicts
}

func planEdits(root *yaml.Node, lines []string, found []Retirement) ([]edit, error) {
	var cuts []edit
	// anchors records, per destination section, the cut it should be written
	// into when the file has no such section yet.
	anchors := make(map[string]int)
	moves := make(map[string][]movedValue)
	var order []string

	for _, retirement := range found {
		key, value := lookupPath(root, retirement.Path)
		if key == nil {
			continue
		}
		parent, leaf := splitPath(retirement.ReplacedBy)
		if retirement.ReplacedBy != "" && value.Kind == yaml.ScalarNode {
			if parent == "" {
				return nil, fmt.Errorf("%s is replaced by a top-level scalar, which this migrator does not write", retirement.Path)
			}
			if _, seen := moves[parent]; !seen {
				order = append(order, parent)
			}
			moves[parent] = append(moves[parent], movedValue{to: leaf, value: value.Value})
		}
		if coveredByACut(cuts, key.Line) {
			continue
		}
		cut := blockExtent(lines, key.Line, key.Column-1)
		cuts = append(cuts, edit{
			start:       cut[0],
			end:         cut[1],
			description: fmt.Sprintf("remove %s", retirement.Path),
		})
		if retirement.ReplacedBy != "" {
			section, _ := splitPath(retirement.ReplacedBy)
			if section == "" {
				section = retirement.ReplacedBy
			}
			if _, seen := anchors[section]; !seen {
				anchors[section] = len(cuts) - 1
			}
		}
	}

	for _, parent := range order {
		entries := moves[parent]
		if key, _ := lookupPath(root, parent); key != nil {
			indent := strings.Repeat(" ", key.Column-1+2)
			added := make([]string, 0, len(entries))
			for _, entry := range entries {
				added = append(added, fmt.Sprintf("%s%s: %s", indent, entry.to, entry.value))
			}
			cuts = append(cuts, edit{
				start:       key.Line + 1,
				end:         key.Line,
				replacement: added,
				description: fmt.Sprintf("write %d value(s) into the existing %s", len(added), parent),
			})
			continue
		}
		index, anchored := anchors[parent]
		if !anchored {
			return nil, fmt.Errorf("nothing to anchor the new %s section to", parent)
		}
		block := []string{
			fmt.Sprintf("# Written by `halro config migrate` from %s.", retiredSourceOf(found, parent)),
			parent + ":",
		}
		for _, entry := range entries {
			block = append(block, fmt.Sprintf("  %s: %s", entry.to, entry.value))
		}
		cuts[index].replacement = block
		cuts[index].description = fmt.Sprintf("replace %s with %s, carrying its values",
			retiredSourceOf(found, parent), parent)
	}

	// A pure deletion between two blank lines would otherwise leave a double
	// blank where the section was.
	for index, cut := range cuts {
		if len(cut.replacement) > 0 || cut.end < cut.start {
			continue
		}
		if cut.start > 1 && strings.TrimSpace(lines[cut.start-2]) == "" &&
			cut.end < len(lines) && strings.TrimSpace(lines[cut.end]) == "" {
			cuts[index].end++
		}
	}
	return cuts, nil
}

// retiredSourceOf names the retired section whose values a destination is
// receiving, for the provenance comment the migration writes.
func retiredSourceOf(found []Retirement, destination string) string {
	for _, retirement := range found {
		if retirement.ReplacedBy == destination {
			return retirement.Path
		}
		if parent, _ := splitPath(retirement.ReplacedBy); parent == destination {
			if section, _ := splitPath(retirement.Path); section != "" {
				return section
			}
			return retirement.Path
		}
	}
	return "a retired key"
}

func splitPath(path string) (parent, leaf string) {
	index := strings.LastIndex(path, ".")
	if index < 0 {
		return "", path
	}
	return path[:index], path[index+1:]
}

func coveredByACut(cuts []edit, line int) bool {
	for _, cut := range cuts {
		if line >= cut.start && line <= cut.end {
			return true
		}
	}
	return false
}

// blockExtent returns the inclusive 1-indexed line range a key owns: the
// contiguous comment block above it, the key itself, and everything indented
// under it. Comments below the block belong to the key that follows and stay.
func blockExtent(lines []string, keyLine, keyIndent int) [2]int {
	start := keyLine
	for start > 1 && isComment(lines[start-2]) {
		start--
	}
	end := keyLine
	for line := keyLine + 1; line <= len(lines); line++ {
		text := lines[line-1]
		if strings.TrimSpace(text) == "" || isComment(text) {
			continue
		}
		if indentOf(text) <= keyIndent {
			break
		}
		end = line
	}
	return [2]int{start, end}
}

func isComment(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), "#")
}

func indentOf(line string) int {
	return len(line) - len(strings.TrimLeft(line, " "))
}

func splitLines(source []byte) []string {
	text := strings.TrimSuffix(string(source), "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

// refuseRetiredKeys is the refusal Decode runs before the strict decode.
func refuseRetiredKeys(source []byte) error {
	var root yaml.Node
	if err := yaml.Unmarshal(source, &root); err != nil {
		// Not YAML at all is the strict decoder's error to report, not this one's.
		return nil
	}
	found := findRetired(&root)
	if len(found) == 0 {
		return nil
	}
	migratable := true
	for _, retirement := range found {
		if retirement.Judgement != "" {
			migratable = false
		}
	}
	return &RetiredKeyError{Found: found, Migratable: migratable}
}
