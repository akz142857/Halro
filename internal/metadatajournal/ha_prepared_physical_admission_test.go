package metadatajournal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

type preparedReleaseDebt struct {
	releases  int
	uncertain bool
}

func (a *preparedReleaseDebt) Release() error {
	a.releases++
	if a.uncertain && a.releases == 1 {
		return errors.New("admission join uncertain")
	}
	return nil
}

func TestBoundPreparedPhysicalRefusalJoinsOriginalAdmission(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		t.Run(map[bool]string{false: "known", true: "unknown"}[uncertain], func(t *testing.T) {
			path, l, a := boundReplicationFixture(t)
			guard, err := lock.Acquire(filepath.Dir(path))
			if err != nil {
				t.Fatal(err)
			}
			defer guard.Close()
			admission := &preparedReleaseDebt{uncertain: uncertain}
			type requestKey struct{}
			ctx := context.WithValue(context.Background(), requestKey{}, "actual-request")
			if err = l.SetBoundReplicationAdmissionAndAfterDurable(a, func(b DurableBatch) (AppendAdmission, error) {
				if b.Context != ctx || b.Context.Value(requestKey{}) != "actual-request" {
					t.Fatal("original prepared context lost")
				}
				return admission, nil
			}, func(DurableBatch) (uint64, error) {
				t.Fatal("prewrite refusal transferred admission to Ordering")
				return 0, nil
			}, func(context.Context, uint64) error { t.Fatal("prewrite refusal confirmed"); return nil }); err != nil {
				t.Fatal(err)
			}
			if err = l.WithBoundPhysicalNative(ctx, metadataFrozenBudget(t), guard, a, false, func(q *PhysicalNativeLoan) error { return q.Enable() }); err != nil {
				t.Fatal(err)
			}
			before := l.Head()
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			_, err = l.AppendBoundPreparedContext(ctx, a, []Op{put("projects", "p", "v")}, func(_ Record, durable func() error) error { return durable() })
			if !errors.Is(err, resourcebudget.ErrPhysicalAllocation) || l.Head() != before || admission.releases != 1 {
				t.Fatal(err, l.Head(), before, admission)
			}
			after, readErr := os.ReadFile(path)
			if readErr != nil || string(after) != string(original) {
				t.Fatal("prewrite refusal changed native bytes", readErr)
			}
			if uncertain {
				if len(l.uncertainAdmissions) != 1 || l.uncertainAdmissions[0] != admission || l.terminalErr == nil {
					t.Fatal("lost admission debt", l.uncertainAdmissions, l.terminalErr)
				}
				entered := false
				if _, err = l.AppendBoundPreparedContext(ctx, a, []Op{put("projects", "again", "v")}, func(Record, func() error) error { entered = true; return nil }); err == nil || entered {
					t.Fatal("uncertain owner admitted further IO", err)
				}
			} else if len(l.uncertainAdmissions) != 0 || l.terminalErr != nil {
				t.Fatal("known release retained debt")
			}
			if err = l.Close(); err != nil {
				t.Fatal(err)
			}
			expected := 1
			if uncertain {
				expected = 2
			}
			if admission.releases != expected {
				t.Fatal("known native close failed to join original debt", admission)
			}
		})
	}
}
