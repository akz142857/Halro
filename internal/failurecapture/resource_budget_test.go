package failurecapture

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/resourcebudget"
)

func captureBudget(t *testing.T, owners int) *resourcebudget.Manager {
	t.Helper()
	l := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: 1 << 20}, MaxClaims: 8, MaxOwners: owners, MaxPressureAge: time.Minute}
	l.Classes[resourcebudget.Background] = l.Total
	l.Classes[resourcebudget.Control] = resourcebudget.Cost{RAM: 8 * resourcebudget.ClaimRAMBytes}
	m, err := resourcebudget.New(l)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	return m
}
func capturePermit(t *testing.T, m *resourcebudget.Manager) resourcebudget.Permit {
	t.Helper()
	p, err := m.TryAcquire(resourcebudget.Background, resourcebudget.Cost{RAM: 65536})
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func captureZero(t *testing.T, m *resourcebudget.Manager) {
	t.Helper()
	s := m.Snapshot()
	if s.Used != (resourcebudget.Cost{}) || s.NormalClaims != 0 || s.Owners != 0 {
		t.Fatal("capture RAM leaked", s)
	}
}

func TestCaptureBudgetCancelledPutKeepsWriterOwnedAndCloseJoinsBeforeRefund(t *testing.T) {
	store, _ := newStore(t, nil)
	m := captureBudget(t, 4)
	permit := capturePermit(t, m)
	entered, release := make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	defer func() { unblock.Do(func() { close(release) }); _ = store.Close() }()
	store.write = func(ctx context.Context, dir, path string, payload []byte) error {
		close(entered)
		<-release
		// Parent cleanup must not wipe or detach the child writer's ciphertext.
		if len(payload) == 0 || payload[0] == 0 {
			t.Error("cancelled parent cleared live writer bytes")
		}
		return writeSealedCapture(ctx, dir, path, payload)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	putDone := make(chan error, 1)
	go func() { _, err := store.PutContextWithPermit(ctx, record("req_budget"), permit); putDone <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("writer did not start")
	}
	cancel()
	select {
	case err := <-putDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled Put did not return")
	}
	if _, err := permit.Cost(); err == nil {
		t.Fatal("input ownership was not revoked")
	}
	if s := m.Snapshot(); s.Used.RAM != 65536+resourcebudget.ClaimRAMBytes || s.Owners != 1 {
		t.Fatal("returned Put refunded live writer", s)
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- store.Close() }()
	deadline := time.Now().Add(time.Second)
	for {
		store.writeMu.Lock()
		closed := store.writeClosed
		store.writeMu.Unlock()
		if closed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Close did not seal new writes")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-closeDone:
		t.Fatal("Close returned while its writer was blocked")
	default:
	}
	late := capturePermit(t, m)
	if _, err := store.PutContextWithPermit(context.Background(), record("req_late"), late); !errors.Is(err, ErrClosed) {
		t.Fatal("new put after Close", err)
	}
	if s := m.Snapshot(); s.Owners != 1 {
		t.Fatal("closed-store rejection leaked", s)
	}
	unblock.Do(func() { close(release) })
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not join completed writer")
	}
	captureZero(t, m)
	if s := store.Saturation(); s.Captured != 0 {
		t.Fatal("cancelled writer spent daily quota", s)
	}
}

func TestCaptureBudgetEveryFailureExitReturnsRAMAndResolvesDailyReservation(t *testing.T) {
	for _, mode := range []string{"validation", "cancelled", "nil-context", "quota", "encoding", "sealing", "write", "published-sync-error", "closed", "writer-owners", "success"} {
		t.Run(mode, func(t *testing.T) {
			store, _ := newStore(t, func(o *Options) { o.MaxRecordsPerDay = 1 })
			defer store.Close()
			owners := 4
			if mode == "writer-owners" {
				owners = 1
			}
			m := captureBudget(t, owners)
			p := capturePermit(t, m)
			r := record("req_exit")
			ctx := context.Background()
			wantCaptured := 0
			wantWritten, wantError := false, true
			switch mode {
			case "validation":
				r.RequestID = "../escape"
			case "cancelled":
				c, cancel := context.WithCancel(ctx)
				cancel()
				ctx = c
			case "nil-context":
				ctx = nil
			case "quota":
				if _, err := store.Put(record("req_seed")); err != nil {
					t.Fatal(err)
				}
				wantCaptured, wantError = 1, false
			case "encoding":
				r.Request = []byte(`{broken`)
			case "sealing":
				store.sealer = fakeSealer{failEncrypt: true}
			case "write":
				store.write = func(context.Context, string, string, []byte) error { return os.ErrPermission }
			case "published-sync-error":
				store.write = func(_ context.Context, _ string, path string, payload []byte) error {
					if err := os.WriteFile(path, payload, FilePerm); err != nil {
						return err
					}
					return os.ErrPermission
				}
				wantCaptured = 1
			case "closed":
				_ = store.Close()
			case "writer-owners":
				c, cancel := context.WithCancel(ctx)
				defer cancel()
				ctx = c
			case "success":
				wantWritten, wantError, wantCaptured = true, false, 1
			}
			written, err := store.PutContextWithPermit(ctx, r, p)
			if written != wantWritten || (err != nil) != wantError {
				t.Fatal(mode, written, err)
			}
			if _, err := p.Cost(); err == nil {
				t.Fatal("old handle stayed live")
			}
			_ = store.Close()
			captureZero(t, m)
			if s := store.Saturation(); s.Captured != wantCaptured {
				t.Fatal("incorrect commit-point quota", mode, s)
			}
			store.mu.Lock()
			pending := store.dayPending
			store.mu.Unlock()
			if pending != 0 {
				t.Fatal("daily reservation leaked", pending)
			}
		})
	}
}

func TestCaptureCloseWaitsForAcceptedSealingBeforeChildRegistration(t *testing.T) {
	store, _ := newStore(t, nil)
	entered, release := make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	defer func() { unblock.Do(func() { close(release) }); _ = store.Close() }()
	store.sealer = captureBlockingSealer{entered: entered, release: release}
	m := captureBudget(t, 4)
	p := capturePermit(t, m)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := store.PutContextWithPermit(ctx, record("req_sealing"), p); done <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("sealer did not enter")
	}
	closed := make(chan error, 1)
	go func() { closed <- store.Close() }()
	select {
	case <-closed:
		t.Fatal("Close ignored accepted pre-writer owner")
	case <-time.After(10 * time.Millisecond):
	}
	unblock.Do(func() { close(release) })
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("accepted put did not complete")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close missed child writer registration")
	}
	captureZero(t, m)
}

type captureBlockingSealer struct {
	entered chan<- struct{}
	release <-chan struct{}
}

func (s captureBlockingSealer) EncryptFailurePayload(request, project string, plaintext []byte) ([]byte, error) {
	close(s.entered)
	<-s.release
	return fakeSealer{}.EncryptFailurePayload(request, project, plaintext)
}
func (s captureBlockingSealer) DecryptFailurePayload(request, project string, envelope []byte) ([]byte, error) {
	return fakeSealer{}.DecryptFailurePayload(request, project, envelope)
}
