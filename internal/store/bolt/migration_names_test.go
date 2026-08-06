package bolt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Migration names are recorded in every instance that has run them. Renaming
// one is not a refactor: it makes an upgraded instance disagree with its own
// history. The "phase2" identifiers were renamed to InferenceResources and
// these strings deliberately were not, so this pins them against a future sweep
// that tidies up the last remaining occurrences.
func TestMigrationHistoryNamesAreFrozen(t *testing.T) {
	frozen := map[uint64]string{
		6: "phase2_capability_evidence",
	}
	byVersion := make(map[uint64]string, len(migrations))
	for _, migration := range migrations {
		byVersion[migration.version] = migration.name
	}
	for version, name := range frozen {
		if byVersion[version] != name {
			t.Fatalf("migration %d is named %q, want the recorded %q — renaming it breaks upgrades", version, byVersion[version], name)
		}
	}
	// The step names travel into the same recorded history as the migration name.
	source, err := os.ReadFile(filepath.Join(".", "store.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []string{"before_phase2_capability_evidence", "after_phase2_capability_evidence"} {
		if !strings.Contains(string(source), `"`+step+`"`) {
			t.Fatalf("migration step %q is gone from store.go — step names are recorded history too", step)
		}
	}
}
