package resourcebudget

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/akz142857/Halro/internal/ownedfile"
)

// This fixture exercises only actual FD retirement and shared fee lifecycle.
// It makes no physical extent/OS qualification claim (those run on ext4).
func physicalCandidateCloseFixture(t *testing.T) (*PhysicalCandidate, *Manager, Permit) {
	t.Helper()
	manager := fixtureManager(t)
	parent, err := manager.TryAcquire(Background, Cost{Disk: 100})
	if err != nil {
		t.Fatal(err)
	}
	disk, err := parent.RetainCovering(manager, Cost{Disk: 100})
	if err != nil {
		t.Fatal(err)
	}
	work, err := manager.TryAcquire(Background, Cost{RAM: 100})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "real-original-target")
	file, owner, err := ownedfile.Create(path, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	return &PhysicalCandidate{file: file, owner: owner, path: path, bytes: 100, manager: manager, disk: disk, work: work, ready: true}, manager, parent
}

func TestPhysicalCandidateConcurrentCloseJoinsOneActualResult(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		name := "known"
		if uncertain {
			name = "actual-unknown"
		}
		t.Run(name, func(t *testing.T) {
			c, manager, parent := physicalCandidateCloseFixture(t)
			before := manager.Snapshot().Used
			if uncertain {
				if err := c.file.Close(); err != nil {
					t.Fatal(err)
				}
			}
			results := make(chan error, 24)
			var group sync.WaitGroup
			for i := 0; i < cap(results); i++ {
				group.Add(1)
				go func() { defer group.Done(); results <- c.CloseFile() }()
			}
			group.Wait()
			close(results)
			for err := range results {
				if uncertain && !errors.Is(err, ownedfile.ErrRetirement) || !uncertain && err != nil {
					t.Fatal("different actual Close result", err)
				}
			}
			if uncertain {
				for i := 0; i < 2; i++ {
					if err := c.ClosePreserving(); !errors.Is(err, ownedfile.ErrRetirement) || manager.Snapshot().Used != before {
						t.Fatal("unknown exit lost fee", err, manager.Snapshot())
					}
				}
				if _, err := os.Stat(c.path); err != nil {
					t.Fatal("unknown target was removed", err)
				}
			} else {
				if err := c.ClosePreserving(); err != nil {
					t.Fatal(err)
				}
				if err := c.ClosePreserving(); err != nil {
					t.Fatal("known repeated Close leaked", err)
				}
				if manager.Snapshot().Used.Disk != before.Disk {
					t.Fatal("Close refunded existing physical target")
				}
				if err := c.Discard(); err != nil {
					t.Fatal(err)
				}
				if err := c.Discard(); err != nil {
					t.Fatal("known repeated Discard", err)
				}
				if err := parent.Release(); err != nil {
					t.Fatal(err)
				}
				zeroUsage(t, manager)
			}
		})
	}
}

func TestPhysicalCandidateClaimedClosePreservesLiveOriginalDescriptor(t *testing.T) {
	c, manager, _ := physicalCandidateCloseFixture(t)
	c.claimed = true // lifecycle marker only; no claim of physical range permission
	before := manager.Snapshot().Used
	for i := 0; i < 2; i++ {
		if err := c.ClosePreserving(); !errors.Is(err, ownedfile.ErrRetirement) || manager.Snapshot().Used != before {
			t.Fatal("Close erased unresolved actual child", err)
		}
	}
	if _, err := c.file.Stat(); err != nil {
		t.Fatal("parent closed child's actual live descriptor", err)
	}
	if err := c.CloseFile(); err != nil {
		t.Fatal(err)
	}
}
