package app

import (
	"os"
	"slices"
	"testing"
)

// Crash-point matrices SIGKILL a real process at every publication phase and
// dominate this package's run time. By default each runs representative
// phases (its start, a middle publication and its end, across every role or
// mode); HALRO_STRESS=1, the release stress gate, runs every phase.
func stressMatrixCase(t *testing.T, representative bool) {
	t.Helper()
	if !representative && os.Getenv("HALRO_STRESS") != "1" {
		t.Skip("full crash-point matrix runs with HALRO_STRESS=1")
	}
}

func stressPhase(t *testing.T, phase string, representative ...string) {
	t.Helper()
	stressMatrixCase(t, slices.Contains(representative, phase))
}
