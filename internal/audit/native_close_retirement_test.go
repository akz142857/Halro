package audit

import (
	"context"
	"errors"
	"github.com/akz142857/Halro/internal/auditlimits"
	"github.com/akz142857/Halro/internal/ownedfile"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNativeActualCloseDebtIsPermanentAndKeepsCacheFees(t *testing.T) {
	budget := auditCacheBudget(t)
	log, err := OpenWithOptions(filepath.Join(t.TempDir(), "native.log"), make([]byte, 32), Options{CacheBudget: budget, CacheRAMBytes: auditlimits.MinCacheRAMBytes})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(context.Background(), validEvent(1, "key.create")); err != nil {
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
	if _, err := log.Append(context.Background(), validEvent(1, "key.create")); err == nil {
		t.Fatal("closed native writer accepted append")
	}
	if _, err := log.Replay(nil); err == nil {
		t.Fatal("closed native writer accepted replay")
	}
}

func TestNativeConstructorActualCloseDebtCannotRefundCache(t *testing.T) {
	budget := auditCacheBudget(t)
	log, err := OpenWithOptions(filepath.Join(t.TempDir(), "native.log"), make([]byte, 32), Options{CacheBudget: budget, CacheRAMBytes: auditlimits.MinCacheRAMBytes, WrapDurability: func(file *os.File) DurabilityWriter {
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		return nil
	}})
	if log != nil || !errors.Is(err, ownedfile.ErrRetirement) {
		t.Fatal(log, err)
	}
	if got := budget.Snapshot().Used.RAM; got < auditlimits.MinCacheRAMBytes {
		t.Fatal("constructor forgot actual descriptor debt", got)
	}
}

func TestAuditReplayActualReaderCloseDebtWithdrawsOwnerAndKeepsCache(t *testing.T) {
	budget := auditCacheBudget(t)
	log, err := OpenWithOptions(filepath.Join(t.TempDir(), "native.log"), make([]byte, 32), Options{CacheBudget: budget, CacheRAMBytes: auditlimits.MinCacheRAMBytes})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(context.Background(), validEvent(1, "key.create")); err != nil {
		t.Fatal(err)
	}
	log.closeReplayFile = func(file *os.File) error {
		if err := file.Close(); err != nil {
			return err
		}
		return file.Close()
	}
	before := budget.Snapshot().Used
	if summary, err := log.Replay(nil); !errors.Is(err, ownedfile.ErrRetirement) || summary != (Summary{}) {
		t.Fatal(summary, err)
	}
	if _, err := log.Append(context.Background(), validEvent(2, "key.create")); err == nil {
		t.Fatal("reader debt did not withdraw append")
	}
	first := log.Close()
	if !errors.Is(first, ownedfile.ErrRetirement) || log.Close() != first {
		t.Fatal(first)
	}
	if got := budget.Snapshot().Used; got != before {
		t.Fatal("reader debt refunded lifetime owner", before, got)
	}
}

func TestNativeCloseJoinsActualSealReaderAndRetainsUnknownIOFee(t *testing.T) {
	log, err := Open(filepath.Join(t.TempDir(), "native.log"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(context.Background(), validEvent(1, "key.create")); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	log.closeReplayFile = func(file *os.File) error {
		if err := file.Close(); err != nil {
			return err
		}
		close(entered)
		<-release
		return file.Close()
	}
	budget := auditSealBudget(t)
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
