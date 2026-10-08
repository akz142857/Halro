package governance

import (
	"context"
	"errors"
	"github.com/akz142857/Halro/internal/governancelimits"
	"github.com/akz142857/Halro/internal/ownedfile"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNativeActualCloseDebtIsPermanentAndKeepsCacheFees(t *testing.T) {
	budget := governanceCacheBudget(t)
	log, err := OpenWithOptions(filepath.Join(t.TempDir(), "native.log"), make([]byte, 32), Options{CacheBudget: budget, CacheRAMBytes: governancelimits.MinCacheRAMBytes})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(context.Background(), projectionEvent(1)); err != nil {
		t.Fatal(err)
	}
	before := budget.Snapshot().Used
	if err := log.file.Close(); err != nil {
		t.Fatal(err)
	}
	first := log.Close()
	if !errors.Is(first, ownedfile.ErrRetirement) {
		t.Fatal(first)
	}
	if second := log.Close(); second != first {
		t.Fatal("repeated Close erased or replaced actual debt", first, second)
	}
	if got := budget.Snapshot().Used; got != before {
		t.Fatal("unknown actual close refunded native cache", before, got)
	}
	if _, err := log.Append(context.Background(), projectionEvent(1)); err == nil {
		t.Fatal("closed native writer accepted append")
	}
	if _, err := log.Replay(nil); err == nil {
		t.Fatal("closed native writer accepted replay")
	}
}

func TestNativeConstructorActualCloseDebtCannotRefundCache(t *testing.T) {
	budget := governanceCacheBudget(t)
	log, err := OpenWithOptions(filepath.Join(t.TempDir(), "native.log"), make([]byte, 32), Options{CacheBudget: budget, CacheRAMBytes: governancelimits.MinCacheRAMBytes, WrapDurability: func(file *os.File) DurabilityWriter {
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		return nil
	}})
	if log != nil || !errors.Is(err, ownedfile.ErrRetirement) {
		t.Fatal(log, err)
	}
	if got := budget.Snapshot().Used.RAM; got < governancelimits.MinCacheRAMBytes {
		t.Fatal("constructor forgot actual descriptor debt", got)
	}
}

func TestNativeCloseJoinsActualSealReaderAndRetainsUnknownIOFee(t *testing.T) {
	log, err := Open(filepath.Join(t.TempDir(), "native.log"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(context.Background(), projectionEvent(1)); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	log.closeReaderFile = func(file *os.File) error {
		if err := file.Close(); err != nil {
			return err
		}
		close(entered)
		<-release
		return file.Close()
	}
	budget := governanceSealBudget(t)
	result := make(chan error, 1)
	go func() { _, err := log.SealReference(context.Background(), budget, log.Summary()); result <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("actual reader Close not reached")
	}
	stopped := make(chan error, 1)
	go func() { stopped <- log.Close() }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		log.mu.Lock()
		closing := log.closed
		log.mu.Unlock()
		if closing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Close did not withdraw admission")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-stopped:
		t.Fatal("native Close escaped actual reader", err)
	default:
	}
	before := budget.Snapshot().Used
	close(release)
	if err := <-result; !errors.Is(err, ownedfile.ErrRetirement) {
		t.Fatal(err)
	}
	first := <-stopped
	if !errors.Is(first, ownedfile.ErrRetirement) || log.Close() != first {
		t.Fatal("native Close lost actual reader debt", first)
	}
	if got := budget.Snapshot().Used; got != before {
		t.Fatal("actual unknown reader refunded IO", before, got)
	}
}
