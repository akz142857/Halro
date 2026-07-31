package provider

import (
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

func TestProviderConcurrencyLimitAndIdempotentRelease(t *testing.T) {
	manager := NewConcurrencyManager()
	first, err := manager.Acquire("provider_1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Acquire("provider_1", 1); !errors.Is(err, ErrConcurrency) {
		t.Fatalf("expected concurrency rejection, got %v", err)
	}
	if second, err := manager.Acquire("provider_2", 1); err != nil {
		t.Fatalf("independent provider was blocked: %v", err)
	} else {
		second.Release()
	}
	first.Release()
	first.Release()
	if active := manager.Active(); len(active) != 0 {
		t.Fatalf("active leases leaked: %#v", active)
	}
	if next, err := manager.Acquire("provider_1", 1); err != nil {
		t.Fatalf("released capacity was not reusable: %v", err)
	} else {
		next.Release()
	}
}

func TestProviderConcurrencyNeverExceedsLimit(t *testing.T) {
	manager := NewConcurrencyManager()
	const workers = 1000
	var admitted atomic.Int64
	var rejected atomic.Int64
	var peak atomic.Int64
	start := make(chan struct{})
	release := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(workers)
	for range workers {
		go func() {
			defer wait.Done()
			<-start
			lease, err := manager.Acquire("provider_1", 7)
			if errors.Is(err, ErrConcurrency) {
				rejected.Add(1)
				return
			}
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			active := manager.Active()["provider_1"]
			for current := peak.Load(); active > current && !peak.CompareAndSwap(current, active); current = peak.Load() {
			}
			admitted.Add(1)
			<-release
			lease.Release()
		}()
	}
	close(start)
	for admitted.Load()+rejected.Load() < workers {
		runtime.Gosched()
	}
	close(release)
	wait.Wait()
	if peak.Load() > 7 || admitted.Load() != 7 || rejected.Load() != workers-7 {
		t.Fatalf("admitted=%d rejected=%d peak=%d", admitted.Load(), rejected.Load(), peak.Load())
	}
	if len(manager.Active()) != 0 {
		t.Fatal("provider concurrency leases leaked")
	}
}

func TestProviderConcurrencyHotReloadedLimitSeesUnlimitedInflightCalls(t *testing.T) {
	manager := NewConcurrencyManager()
	first, err := manager.Acquire("provider_1", 0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Acquire("provider_1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Acquire("provider_1", 1); !errors.Is(err, ErrConcurrency) {
		t.Fatalf("lowered limit ignored in-flight calls: %v", err)
	}
	first.Release()
	if _, err := manager.Acquire("provider_1", 1); !errors.Is(err, ErrConcurrency) {
		t.Fatalf("limit admitted while one call remained active: %v", err)
	}
	second.Release()
	if lease, err := manager.Acquire("provider_1", 1); err != nil {
		t.Fatalf("limit did not admit after drain: %v", err)
	} else {
		lease.Release()
	}
}
