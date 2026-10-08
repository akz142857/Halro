package budget

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
)

var admissionReadFailure = errors.New("controlled accounting read failure")

// Returning both an otherwise useful value and an error catches callers that
// accidentally interpret the value/found flag before checking the error.
type failingAdmissionReader struct {
	ledger.AccountingAdmissionReader
	point string
	nth   int
	calls int
	cause error
}

func (r *failingAdmissionReader) failure(point string) error {
	if r.point != point {
		return nil
	}
	r.calls++
	if r.nth == 0 || r.calls == r.nth {
		if r.cause != nil {
			return r.cause
		}
		return admissionReadFailure
	}
	return nil
}

// The Manager must propagate the projection's typed corruption error before
// interpreting either zero balance or a useful idempotency replay result.
func TestProjectedAdmissionCorruptionDoesNotWriteOrCreditReplay(t *testing.T) {
	for _, point := range []string{"balance", "idempotency"} {
		t.Run(point, func(t *testing.T) {
			m, projection, _ := newProjectedManagerFixture(t, ledger.Options{})
			ctx := context.Background()
			intent := testGovernanceIntent("corrupt-replay")
			unit, _, err := m.CreateWorkUnit(ctx, "prj_1", "key_1", 10, intent)
			if err != nil {
				t.Fatal(err)
			}
			request, err := m.BeginRequest(ctx, "prj_1", "req_corrupt_admission")
			if err != nil {
				t.Fatal(err)
			}
			before := m.log.Stats().Records
			anchor, err := projection.ReadReplayAnchor(ctx)
			if err != nil {
				t.Fatal(err)
			}
			// This injects the already-tested Ledger error at the owner interface;
			// the Ledger tests mutate actual ordinary/authenticated disk rows.
			m.reads = &failingAdmissionReader{AccountingAdmissionReader: m.reads, point: point, cause: ledger.ErrProjectionCorrupt}
			if point == "balance" {
				_, err = m.ReserveAttemptDetailed(ctx, request, 1000, 10, AttemptMetadata{})
			} else {
				var got domain.WorkUnit
				var replay bool
				got, replay, err = m.CreateWorkUnit(ctx, "prj_1", "key_1", 10, intent)
				if got.ID != "" || replay {
					t.Fatal("failed idempotency read credited a replay", got.ID, replay, unit.ID)
				}
			}
			if !errors.Is(err, ledger.ErrProjectionCorrupt) || errors.Is(err, ErrExceeded) || errors.Is(err, ErrIdempotencyConflict) || errors.Is(err, ErrWorkUnitNotFound) {
				t.Fatal("corruption became a business result", err)
			}
			if m.log.Stats().Records != before {
				t.Fatal("failed read appended a native fact")
			}
			if after, e := projection.ReadReplayAnchor(ctx); e != nil || after != anchor {
				t.Fatal("failed read advanced projection", after, e)
			}
			admission, unlock := m.lockProject("prj_1")
			defer unlock()
			if len(admission.pending) != 0 || len(admission.runPending) != 0 || len(admission.runAdmissions) != 0 {
				t.Fatal("failed read retained admitted money")
			}
		})
	}
}

func (r *failingAdmissionReader) ReadBalance(ctx context.Context, key ledger.BalanceKey) (ledger.Balance, error) {
	v, err := r.AccountingAdmissionReader.ReadBalance(ctx, key)
	return v, errors.Join(err, r.failure("balance"))
}
func (r *failingAdmissionReader) ReadRun(ctx context.Context, id string) (domain.Run, bool, error) {
	v, found, err := r.AccountingAdmissionReader.ReadRun(ctx, id)
	return v, found, errors.Join(err, r.failure("run"))
}
func (r *failingAdmissionReader) ReadWorkUnit(ctx context.Context, id string) (domain.WorkUnit, bool, error) {
	v, found, err := r.AccountingAdmissionReader.ReadWorkUnit(ctx, id)
	return v, found, errors.Join(err, r.failure("work_unit"))
}
func (r *failingAdmissionReader) ReadGovernanceIdempotency(ctx context.Context, project, operation, key string) (ledger.GovernanceIdempotency, bool, error) {
	v, found, err := r.AccountingAdmissionReader.ReadGovernanceIdempotency(ctx, project, operation, key)
	return v, found, errors.Join(err, r.failure("idempotency"))
}
func (r *failingAdmissionReader) ReadOpenWorkUnitCount(ctx context.Context, project string) (int64, error) {
	v, err := r.AccountingAdmissionReader.ReadOpenWorkUnitCount(ctx, project)
	return v, errors.Join(err, r.failure("open_count"))
}
func (r *failingAdmissionReader) ReadRunCapacity(ctx context.Context, project, unit string, at time.Time) (ledger.RunCapacity, error) {
	v, err := r.AccountingAdmissionReader.ReadRunCapacity(ctx, project, unit, at)
	return v, errors.Join(err, r.failure("run_capacity"))
}

func TestAccountingAdmissionReadErrorsRefuseBeforeNativeWrite(t *testing.T) {
	for _, test := range []struct {
		operation, point string
		nth              int
	}{
		{"read_unit", "work_unit", 0}, {"read_run", "run", 1}, {"read_run", "run", 2},
		{"active_run", "run", 0}, {"attach", "run", 0},
		{"reserve", "balance", 0}, {"reserve", "run", 0},
		{"create_unit", "idempotency", 0}, {"create_unit", "open_count", 0},
		{"replay_unit", "idempotency", 0}, {"replay_unit", "work_unit", 0},
		{"close_unit", "work_unit", 0}, {"close_unit", "idempotency", 0},
		{"create_run", "work_unit", 0}, {"create_run", "run_capacity", 0},
		{"close_run", "run", 0}, {"close_run", "idempotency", 0},
	} {
		t.Run(test.operation+"/"+test.point+string(rune('0'+test.nth)), func(t *testing.T) {
			manager, state, closeLog := newTestManager(t)
			defer closeLog()
			ctx := context.Background()
			unit, _, err := manager.CreateWorkUnit(ctx, "prj_1", "key_1", 10, testGovernanceIntent("initial-unit"))
			if err != nil {
				t.Fatal(err)
			}
			run, _, err := manager.CreateRun(ctx, "prj_1", "key_1", unit.ID, 100, time.Hour, 10, testGovernanceIntent("initial-run"))
			if err != nil {
				t.Fatal(err)
			}
			request, err := manager.BeginRequestAttributed(ctx, "prj_1", "key_1", "req_1", "model", unit.ID, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			before := manager.log.Stats().Records
			manager.reads = &failingAdmissionReader{AccountingAdmissionReader: state, point: test.point, nth: test.nth}
			switch test.operation {
			case "read_unit":
				_, found, readErr := manager.ReadWorkUnit(ctx, "prj_1", unit.ID)
				if found {
					t.Fatal("error credited a work unit")
				}
				err = readErr
			case "read_run":
				_, found, readErr := manager.ReadRun(ctx, "prj_1", run.ID)
				if found {
					t.Fatal("error credited a run")
				}
				err = readErr
			case "active_run":
				_, err = manager.ActiveRun(ctx, "prj_1", run.ID)
			case "attach":
				_, err = manager.BeginRequestAttributed(ctx, "prj_1", "key_1", "req_refused", "model", unit.ID, run.ID)
			case "reserve":
				_, err = manager.ReserveAttemptDetailed(ctx, request, 1000, 10, AttemptMetadata{})
			case "create_unit":
				_, _, err = manager.CreateWorkUnit(ctx, "prj_1", "key_1", 10, testGovernanceIntent("next-unit"))
			case "replay_unit":
				_, _, err = manager.CreateWorkUnit(ctx, "prj_1", "key_1", 10, testGovernanceIntent("initial-unit"))
			case "close_unit":
				_, _, err = manager.CloseWorkUnit(ctx, "prj_1", "key_1", unit.ID, testGovernanceIntent("close-unit"))
			case "create_run":
				_, _, err = manager.CreateRun(ctx, "prj_1", "key_1", unit.ID, 100, time.Hour, 10, testGovernanceIntent("next-run"))
			case "close_run":
				_, _, err = manager.CloseRun(ctx, "prj_1", "key_1", unit.ID, run.ID, "operator", testGovernanceIntent("close-run"))
			}
			if !errors.Is(err, admissionReadFailure) || errors.Is(err, ErrRunNotFound) || errors.Is(err, ErrWorkUnitNotFound) || errors.Is(err, ErrResourceLimit) {
				t.Fatal("read failure became absence/capacity", err)
			}
			if manager.log.Stats().Records != before || state.Watermark().Sequence != before {
				t.Fatal("failed read wrote an unapproved native fact")
			}
			admission, unlock := manager.lockProject("prj_1")
			defer unlock()
			if len(admission.pending) != 0 || len(admission.runPending) != 0 || len(admission.runAdmissions) != 0 {
				t.Fatal("failed read retained in-flight money/admissions")
			}
		})
	}
}

func TestAccountingReadFailureAfterDurableMutationRetainsIdempotentFact(t *testing.T) {
	for _, operation := range []string{"create_unit", "create_run", "close_unit", "close_run"} {
		t.Run(operation, func(t *testing.T) {
			manager, state, closeLog := newTestManager(t)
			defer closeLog()
			ctx := context.Background()
			unit, _, err := manager.CreateWorkUnit(ctx, "prj_1", "key_1", 10, testGovernanceIntent("initial-unit"))
			if err != nil {
				t.Fatal(err)
			}
			run, _, err := manager.CreateRun(ctx, "prj_1", "key_1", unit.ID, 100, time.Hour, 10, testGovernanceIntent("initial-run"))
			if err != nil {
				t.Fatal(err)
			}
			point, nth := "work_unit", 1
			switch operation {
			case "close_unit":
				nth = 2
			case "create_run":
				point = "run"
			case "close_run":
				point, nth = "run", 3
			}
			manager.reads = &failingAdmissionReader{AccountingAdmissionReader: state, point: point, nth: nth}
			perform := func() (bool, error) {
				var replay bool
				var err error
				switch operation {
				case "create_unit":
					_, replay, err = manager.CreateWorkUnit(ctx, "prj_1", "key_1", 10, testGovernanceIntent("final-operation"))
				case "create_run":
					_, replay, err = manager.CreateRun(ctx, "prj_1", "key_1", unit.ID, 100, time.Hour, 10, testGovernanceIntent("final-operation"))
				case "close_unit":
					_, replay, err = manager.CloseWorkUnit(ctx, "prj_1", "key_1", unit.ID, testGovernanceIntent("final-operation"))
				case "close_run":
					_, replay, err = manager.CloseRun(ctx, "prj_1", "key_1", unit.ID, run.ID, "operator", testGovernanceIntent("final-operation"))
				}
				return replay, err
			}
			before := manager.log.Stats().Records
			if _, err := perform(); !errors.Is(err, admissionReadFailure) {
				t.Fatal(err)
			}
			if manager.log.Stats().Records != before+1 || state.Watermark().Sequence != before+1 {
				t.Fatal("accepted fact was discarded on return-value read failure")
			}
			manager.reads = state
			if replay, err := perform(); err != nil || !replay {
				t.Fatal(replay, err)
			}
			if manager.log.Stats().Records != before+1 {
				t.Fatal("retry duplicated the durable fact")
			}
		})
	}
}
