package budget

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
)

func TestGovernanceCohortRetireJoinsVisitorAndRefusesNewQueries(t *testing.T) {
	m, _, closeLog := newTestManager(t)
	defer closeLog()
	ctx := context.Background()
	w, _, err := m.CreateWorkUnitWithDefinitions(ctx, "prj_1", "key_1", 10,
		[]domain.OutcomeDefinitionRef{{ID: "odef_cohort", Version: 1}}, testGovernanceIntent("cohort"))
	if err != nil {
		t.Fatal(err)
	}
	q := ledger.GovernanceCohortQuery{ProjectID: "prj_1", DefinitionID: "odef_cohort", DefinitionVersion: 1, CohortStart: w.PeriodID, CohortEnd: w.PeriodID}
	entered, release := make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	defer unblock.Do(func() { close(release) })
	done := make(chan error, 1)
	go func() {
		_, err := m.VisitGovernanceCohort(ctx, q, func(row ledger.GovernanceCohortRow) error {
			if row.WorkUnitID != w.ID {
				return errors.New("cohort lost admitted unit")
			}
			close(entered)
			<-release
			return nil
		})
		done <- err
	}()
	<-entered
	retireWithDeadline(t, m)
	m.operationMu.Lock()
	if m.owner == nil || m.activeOperations != 1 {
		t.Error("visitor outlived its accounting owner")
	}
	m.operationMu.Unlock()
	if head, err := m.VisitGovernanceCohort(ctx, q, func(ledger.GovernanceCohortRow) error { t.Error("retired visitor ran"); return nil }); !errors.Is(err, ErrAccountingRetired) || head != (ledger.Watermark{}) {
		t.Fatal(head, err)
	}
	unblock.Do(func() { close(release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	assertRetiredOwnerReleased(t, m)
}
