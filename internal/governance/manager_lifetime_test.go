package governance

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

type retirementCheckpoint struct {
	CheckpointStore
	entered chan struct{}
	release chan struct{}
}

func (s *retirementCheckpoint) PutGovernanceJournalAnchor(sequence uint64, offset int64, hash, auth [32]byte) error {
	close(s.entered)
	<-s.release
	return s.CheckpointStore.PutGovernanceJournalAnchor(sequence, offset, hash, auth)
}

func retirementTimeout(t *testing.T, m *Manager) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := m.Retire(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("retirement did not join accepted operation", err)
	}
}

func waitRetirementSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("accepted operation did not reach controlled phase")
	}
}

func assertRetiredOwnerCleared(t *testing.T, m *Manager) {
	t.Helper()
	m.operationMu.Lock()
	defer m.operationMu.Unlock()
	if !m.retiring || m.activeOperations != 0 || m.log != nil || m.state != nil || m.projection != nil || m.accounting != nil || m.definitions != nil || m.checkpoint != nil || m.now != nil || m.pageKey != nil || m.unavailable != nil {
		t.Fatal("retired Manager retains an owner")
	}
}

func TestGovernanceRetireJoinsDurableReportCheckpointAndCancellation(t *testing.T) {
	for _, backend := range []string{"state", "disk"} {
		for _, canceled := range []bool{false, true} {
			t.Run(backend+map[bool]string{false: "-live", true: "-canceled"}[canceled], func(t *testing.T) {
				m, log, _, _, checkpoint := testManager(t)
				defer log.Close()
				if backend == "disk" {
					m = projectedTestManager(t, m, buildTestGovernanceProjection(t, log))
				}
				block := &retirementCheckpoint{CheckpointStore: checkpoint, entered: make(chan struct{}), release: make(chan struct{})}
				m.checkpoint = block
				var once sync.Once
				defer once.Do(func() { close(block.release) })
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				result := make(chan error, 1)
				go func() { _, _, err := m.Report(ctx, reportInput("accepted", "", "retire")); result <- err }()
				waitRetirementSignal(t, block.entered)
				if canceled {
					cancel()
				}
				retirementTimeout(t, m)
				if ready, err := m.Ready(); ready || !errors.Is(err, ErrRetired) {
					t.Fatal("retired readiness reopened", ready, err)
				}
				if _, _, err := m.Report(context.Background(), reportInput("accepted", "", "new")); !errors.Is(err, ErrRetired) {
					t.Fatal("retired Report admitted", err)
				}
				m.operationMu.Lock()
				retained := m.activeOperations == 1 && m.log == log && m.accounting != nil && m.checkpoint == block && m.now != nil
				m.operationMu.Unlock()
				if !retained {
					t.Fatal("checkpoint wait released dependencies early")
				}
				once.Do(func() { close(block.release) })
				err := <-result
				if canceled && !errors.Is(err, context.Canceled) || !canceled && err != nil {
					t.Fatal("accepted Report completion", err)
				}
				// References are cleared by the final release, without another Retire.
				assertRetiredOwnerCleared(t, m)
				if err := m.Retire(context.Background()); err != nil {
					t.Fatal(err)
				}
				state := NewState()
				if head, err := log.Replay(state.Apply); err != nil || head.Records != 1 || len(state.Snapshot().Records) != 1 {
					t.Fatal("accepted durable fact lost", head, err)
				}
				checkpoint.mu.Lock()
				anchors := checkpoint.anchors
				checkpoint.mu.Unlock()
				if anchors != 1 {
					t.Fatal("accepted anchor skipped", anchors)
				}
			})
		}
	}
}

func TestGovernanceRetireJoinsCompleteAndActualManifestPublication(t *testing.T) {
	m, log, _, _, _ := testManager(t)
	defer log.Close()
	if _, _, err := m.Report(context.Background(), reportInput("accepted", "", "export")); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "export")
	gate, published := make(chan struct{}), make(chan struct{})
	resumeGate, resumeCallback := make(chan struct{}), make(chan struct{})
	var releaseGate, releaseCallback sync.Once
	defer releaseGate.Do(func() { close(resumeGate) })
	defer releaseCallback.Do(func() { close(resumeCallback) })
	result := make(chan error, 1)
	var captured context.Context
	var retained OutcomeExportView
	go func() {
		result <- m.WithOutcomeExport(context.Background(), func(view OutcomeExportView) error {
			captured, retained = view.(*outcomeExportView).ctx, view
			_, err := WriteExportStream(captured, dir, ExportStreamInput{GovernanceSequence: view.Head().Records, GovernanceOffset: view.Head().Bytes,
				Outcomes: func(ctx context.Context, yield func(domain.Outcome) error) error {
					for {
						page, err := view.ReadPage(ctx, OutcomePageMaxRows, OutcomePageMaxBytes)
						if err != nil {
							return err
						}
						for _, row := range page.Items {
							if err := yield(row); err != nil {
								return err
							}
						}
						if page.Done {
							return nil
						}
					}
				}, BeforePublish: func(ctx context.Context) error {
					if err := view.Complete(ctx); err != nil {
						return err
					}
					close(gate)
					<-resumeGate
					// The accepted parent remains live after Complete/Retire.
					_, err := m.CaptureOutcomeFence(captured)
					return err
				}})
			if err != nil {
				return err
			}
			close(published)
			<-resumeCallback
			return nil
		})
	}()
	waitRetirementSignal(t, gate)
	retirementTimeout(t, m)
	if _, err := m.CaptureOutcomeFence(context.Background()); !errors.Is(err, ErrRetired) {
		t.Fatal("fresh read admitted after withdrawal", err)
	}
	releaseGate.Do(func() { close(resumeGate) })
	waitRetirementSignal(t, published)
	if manifest, err := VerifyExport(dir); err != nil || len(manifest.Files) != 4 || manifest.Files[1].Dataset != "outcomes" || manifest.Files[1].RecordCount != 1 {
		t.Fatal("accepted publication missing", manifest, err)
	}
	retirementTimeout(t, m)
	releaseCallback.Do(func() { close(resumeCallback) })
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	assertRetiredOwnerCleared(t, m)
	if _, err := m.CaptureOutcomeFence(captured); !errors.Is(err, ErrRetired) {
		t.Fatal("finished parent context reopened admission", err)
	}
	if _, err := retained.ReadPage(context.Background(), 1, OutcomePageMaxBytes); !errors.Is(err, ErrOutcomeExportClosed) {
		t.Fatal("retained source read after join", err)
	}
}

type retirementCapturedError struct{ state *State }

func (retirementCapturedError) Error() string { return "captured owner error" }

func TestGovernanceRetireClearsUnavailableErrorsCapturingHistoricalState(t *testing.T) {
	m := NewUnavailable(retirementCapturedError{NewState()})
	if err := m.Retire(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertRetiredOwnerCleared(t, m)
}

func TestGovernanceRetireRefusesEveryPublicOwnerPath(t *testing.T) {
	m, log, _, _, _ := testManager(t)
	defer log.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.Retire(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if ready, err := m.Ready(); !ready || err != nil {
		t.Fatal("pre-canceled Retire withdrew", err)
	}
	if err := m.Retire(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertRetiredOwnerCleared(t, m)
	ctx = context.Background()
	if m.State() != nil {
		t.Fatal("retired State alias exposed")
	}
	read := func(context.Context, func(domain.OutcomeDefinition) error) error {
		t.Fatal("retired producer ran")
		return nil
	}
	checks := []func() error{
		func() error { _, err := m.Ready(); return err },
		func() error { _, _, err := m.Report(ctx, ReportInput{}); return err },
		func() error { _, err := m.OutcomesContext(ctx, "", "", ""); return err },
		func() error { _, err := m.ReadOutcomeSnapshot(ctx, "", "", ""); return err },
		func() error {
			_, err := m.ReadOutcomePage(ctx, OutcomePageQuery{Rows: 1, Bytes: OutcomePageMaxBytes})
			return err
		},
		func() error { _, err := m.CaptureOutcomeFence(ctx); return err },
		func() error { _, err := m.ReadCurrentOutcomeBatch(ctx, OutcomeFence{}, nil); return err },
		func() error { _, err := m.ReadWorkUnitOutcomes(ctx, WorkUnitOutcomeQuery{}); return err },
		func() error {
			return m.WithOutcomeExport(ctx, func(OutcomeExportView) error { t.Fatal("retired view captured"); return nil })
		},
		func() error {
			_, err := m.WriteExportFromOwners(ctx, filepath.Join(t.TempDir(), "export"), ExportDefinitionSource{Read: read, Complete: func(context.Context) error { return nil }})
			return err
		},
	}
	for i, check := range checks {
		if err := check(); !errors.Is(err, ErrRetired) {
			t.Fatal("retired API", i, err)
		}
	}
}

func TestGovernanceRetireJoinsReadAlreadyWaitingForReportMutex(t *testing.T) {
	m, log, _, _, checkpoint := testManager(t)
	defer log.Close()
	block := &retirementCheckpoint{CheckpointStore: checkpoint, entered: make(chan struct{}), release: make(chan struct{})}
	m.checkpoint = block
	var once sync.Once
	defer once.Do(func() { close(block.release) })
	reported, read := make(chan error, 1), make(chan error, 1)
	go func() {
		_, _, err := m.Report(context.Background(), reportInput("accepted", "", "queued-read"))
		reported <- err
	}()
	waitRetirementSignal(t, block.entered)
	go func() {
		page, err := m.ReadOutcomePage(context.Background(), OutcomePageQuery{Rows: 1, Bytes: OutcomePageMaxBytes})
		if err == nil && len(page.Items) != 1 {
			err = errors.New("accepted queued read lost row")
		}
		read <- err
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		m.operationMu.Lock()
		active := m.activeOperations
		m.operationMu.Unlock()
		if active == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("read never acquired ticket before mutex wait")
		}
		time.Sleep(time.Millisecond)
	}
	retirementTimeout(t, m)
	once.Do(func() { close(block.release) })
	if err := <-reported; err != nil {
		t.Fatal(err)
	}
	if err := <-read; err != nil {
		t.Fatal("accepted queued read revoked", err)
	}
	assertRetiredOwnerCleared(t, m)
}
