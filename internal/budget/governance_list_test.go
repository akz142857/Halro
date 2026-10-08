package budget

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
)

type failingListReader struct {
	ledger.AccountingAdmissionReader
}

func (r failingListReader) ReadRunPage(ctx context.Context, q ledger.RunListQuery) (ledger.GovernanceListPage[domain.Run], error) {
	page, err := r.AccountingAdmissionReader.ReadRunPage(ctx, q)
	return page, errors.Join(err, admissionReadFailure)
}

func (r failingListReader) ReadWorkUnitPage(ctx context.Context, q ledger.WorkUnitListQuery) (ledger.GovernanceListPage[domain.WorkUnit], error) {
	page, err := r.AccountingAdmissionReader.ReadWorkUnitPage(ctx, q)
	return page, errors.Join(err, admissionReadFailure)
}

func TestGovernanceListManagerPendingMoneyByteResumeAndReadFailures(t *testing.T) {
	m, state, closeLog := newTestManager(t)
	defer closeLog()
	ctx := context.Background()
	unit, _, err := m.CreateWorkUnit(ctx, "prj_1", "key_1", 10, testGovernanceIntent("unit"))
	if err != nil {
		t.Fatal(err)
	}
	for _, intent := range []string{"first", "second"} {
		if _, _, err := m.CreateRun(ctx, "prj_1", "key_1", unit.ID, 100, time.Hour, 10, testGovernanceIntent(intent)); err != nil {
			t.Fatal(err)
		}
	}
	q := ledger.RunListQuery{ProjectID: "prj_1", WorkUnitID: unit.ID, At: m.now(), GovernanceListOptions: ledger.GovernanceListOptions{Limits: ledger.ProjectionPageLimits{Rows: 2, Bytes: ledger.GovernanceListMaxBytes}}}
	native, err := state.ReadRunPage(ctx, q)
	if err != nil || len(native.Items) != 2 {
		t.Fatal(native, err)
	}
	admission, unlock := m.lockProject("prj_1")
	admission.runPending = map[string]int64{native.Items[0].ID: 7}
	unlock()
	page, err := m.ReadRunPage(ctx, q)
	if err != nil || len(page.Items) != 2 || page.Items[0].ReservedMicrosUSD != 0 || page.Items[0].RemainingMicrosUSD != 93 || page.Items[0].BudgetState != domain.RunBudgetAvailable {
		t.Fatal(page, err)
	}
	firstJSON, _ := json.Marshal(page.Items[0])
	// Native rows fit together, but annotation makes the returned pair larger.
	q.Limits.Bytes = native.Bytes
	if q.Limits.Bytes < len(firstJSON) {
		t.Fatal("fixture does not fit one annotated row")
	}
	page, err = m.ReadRunPage(ctx, q)
	if err != nil || len(page.Items) != 1 || page.NextID != native.Items[0].ID || page.Bytes != len(firstJSON) {
		t.Fatal("annotated byte cap lost continuation", page, err)
	}
	q.After = page.NextID
	page, err = m.ReadRunPage(ctx, q)
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != native.Items[1].ID || page.NextID != "" {
		t.Fatal(page, err)
	}
	q.After, q.Limits.Bytes = "", len(firstJSON)-1
	page, err = m.ReadRunPage(ctx, q)
	if !errors.Is(err, ledger.ErrProjectionPageLimit) || len(page.Items) != 0 || page.NextID != "" {
		t.Fatal(page, err)
	}
	q.Limits.Bytes = ledger.GovernanceListMaxBytes
	before := state.Watermark()
	m.reads = failingListReader{state}
	units, err := m.ReadWorkUnitPage(ctx, ledger.WorkUnitListQuery{GovernanceListOptions: q.GovernanceListOptions})
	if !errors.Is(err, admissionReadFailure) || len(units.Items) != 0 || units.NextID != "" {
		t.Fatal("work unit page survived read failure", units, err)
	}
	for _, reader := range []ledger.AccountingAdmissionReader{
		failingListReader{state},
		&failingAdmissionReader{AccountingAdmissionReader: state, point: "run", nth: 3},
		&failingAdmissionReader{AccountingAdmissionReader: state, point: "run", nth: 4},
	} {
		m.reads = reader
		page, err = m.ReadRunPage(ctx, q)
		if !errors.Is(err, admissionReadFailure) || len(page.Items) != 0 || page.NextID != "" || page.Bytes != 0 {
			t.Fatal("partial financial page survived error", page, err)
		}
	}
	m.reads = &failingAdmissionReader{AccountingAdmissionReader: state, point: "run_capacity"}
	if count, err := m.ReadActiveRunCount(ctx, "prj_1", m.now()); count != 0 || !errors.Is(err, admissionReadFailure) {
		t.Fatal(count, err)
	}
	if state.Watermark() != before {
		t.Fatal("read-only pages wrote facts")
	}
	m.reads = state
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	page, err = m.ReadRunPage(canceled, q)
	if !errors.Is(err, context.Canceled) || len(page.Items) != 0 || page.NextID != "" {
		t.Fatal(page, err)
	}
}
