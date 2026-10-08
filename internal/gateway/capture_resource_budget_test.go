package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/failurecapture"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

const captureTestRAM = 8192

func captureRAMManager(t *testing.T) *resourcebudget.Manager {
	t.Helper()
	l := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: 8 << 20}, MaxClaims: 80, MaxOwners: 4, MaxPressureAge: time.Minute}
	l.Classes[resourcebudget.Background] = resourcebudget.Cost{RAM: 2 << 20}
	l.Classes[resourcebudget.Control] = resourcebudget.Cost{RAM: 80 * resourcebudget.ClaimRAMBytes}
	m, err := resourcebudget.New(l)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

type ownedBlockingCapture struct {
	blockingCapture
	panicPrepare bool
}

func (c *ownedBlockingCapture) PrepareRecord(r failurecapture.Record) failurecapture.Record {
	if c.panicPrepare {
		panic("capture prepare fixture")
	}
	return c.blockingCapture.PrepareRecord(r)
}

func (c *ownedBlockingCapture) PutContextWithPermit(ctx context.Context, r failurecapture.Record, p resourcebudget.Permit) (bool, error) {
	owner, err := p.Transfer()
	if err != nil {
		return false, err
	}
	defer owner.Release()
	return c.PutContext(ctx, r)
}

func budgetedCapture(t *testing.T, f *fixture) (*ownedBlockingCapture, *resourcebudget.Manager) {
	t.Helper()
	c := &ownedBlockingCapture{blockingCapture: blockingCapture{entered: make(chan struct{}), release: make(chan struct{})}}
	m := captureRAMManager(t)
	f.service.failureCapture, f.service.resourceBudget, f.service.captureRAMBytes = c, m, captureTestRAM
	t.Cleanup(func() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_ = f.service.ShutdownFailureCapture(ctx)
	})
	return c, m
}

func captureBudgetZero(t *testing.T, m *resourcebudget.Manager) {
	t.Helper()
	if s := m.Snapshot(); s.Used != (resourcebudget.Cost{}) || s.Owners != 0 || s.NormalClaims != 0 {
		t.Fatal("capture ownership leaked", s)
	}
}

type captureEncodingProbe struct {
	calls       *atomic.Int32
	panicEncode bool
}

func (p captureEncodingProbe) MarshalJSON() ([]byte, error) {
	p.calls.Add(1)
	if p.panicEncode {
		panic("capture encoding fixture")
	}
	return []byte(`{"probe":true}`), nil
}

func TestCaptureRAMRefusalAndClosedQueueDoNotEncodeIndependentCopies(t *testing.T) {
	for _, state := range []string{"unknown", "pressured", "exhausted", "closed"} {
		t.Run(state, func(t *testing.T) {
			f := newFixture(t, 1000000)
			defer f.close()
			_, m := budgetedCapture(t, &f)
			if state != "unknown" {
				if err := m.SetPressure(resourcebudget.Healthy); err != nil {
					t.Fatal(err)
				}
			}
			switch state {
			case "pressured":
				if err := m.SetPressure(resourcebudget.Pressured); err != nil {
					t.Fatal(err)
				}
			case "exhausted":
				p, err := m.TryAcquire(resourcebudget.Background, resourcebudget.Cost{RAM: 2 << 20})
				if err != nil {
					t.Fatal(err)
				}
				defer p.Release()
			case "closed":
				if err := f.service.ShutdownFailureCapture(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			var calls atomic.Int32
			run := &requestRun{service: f.service, capturedGatewayRequest: captureEncodingProbe{calls: &calls}}
			run.writeCapture("provider_error")
			if calls.Load() != 0 || f.service.captureQueue != nil {
				t.Fatal("rejected capture encoded or started worker")
			}
			if state != "exhausted" {
				captureBudgetZero(t, m)
			}
		})
	}
}

func TestCaptureRAMPanicsBeforePublicationRefundOwnership(t *testing.T) {
	for _, stage := range []string{"encoding", "prepare"} {
		t.Run(stage, func(t *testing.T) {
			f := newFixture(t, 1000000)
			defer f.close()
			c, m := budgetedCapture(t, &f)
			if err := m.SetPressure(resourcebudget.Healthy); err != nil {
				t.Fatal(err)
			}
			c.panicPrepare = stage == "prepare"
			var calls atomic.Int32
			run := &requestRun{service: f.service, capturedGatewayRequest: captureEncodingProbe{calls: &calls, panicEncode: stage == "encoding"}}
			func() {
				defer func() {
					if recover() == nil {
						t.Error("fixture panic did not occur")
					}
				}()
				run.writeCapture("provider_error")
			}()
			captureBudgetZero(t, m)
		})
	}
}

func TestCaptureRAMFullQueueAndCancelledTailDropReferencesBeforeRefund(t *testing.T) {
	f := newFixture(t, 1000000)
	defer f.close()
	c, m := budgetedCapture(t, &f)
	if err := m.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	f.service.startFailureCapture()
	f.service.enqueueCapture(failurecapture.Record{RequestID: "active", ProjectID: "project_1"})
	select {
	case <-c.entered:
	case <-time.After(time.Second):
		t.Fatal("capture did not enter store")
	}
	packets := make([]*captureRecord, 0, failureCaptureQueueCapacity+1)
	for i := 0; i < failureCaptureQueueCapacity+1; i++ {
		permit, ok := f.service.acquireCaptureRAM()
		if !ok {
			t.Fatal("fixture acquisition failed")
		}
		packet := &captureRecord{Record: failurecapture.Record{RequestID: "queued", GatewayRequest: json.RawMessage(`{"retained":true}`)}, permit: permit}
		f.service.enqueueCaptureOwned(packet)
		packets = append(packets, packet)
	}
	if s := m.Snapshot(); s.Classes[resourcebudget.Background].RAM != 65*captureTestRAM || s.NormalClaims != 65 || s.Owners != 66 {
		t.Fatal("queue overflow or active store owner mischarged", s)
	}
	if last := packets[len(packets)-1]; last.RequestID != "" || last.GatewayRequest != nil || last.permit != (resourcebudget.Permit{}) {
		t.Fatal("full-queue packet still referenced")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.service.ShutdownFailureCapture(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	for _, packet := range packets {
		if packet.RequestID != "" || packet.GatewayRequest != nil || packet.permit != (resourcebudget.Permit{}) {
			t.Fatal("cancelled tail still owns payload")
		}
	}
	captureBudgetZero(t, m)
	f.service.enqueueCapture(failurecapture.Record{RequestID: "late"})
	captureBudgetZero(t, m)
}

func TestCaptureRAMConcurrentEnqueueAndShutdownLeaveNoOwners(t *testing.T) {
	f := newFixture(t, 1000000)
	defer f.close()
	_, m := budgetedCapture(t, &f)
	if err := m.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	f.service.startFailureCapture()
	start := make(chan struct{})
	var wg sync.WaitGroup
	for producer := 0; producer < 8; producer++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < 100; i++ {
				f.service.enqueueCapture(failurecapture.Record{RequestID: "concurrent", GatewayRequest: json.RawMessage(`{}`)})
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_ = f.service.ShutdownFailureCapture(ctx)
	}()
	close(start)
	wg.Wait()
	captureBudgetZero(t, m)
}

func TestCaptureRAMStalledDiagnosticPreservesCallerFailureAndLedgerCompletion(t *testing.T) {
	f := newFixture(t, 1000000)
	defer f.close()
	c, m := budgetedCapture(t, &f)
	if err := m.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	f.service.startFailureCapture()
	f.adapter.err = &provider.Error{Class: provider.ErrorBadRequest, StatusCode: 400, Message: "refused"}
	if _, err := f.service.Chat(context.Background(), f.plaintext, chatRequest()); err == nil {
		t.Fatal("caller failure disappeared")
	}
	select {
	case <-c.entered:
	case <-time.After(time.Second):
		t.Fatal("capture did not start")
	}
	if activeRequestsAfterReplay(t, f.log) != 0 {
		t.Fatal("diagnostic retained accounting lease")
	}
	if s := m.Snapshot(); s.Classes[resourcebudget.Background].RAM != captureTestRAM || s.Owners != 2 {
		t.Fatal("returned request prematurely refunded diagnostic", s)
	}
	if err := m.SetPressure(resourcebudget.Pressured); err != nil {
		t.Fatal(err)
	}
	close(c.release)
	if err := f.service.ShutdownFailureCapture(context.Background()); err != nil {
		t.Fatal(err)
	}
	captureBudgetZero(t, m)
}

func TestCaptureRAMServiceOptionsRejectLegacyStoreAndMissingEnvelope(t *testing.T) {
	f := newFixture(t, 1000000)
	defer f.close()
	c := &ownedBlockingCapture{}
	for _, options := range []ServiceOptions{
		{ResourceBudget: captureRAMManager(t), DeferredRAMBytes: 100, FailureCapture: &recordingCapture{}, CaptureRAMBytes: captureTestRAM},
		{ResourceBudget: captureRAMManager(t), DeferredRAMBytes: 100, FailureCapture: c},
		{CaptureRAMBytes: captureTestRAM},
	} {
		if _, err := NewServiceWithOptions(f.service.auth, f.registry, f.accounting, options); err == nil {
			t.Fatal("invalid capture ownership contract accepted")
		}
	}
}
