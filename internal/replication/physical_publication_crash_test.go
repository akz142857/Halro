package replication

import (
	"bytes"
	"context"
	"errors"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

func TestPhysicalPublicationSIGKILLChild(t *testing.T) {
	path := os.Getenv("HALRO_HA_PHYSICAL_PUBLICATION_CHILD")
	if path == "" {
		t.Skip("actual subprocess only")
	}
	key := bytes.Repeat([]byte{32}, 32)
	state, err := ReadState(filepath.Join(path, "state.json"), key)
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewStatePublisher(filepath.Join(path, "state.json"), key, state)
	if err != nil {
		t.Fatal(err)
	}
	j, err := OpenExistingOrderingJournalWithOptions(filepath.Join(path, "ordering.journal"), key, state.ClusterID, state.Incarnation, state.DurableIndex, state.OrderingHeadMAC, OrderingJournalOptions{MaxDiskBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	m := physicalPublicationBudget(t)
	fee, err := m.TryAcquire(resourcebudget.IO, resourcebudget.Cost{Disk: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	phase := os.Getenv("HALRO_HA_PHYSICAL_PUBLICATION_PHASE")
	_, err = p.PreparePhysicalPublication(context.Background(), j, m, PhysicalPublicationOptions{Records: 8, OrderingDisk: fee, phase: func(actual string) error {
		if phase == actual {
			if err := syscall.Kill(os.Getpid(), syscall.SIGKILL); err != nil {
				return err
			}
			select {}
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	record, err := j.Append(testLeadershipRecord(1, 7, "actual-durable"))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.PublishPrimary(PrimaryProgress{DurableIndex: 1, ConfirmedIndex: 1, OrderingHeadMAC: record.MAC, Projection: ProjectionState{Index: 1}}); err != nil {
		t.Fatal(err)
	}
	t.Fatal("did not reach actual SIGKILL phase", phase)
}

func TestPhysicalPublicationActualSIGKILLRequalifiesIndependentParents(t *testing.T) {
	if os.Getenv("HALRO_TERMINAL_ENOSPC_FS") != "ext4" {
		t.Skip("requires isolated ext4")
	}
	for _, phase := range []string{"bank-created", "receipt-synced", "ordering-allocated", "member-exchanged"} {
		t.Run(phase, func(t *testing.T) {
			p, j, _, _ := physicalPublicationFixture(t)
			path := filepath.Dir(p.path)
			p.Close()
			if err := j.Close(); err != nil {
				t.Fatal(err)
			}
			child := exec.Command(os.Args[0], "-test.run=^TestPhysicalPublicationSIGKILLChild$", "-test.count=1", "-test.timeout=30s")
			child.Env = append(os.Environ(), "HALRO_HA_PHYSICAL_PUBLICATION_CHILD="+path, "HALRO_HA_PHYSICAL_PUBLICATION_PHASE="+phase)
			output, err := child.CombinedOutput()
			var exited *exec.ExitError
			if !errors.As(err, &exited) {
				t.Fatalf("no actual child crash: %v %s", err, output)
			}
			status, ok := exited.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatalf("wrong exit: %v %s", err, output)
			}
			key := bytes.Repeat([]byte{32}, 32)
			member, err := ReadState(p.path, key)
			if err != nil {
				t.Fatal(err)
			}
			fresh, err := NewStatePublisher(p.path, key, member)
			if err != nil {
				t.Fatal(err)
			}
			defer fresh.Close()
			journal, err := OpenExistingOrderingJournalWithOptions(filepath.Join(path, "ordering.journal"), key, member.ClusterID, member.Incarnation, member.DurableIndex, member.OrderingHeadMAC, OrderingJournalOptions{MaxDiskBytes: 1 << 20})
			if err != nil {
				t.Fatal(err)
			}
			defer journal.Close()
			manager := physicalPublicationBudget(t)
			fee, err := manager.TryAcquire(resourcebudget.IO, resourcebudget.Cost{Disk: 1 << 20})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := fresh.PreparePhysicalPublication(context.Background(), journal, manager, PhysicalPublicationOptions{Records: 8, OrderingDisk: fee, Restart: true}); err != nil {
				t.Fatal("independent parents could not resume", err)
			}
			if phase == "bank-created" {
				if _, err := os.Stat(p.path + ".physical.unqualified-0/bank"); err != nil {
					t.Fatal("unknown creator bank was not preserved", err)
				}
			}
			if phase == "member-exchanged" && member.ConfirmedIndex != 1 {
				t.Fatal("lost durably exchanged source progress", member)
			}
			if _, err := fresh.Promise(8); err != nil {
				t.Fatal("actual reopened publisher unusable", err)
			}
		})
	}
}
