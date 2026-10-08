package app

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/usage"
)

// ReadArchivePosition checks admission twice, then walks an actually opened
// manifest. Block that third Err check to model non-interruptible read work.
type archiveOpenedContext struct {
	context.Context
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (c *archiveOpenedContext) Err() error {
	if c.calls.Add(1) == 3 {
		close(c.entered)
		<-c.release
	}
	return c.Context.Err()
}

func awaitArchiveRetired(t *testing.T, e *usage.Exporter) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := e.ReadArchivePosition(context.Background())
		if errors.Is(err, usage.ErrDiskUsageClosed) {
			return
		}
		if err != nil && !errors.Is(err, usage.ErrDiskUsageBusy) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("exporter not retired")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRuntimeCloseJoinsArchiveReaderBeforeMetadataAndDirectoryLock(t *testing.T) {
	r, _ := summaryRuntime(t, 1)
	r.stopBackgroundWork()
	r.exportUsageParquet()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &archiveOpenedContext{Context: ctx, entered: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	unblock := func() { once.Do(func() { close(c.release) }) }
	t.Cleanup(unblock)
	read := make(chan error, 1)
	go func() {
		out, err := r.usageExporter.ReadArchivePosition(c)
		if out != (usage.ArchivePosition{}) {
			err = errors.Join(err, errors.New("retired read returned position"))
		}
		read <- err
	}()
	awaitDrainSignal(t, c.entered)
	closed := make(chan error, 1)
	go func() { closed <- r.Close() }()
	awaitArchiveRetired(t, r.usageExporter)
	cancel()
	select {
	case err := <-closed:
		t.Fatal("Close preceded actual manifest reader", err)
	default:
	}
	if _, err := r.store.ReadMetadataAppliedPosition(context.Background()); err != nil {
		t.Fatal("Metadata closed before archive reader", err)
	}
	if next, err := lock.AcquireExistingReadOnly(r.config.Storage.DataDir); err == nil {
		_ = next.Close()
		t.Fatal("directory lock released while archive reader active")
	}
	unblock()
	if err := <-read; !errors.Is(err, context.Canceled) || !errors.Is(err, usage.ErrDiskUsageClosed) {
		t.Fatal(err)
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not join reader")
	}
	guard, err := lock.AcquireExistingReadOnly(r.config.Storage.DataDir)
	if err != nil {
		t.Fatal("directory lock retained after join", err)
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOfflineReferenceCloseJoinsArchiveReaderAndWithdrawsLateCanceledScope(t *testing.T) {
	r, _ := summaryRuntime(t, 1)
	r.stopBackgroundWork()
	r.exportUsageParquet()
	cfg := r.config
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &archiveOpenedContext{Context: context.Background(), entered: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	unblock := func() { once.Do(func() { close(c.release) }) }
	t.Cleanup(unblock)
	selected := make(chan *usage.Exporter, 1)
	read := make(chan error, 1)
	done := make(chan error, 1)
	go func() {
		out, err := withUsageDiskReference(ctx, cfg, func(_ context.Context, _ *usage.DiskProjection, e *usage.Exporter, _ usage.ReconciliationReport) error {
			go func() {
				pos, err := e.ReadArchivePosition(c)
				if pos != (usage.ArchivePosition{}) {
					err = errors.Join(err, errors.New("retired reference returned position"))
				}
				read <- err
			}()
			<-c.entered
			selected <- e
			return nil
		}, nil)
		if out != (usage.ReconciliationReport{}) {
			err = errors.Join(err, errors.New("late canceled cleanup returned scope"))
		}
		done <- err
	}()
	var e *usage.Exporter
	select {
	case e = <-selected:
	case <-time.After(10 * time.Second):
		t.Fatal("offline reference not selected")
	}
	awaitArchiveRetired(t, e)
	cancel()
	select {
	case err := <-done:
		t.Fatal("reference returned before archive read exited", err)
	default:
	}
	if next, err := lock.AcquireExistingReadOnly(cfg.Storage.DataDir); err == nil {
		_ = next.Close()
		t.Fatal("offline lock released while reading")
	}
	unblock()
	if err := <-read; !errors.Is(err, usage.ErrDiskUsageClosed) {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("offline reference did not join")
	}
}
