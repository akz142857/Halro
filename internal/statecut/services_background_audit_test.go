package statecut

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/startupaudit"
)

func TestCommonSelectedServicesUseActualFiniteAuditGrantBeforeHistoryCompletes(t *testing.T) {
	f := newCommonFixture(t)
	o := prepareCommonSelectedReopen(t, f)
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	o.AuditOptions = startupaudit.Options{Deadline: time.Minute, ProgressTimeout: 30 * time.Second,
		ObserveProgress: func(ctx context.Context, p startupaudit.Progress) error {
			if p.Records != 0 && p.Bytes > 0 {
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		},
	}
	ctx := context.Background()
	r, err := ReopenSelectedStandalone(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { retireSelectedFixture(t, f, r) })
	if p := r.startupAudits[0].Snapshot(); !p.Ready || p.Complete || p.Records != 1 {
		t.Fatal("actual Ledger reader was not paused after authenticating one frame", p)
	}
	type serviceResult struct {
		services *SelectedServices
		err      error
	}
	done := make(chan serviceResult, 1)
	go func() {
		s, err := r.StartSelectedServices(ctx, SelectedServicesOptions{Authority: o.Authority, Timezone: "UTC", QueueCapacity: 4})
		done <- serviceResult{s, err}
	}()
	var s *SelectedServices
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatal("actual service handoff required whole history", result.err)
		}
		s = result.services
	case <-time.After(10 * time.Second):
		once.Do(func() { close(release) })
		<-done
		t.Fatal("service handoff waited for paused historical reader")
	}
	if _, err := r.Owners.Ledger.SealReference(ctx); err == nil {
		t.Fatal("finite progress granted structural publication")
	}
	project := domain.Project{ID: "prj_audit_grant", Name: "Finite actual grant", Enabled: true}
	if _, err := s.Metadata.PutProject(ctx, project, 0, nil); err != nil {
		t.Fatal("actual Metadata DAO", err)
	}
	request, err := s.Accounting.BeginRequestDetailed(ctx, project.ID, "key_audit_grant", "req_audit_grant", "chat")
	if err != nil {
		t.Fatal("actual Ledger append under finite grant", err)
	}
	if err := s.Accounting.Finalize(ctx, request, "success"); err != nil {
		t.Fatal(err)
	}
	if err := s.Collector.CatchUp(ctx); err != nil {
		t.Fatal("live Usage source under finite grant", err)
	}
	if p := r.startupAudits[0].Snapshot(); p.Complete {
		t.Fatal("business path completed the paused history itself", p)
	}
	// Cancel this exact historical owner, rather than replacing its authority
	// with a test object. Its withdrawal must close the actual native gate.
	r.startupAudits[0].Cancel()
	if err := r.startupAudits[0].Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if r.Owners.NativeLedger.Status().Load() != ledger.AccountingRecoveryRequired {
		t.Fatal("native readiness survived audit withdrawal")
	}
	before, _, _ := r.Owners.NativeLedger.ChainHead()
	if _, err := s.Accounting.BeginRequest(ctx, project.ID, "req_after_withdraw"); err == nil {
		t.Fatal("withdrawn actual grant admitted business", err)
	}
	after, _, _ := r.Owners.NativeLedger.ChainHead()
	if after != before {
		t.Fatal("withdrawn audit appended an unauthorized fact")
	}
	once.Do(func() { close(release) })
	for _, audit := range r.startupAudits[1:] {
		if err := audit.Wait(ctx); err != nil {
			t.Fatal("other actual historical readers did not retire", err)
		}
	}
}
