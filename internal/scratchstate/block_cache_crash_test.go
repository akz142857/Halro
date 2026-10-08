package scratchstate

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestScratchBlockBankActualKillDoesNotMakeSavedScratchAuthority(t *testing.T) {
	if path := os.Getenv("HALRO_SCRATCH_BLOCK_KILL_CHILD"); path != "" {
		s, err := Create(context.Background(), path, Options{Budget: testManager(t), MaxDiskBytes: 4 << 20})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Update(context.Background(), func(tx *Transaction) error { return tx.Put([]byte("row"), []byte("initial")) }); err != nil {
			t.Fatal(err)
		}
		err = s.Update(context.Background(), func(tx *Transaction) error {
			if err := tx.Put([]byte("row"), []byte("unpublished")); err != nil {
				return err
			}
			if _, _, err := tx.Get([]byte("row")); err != nil {
				return err
			}
			block, _ := s.cachedBlock(0, s.root.id)
			if block.dirty == 0 {
				t.Fatal("kill boundary has no dirty ancestor")
			}
			for _, file := range s.files {
				if err := file.Sync(); err != nil {
					return err
				}
			}
			fmt.Println("SCRATCH_BANK_ACTIVE_ACTUAL_FDS_SYNCED")
			select {}
		})
		t.Fatal("kill child returned", err)
	}
	path := filepath.Join(t.TempDir(), "interrupted")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run", "^TestScratchBlockBankActualKillDoesNotMakeSavedScratchAuthority$", "-test.count=1")
	child.Env = append(os.Environ(), "HALRO_SCRATCH_BLOCK_KILL_CHILD="+path)
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { child.Process.Kill(); child.Wait() }()
	ready := false
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		if scanner.Text() == "SCRATCH_BANK_ACTIVE_ACTUAL_FDS_SYNCED" {
			ready = true
			break
		}
	}
	if !ready {
		t.Fatal("actual child did not reach the active bank boundary", scanner.Err(), ctx.Err())
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := child.Wait(); err == nil {
		t.Fatal("actual killed child reported success")
	}
	before, err := os.ReadFile(filepath.Join(path, "nodes"))
	if err != nil {
		t.Fatal(err)
	}
	manager := testManager(t)
	used := manager.Snapshot().Used
	if reopened, err := Create(context.Background(), path, Options{Budget: manager, MaxDiskBytes: 4 << 20}); err == nil || reopened != nil || manager.Snapshot().Used != used {
		t.Fatal("saved scratch was adopted or leaked a new creator claim", err)
	}
	after, err := os.ReadFile(filepath.Join(path, "nodes"))
	if err != nil || string(after) != string(before) {
		t.Fatal("interrupted scratch bytes were changed", err)
	}
	// The actual builder must start a new private reducer, rather than importing
	// a saved bank, MAC key or scratch root from the killed process.
	fresh, err := Create(context.Background(), filepath.Join(t.TempDir(), "fresh"), Options{Budget: manager, MaxDiskBytes: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if err := fresh.Update(context.Background(), func(tx *Transaction) error { return tx.Put([]byte("row"), []byte("replayed")) }); err != nil {
		t.Fatal(err)
	}
	verifyRows(t, fresh, map[string][]byte{"row": []byte("replayed")})
	if err := fresh.Discard(); err != nil || manager.Snapshot().Used != used {
		t.Fatal("new actual creator failed to retire", err)
	}
}
