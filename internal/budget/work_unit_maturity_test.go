package budget

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/akz142857/Halro/internal/ledger"
)

type blockedMaturityReader struct {
	ledger.AccountingAdmissionReader
	entered, release chan struct{}
	err              error
}

func (r *blockedMaturityReader) ReadWorkUnitMaturityPage(ctx context.Context, refs []ledger.WorkUnitReference) (ledger.WorkUnitMaturityPage, error) {
	if r.entered != nil {
		close(r.entered)
		<-r.release
	}
	page, err := r.AccountingAdmissionReader.ReadWorkUnitMaturityPage(ctx, refs)
	return page, errors.Join(err, r.err)
}

func TestWorkUnitMaturityOwnerJoinsActualPageAndClearsErrorValue(t *testing.T) {
	m, state, closeLog := newTestManager(t)
	defer closeLog()
	ctx := context.Background()
	w, _, err := m.CreateWorkUnit(ctx, "prj_1", "key_1", 10, testGovernanceIntent("maturity"))
	if err != nil {
		t.Fatal(err)
	}
	refs := []ledger.WorkUnitReference{{ProjectID: "prj_1", WorkUnitID: w.ID}}
	r := &blockedMaturityReader{AccountingAdmissionReader: state, entered: make(chan struct{}), release: make(chan struct{})}
	m.reads = r
	var unblock sync.Once
	defer unblock.Do(func() { close(r.release) })
	done := make(chan error, 1)
	go func() {
		page, err := m.ReadWorkUnitMaturityPage(ctx, refs)
		if err == nil && (len(page.Items) != 1 || !page.Items[0].Found) {
			err = errors.New("live maturity page lost")
		}
		done <- err
	}()
	<-r.entered
	retireWithDeadline(t, m)
	m.operationMu.Lock()
	if m.owner == nil || m.activeOperations != 1 {
		t.Error("live page lost owner")
	}
	m.operationMu.Unlock()
	if page, err := m.ReadWorkUnitMaturityPage(ctx, refs); !errors.Is(err, ErrAccountingRetired) || page.Watermark != (ledger.Watermark{}) || len(page.Items) != 0 {
		t.Fatal(page, err)
	}
	unblock.Do(func() { close(r.release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	assertRetiredOwnerReleased(t, m)
	fresh, freshState, closeFresh := newTestManager(t)
	defer closeFresh()
	injected := errors.New("maturity read failure")
	fresh.reads = &blockedMaturityReader{AccountingAdmissionReader: freshState, err: injected}
	if page, err := fresh.ReadWorkUnitMaturityPage(ctx, refs); !errors.Is(err, injected) || len(page.Items) != 0 || page.Watermark != (ledger.Watermark{}) {
		t.Fatal(page, err)
	}
}
