package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
)

// These fixtures contain no HTTP/accounting workers. They establish a fresh
// authenticated native cut, but do not supply the production D-03 protocol.
func projectedWriterCandidate(t *testing.T, l *ledger.Log, path string, key []byte) (*ledger.MaintenanceLease, *ledger.LedgerProjection) {
	t.Helper()
	lease, err := l.BeginMaintenance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var p *ledger.LedgerProjection
	if err := lease.WithHead(context.Background(), func(ctx context.Context, head ledger.Watermark, hash [32]byte) error {
		report, partial, err := ledger.InspectReplayAuthenticated(path, key, nil)
		if err != nil || partial || report.Head != head || report.ChainHash != hash {
			return errors.Join(err, errors.New("fixture native authentication mismatch"))
		}
		p, err = ledger.BuildLedgerProjection(ctx, path, key, filepath.Join(t.TempDir(), "candidate.db"), report)
		return err
	}); err != nil {
		lease.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { lease.Close(); p.Close() })
	return lease, p
}

func projectedWriterLog(t *testing.T, events []ledger.Event, options ledger.Options) (*ledger.Log, string, []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ledger.wal")
	key := bytes.Repeat([]byte{0x61}, 32)
	options.ChainKey = key
	l, err := ledger.OpenWithOptions(path, ledger.NewStatus(), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	for _, event := range events {
		if _, err := l.Append(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
	return l, path, key
}

func projectedWriterAcceptance(id string) ledger.Event {
	return ledger.Event{EventID: "accepted_" + id, Kind: ledger.EventRequestAccepted, RequestID: id,
		ProjectID: "prj_recovery", KeyID: "key", RequestedModel: "model", PeriodID: "2026-10-03",
		PeriodTimezone: "UTC", PeriodTimezoneVersion: 1, OccurredAt: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
}

func TestProjectedRecoveryWriterStandaloneInitialEmptyLedger(t *testing.T) {
	l, path, key := projectedWriterLog(t, nil, ledger.Options{})
	lease, p := projectedWriterCandidate(t, l, path, key)
	r := &Runtime{ledger: l}
	r.draining.Store(true)
	w, err := r.newProjectedRecoveryWriter(context.Background(), lease, p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	before, hash, _ := l.ChainHead()
	result, err := budget.RecoverProjectedRequests(context.Background(), p, w, w.role, ledger.ProjectionPageLimits{Rows: 8, Bytes: 8 << 20})
	if err != nil || result.AttemptsSettled != 0 || result.RequestsFinalized != 0 || result.ConfirmedHead != before {
		t.Fatal(result, err)
	}
	after, afterHash, _ := l.ChainHead()
	if after != before || afterHash != hash || before.Generation != 1 || before.Sequence != 0 {
		t.Fatal("empty recovery invented native facts", after, before)
	}
}

func TestProjectedRecoveryWriterStandaloneTwoLevelRecovery(t *testing.T) {
	var events []ledger.Event
	// Include multiple pages, accepted-before-lease crashes, an unstarted
	// reservation and a finalized request with a late started settlement.
	for i := 0; i < 66; i++ {
		events = append(events, projectedWriterAcceptance(fmt.Sprint("request_", i)))
	}
	price, err := domain.NewVersionedPriceSnapshot(domain.DeploymentPriceVersion{
		ID: "price", DeploymentID: "dep", Version: 1, Revision: 1,
		BillingMode: domain.BillingModeMetered, Currency: "USD", FormulaVersion: domain.PriceFormulaUSDTokensV1,
		InputMicrosPerMillion: 1_000_000, OutputMicrosPerMillion: 2_000_000,
		EffectiveFrom: events[0].OccurredAt.Add(-time.Hour), CreatedAt: events[0].OccurredAt.Add(-time.Hour), CreatedBy: "test",
		Source: domain.PriceSource{Type: domain.PriceSourceManual, Assurance: domain.PriceAssuranceAsserted,
			ReceivedAt: events[0].OccurredAt.Add(-time.Hour), ContentSHA256: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Reference: "fixture", AssertedWithoutArchive: true},
	}, events[0].OccurredAt)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		r := events[i]
		r.Kind, r.EventID, r.AttemptID = ledger.EventReservationCreated, fmt.Sprint("lease_", i), fmt.Sprint("attempt_", i)
		r.ReservationMicrosUSD, r.LeaseMode, r.PriceSnapshot = ledger.MicrosUSD(50), ledger.LeaseModeMetered, &price
		r.PreparedInputTokens, r.PreparedOutputTokens, r.DeploymentID = 10, 20, "dep"
		r.TokenGuardPricingViewDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		events = append(events, r)
		if i > 0 {
			r.Kind, r.EventID = ledger.EventAttemptStarted, fmt.Sprint("started_", i)
			events = append(events, r)
		}
	}
	original := events[2]
	original.Kind, original.EventID, original.Outcome = ledger.EventRequestFinalized, "original_final", "interrupted"
	events = append(events, original)
	l, path, key := projectedWriterLog(t, events, ledger.Options{})
	lease, p := projectedWriterCandidate(t, l, path, key)
	r := &Runtime{ledger: l}
	r.draining.Store(true)
	w, err := r.newProjectedRecoveryWriter(context.Background(), lease, p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	result, err := budget.RecoverProjectedRequests(context.Background(), p, w, w.role, ledger.ProjectionPageLimits{Rows: 8, Bytes: 8 << 20})
	if err != nil || result.AttemptsSettled != 3 || result.RequestsFinalized != 65 || result.ConfirmedHead.Sequence != uint64(len(events))+68 {
		t.Fatal(result, err)
	}
	for i := 0; i < 3; i++ {
		s, found, err := p.ReadSettlement(context.Background(), fmt.Sprint("attempt_", i))
		if err != nil || !found {
			t.Fatal(s, found, err)
		}
		want := int64(50)
		if i == 0 {
			want = 0
		}
		if s.Settlement.CommittedMicrosUSD == nil || *s.Settlement.CommittedMicrosUSD != want {
			t.Fatal("lost conservative cost", s)
		}
	}
	final, found, err := p.ReadFinalization(context.Background(), "request_2")
	if err != nil || !found || !reflect.DeepEqual(final.Event, original) {
		t.Fatal("late settlement replaced finalization", final, err)
	}
	head, hash, _ := l.ChainHead()
	seen := 0
	report, partial, err := ledger.InspectReplayAuthenticated(path, key, func(record ledger.Record) error {
		position, found, err := p.ReadEventPosition(context.Background(), record.Event.EventID)
		if err != nil || !found || position != (ledger.Watermark{Generation: record.Generation, Sequence: record.Sequence, Offset: record.Offset}) {
			return errors.New("projection lost physical native evidence")
		}
		seen++
		return nil
	})
	if err != nil || partial || report.Head != head || report.ChainHash != hash || seen != int(head.Sequence) {
		t.Fatal(report, partial, seen, err)
	}
	again, err := budget.RecoverProjectedRequests(context.Background(), p, w, w.role, ledger.ProjectionPageLimits{Rows: 8, Bytes: 8 << 20})
	if err != nil || again.AttemptsSettled != 0 || again.RequestsFinalized != 0 || again.ConfirmedHead != head {
		t.Fatal("retry appended duplicate facts", again, err)
	}
}

func TestProjectedRecoveryWriterRequiresOwnerAndMatchingNativeMode(t *testing.T) {
	for _, mode := range []string{"not_withdrawn", "native_ha_without_bridge", "unavailable_ha_bridge", "config_ha_on_standalone", "runtime_ha_on_standalone", "wrong_owner"} {
		t.Run(mode, func(t *testing.T) {
			options := ledger.Options{}
			if mode == "native_ha_without_bridge" || mode == "unavailable_ha_bridge" {
				options.AfterDurable = func(ledger.DurableBatch) (uint64, error) { return 1, nil }
				options.WaitConfirmed = func(context.Context, uint64) error { return nil }
			}
			l, path, key := projectedWriterLog(t, []ledger.Event{projectedWriterAcceptance("request")}, options)
			lease, p := projectedWriterCandidate(t, l, path, key)
			r := &Runtime{ledger: l}
			r.draining.Store(mode != "not_withdrawn")
			switch mode {
			case "unavailable_ha_bridge":
				r.config.Replication = &config.Replication{}
				r.replication = &replicationRuntime{}
			case "config_ha_on_standalone":
				r.config.Replication = &config.Replication{}
			case "runtime_ha_on_standalone":
				r.replication = &replicationRuntime{}
			case "wrong_owner":
				other, _, _ := projectedWriterLog(t, nil, ledger.Options{})
				r.ledger = other
			}
			if w, err := r.newProjectedRecoveryWriter(context.Background(), lease, p); err == nil || w != nil {
				t.Fatal("accepted missing withdrawal/owner/replication authority", w, err)
			}
			if l.Status().Load() != ledger.AccountingHealthy {
				t.Fatal("pre-admission rejection poisoned native history")
			}
		})
	}
}

func TestProjectedRecoveryWriterRejectsUnappliedOrMismatchedNativeCut(t *testing.T) {
	for _, fault := range []string{"unapplied", "wrong_hash"} {
		t.Run(fault, func(t *testing.T) {
			l, path, key := projectedWriterLog(t, []ledger.Event{projectedWriterAcceptance("request")}, ledger.Options{})
			lease, p := projectedWriterCandidate(t, l, path, key)
			_, _, err := lease.AppendApply(context.Background(), projectedWriterAcceptance("extra"), func(ctx context.Context, record ledger.Record) error {
				if fault == "unapplied" {
					return nil
				}
				// Deliberately feed false hash evidence to a detached candidate.
				// This is not a supported production trust/publication path.
				record.Hash[0] ^= 0xff
				return p.Apply(ctx, record)
			})
			if err != nil {
				t.Fatal(err)
			}
			r := &Runtime{ledger: l}
			r.draining.Store(true)
			if w, err := r.newProjectedRecoveryWriter(context.Background(), lease, p); err == nil || w != nil {
				t.Fatal("accepted mismatched native/applied cut", w, err)
			}
			if l.Status().Load() != ledger.AccountingRecoveryRequired {
				t.Fatal("native/applied mismatch not fenced")
			}
		})
	}
}

func TestProjectedRecoveryWriterFailsClosedOnCutAndOwnerRevocation(t *testing.T) {
	for _, fault := range []string{"wrong_cut", "closed_projection", "admission_reopened", "lease_released", "closed_writer", "config_changed"} {
		t.Run(fault, func(t *testing.T) {
			l, path, key := projectedWriterLog(t, []ledger.Event{projectedWriterAcceptance("request")}, ledger.Options{})
			lease, p := projectedWriterCandidate(t, l, path, key)
			r := &Runtime{ledger: l}
			r.draining.Store(true)
			w, err := r.newProjectedRecoveryWriter(context.Background(), lease, p)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { w.Close() })
			head, _, _ := l.ChainHead()
			target := head
			switch fault {
			case "wrong_cut":
				target.Sequence++
			case "closed_projection":
				p.Close()
			case "admission_reopened":
				r.draining.Store(false)
			case "lease_released":
				lease.Close()
			case "closed_writer":
				w.Close()
			case "config_changed":
				r.config.Replication = &config.Replication{}
			}
			if err := w.ConfirmRecoveryPrefix(context.Background(), target); err == nil {
				t.Fatal("revoked writer confirmed")
			}
			if record, err := w.AppendApplyRecovery(context.Background(), projectedWriterAcceptance("extra")); err == nil || !reflect.DeepEqual(record, ledger.Record{}) {
				t.Fatal("revoked writer appended", record, err)
			}
			actual, _, _ := l.ChainHead()
			if actual != head {
				t.Fatal("revoked writer changed native history")
			}
			if fault != "closed_writer" && l.Status().Load() != ledger.AccountingRecoveryRequired {
				t.Fatal("failed operation did not require fresh recovery")
			}
		})
	}
}

type projectedWriterBlockedDurability struct {
	file    *os.File
	block   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (d *projectedWriterBlockedDurability) Write(p []byte) (int, error) { return d.file.Write(p) }
func (d *projectedWriterBlockedDurability) Sync() error {
	if d.block.CompareAndSwap(true, false) {
		close(d.entered)
		<-d.release
	}
	return d.file.Sync()
}

func TestProjectedRecoveryWriterDurableBeforeApplyRequiresAuthenticatedRetry(t *testing.T) {
	var io *projectedWriterBlockedDurability
	l, path, key := projectedWriterLog(t, []ledger.Event{projectedWriterAcceptance("request")}, ledger.Options{WrapDurability: func(f *os.File) ledger.DurabilityWriter {
		io = &projectedWriterBlockedDurability{file: f, entered: make(chan struct{}), release: make(chan struct{})}
		return io
	}})
	var unblock sync.Once
	release := func() { unblock.Do(func() { close(io.release) }) }
	defer release()
	lease, p := projectedWriterCandidate(t, l, path, key)
	r := &Runtime{ledger: l}
	r.draining.Store(true)
	w, err := r.newProjectedRecoveryWriter(context.Background(), lease, p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	io.block.Store(true)
	type outcome struct {
		record ledger.Record
		err    error
	}
	done := make(chan outcome, 1)
	event := projectedWriterAcceptance("request")
	event.Kind, event.EventID, event.Outcome = ledger.EventRequestFinalized, "fixture_final", "interrupted"
	go func() { rec, err := w.AppendApplyRecovery(context.Background(), event); done <- outcome{rec, err} }()
	select {
	case <-io.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("native fsync not reached")
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	release()
	got := <-done
	if got.err == nil || !reflect.DeepEqual(got.record, ledger.Record{}) || l.Status().Load() != ledger.AccountingRecoveryRequired {
		t.Fatal("durable-before-apply returned success", got, l.Status().Load())
	}
	if _, err := w.AppendApplyRecovery(context.Background(), projectedWriterAcceptance("extra")); err == nil {
		t.Fatal("failed writer reused")
	}
	w.Close()
	l.Close()
	reopened, err := ledger.OpenWithOptions(path, ledger.NewStatus(), ledger.Options{ChainKey: key})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	newLease, fresh := projectedWriterCandidate(t, reopened, path, key)
	r2 := &Runtime{ledger: reopened}
	r2.draining.Store(true)
	w2, err := r2.newProjectedRecoveryWriter(context.Background(), newLease, fresh)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w2.Close() })
	result, err := budget.RecoverProjectedRequests(context.Background(), fresh, w2, w2.role, ledger.ProjectionPageLimits{Rows: 8, Bytes: 8 << 20})
	if err != nil || result.RequestsFinalized != 0 || result.ConfirmedHead.Sequence != 2 {
		t.Fatal("retry lost durable finalization or duplicated it", result, err)
	}
	final, found, err := fresh.ReadFinalization(context.Background(), "request")
	if err != nil || !found || final.Event.EventID != event.EventID {
		t.Fatal(final, found, err)
	}
}

func TestProjectedRecoveryWriterCloseJoinsNativeIOAndKeepsExclusivity(t *testing.T) {
	var io *projectedWriterBlockedDurability
	l, path, key := projectedWriterLog(t, []ledger.Event{projectedWriterAcceptance("request")}, ledger.Options{WrapDurability: func(f *os.File) ledger.DurabilityWriter {
		io = &projectedWriterBlockedDurability{file: f, entered: make(chan struct{}), release: make(chan struct{})}
		return io
	}})
	var unblock sync.Once
	release := func() { unblock.Do(func() { close(io.release) }) }
	defer release()
	lease, p := projectedWriterCandidate(t, l, path, key)
	r := &Runtime{ledger: l}
	r.draining.Store(true)
	w, err := r.newProjectedRecoveryWriter(context.Background(), lease, p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	io.block.Store(true)
	appendDone := make(chan error, 1)
	go func() {
		_, err := w.AppendApplyRecovery(context.Background(), projectedWriterAcceptance("extra"))
		appendDone <- err
	}()
	select {
	case <-io.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("native fsync not reached")
	}
	closed := make(chan error, 1)
	go func() { closed <- w.Close() }()
	select {
	case <-w.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel")
	}
	select {
	case <-closed:
		t.Fatal("Close returned before accepted IO")
	case <-time.After(10 * time.Millisecond):
	}
	if _, err := l.Append(context.Background(), projectedWriterAcceptance("business")); !errors.Is(err, ledger.ErrLedgerMaintenance) {
		t.Fatal("Close released native exclusivity early", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := w.AppendApplyRecovery(cancelled, projectedWriterAcceptance("cancelled")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	release()
	if err := <-appendDone; err == nil {
		t.Fatal("cancelled operation succeeded")
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if l.Status().Load() != ledger.AccountingRecoveryRequired {
		t.Fatal("cancelled durable write was not fenced")
	}
	if err := w.Close(); err != nil {
		t.Fatal("repeated Close", err)
	}
}
