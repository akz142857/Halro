package limiter

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Heimdall/internal/domain"
)

func TestConcurrentProjectsHaveIndependentStateLocks(t *testing.T) {
	manager := New()
	now := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	const projects = 64
	var wait sync.WaitGroup
	errorsByProject := make(chan error, projects)
	for index := 0; index < projects; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			lease, err := manager.Acquire(domain.Project{ID: fmt.Sprintf("p_%d", index), MaxConcurrency: 1}, 1, now)
			if err == nil {
				lease.Release()
			}
			errorsByProject <- err
		}(index)
	}
	wait.Wait()
	close(errorsByProject)
	for err := range errorsByProject {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestAtomicPolicyAdmissionAndRelease(t *testing.T) {
	manager := New()
	now := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	project := domain.Project{
		ID:             "project_1",
		RPM:            2,
		TPM:            10,
		MaxConcurrency: 1,
	}
	first, err := manager.Acquire(project, 6, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Acquire(project, 1, now); !errors.Is(err, ErrConcurrency) {
		t.Fatalf("expected concurrency rejection, got %v", err)
	}
	first.Release()
	first.Release()
	if _, err := manager.Acquire(project, 5, now); !errors.Is(err, ErrTPM) {
		t.Fatalf("expected TPM rejection, got %v", err)
	}
	second, err := manager.Acquire(project, 4, now)
	if err != nil {
		t.Fatal(err)
	}
	second.Release()
	if _, err := manager.Acquire(project, 0, now); !errors.Is(err, ErrRPM) {
		t.Fatalf("expected RPM rejection, got %v", err)
	} else {
		var limitErr *Error
		if !errors.As(err, &limitErr) || limitErr.RetryAfter != 30*time.Second {
			t.Fatalf("unexpected RPM retry delay: %#v", err)
		}
	}
}

func TestBucketsRefillAndZeroMeansUnlimited(t *testing.T) {
	manager := New()
	now := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	project := domain.Project{ID: "project_1", RPM: 1}
	lease, err := manager.Acquire(project, 1_000_000, now)
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	if _, err := manager.Acquire(project, 1, now); !errors.Is(err, ErrRPM) {
		t.Fatalf("expected RPM rejection, got %v", err)
	} else {
		var limitErr *Error
		if !errors.As(err, &limitErr) || limitErr.RetryAfter != time.Minute {
			t.Fatalf("unexpected retry delay: %#v", err)
		}
	}
	lease, err = manager.Acquire(project, 1, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	unlimited := domain.Project{ID: "unlimited"}
	for index := 0; index < 100; index++ {
		lease, err = manager.Acquire(unlimited, 1_000_000, now)
		if err != nil {
			t.Fatal(err)
		}
		lease.Release()
	}
}

func TestTPMReconcileRefundsUnusedCapacityAndChargesOverageDebt(t *testing.T) {
	now := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	t.Run("refund capped at bucket capacity", func(t *testing.T) {
		manager := New()
		project := domain.Project{ID: "refund", TPM: 10}
		lease, err := manager.Acquire(project, 10, now)
		if err != nil {
			t.Fatal(err)
		}
		if err := lease.Reconcile(4, now); err != nil {
			t.Fatal(err)
		}
		lease.Release()
		lease, err = manager.Acquire(project, 6, now)
		if err != nil {
			t.Fatalf("unused output capacity was not refunded: %v", err)
		}
		lease.Release()
		if _, err := manager.Acquire(project, 1, now); !errors.Is(err, ErrTPM) {
			t.Fatalf("refund exceeded actual unused capacity: %v", err)
		}
	})
	t.Run("actual over estimate creates debt", func(t *testing.T) {
		manager := New()
		project := domain.Project{ID: "debt", TPM: 10}
		lease, err := manager.Acquire(project, 5, now)
		if err != nil {
			t.Fatal(err)
		}
		if err := lease.Reconcile(8, now); err != nil {
			t.Fatal(err)
		}
		lease.Release()
		if _, err := manager.Acquire(project, 3, now); !errors.Is(err, ErrTPM) {
			t.Fatalf("actual overage did not reduce future capacity: %v", err)
		}
		lease, err = manager.Acquire(project, 2, now)
		if err != nil {
			t.Fatal(err)
		}
		lease.Release()
	})
	t.Run("in flight reconcile preserves newer limit", func(t *testing.T) {
		manager := New()
		oldProject := domain.Project{ID: "updated", TPM: 10}
		oldLease, err := manager.Acquire(oldProject, 5, now)
		if err != nil {
			t.Fatal(err)
		}
		newProject := domain.Project{ID: "updated", TPM: 20}
		refreshLease, err := manager.Acquire(newProject, 0, now)
		if err != nil {
			t.Fatal(err)
		}
		refreshLease.Release()
		if err := oldLease.Reconcile(0, now); err != nil {
			t.Fatal(err)
		}
		oldLease.Release()
		lease, err := manager.Acquire(newProject, 20, now)
		if err != nil {
			t.Fatalf("old request reset updated TPM limit: %v", err)
		}
		lease.Release()
	})
}
