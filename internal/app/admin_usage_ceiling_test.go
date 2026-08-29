package app

import "testing"

// The card answers "how many requests per second can this instance take", and
// there are two independent serialization points behind that: one Ledger writer
// for the whole process, and one accounting lock per project. The headline used
// to be the project lock alone, which read as a servable rate while the
// durability barrier — the one the card's own caption names — held it three
// orders of magnitude lower.
func TestRequestCeilingTakesWhicheverWritePathBindsFirst(t *testing.T) {
	for _, test := range []struct {
		name            string
		wal, project    float64
		wantRate        float64
		wantBoundBy     string
		wantUnreachable bool
	}{
		{
			// The reading this fix came from: a 3.07 ms barrier carrying one
			// record at a time is 326 events/s, against a project lock that would
			// allow half a million.
			name: "an uncoalesced barrier binds far below the project lock",
			wal:  325.7, project: 500_000,
			wantRate: 65.14, wantBoundBy: "wal",
		},
		{
			// And the other way round: a project holding its lock for 22 ms is
			// the constraint even on a fast disk with healthy coalescing.
			name: "a slow project lock binds below a coalescing barrier",
			wal:  1918.6, project: 45.2,
			wantRate: 9.04, wantBoundBy: "project",
		},
		{
			// Zero is "not measured yet". An instance that has taken project-lock
			// traffic but not yet synced must not be reported as serving nothing.
			name: "an unmeasured barrier does not win the minimum",
			wal:  0, project: 45.2,
			wantRate: 9.04, wantBoundBy: "project",
		},
		{
			name: "an unmeasured project lock does not win the minimum",
			wal:  1918.6, project: 0,
			wantRate: 383.72, wantBoundBy: "wal",
		},
		{
			// Nothing measured has no answer, and must not reach the screen as a
			// zero rate an operator would read as a stalled instance.
			name: "nothing measured yields no answer at all",
			wal:  0, project: 0,
			wantUnreachable: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			rate, boundBy := requestCeiling(test.wal, test.project)
			if test.wantUnreachable {
				if rate != 0 || boundBy != "" {
					t.Fatalf("an unmeasured instance reported %g req/s bound by %q", rate, boundBy)
				}
				return
			}
			if boundBy != test.wantBoundBy {
				t.Errorf("bound by %q, want %q", boundBy, test.wantBoundBy)
			}
			if delta := rate - test.wantRate; delta > 0.01 || delta < -0.01 {
				t.Errorf("ceiling is %g req/s, want %g", rate, test.wantRate)
			}
		})
	}
}
